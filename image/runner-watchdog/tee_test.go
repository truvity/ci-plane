package main

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLineFeedSplitsAcrossWrites(t *testing.T) {
	ch := make(chan string, 8)
	var dropped atomic.Uint64
	f := newLineFeed(ch, &dropped)
	f.Write([]byte("one\ntw"))
	f.Write([]byte("o\nthree"))
	f.flush()
	close(ch)
	var got []string
	for l := range ch {
		got = append(got, l)
	}
	if strings.Join(got, "|") != "one|two|three" {
		t.Fatalf("lines = %q", got)
	}
}

func TestLineFeedCutsLongLines(t *testing.T) {
	ch := make(chan string, 4)
	var dropped atomic.Uint64
	f := newLineFeed(ch, &dropped)
	f.Write([]byte(strings.Repeat("x", maxLine+100) + "\nnext\n"))
	if l := <-ch; len(l) != maxLine {
		t.Errorf("long line kept %d bytes, want %d", len(l), maxLine)
	}
	if l := <-ch; l != "next" {
		t.Errorf("line after the long one = %q, want next", l)
	}
}

// The runner must never wait on the matcher: with nobody reading the
// queue, every byte still reaches the log promptly, and what did not fit
// is counted as dropped.
func TestCopyTeeNeverBlocksOnAFullQueue(t *testing.T) {
	const n = 10000
	ch := make(chan string, 1) // nobody reads it
	var dropped atomic.Uint64
	var in bytes.Buffer
	for i := 0; i < n; i++ {
		in.WriteString("[RUNNER 2026-10-13 02:42:38Z INFO Runner] a line of runner output\n")
	}
	want := in.String()
	var out bytes.Buffer

	done := make(chan struct{})
	go func() { copyTee(&in, &out, newLineFeed(ch, &dropped)); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("copyTee blocked on a full queue")
	}
	if out.String() != want {
		t.Fatal("output was altered")
	}
	if got := dropped.Load(); got != n-1 {
		t.Errorf("dropped = %d, want %d (one line fits the queue)", got, n-1)
	}
}
