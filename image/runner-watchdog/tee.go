package main

import (
	"bytes"
	"io"
	"sync/atomic"
)

// maxLine bounds what one line may cost the matcher. A longer line is
// cut there; the rest of it up to the next newline is not matched. The
// bytes still reach stdout untouched: the cap is on the copy that is
// matched, never on the copy that is logged.
const maxLine = 16 << 10

// lineFeed splits a byte stream into lines and offers each to a bounded
// channel WITHOUT waiting: when the matcher falls behind, a line is
// dropped and counted, and the runner's own output never slows down
// because of the watchdog. One feed per stream (stdout, stderr), so a
// partial line on one is never glued to the other.
type lineFeed struct {
	out     chan<- string
	dropped *atomic.Uint64
	buf     []byte
	cut     bool // the current line passed maxLine; skip to its newline
}

func newLineFeed(out chan<- string, dropped *atomic.Uint64) *lineFeed {
	return &lineFeed{out: out, dropped: dropped}
}

func (f *lineFeed) Write(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			f.add(p)
			return
		}
		f.add(p[:i])
		f.emit()
		p = p[i+1:]
	}
}

func (f *lineFeed) add(p []byte) {
	if f.cut {
		return
	}
	if room := maxLine - len(f.buf); len(p) > room {
		p = p[:room]
		f.cut = true
	}
	f.buf = append(f.buf, p...)
}

func (f *lineFeed) emit() {
	line := string(f.buf)
	f.buf, f.cut = f.buf[:0], false
	select {
	case f.out <- line:
	default:
		f.dropped.Add(1)
	}
}

// flush offers a final line that had no newline.
func (f *lineFeed) flush() {
	if len(f.buf) > 0 || f.cut {
		f.emit()
	}
}

// copyTee copies r to w unchanged and feeds every line to f, until r
// ends. A failing w (stdout gone) is not a reason to stop reading: the
// child would block on a full pipe, and a blocked runner is worse than a
// lost log line.
func copyTee(r io.Reader, w io.Writer, f *lineFeed) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			f.Write(buf[:n])
		}
		if err != nil {
			f.flush()
			return
		}
	}
}
