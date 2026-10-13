//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe to read while a copier writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	stdout, stderr syncBuffer
	lines          chan string
	dropped        atomic.Uint64
	signals        chan os.Signal
}

func newHarness() *harness {
	return &harness{lines: make(chan string, 64), signals: make(chan os.Signal, 1)}
}

func (h *harness) supervisor(script string) *supervisor {
	return &supervisor{
		argv: []string{"sh", "-c", script}, stdout: &h.stdout, stderr: &h.stderr,
		lines: h.lines, dropped: &h.dropped, signals: h.signals,
	}
}

type result struct {
	code int
	err  error
	took time.Duration
}

// start runs the supervisor in the background.
func start(s *supervisor) <-chan result {
	ch := make(chan result, 1)
	go func() {
		t0 := time.Now()
		code, err := s.run()
		ch <- result{code, err, time.Since(t0)}
	}()
	return ch
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wait(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not return")
	}
	return result{}
}

func TestSuperviseExitCodeAndOutput(t *testing.T) {
	h := newHarness()
	r := wait(t, start(h.supervisor(`echo out; echo err >&2; printf 'no newline'; exit 7`)))
	if r.code != 7 {
		t.Errorf("exit code = %d, want 7", r.code)
	}
	if got := h.stdout.String(); got != "out\nno newline" {
		t.Errorf("stdout = %q, copied unchanged", got)
	}
	if got := h.stderr.String(); got != "err\n" {
		t.Errorf("stderr = %q", got)
	}
	close(h.lines)
	var lines []string
	for l := range h.lines {
		lines = append(lines, l)
	}
	if len(lines) != 3 {
		t.Errorf("teed lines = %q, want out, err and the unterminated last line", lines)
	}
}

func TestSuperviseForwardsTermToTheChild(t *testing.T) {
	h := newHarness()
	ch := start(h.supervisor(`trap 'echo got-term; exit 42' TERM; echo ready; while :; do sleep 0.05; done`))
	waitFor(t, "the child to be ready", func() bool { return strings.Contains(h.stdout.String(), "ready") })
	h.signals <- syscall.SIGTERM
	r := wait(t, ch)
	if r.code != 42 {
		t.Errorf("exit code = %d, want the child's own 42", r.code)
	}
	if !strings.Contains(h.stdout.String(), "got-term") {
		t.Error("the child never saw SIGTERM")
	}
}

func TestSuperviseReportsDeathBySignalAsAShellWould(t *testing.T) {
	h := newHarness()
	ch := start(h.supervisor(`echo ready; exec sleep 30`))
	waitFor(t, "the child to be ready", func() bool { return strings.Contains(h.stdout.String(), "ready") })
	h.signals <- syscall.SIGINT
	if r := wait(t, ch); r.code != 128+int(syscall.SIGINT) {
		t.Errorf("exit code = %d, want %d", r.code, 128+int(syscall.SIGINT))
	}
}

// The wrapper never exits before the child: a child that ignores TERM
// keeps the wrapper (and with it the container) alive until it is done.
func TestSuperviseNeverExitsBeforeTheChild(t *testing.T) {
	h := newHarness()
	ch := start(h.supervisor(`trap '' TERM; echo ready; sleep 0.5; exit 3`))
	waitFor(t, "the child to be ready", func() bool { return strings.Contains(h.stdout.String(), "ready") })
	h.signals <- syscall.SIGTERM
	r := wait(t, ch)
	if r.code != 3 {
		t.Errorf("exit code = %d, want 3", r.code)
	}
	if r.took < 400*time.Millisecond {
		t.Errorf("returned after %s, before the child finished", r.took)
	}
}

// An orphan reparented to the wrapper is reaped while the child still
// runs, rather than left as a zombie for the life of the pod.
func TestSuperviseReapsOrphans(t *testing.T) {
	h := newHarness()
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	ch := start(h.supervisor(`(sh -c 'exit 0' & echo $! > ` + pidFile + `); echo ready; sleep 1.5; exit 0`))
	waitFor(t, "the child to be ready", func() bool { return strings.Contains(h.stdout.String(), "ready") })

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the orphan to be reaped", func() bool {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return true // gone: reaped
		}
		// /proc/<pid>/stat: "pid (comm) S ..."; Z is a zombie.
		i := bytes.LastIndexByte(stat, ')')
		return i > 0 && len(stat) > i+2 && stat[i+2] != 'Z'
	})
	if r := wait(t, ch); r.code != 0 {
		t.Errorf("exit code = %d, want 0", r.code)
	}
}

func TestSuperviseMissingCommand(t *testing.T) {
	h := newHarness()
	s := h.supervisor("")
	s.argv = []string{"/no/such/runner.sh"}
	code, err := s.run()
	if err == nil || code != 127 {
		t.Fatalf("code=%d err=%v, want 127 and an error", code, err)
	}
}
