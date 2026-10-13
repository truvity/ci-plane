//go:build linux

// runner-watchdog wraps the GitHub Actions runner as its container's
// entrypoint and reports, over HTTP, when the runner has stopped taking
// jobs: upstream actions/actions-runner-controller#4307, where a
// cancelled or interrupted job leaves an ephemeral runner idling forever
// on "Skipping message Job. Job message already acquired" (or "...not
// found", or "Registration ... was not found").
//
//	runner-watchdog [-port 8091] [-grace 45s] -- /home/runner/run.sh
//
// It is a transparent PID 1: the child runs in its own process group,
// its stdout and stderr reach the container's unchanged (kubectl logs
// reads the same), the termination signals are forwarded to its group,
// every orphan is reaped, and the wrapper exits with the child's code
// and never before the child has exited. Nothing the watchdog does can
// stop the runner: the log lines it matches are offered to a bounded
// queue and dropped (and counted) when the matcher falls behind, and an
// HTTP server that cannot bind is reported and the runner runs on.
//
// The verdict is job-aware: STUCK only while no job is running AND a
// stuck signature first seen while idle has persisted for -grace. A
// signature during a job is counted, never acted on.
//
// /healthz answers 200 or 503 (stuck) for the pod's READINESS probe;
// /metrics is the Prometheus text format. See docs/architecture.md
// ("Runner watchdog") for why readiness and not liveness.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// queueLen is how many lines may wait for the matcher. The runner logs a
// few lines a second at its busiest; a full queue means the matcher is
// starved, and dropping is the designed answer, not blocking.
const queueLen = 1024

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	fs := flag.NewFlagSet("runner-watchdog", flag.ContinueOnError)
	port := fs.Int("port", 8091, "TCP port for /healthz and /metrics (0 disables the server)")
	grace := fs.Duration("grace", 45*time.Second, "how long an idle stuck signature must persist before the runner is reported stuck")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: runner-watchdog [-port N] [-grace D] -- command [args...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 || *grace < 0 || *port < 0 || *port > 65535 {
		fs.Usage()
		return 2
	}

	start := time.Now()
	m := NewMatcher(*grace, start)
	var dropped atomic.Uint64
	lines := make(chan string, queueLen)

	go func() {
		for line := range lines {
			m.Observe(line, time.Now())
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go report(ctx, m)
	if *port != 0 {
		go serve(ctx, net.JoinHostPort("", strconv.Itoa(*port)), handler(m, &dropped, time.Now))
	}

	s := &supervisor{argv: argv, stdout: os.Stdout, stderr: os.Stderr, lines: lines, dropped: &dropped}
	code, err := s.run()
	if err != nil {
		logf("cannot start %s: %v", argv[0], err)
	}
	return code
}

// report evaluates once a second, so a transition is logged when it
// happens rather than when something next asks. These are the only lines
// the watchdog adds to the runner's log, and they are what an operator
// greps for.
func report(ctx context.Context, m *Matcher) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s, changed := m.Evaluate(now)
			if !changed {
				continue
			}
			if s.Stuck {
				logf("stuck: %s for %s with no job running; /healthz now answers 503", s.Signature, s.ArmedFor.Truncate(time.Second))
			} else {
				logf("recovered: a job started; /healthz answers 200 again")
			}
		}
	}
}

func serve(ctx context.Context, addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// The runner runs on without the probe target; the readiness
		// probe then fails, which is visible, rather than the runner
		// failing, which would cost a job.
		logf("health/metrics server stopped: %v", err)
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "runner-watchdog: "+format+"\n", args...)
}
