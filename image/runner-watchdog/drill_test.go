//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrillFeedsOneLinePerAppearance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drill")
	lines := make(chan string, 4)
	var dropped atomic.Uint64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go drill(ctx, path, 5*time.Millisecond, lines, &dropped)

	expectNone := func(why string) {
		t.Helper()
		select {
		case l := <-lines:
			t.Fatalf("%s: got %q, want nothing", why, l)
		case <-time.After(50 * time.Millisecond):
		}
	}
	expectOne := func(why string) {
		t.Helper()
		select {
		case l := <-lines:
			if l != drillLine {
				t.Fatalf("%s: got %q, want %q", why, l, drillLine)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no line", why)
		}
	}

	expectNone("no file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	expectOne("file appeared")
	expectNone("file still there")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expectNone("file removed")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	expectOne("file appeared again")
	if n := dropped.Load(); n != 0 {
		t.Fatalf("dropped = %d, want 0", n)
	}
}

func TestDrillLineArmsOnlyWhileIdle(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	m := NewMatcher(45*time.Second, t0)

	m.Observe("2026-10-13 12:00:01Z: Running job: build", t0.Add(time.Second))
	m.Observe(drillLine, t0.Add(2*time.Second))
	if s, _ := m.Evaluate(t0.Add(time.Minute)); s.Stuck {
		t.Fatal("a drill during a job must not report stuck")
	}
	m.Observe("2026-10-13 12:01:10Z: Job build completed with result: Succeeded", t0.Add(70*time.Second))
	m.Observe(drillLine, t0.Add(71*time.Second))
	if s, _ := m.Evaluate(t0.Add(100 * time.Second)); s.Stuck {
		t.Fatal("stuck before the grace period")
	}
	s, _ := m.Evaluate(t0.Add(117 * time.Second))
	if !s.Stuck || s.Signature != "drill" {
		t.Fatalf("stuck=%v signature=%q, want stuck on drill", s.Stuck, s.Signature)
	}
	if got := s.SigCounts["drill"]; got != 2 {
		t.Fatalf("drill lines counted = %d, want 2", got)
	}
}
