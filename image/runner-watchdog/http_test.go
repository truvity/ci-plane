package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthzAndMetrics(t *testing.T) {
	t0 := time.Unix(0, 0)
	now := t0
	m := NewMatcher(45*time.Second, t0)
	var dropped atomic.Uint64
	dropped.Store(3)
	srv := httptest.NewServer(handler(m, &dropped, func() time.Time { return now }))
	defer srv.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Fatalf("healthy runner: /healthz = %d, want 200", code)
	}

	m.Observe("[RUNNER x INFO Runner] Skipping message Job. Job message already acquired 'x'. job assignment is invalid: MissingKey", t0)
	now = t0.Add(46 * time.Second)
	code, body := get("/healthz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "job-message-already-acquired") {
		t.Fatalf("stuck runner: /healthz = %d %q, want 503 naming the signature", code, body)
	}

	code, body = get("/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d", code)
	}
	for _, want := range []string{
		"# TYPE runner_watchdog_stuck gauge\nrunner_watchdog_stuck 1\n",
		"runner_watchdog_stuck_detections_total 1\n",
		"runner_watchdog_job_running 0\n",
		"runner_watchdog_jobs_started_total 0\n",
		"runner_watchdog_lines_dropped_total 3\n",
		"runner_watchdog_seconds_since_last_line 46.000\n",
		"runner_watchdog_grace_seconds 45.000\n",
		`runner_watchdog_signature_lines_total{signature="job-message-already-acquired"} 1` + "\n",
		`runner_watchdog_signature_lines_total{signature="registration-not-found"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}
