package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Results a request is counted under.
const (
	resultHit           = "hit"            // served from the store or memory, body and all
	resultRedirect      = "redirect"       // 302 to a presigned URL of the store
	resultMiss          = "miss"           // fetched from upstream (and stored, if immutable)
	resultNotFound      = "not_found"      // 404/410: upstream's, remembered, or a path no front-end serves
	resultUpstreamError = "upstream_error" // upstream failed: 502
	resultStoreError    = "store_error"    // the store failed (a 403 above all): 502
	resultVerifyFailed  = "verify_failed"  // a NAR's body is whole but not the size/FileHash its narinfo says: served, not stored
	resultNoIndex       = "no_index"       // a NAR no narinfo of ours announced: passed through, not stored
	resultStored        = "stored"         // a client's write was committed to the store (the Gradle build cache)
	resultRejected      = "rejected"       // a client's write was refused: no length, or too large (the Gradle build cache)
)

var allResults = []string{resultHit, resultRedirect, resultMiss, resultNotFound, resultUpstreamError, resultStoreError, resultVerifyFailed, resultNoIndex, resultStored, resultRejected}

// upstreamBuckets are the histogram bounds, in seconds, of the time to
// upstream response headers.
var upstreamBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Metrics is the service's Prometheus text exposition, by hand: a dozen
// series do not justify a client library, the same call runner-watchdog
// made.
type Metrics struct {
	mu        sync.Mutex
	requests  map[[2]string]uint64 // frontend, result
	uploads   map[[2]string]uint64 // frontend, ok|failed
	upBytes   map[string]uint64    // frontend
	upstream  map[string]*histogram
	frontends []string
	inFlight  atomic.Int64
}

type histogram struct {
	counts []uint64 // per bucket, not cumulative; the last is +Inf
	sum    float64
	count  uint64
}

func newMetrics(frontends ...string) *Metrics {
	m := &Metrics{
		requests: map[[2]string]uint64{}, uploads: map[[2]string]uint64{},
		upBytes: map[string]uint64{}, upstream: map[string]*histogram{}, frontends: frontends,
	}
	// Every series exists from the start, at zero: a rate() over a series
	// that appears only with its first increment misses that increment.
	for _, f := range frontends {
		for _, r := range allResults {
			m.requests[[2]string{f, r}] = 0
		}
		m.uploads[[2]string{f, "ok"}] = 0
		m.uploads[[2]string{f, "failed"}] = 0
		m.upBytes[f] = 0
		m.upstream[f] = &histogram{counts: make([]uint64, len(upstreamBuckets)+1)}
	}
	return m
}

func (m *Metrics) Request(frontend, result string) {
	m.mu.Lock()
	m.requests[[2]string{frontend, result}]++
	m.mu.Unlock()
}

func (m *Metrics) Upload(frontend string, n int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.uploads[[2]string{frontend, "failed"}]++
		return
	}
	m.uploads[[2]string{frontend, "ok"}]++
	m.upBytes[frontend] += uint64(n)
}

func (m *Metrics) Upstream(frontend string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.upstream[frontend]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(upstreamBuckets)+1)}
		m.upstream[frontend] = h
	}
	s := d.Seconds()
	i := sort.SearchFloat64s(upstreamBuckets, s)
	h.counts[i]++
	h.sum += s
	h.count++
}

func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	head := func(name, typ, help string) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	head("cache_redirect_requests_total", "counter",
		"Requests by front-end and result (hit, redirect, miss, not_found, upstream_error, store_error, verify_failed, no_index, stored, rejected).")
	for _, k := range sortedKeys2(m.requests) {
		_, _ = fmt.Fprintf(w, "cache_redirect_requests_total{frontend=%q,result=%q} %d\n", k[0], k[1], m.requests[k])
	}

	head("cache_redirect_uploads_total", "counter",
		"Objects pulled through from upstream and written to the store (ok), or discarded unwritten (failed).")
	for _, k := range sortedKeys2(m.uploads) {
		_, _ = fmt.Fprintf(w, "cache_redirect_uploads_total{frontend=%q,result=%q} %d\n", k[0], k[1], m.uploads[k])
	}

	head("cache_redirect_upload_bytes_total", "counter", "Bytes of objects committed to the store.")
	for _, f := range sortedKeys(m.upBytes) {
		_, _ = fmt.Fprintf(w, "cache_redirect_upload_bytes_total{frontend=%q} %d\n", f, m.upBytes[f])
	}

	const hn = "cache_redirect_upstream_request_duration_seconds"
	head(hn, "histogram", "Time from an upstream request to its response headers.")
	for _, f := range sortedKeys(m.upstream) {
		h := m.upstream[f]
		var cum uint64
		for i, le := range upstreamBuckets {
			cum += h.counts[i]
			_, _ = fmt.Fprintf(w, "%s_bucket{frontend=%q,le=%q} %d\n", hn, f, trimFloat(le), cum)
		}
		cum += h.counts[len(upstreamBuckets)]
		_, _ = fmt.Fprintf(w, "%s_bucket{frontend=%q,le=\"+Inf\"} %d\n", hn, f, cum)
		_, _ = fmt.Fprintf(w, "%s_sum{frontend=%q} %g\n%s_count{frontend=%q} %d\n", hn, f, h.sum, hn, f, h.count)
	}

	head("cache_redirect_fetches_in_flight", "gauge", "Upstream fetches being pulled through right now.")
	_, _ = fmt.Fprintf(w, "cache_redirect_fetches_in_flight %d\n", m.inFlight.Load())
}

func trimFloat(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", f), "0"), ".")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys2(m map[[2]string]uint64) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
