//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// forwarded are the signals passed on to the child's process group. The
// runner's run.sh traps INT and TERM (RUNNER_MANUALLY_TRAP_SIG=1 in the
// upstream image) and hands them to its listener; the rest are passed
// on so the wrapper is as transparent as the shell it replaces as PID 1.
var forwarded = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT,
	syscall.SIGUSR1, syscall.SIGUSR2,
}

// drainTimeout bounds how long the wrapper waits, after the child has
// exited, for the last of its output. A grandchild that inherited the
// pipe (a job's background process) can hold it open forever; the
// container ends when PID 1 does, so waiting longer buys nothing.
const drainTimeout = 2 * time.Second

// supervisor runs one child and stays until it exits.
type supervisor struct {
	argv           []string
	stdin          *os.File
	stdout, stderr io.Writer
	lines          chan<- string
	dropped        *atomic.Uint64
	// signals delivers the signals to forward; nil means the process's
	// own (signal.Notify). Tests inject their own.
	signals <-chan os.Signal
}

// run starts the child in its own process group, copies and tees its
// output, forwards signals to its group, reaps every process that is
// reparented to the wrapper, and returns the child's exit code: its
// status, or 128+N when signal N killed it (a shell's convention).
//
// It never returns while the child runs. An error means the child never
// started.
func (s *supervisor) run() (int, error) {
	// Orphans of the child must come to us to be reaped: as PID 1 they
	// do anyway; anywhere else (tests, a shell) PR_SET_CHILD_SUBREAPER
	// makes it so. Failure only means zombies outside a container.
	_ = setChildSubreaper()

	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	defer signal.Stop(sigchld)

	sigs := s.signals
	if sigs == nil {
		ch := make(chan os.Signal, 8)
		signal.Notify(ch, forwarded...)
		defer signal.Stop(ch)
		sigs = ch
	}

	path, err := exec.LookPath(s.argv[0])
	if err != nil {
		return 127, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	stdin := s.stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	proc, err := os.StartProcess(path, s.argv, &os.ProcAttr{
		Files: []*os.File{stdin, outW, errW},
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	// The parent's copies of the write ends must close, or the readers
	// never see EOF.
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		_ = outR.Close()
		_ = errR.Close()
		return 126, err
	}
	pid := proc.Pid

	var copiers sync.WaitGroup
	copiers.Add(2)
	go func() { defer copiers.Done(); copyTee(outR, s.stdout, newLineFeed(s.lines, s.dropped)) }()
	go func() { defer copiers.Done(); copyTee(errR, s.stderr, newLineFeed(s.lines, s.dropped)) }()

	// A periodic reap as well as SIGCHLD: signals coalesce, and a reap
	// that is never prompted again would leave the child's status unread.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	var status syscall.WaitStatus
	for exited := false; !exited; {
		select {
		case sig := <-sigs:
			forward(pid, sig)
		case <-sigchld:
			status, exited = reap(pid)
		case <-tick.C:
			status, exited = reap(pid)
		}
	}

	done := make(chan struct{})
	go func() { copiers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(drainTimeout):
	}
	return exitCode(status), nil
}

// forward sends sig to the child's whole process group, falling back to
// the child alone.
func forward(pid int, sig os.Signal) {
	n, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	if err := syscall.Kill(-pid, n); err != nil {
		_ = syscall.Kill(pid, n)
	}
}

// reap collects every exited process it can without blocking, and
// reports whether pid was among them.
func reap(pid int) (status syscall.WaitStatus, exited bool) {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || got <= 0 {
			return status, exited
		}
		if got == pid && (ws.Exited() || ws.Signaled()) {
			status, exited = ws, true
		}
	}
}

func exitCode(ws syscall.WaitStatus) int {
	switch {
	case ws.Exited():
		return ws.ExitStatus()
	case ws.Signaled():
		return 128 + int(ws.Signal())
	}
	return 1
}

// prctl(PR_SET_CHILD_SUBREAPER, 1).
func setChildSubreaper() error {
	const prSetChildSubreaper = 36
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return fmt.Errorf("prctl(PR_SET_CHILD_SUBREAPER): %w", e)
	}
	return nil
}
