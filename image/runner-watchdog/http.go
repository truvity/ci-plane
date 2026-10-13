package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"
	"time"
)

// handler serves /healthz and /metrics from the matcher.
//
// /healthz is what the pod's READINESS probe asks: 200 while the runner
// is healthy, 503 while it is stuck. Readiness, never liveness: a stuck
// runner is not fixed by restarting its container (its JIT registration
// is single-use, so a restarted runner cannot register again); the
// remedy is deleting its EphemeralRunner from outside, which the
// arc-runners janitor does for a pod whose runner container is NotReady.
func handler(m *Matcher, dropped *atomic.Uint64, now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		s, _ := m.Evaluate(now())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if s.Stuck {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "stuck: %s for %s with no job running\n", s.Signature, s.ArmedFor.Truncate(time.Second))
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		s, _ := m.Evaluate(now())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, &s, dropped.Load())
	})
	return mux
}

// writeMetrics renders the Prometheus text exposition format by hand: a
// dozen series do not justify a client library in the runner image.
func writeMetrics(w io.Writer, s *Snapshot, dropped uint64) {
	metric := func(name, typ, help string, value any) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, value)
	}
	metric("runner_watchdog_stuck", "gauge",
		"1 while no job runs and a stuck signature has persisted past the grace period.", b2i(s.Stuck))
	metric("runner_watchdog_stuck_detections_total", "counter",
		"Transitions into the stuck state.", s.Detections)
	metric("runner_watchdog_job_running", "gauge",
		"1 between a job's \"Running job:\" line and its \"completed with result:\" line.", b2i(s.JobRunning))
	metric("runner_watchdog_jobs_started_total", "counter",
		"Jobs the runner started.", s.JobsStarted)
	metric("runner_watchdog_jobs_completed_total", "counter",
		"Jobs the runner completed, whatever the result.", s.JobsCompleted)
	metric("runner_watchdog_lines_total", "counter",
		"Log lines the matcher read.", s.Lines)
	metric("runner_watchdog_lines_dropped_total", "counter",
		"Log lines not matched because the matcher fell behind (they still reached the log).", dropped)
	metric("runner_watchdog_seconds_since_last_line", "gauge",
		"Seconds since the runner's last log line (since the watchdog started, before the first).", secs(s.SinceLastLine))
	metric("runner_watchdog_seconds_since_last_job", "gauge",
		"Seconds since a job last started or completed (since the watchdog started, before the first).", secs(s.SinceLastJob))
	metric("runner_watchdog_grace_seconds", "gauge",
		"How long an idle stuck signature must persist before the runner is reported stuck.", secs(s.Grace))

	const name = "runner_watchdog_signature_lines_total"
	_, _ = fmt.Fprintf(w, "# HELP %s Log lines matching each stuck signature, during a job or not.\n# TYPE %s counter\n", name, name)
	keys := make([]string, 0, len(s.SigCounts))
	for k := range s.SigCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "%s{signature=%q} %d\n", name, k, s.SigCounts[k])
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func secs(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Seconds())
}
