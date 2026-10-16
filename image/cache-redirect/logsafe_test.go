package main

import "testing"

func TestLogSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain/key": "plain/key",
		"a\nb":      "a b",
		"a\r\nb":    "a  b",
		"x\ry\nz":   "x y z",
		"":          "",
	} {
		if got := logSafe(in); got != want {
			t.Errorf("logSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
