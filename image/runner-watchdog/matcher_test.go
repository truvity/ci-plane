package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// feed plays a fixture into m, one line a second from t0, and returns
// the time of the last line.
func feed(t *testing.T, m *Matcher, fixture string, t0 time.Time) time.Time {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	now := t0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		now = now.Add(time.Second)
		m.Observe(sc.Text(), now)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return now
}

func TestMatcherFixtures(t *testing.T) {
	const grace = 45 * time.Second
	cases := []struct {
		fixture       string
		stuck         bool // after the grace period has passed
		signature     string
		jobRunning    bool
		jobsStarted   uint64
		jobsCompleted uint64
	}{
		{fixture: "healthy-idle.log"},
		{fixture: "running-job.log", jobRunning: true, jobsStarted: 1},
		{fixture: "job-completed.log", jobsStarted: 1, jobsCompleted: 1},
		{fixture: "already-acquired.log", stuck: true, signature: "job-message-already-acquired"},
		{fixture: "not-found.log", stuck: true, signature: "job-message-not-found"},
		{fixture: "registration-not-found.log", stuck: true, signature: "registration-not-found"},
		// The job is the evidence the runner works: a signature during it
		// arms nothing.
		{fixture: "signature-during-job.log", jobRunning: true, jobsStarted: 1},
		// A job after the signature: the runner recovered.
		{fixture: "signature-then-job.log", jobRunning: true, jobsStarted: 1},
		// The traced echo of "Running job:" is not the job line itself;
		// counting it would count every job twice.
		{fixture: "job-text-in-trace-only.log", stuck: true, signature: "job-message-not-found"},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSuffix(tc.fixture, ".log"), func(t *testing.T) {
			t0 := time.Date(2026, 10, 13, 2, 41, 0, 0, time.UTC)
			m := NewMatcher(grace, t0)
			last := feed(t, m, tc.fixture, t0)

			// Never stuck before the grace period: a signature has to persist.
			if s, _ := m.Evaluate(last); s.Stuck {
				t.Fatalf("stuck immediately after the last line; want only after %s", grace)
			}

			s, changed := m.Evaluate(last.Add(grace + time.Second))
			if s.Stuck != tc.stuck {
				t.Fatalf("stuck = %v, want %v (signature %q)", s.Stuck, tc.stuck, s.Signature)
			}
			if changed != tc.stuck {
				t.Errorf("changed = %v, want %v", changed, tc.stuck)
			}
			if tc.stuck && s.Signature != tc.signature {
				t.Errorf("signature = %q, want %q", s.Signature, tc.signature)
			}
			if s.JobRunning != tc.jobRunning || s.JobsStarted != tc.jobsStarted || s.JobsCompleted != tc.jobsCompleted {
				t.Errorf("jobs: running=%v started=%d completed=%d, want running=%v started=%d completed=%d",
					s.JobRunning, s.JobsStarted, s.JobsCompleted, tc.jobRunning, tc.jobsStarted, tc.jobsCompleted)
			}
			wantDetections := uint64(0)
			if tc.stuck {
				wantDetections = 1
			}
			// A second evaluation is not a second detection.
			if s, _ = m.Evaluate(last.Add(2 * grace)); s.Detections != wantDetections {
				t.Errorf("detections = %d, want %d", s.Detections, wantDetections)
			}
		})
	}
}

func TestMatcherSignatureCountsDuringJob(t *testing.T) {
	t0 := time.Unix(0, 0)
	m := NewMatcher(time.Second, t0)
	feed(t, m, "signature-during-job.log", t0)
	s, _ := m.Evaluate(t0.Add(time.Hour))
	if got := s.SigCounts["job-message-already-acquired"]; got != 1 {
		t.Errorf("signature lines counted = %d, want 1 (counted, not acted on)", got)
	}
}

func TestMatcherRecoversWhenAJobStarts(t *testing.T) {
	t0 := time.Unix(0, 0)
	m := NewMatcher(10*time.Second, t0)
	m.Observe("[RUNNER 2026-10-13 02:42:38Z INFO Runner] Skipping message Job. Job message not found 'x'. job was canceled", t0)
	if s, _ := m.Evaluate(t0.Add(11 * time.Second)); !s.Stuck {
		t.Fatal("not stuck after the grace period")
	}
	m.Observe("2026-10-13 02:43:00Z: Running job: build\r", t0.Add(12*time.Second))
	s, changed := m.Evaluate(t0.Add(13 * time.Second))
	if s.Stuck || !changed {
		t.Fatalf("stuck=%v changed=%v after a job started; want recovered", s.Stuck, changed)
	}
	// The job ends, the runner idles with no new signature: still healthy.
	m.Observe("2026-10-13 02:44:00Z: Job build completed with result: Canceled", t0.Add(14*time.Second))
	if s, _ := m.Evaluate(t0.Add(time.Hour)); s.Stuck {
		t.Fatal("stuck after a clean job with no new signature")
	}
}

func TestMatcherGraceIsMeasuredFromTheFirstIdleSignature(t *testing.T) {
	t0 := time.Unix(0, 0)
	m := NewMatcher(45*time.Second, t0)
	sig := "[RUNNER x INFO Runner] Skipping message Job. Job message already acquired 'x'. job assignment is invalid: MissingKey"
	m.Observe(sig, t0)
	m.Observe(sig, t0.Add(40*time.Second)) // a repeat does not restart the clock
	if s, _ := m.Evaluate(t0.Add(44 * time.Second)); s.Stuck {
		t.Fatal("stuck before the grace period")
	}
	if s, _ := m.Evaluate(t0.Add(45 * time.Second)); !s.Stuck {
		t.Fatal("not stuck at the grace period")
	}
}

func TestMatcherSinceCounters(t *testing.T) {
	t0 := time.Unix(1000, 0)
	m := NewMatcher(time.Second, t0)
	s, _ := m.Evaluate(t0.Add(30 * time.Second))
	if s.SinceLastLine != 30*time.Second || s.SinceLastJob != 30*time.Second {
		t.Errorf("before any line: since line %s, since job %s; want 30s each (since start)", s.SinceLastLine, s.SinceLastJob)
	}
	m.Observe("2026-10-13 02:43:00Z: Running job: build", t0.Add(40*time.Second))
	m.Observe("noise", t0.Add(50*time.Second))
	s, _ = m.Evaluate(t0.Add(60 * time.Second))
	if s.SinceLastLine != 10*time.Second || s.SinceLastJob != 20*time.Second {
		t.Errorf("since line %s, since job %s; want 10s, 20s", s.SinceLastLine, s.SinceLastJob)
	}
}
