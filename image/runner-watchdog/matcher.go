package main

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// A signature is one log line shape that, on an idle runner, means the
// runner will never take another job (upstream
// actions/actions-runner-controller#4307).
//
// The first three are the runner listener's own trace line when it gives
// up on a job message (actions/runner, src/Runner.Listener/Runner.cs:
// `Trace.Info($"Skipping message Job. {ex.Message}")`), one per HTTP
// status the acquirejob call can fail with
// (src/Sdk/RSWebApi/RunServiceHttpClient.cs: 404, 409, 422). The text
// after the message id is GitHub's own error body, e.g.
// "job was canceled" or "job assignment is invalid: MissingKey". The
// trace reaches stdout because the upstream runner image sets
// ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1:
//
//	[RUNNER 2025-11-07 14:43:38Z INFO Runner] Skipping message Job. Job message already acquired '<id>'. job assignment is invalid: MissingKey
//	[RUNNER 2025-11-07 14:43:16Z INFO Runner] Skipping message Job. Job message not found '<id>'. job was canceled
//
// The fourth is the broker session's token refresh failing because the
// runner's JIT registration is gone. The runner retries it forever:
//
//	[RUNNER 2025-11-07 15:33:40Z ERR  BrokerServer] GitHub.Services.OAuth.VssOAuthTokenRequestException: Registration <id> was not found.
type signature struct {
	name string
	re   *regexp.Regexp
}

var signatures = []signature{
	{"job-message-already-acquired", regexp.MustCompile(`Skipping message Job\. Job message already acquired `)},
	{"job-message-not-found", regexp.MustCompile(`Skipping message Job\. Job message not found `)},
	{"job-unprocessable", regexp.MustCompile(`Skipping message Job\. Unprocessable job `)},
	{"registration-not-found", regexp.MustCompile(`Registration [0-9A-Fa-f-]+ was not found`)},
}

// The job lifecycle as the listener prints it to the terminal
// (src/Runner.Listener/JobDispatcher.cs), each line starting with
// `{DateTime.UtcNow:u}: `:
//
//	2026-10-13 02:41:54Z: Running job: build
//	2026-10-13 02:43:10Z: Job build completed with result: Succeeded
//
// Anchored at the start of the line on purpose: the same text is ALSO
// traced as `[RUNNER ... INFO Terminal] WRITE LINE: 2026-...Z: Running job:
// build`, and counting both would count every job twice.
var (
	jobStarted   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}Z: Running job: `)
	jobCompleted = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}Z: Job .* completed with result: \S+$`)
)

// Matcher folds runner log lines into one verdict: STUCK when no job is
// running AND a stuck signature, first seen while idle, has persisted for
// at least grace. A signature seen while a job runs is counted but arms
// nothing: the job is the evidence that the runner is working. A job
// starting clears an armed signature: the runner recovered.
type Matcher struct {
	mu    sync.Mutex
	grace time.Duration
	start time.Time

	jobRunning    bool
	jobsStarted   uint64
	jobsCompleted uint64
	lastJob       time.Time // last start or completion; zero until one

	armedAt  time.Time // first idle signature since the last job start
	armedSig string

	stuck      bool
	detections uint64

	lines     uint64
	lastLine  time.Time
	sigCounts map[string]uint64
}

// NewMatcher returns a matcher whose clock starts at start.
func NewMatcher(grace time.Duration, start time.Time) *Matcher {
	counts := make(map[string]uint64, len(signatures))
	for _, s := range signatures {
		counts[s.name] = 0
	}
	return &Matcher{grace: grace, start: start, sigCounts: counts}
}

// Observe feeds one line (without its newline) seen at now.
func (m *Matcher) Observe(line string, now time.Time) {
	line = strings.TrimRight(line, "\r")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines++
	m.lastLine = now

	switch {
	case jobStarted.MatchString(line):
		m.jobRunning = true
		m.jobsStarted++
		m.lastJob = now
		m.armedAt, m.armedSig = time.Time{}, ""
		return
	case jobCompleted.MatchString(line):
		m.jobRunning = false
		m.jobsCompleted++
		m.lastJob = now
		return
	}

	for _, s := range signatures {
		if !s.re.MatchString(line) {
			continue
		}
		m.sigCounts[s.name]++
		if !m.jobRunning && m.armedAt.IsZero() {
			m.armedAt, m.armedSig = now, s.name
		}
		return
	}
}

// Snapshot is the matcher's state at one instant.
type Snapshot struct {
	Now           time.Time
	Grace         time.Duration
	Stuck         bool
	Signature     string        // the armed signature, if any
	ArmedFor      time.Duration // how long it has been armed
	JobRunning    bool
	JobsStarted   uint64
	JobsCompleted uint64
	Detections    uint64
	Lines         uint64
	SinceLastLine time.Duration // since the watchdog started, before any line
	SinceLastJob  time.Duration // since the watchdog started, before any job
	SigCounts     map[string]uint64
}

// Evaluate recomputes the verdict at now. changed reports a transition
// into or out of STUCK; each transition INTO it is one detection.
func (m *Matcher) Evaluate(now time.Time) (s Snapshot, changed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stuck := !m.jobRunning && !m.armedAt.IsZero() && now.Sub(m.armedAt) >= m.grace
	if stuck != m.stuck {
		changed = true
		if stuck {
			m.detections++
		}
		m.stuck = stuck
	}

	s = Snapshot{
		Now:           now,
		Grace:         m.grace,
		Stuck:         m.stuck,
		Signature:     m.armedSig,
		JobRunning:    m.jobRunning,
		JobsStarted:   m.jobsStarted,
		JobsCompleted: m.jobsCompleted,
		Detections:    m.detections,
		Lines:         m.lines,
		SinceLastLine: since(now, m.lastLine, m.start),
		SinceLastJob:  since(now, m.lastJob, m.start),
		SigCounts:     make(map[string]uint64, len(m.sigCounts)),
	}
	if !m.armedAt.IsZero() {
		s.ArmedFor = now.Sub(m.armedAt)
	}
	for k, v := range m.sigCounts {
		s.SigCounts[k] = v
	}
	return s, changed
}

func since(now, t, fallback time.Time) time.Duration {
	if t.IsZero() {
		t = fallback
	}
	if d := now.Sub(t); d > 0 {
		return d
	}
	return 0
}
