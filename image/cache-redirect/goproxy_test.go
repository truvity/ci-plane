package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoImmutableHitMissRedirect(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".info"):
			_, _ = w.Write([]byte(`{"Version":"v1.6.0","Time":"2026-01-01T00:00:00Z"}`))
		case strings.HasSuffix(r.URL.Path, ".mod"):
			_, _ = w.Write([]byte("module example.com/mod\n"))
		case strings.HasSuffix(r.URL.Path, ".zip"):
			_, _ = w.Write([]byte("PK zip bytes"))
		}
	})
	f := newFixture(t, up)
	base := "/goproxy/example.com/mod/@v/v1.6.0"

	for _, ext := range []string{".info", ".mod", ".zip"} {
		r := f.get(t, base+ext)
		if r.code != 200 || r.body == "" {
			t.Fatalf("%s miss: %d %q", ext, r.code, r.body)
		}
	}
	// .info and .mod are small: served directly, body and all.
	for _, ext := range []string{".info", ".mod"} {
		if r := f.get(t, base+ext); r.code != 200 || r.body == "" {
			t.Errorf("%s hit: %d %q", ext, r.code, r.body)
		}
	}
	// .zip is large: a redirect to the store.
	r := f.get(t, base+".zip")
	if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://store.example/goproxy/example.com/mod/@v/v1.6.0.zip?") {
		t.Fatalf(".zip hit: %d %q", r.code, r.header.Get("Location"))
	}
	if n := up.total(); n != 3 {
		t.Errorf("%d upstream fetches, want 3", n)
	}
	for _, k := range []string{"info", "mod", "zip"} {
		if _, ok := f.store.object("goproxy/example.com/mod/@v/v1.6.0." + k); !ok {
			t.Errorf(".%s not stored", k)
		}
	}
	if !strings.HasPrefix(up.paths[0], "/proxy/example.com/mod/@v/") {
		t.Errorf("upstream asked for %s, want it under the upstream's own path", up.paths[0])
	}
}

func TestGoModulePathEscapingIsKept(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("PK")) })
	f := newFixture(t, up)

	// github.com/BurntSushi/toml at v1.0.0-RC1, as the go command escapes it.
	p := "/github.com/!burnt!sushi/toml/@v/v1.0.0-!r!c1.zip"
	if r := f.get(t, "/goproxy"+p); r.code != 200 {
		t.Fatalf("escaped path: %d", r.code)
	}
	if got := up.paths[0]; got != "/proxy"+p {
		t.Errorf("upstream asked for %s, want %s byte for byte", got, "/proxy"+p)
	}
	if _, ok := f.store.object("goproxy" + p); !ok {
		t.Errorf("stored under %v, want goproxy%s", f.store.keys(), p)
	}

	// An unescaped capital is not a module path the go command can send.
	for _, bad := range []string{
		"/goproxy/github.com/BurntSushi/toml/@v/list",
		"/goproxy/github.com/BurntSushi/toml/@v/v1.0.0.zip",
		"/goproxy/example.com/mod/@v/v1.0.0.tar",
		"/goproxy/example.com/mod%3Fx=1/@v/v1.0.0.zip",
	} {
		if r := f.get(t, bad); r.code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", bad, r.code)
		}
	}
	if up.total() != 1 {
		t.Errorf("an invalid path went upstream: %v", up.paths)
	}
}

func TestGoMutableEndpointsAreNeverStored(t *testing.T) {
	var gen atomic.Int64
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n := gen.Load()
		switch {
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			_, _ = w.Write([]byte("v1.0.0\n" + strings.Repeat("v1.1.0\n", int(n))))
		case strings.HasSuffix(r.URL.Path, "/@latest"), strings.HasSuffix(r.URL.Path, "/master.info"):
			_, _ = w.Write([]byte(`{"Version":"v1.` + strconv.FormatInt(n, 10) + `.0"}`))
		case strings.HasSuffix(r.URL.Path, "/latest"):
			_, _ = w.Write([]byte("go.sum database tree\n" + strconv.FormatInt(n, 10) + "\n"))
		case strings.Contains(r.URL.Path, "/lookup/"):
			_, _ = w.Write([]byte("record " + strconv.FormatInt(n, 10)))
		}
	})
	f := newFixture(t, up, "-mutable-ttl", "50ms")
	paths := []string{
		"/goproxy/example.com/mod/@v/list",
		"/goproxy/example.com/mod/@latest",
		"/goproxy/example.com/mod/@v/master.info",
		"/goproxy/sumdb/sum.golang.org/latest",
		"/goproxy/sumdb/sum.golang.org/lookup/example.com/mod@v1.0.0",
	}
	first := map[string]string{}
	for _, p := range paths {
		r := f.get(t, p)
		if r.code != 200 {
			t.Fatalf("%s: %d", p, r.code)
		}
		first[p] = r.body
	}
	// Within the TTL: remembered, no second upstream request.
	before := up.total()
	for _, p := range paths {
		f.get(t, p)
	}
	if up.total() != before {
		t.Errorf("within -mutable-ttl a mutable answer went upstream again")
	}
	// Past it: upstream's new answer, not the old one.
	gen.Store(1)
	time.Sleep(60 * time.Millisecond)
	for _, p := range paths {
		if r := f.get(t, p); r.body == first[p] {
			t.Errorf("%s: still %q after the TTL", p, r.body)
		}
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("a mutable answer was stored: %v", keys)
	}
}

func TestGoNotFoundReachesTheClientUnchanged(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gone") {
			http.Error(w, "gone: retracted", http.StatusGone)
			return
		}
		http.Error(w, "not found: unknown revision v9.9.9", http.StatusNotFound)
	})
	f := newFixture(t, up)
	for path, want := range map[string]int{
		"/goproxy/example.com/mod/@v/v9.9.9.info": http.StatusNotFound,
		"/goproxy/example.com/mod/@v/v9.9.9.zip":  http.StatusNotFound,
		"/goproxy/example.com/gone/@v/v1.0.0.mod": http.StatusGone,
		"/goproxy/example.com/mod/@v/list":        http.StatusNotFound,
	} {
		r := f.get(t, path)
		if r.code != want {
			t.Errorf("%s: %d, want %d", path, r.code, want)
		}
		if want == http.StatusNotFound && !strings.Contains(r.body, "unknown revision v9.9.9") {
			t.Errorf("%s: body %q lost upstream's text", path, r.body)
		}
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("a not-found was stored: %v", keys)
	}
}

func TestGoSumdb(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("tile bytes")) })
	f := newFixture(t, up)

	if r := f.get(t, "/goproxy/sumdb/sum.golang.org/supported"); r.code != 200 {
		t.Errorf("supported: %d, want 200 for the configured database", r.code)
	}
	// Any other database: 404, so the go command contacts it directly.
	if r := f.get(t, "/goproxy/sumdb/sum.example.org/supported"); r.code != http.StatusNotFound {
		t.Errorf("another database: %d, want 404", r.code)
	}
	for _, tile := range []string{"tile/8/0/x001/234", "tile/8/1/000.p/7", "tile/8/data/x002/003"} {
		p := "/goproxy/sumdb/sum.golang.org/" + tile
		if r := f.get(t, p); r.code != 200 || r.body != "tile bytes" {
			t.Fatalf("%s: %d %q", p, r.code, r.body)
		}
		if _, ok := f.store.object("goproxy/sumdb/sum.golang.org/" + tile); !ok {
			t.Errorf("%s not stored", tile)
		}
		if got := up.paths[len(up.paths)-1]; got != "/sumdb/"+tile {
			t.Errorf("upstream asked for %s", got)
		}
	}
	for _, bad := range []string{"/goproxy/sumdb/sum.golang.org/tile/8/0/12", "/goproxy/sumdb/sum.golang.org/other"} {
		if r := f.get(t, bad); r.code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", bad, r.code)
		}
	}

	// With no database configured, `supported` is a 404 too.
	f = newFixture(t, up, "-goproxy-sumdb", "")
	if r := f.get(t, "/goproxy/sumdb/sum.golang.org/supported"); r.code != http.StatusNotFound {
		t.Errorf("sumdb off: %d, want 404", r.code)
	}
}

func TestGoTruncatedZipIsNeverCommitted(t *testing.T) {
	body := []byte(strings.Repeat("z", 10000))
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { truncate(w, body, 4000) })
	f := newFixture(t, up)
	r := f.get(t, "/goproxy/example.com/mod/@v/v1.0.0.zip")
	if r.readErr == nil && r.code == 200 {
		t.Fatalf("a truncated zip ended cleanly (%d bytes)", len(r.body))
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Fatalf("a truncated zip was committed: %v", keys)
	}
}

func TestCanonicalVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3": true, "v0.0.0-20260101000000-abcdefabcdef": true, "v2.0.0+incompatible": true,
		"v1.0.0-!r!c1": true, "v0.0.1-go1.27.2.linux-amd64": true,
		"v1.2": false, "master": false, "latest": false, "v1.2.3+meta": false, "v1.0.0-RC1": false,
	} {
		if got := canonicalVersion(v); got != want {
			t.Errorf("canonicalVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestHealthzAndMetricNames(t *testing.T) {
	f := newFixture(t, newUpstream(t, func(http.ResponseWriter, *http.Request) {}))
	if r := f.get(t, "/healthz"); r.code != 200 {
		t.Fatalf("/healthz: %d", r.code)
	}
	m := f.metrics(t)
	for _, want := range []string{
		"# TYPE cache_redirect_requests_total counter",
		`cache_redirect_requests_total{frontend="goproxy",result="not_found"} 0`,
		`cache_redirect_requests_total{frontend="nix",result="upstream_error"} 0`,
		"# TYPE cache_redirect_uploads_total counter",
		"# TYPE cache_redirect_upload_bytes_total counter",
		"# TYPE cache_redirect_upstream_request_duration_seconds histogram",
		`cache_redirect_upstream_request_duration_seconds_bucket{frontend="nix",le="+Inf"} 0`,
		`cache_redirect_upstream_request_duration_seconds_bucket{frontend="nix",le="0.025"} 0`,
		"cache_redirect_fetches_in_flight 0",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}
