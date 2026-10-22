package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const npmHost = "npm.cache.example.test"

// fakeRegistry is a tiny npm registry: packuments with an ETag in both
// variants, tarballs, an audit endpoint, and switches to fail it.
type fakeRegistry struct {
	*upstream
	mu       sync.Mutex
	version  string // part of every packument and its ETag; change it to publish
	fail     bool   // answer every packument request 500
	accepts  []string
	inm      []string
	auditReq []auditSeen
}

type auditSeen struct {
	path, encoding, auth, body string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{version: "1"}
	fr.upstream = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		defer fr.mu.Unlock()
		switch p := r.URL.EscapedPath(); {
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/-/npm/v1/security/"):
			b, _ := io.ReadAll(r.Body)
			fr.auditReq = append(fr.auditReq, auditSeen{p, r.Header.Get("Content-Encoding"), r.Header.Get("Authorization"), string(b)})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"advisories":{}}`))
		case p == "/-/v1/search":
			_, _ = fmt.Fprintf(w, `{"q":%q}`, r.URL.RawQuery)
		case strings.HasSuffix(p, ".tgz"):
			_, _ = w.Write([]byte("TARBALL " + p))
		case strings.HasPrefix(p, "/-/package/"):
			_, _ = w.Write([]byte(`{"latest":"1.0.0"}`))
		case p == "/missing":
			http.NotFound(w, r)
		case p == "/big":
			fr.accepts = append(fr.accepts, r.Header.Get("Accept"))
			w.Header().Set("ETag", `"big"`)
			_, _ = w.Write(append([]byte(`{"name":"big","pad":"`), append(bytes.Repeat([]byte("x"), 9<<20), []byte(`"}`)...)...))
		default:
			if fr.fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			fr.accepts = append(fr.accepts, r.Header.Get("Accept"))
			etag := fmt.Sprintf(`W/"%s-%s"`, strings.TrimPrefix(r.Header.Get("Accept"), "application/"), fr.version)
			fr.inm = append(fr.inm, r.Header.Get("If-None-Match"))
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			ct := "application/json"
			if r.Header.Get("Accept") == abbreviatedType {
				ct = abbreviatedType
			}
			w.Header().Set("Content-Type", ct)
			name := strings.ReplaceAll(strings.TrimPrefix(p, "/"), "%2f", "/")
			base := name[strings.LastIndex(name, "/")+1:]
			// The README mentions a tarball URL the way a README may; its quotes are escaped.
			readme := fmt.Sprintf(`see \"tarball\":\"%s/%s/-/%s-0.0.1.tgz\"`, fr.URL, name, base)
			_, _ = fmt.Fprintf(w,
				`{"name":%q,"variant":%q,"v":%q,"readme":"%s","dist-tags":{"latest":"%s.0.0"},`+
					`"versions":{"%s.0.0":{"dist":{"tarball":"%s/%s/-/%s-%s.0.0.tgz","shasum":"abc"}}}}`,
				name, ct, fr.version, readme, fr.version, fr.version, fr.URL, name, base, fr.version)
		}
	})
	return fr
}

func (fr *fakeRegistry) set(f func(fr *fakeRegistry)) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	f(fr)
}

func (fr *fakeRegistry) accepted() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return append([]string(nil), fr.accepts...)
}

// newNpmFixture is a fixture with the npm front-end on, over store (a new one
// when nil, so a second call with the first's store is another pod).
func newNpmFixture(t *testing.T, up *upstream, store *memStore, extra ...string) *fixture {
	t.Helper()
	args := append([]string{
		"-s3-bucket", "example", "-idle-timeout", "5s", "-retry-backoff", "1ms",
		"-nix=false", "-goproxy=false", "-maven=false",
		"-npm", "-npm-registry", up.URL, "-npm-fresh", "1h",
	}, extra...)
	cfg, err := parseConfig(args, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		store = newMemStore()
	}
	h, c := newHandler(context.Background(), cfg, store, upstreamClient(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, store: store, cache: c, up: up}
}

// ask sends a request with headers and an optional Host and body.
func (f *fixture) ask(t *testing.T, method, host, path string, hdr map[string]string, body []byte) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, rerr := io.ReadAll(resp.Body)
	return reply{code: resp.StatusCode, body: string(b), header: resp.Header, readErr: rerr}
}

var abbreviated = map[string]string{"Accept": abbreviatedType + "; q=1.0, application/json; q=0.8, */*"}

func TestNpmScopedNameSpellings(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	for _, path := range []string{"/npm/@scope%2fname", "/npm/@scope/name", "/npm/@scope%2Fname"} {
		r := f.ask(t, http.MethodGet, "", path, nil, nil)
		if r.code != 200 || !strings.Contains(r.body, `"name":"@scope/name"`) {
			t.Fatalf("%s: %d %q", path, r.code, r.body)
		}
	}
	// One packument, however it was spelled: one upstream request, and the
	// registry is asked in its own form.
	if n := fr.total(); n != 1 {
		t.Errorf("%d upstream requests, want 1", n)
	}
	if got := fr.paths[0]; got != "/@scope%2fname" {
		t.Errorf("upstream asked for %s, want /@scope%%2fname", got)
	}
}

func TestNpmVariantsByAccept(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)

	a := f.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	full := f.ask(t, http.MethodGet, "", "/npm/lodash", map[string]string{"Accept": "application/json"}, nil)
	none := f.ask(t, http.MethodGet, "", "/npm/lodash", nil, nil)
	if a.code != 200 || full.code != 200 || none.code != 200 {
		t.Fatalf("codes %d %d %d", a.code, full.code, none.code)
	}
	if a.header.Get("Content-Type") != abbreviatedType {
		t.Errorf("abbreviated answered %q", a.header.Get("Content-Type"))
	}
	if full.header.Get("Content-Type") != "application/json" || none.body != full.body {
		t.Errorf("a client with no Accept must get the full document: %q", none.header.Get("Content-Type"))
	}
	if a.body == full.body {
		t.Error("the two variants share one body")
	}
	if !strings.Contains(a.header.Get("Vary"), "Accept") {
		t.Errorf("Vary = %q", a.header.Get("Vary"))
	}
	if got := fr.accepted(); len(got) != 2 || got[0] != abbreviatedType || got[1] != "application/json" {
		t.Errorf("upstream was asked for %q, want one of each variant", got)
	}
	for _, k := range []string{"npm/_meta/lodash.abbreviated", "npm/_meta/lodash.full"} {
		if _, ok := waitObject(t, f.store, k); !ok {
			t.Errorf("%s not stored; store holds %v", k, f.store.keys())
		}
	}
}

func waitObject(t *testing.T, s *memStore, key string) ([]byte, bool) {
	t.Helper()
	waitFor(t, key, func() bool { _, ok := s.object(key); return ok })
	return s.object(key)
}

func TestNpmTarballMissThenRedirect(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	path := "/npm/@scope/name/-/name-1.2.3.tgz"

	if r := f.get(t, path); r.code != 200 || r.body != "TARBALL /@scope/name/-/name-1.2.3.tgz" {
		t.Fatalf("miss: %d %q", r.code, r.body)
	}
	r := f.get(t, path)
	if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://store.example/npm/@scope/name/-/name-1.2.3.tgz?") {
		t.Fatalf("hit: %d %q", r.code, r.header.Get("Location"))
	}
	if r := f.do(t, http.MethodHead, path); r.code != 200 {
		t.Errorf("HEAD of a stored tarball: %d", r.code)
	}
	if n := fr.count("/@scope/name/-/name-1.2.3.tgz"); n != 1 {
		t.Errorf("%d upstream fetches, want 1", n)
	}
	// The file must be the package's own: <basename>-<version>.tgz.
	for _, bad := range []string{
		"/npm/name/-/other-1.0.0.tgz", "/npm/name/-/name-.tgz", "/npm/name/-/name-1.0.0.zip",
		"/npm/name/-/name-1.0.0.tgz/x", "/npm/name/x/name-1.0.0.tgz",
	} {
		if r := f.get(t, bad); r.code != 404 {
			t.Errorf("%s: %d, want 404", bad, r.code)
		}
	}
}

func TestNpmTarballURLsAreRewrittenToTheArrivalAddress(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil, "-hosts", "npm="+npmHost)
	tar := func(base string) string { return `"tarball":"` + base + `/lodash/-/lodash-1.0.0.tgz"` }

	// The path layout, by an address that is not any front-end's name.
	r := f.ask(t, http.MethodGet, "cache-redirect.internal.example", "/npm/lodash", nil, nil)
	if !strings.Contains(r.body, tar("http://cache-redirect.internal.example/npm")) {
		t.Errorf("service address: %s", r.body)
	}
	// Behind a gateway that terminates TLS, on the front-end's own name: no /npm.
	r = f.ask(t, http.MethodGet, npmHost, "/lodash", map[string]string{"X-Forwarded-Proto": "https"}, nil)
	if !strings.Contains(r.body, tar("https://"+npmHost)) {
		t.Errorf("host root: %s", r.body)
	}
	// A default port is not part of the address a client was given.
	r = f.ask(t, http.MethodGet, strings.ToUpper(npmHost)+":443", "/lodash", map[string]string{"X-Forwarded-Proto": "https"}, nil)
	if !strings.Contains(r.body, tar("https://"+npmHost)) {
		t.Errorf("host with :443: %s", r.body)
	}
	// ... and on another name the /npm layout holds.
	r = f.ask(t, http.MethodGet, "ci.example.test:8080", "/npm/lodash", nil, nil)
	if !strings.Contains(r.body, tar("http://ci.example.test:8080/npm")) {
		t.Errorf("other host: %s", r.body)
	}
	// A tarball URL inside the README is the README's: its quotes are escaped.
	if !strings.Contains(r.body, `\"tarball\":\"`+fr.URL+`/lodash/-/lodash-0.0.1.tgz\"`) {
		t.Errorf("readme rewritten: %s", r.body)
	}
	if strings.Contains(r.body, `"tarball":"`+fr.URL) {
		t.Errorf("a tarball URL still names the registry: %s", r.body)
	}
	// A Host that is not a host name is not written into a document.
	r = f.ask(t, http.MethodGet, `x"y`, "/npm/lodash", nil, nil)
	if r.code != 200 || !strings.Contains(r.body, `"tarball":"`+fr.URL) {
		t.Errorf("hostile Host: %d %s", r.code, r.body)
	}
	// The store keeps the document as the registry sent it.
	if n := fr.total(); n != 1 {
		t.Errorf("%d upstream requests, want 1 (the rewrite is per request, on the cached raw body)", n)
	}
}

func TestNpmCLIKeepsTheDocumentsTarballURLs(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	r := f.ask(t, http.MethodGet, "", "/npm/lodash", map[string]string{"User-Agent": "npm/10.9.9 node/v24.12.0 linux x64 workspaces/false"}, nil)
	if !strings.Contains(r.body, `"tarball":"`+fr.URL+`/lodash/-/lodash-1.0.0.tgz"`) {
		t.Errorf("the npm command was given rewritten URLs: %s", r.body)
	}
	for _, ua := range []string{"pnpm/11.28.7 npm/? node/v24.12.0 linux x64", "yarn/4.18.1", ""} {
		r = f.ask(t, http.MethodGet, "", "/npm/lodash", map[string]string{"User-Agent": ua}, nil)
		if strings.Contains(r.body, `"tarball":"`+fr.URL) {
			t.Errorf("User-Agent %q was given the registry's URLs: %s", ua, r.body)
		}
	}
}

func TestNpmHostRootServesTheRegistryAtTheRoot(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil, "-hosts", "npm="+npmHost)
	for path, want := range map[string]int{"/": 200, "/-/ping": 200, "/lodash": 200, "/lodash/-/lodash-1.0.0.tgz": 200, "/@a%2fb": 200} {
		if r := f.ask(t, http.MethodGet, npmHost, path, nil, nil); r.code != want {
			t.Errorf("%s on the npm host: %d, want %d (%q)", path, r.code, want, r.body)
		}
	}
	if r := f.ask(t, http.MethodGet, "", "/npm/-/ping", nil, nil); r.code != 200 || r.body != "{}" {
		t.Errorf("ping: %d %q", r.code, r.body)
	}
}

func TestNpmRevalidatesWithTheETag(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil, "-npm-fresh", "0s")

	first := f.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	second := f.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	if first.code != 200 || second.code != 200 || first.body != second.body {
		t.Fatalf("%d/%d", first.code, second.code)
	}
	fr.mu.Lock()
	inm := append([]string(nil), fr.inm...)
	fr.mu.Unlock()
	if len(inm) != 2 || inm[0] != "" || inm[1] != `W/"vnd.npm.install-v1+json-1"` {
		t.Errorf("If-None-Match sent upstream: %q, want none then the ETag", inm)
	}
	// Published meanwhile: the new document replaces the old, in memory and in the store.
	fr.set(func(fr *fakeRegistry) { fr.version = "2" })
	third := f.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	if !strings.Contains(third.body, `"latest":"2.0.0"`) {
		t.Errorf("a publish is not seen: %s", third.body)
	}
	waitFor(t, "the new document stored", func() bool {
		b, _ := f.store.object("npm/_meta/lodash.abbreviated")
		return b != nil && strings.Contains(string(f.loadBody(t, "npm/_meta/lodash.abbreviated")), `"latest":"2.0.0"`)
	})
}

// loadBody decodes a stored packument for a test.
func (f *fixture) loadBody(t *testing.T, key string) []byte {
	t.Helper()
	b, _ := f.store.object(key)
	_, rest, _ := bytes.Cut(b, []byte("\n"))
	zr, err := gzip.NewReader(bytes.NewReader(rest))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(zr)
	return out
}

func TestNpmFreshWindowSkipsUpstream(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil) // fresh for an hour
	for i := 0; i < 5; i++ {
		f.get(t, "/npm/lodash")
	}
	if n := fr.total(); n != 1 {
		t.Errorf("%d upstream requests for 5 reads inside the fresh window", n)
	}
}

func TestNpmClientETagGets304(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	r := f.ask(t, http.MethodGet, "h.example.test", "/npm/lodash", abbreviated, nil)
	etag := r.header.Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, "W/") {
		t.Fatalf("ETag = %q", etag)
	}
	h := map[string]string{"Accept": abbreviatedType, "If-None-Match": etag}
	if r := f.ask(t, http.MethodGet, "h.example.test", "/npm/lodash", h, nil); r.code != http.StatusNotModified || r.body != "" {
		t.Errorf("same address: %d %q", r.code, r.body)
	}
	// The document differs by address (its tarball URLs), so does the validator.
	if r := f.ask(t, http.MethodGet, "other.example.test", "/npm/lodash", h, nil); r.code != 200 {
		t.Errorf("another address with that ETag: %d, want 200", r.code)
	}
}

func TestNpmGzipsWhenAsked(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	plain := f.get(t, "/npm/lodash")
	r := f.ask(t, http.MethodGet, "", "/npm/lodash", map[string]string{"Accept-Encoding": "gzip"}, nil)
	if r.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not compressed: %v", r.header)
	}
	zr, err := gzip.NewReader(strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != plain.body {
		t.Errorf("compressed body differs")
	}
}

func TestNpmStaleOnUpstreamError(t *testing.T) {
	fr := newFakeRegistry(t)
	store := newMemStore()
	f := newNpmFixture(t, fr.upstream, store, "-npm-fresh", "0s")
	warm := f.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	waitObject(t, store, "npm/_meta/lodash.abbreviated")

	fr.set(func(fr *fakeRegistry) { fr.fail = true })
	r := f.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	if r.code != 200 || r.body != warm.body {
		t.Fatalf("with the registry down: %d %q", r.code, r.body)
	}
	if !strings.Contains(f.metrics(t), `cache_redirect_requests_total{frontend="npm",result="stale"} 1`) {
		t.Errorf("stale answer not counted:\n%s", grepLines(f.metrics(t), "npm"))
	}
	// Asked again at once: the copy is served without the retries first.
	before := fr.total()
	f.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	if fr.total() != before {
		t.Errorf("upstream asked again inside the failure window")
	}

	// Another pod, which has only the store.
	g := newNpmFixture(t, fr.upstream, store, "-npm-fresh", "0s")
	r = g.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil)
	if r.code != 200 || !strings.Contains(r.body, `"latest":"1.0.0"`) {
		t.Fatalf("a pod with an empty memory: %d %q", r.code, r.body)
	}
	// Never seen: nothing to fall back on.
	if r := g.get(t, "/npm/never-seen"); r.code != http.StatusBadGateway {
		t.Errorf("never-seen with the registry down: %d, want 502", r.code)
	}
	// The registry back: a pod with only the store revalidates and sees what was published.
	fr.set(func(fr *fakeRegistry) { fr.fail = false; fr.version = "3" })
	h := newNpmFixture(t, fr.upstream, store, "-npm-fresh", "0s")
	if r := h.ask(t, http.MethodGet, "", "/npm/lodash", abbreviated, nil); !strings.Contains(r.body, `"latest":"3.0.0"`) {
		t.Errorf("registry back, still stale: %s", r.body)
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestNpmRetriesAFailingRegistry(t *testing.T) {
	fr := newFakeRegistry(t)
	var n counter
	flaky := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.inc(); n.get() < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"name":"x"}`))
	})
	_ = fr
	cfg, err := parseConfig([]string{
		"-s3-bucket", "b", "-nix=false", "-goproxy=false", "-maven=false", "-npm",
		"-npm-registry", flaky.URL, "-retry-backoff", "1ms",
	},
		func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := newHandler(context.Background(), cfg, newMemStore(), upstreamClient(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/npm/x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || n.get() != 3 {
		t.Errorf("status %d after %d upstream requests, want 200 after 3", resp.StatusCode, n.get())
	}
}

func TestNpmNotFoundIsRememberedAndPassedOn(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	for i := 0; i < 3; i++ {
		if r := f.get(t, "/npm/missing"); r.code != 404 {
			t.Fatalf("missing: %d", r.code)
		}
	}
	if n := fr.count("/missing"); n != 1 {
		t.Errorf("%d upstream requests for a missing package, want 1", n)
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("a 404 was stored: %v", keys)
	}
}

func TestNpmLargePackument(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	for i := 0; i < 2; i++ {
		r := f.get(t, "/npm/big")
		if r.code != 200 || len(r.body) < 9<<20 {
			t.Fatalf("big: %d, %d bytes", r.code, len(r.body))
		}
	}
	if n := fr.count("/big"); n != 1 {
		t.Errorf("%d upstream requests; a document over 8 MiB must be kept in memory like any other", n)
	}
}

func TestNpmAuditAndSearchPassThrough(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"lodash":["4.17.21"]}`))
	_ = zw.Close()
	h := map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip", "Authorization": "Bearer secret"}
	for _, p := range []string{"/-/npm/v1/security/advisories/bulk", "/-/npm/v1/security/audits", "/-/npm/v1/security/audits/quick"} {
		r := f.ask(t, http.MethodPost, "", "/npm"+p, h, gz.Bytes())
		if r.code != 200 || r.body != `{"advisories":{}}` {
			t.Fatalf("%s: %d %q", p, r.code, r.body)
		}
	}
	fr.mu.Lock()
	seen := append([]auditSeen(nil), fr.auditReq...)
	fr.mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("%d audit requests reached the registry", len(seen))
	}
	for _, s := range seen {
		if s.encoding != "gzip" || s.body != gz.String() {
			t.Errorf("%s: Content-Encoding %q, body altered: %v", s.path, s.encoding, s.body != gz.String())
		}
		if s.auth != "" {
			t.Errorf("%s: the client's credential went upstream", s.path)
		}
	}
	// A GET of an audit endpoint is not one.
	if r := f.get(t, "/npm/-/npm/v1/security/advisories/bulk"); r.code != http.StatusMethodNotAllowed {
		t.Errorf("GET audit: %d", r.code)
	}
	if r := f.get(t, "/npm/-/v1/search?text=left-pad&size=5"); r.code != 200 || r.body != `{"q":"text=left-pad&size=5"}` {
		t.Errorf("search: %d %q", r.code, r.body)
	}
	if r := f.get(t, "/npm/-/package/@scope%2fname/dist-tags"); r.code != 200 || !strings.Contains(r.body, "latest") {
		t.Errorf("dist-tags: %d %q", r.code, r.body)
	}
	if got := fr.paths[len(fr.paths)-1]; got != "/-/package/@scope%2fname/dist-tags" {
		t.Errorf("dist-tags asked as %s", got)
	}
}

func TestNpmIsReadOnly(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/npm/lodash"}, {http.MethodDelete, "/npm/lodash/-rev/1-abc"}, {http.MethodPost, "/npm/lodash"},
		{http.MethodPut, "/npm/-/user/org.couchdb.user:x"}, {http.MethodPost, "/npm/-/v1/login"}, {http.MethodPut, "/npm/lodash/-/lodash-1.0.0.tgz"},
		{http.MethodPost, "/npm/-/v1/search"},
	} {
		r := f.ask(t, c.method, "", c.path, nil, []byte("{}"))
		if r.code != http.StatusMethodNotAllowed && r.code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 405 or 404", c.method, c.path, r.code)
		}
	}
	if n := fr.total(); n != 0 {
		t.Errorf("%d writes reached the registry", n)
	}
	if len(f.store.keys()) != 0 {
		t.Errorf("store written: %v", f.store.keys())
	}
}

func TestNpmRefusesBadNames(t *testing.T) {
	fr := newFakeRegistry(t)
	f := newNpmFixture(t, fr.upstream, nil)
	for _, p := range []string{
		"/npm/..", "/npm/./x", "/npm/.hidden", "/npm/_meta", "/npm/@scope", "/npm/@/x", "/npm/@scope/", "/npm/a%00b", "/npm/a b",
		"/npm/a/b", "/npm/@s/n/extra", "/npm/%2e%2e/x", "/npm/x/-/", "/npm/-/whoami", "/npm/lodash?x", "/npm/-",
	} {
		r := f.get(t, p)
		if p == "/npm/lodash?x" {
			if r.code != 200 {
				t.Errorf("%s: %d, a query string is ignored", p, r.code)
			}
			continue
		}
		if r.code == 200 && !strings.HasSuffix(p, "/..") {
			t.Errorf("%s: %d %q, want a refusal", p, r.code, r.body)
		}
	}
	for _, path := range fr.paths {
		if strings.Contains(path, "..") || strings.Contains(path, "_meta") {
			t.Errorf("a bad name reached the registry: %s", path)
		}
	}
}

func TestNpmConcurrentFirstRequestsShareOneFetch(t *testing.T) {
	g := newGate()
	slow := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		g.wait()
		_, _ = w.Write([]byte(`{"name":"x"}`))
	})
	f := newNpmFixture(t, slow, nil)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- f.get(t, "/npm/lodash").code
		}()
	}
	<-g.arrived
	time.Sleep(200 * time.Millisecond) // the others reach the front-end and wait
	close(g.release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("a request answered %d", c)
		}
	}
	if n := slow.total(); n != 1 {
		t.Errorf("%d upstream requests for 8 simultaneous ones, want 1", n)
	}
}

func TestNpmConfig(t *testing.T) {
	parse := func(args ...string) error {
		_, err := parseConfig(append([]string{"-s3-bucket", "b"}, args...), func(string) string { return "" }, io.Discard)
		return err
	}
	if err := parse("-npm", "-hosts", "npm="+npmHost); err != nil {
		t.Errorf("a host for npm: %v", err)
	}
	if err := parse("-hosts", "npm="+npmHost); err == nil || !strings.Contains(err.Error(), "npm front-end is off") {
		t.Errorf("a host for a front-end that is off: %v", err)
	}
	if err := parse("-npm", "-npm-registry", "ftp://x"); err == nil {
		t.Error("a registry that is not http(s) was accepted")
	}
	if err := parse("-npm", "-npm-prefix", "nix/"); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Errorf("npm prefix inside the nix prefix: %v", err)
	}
	if err := parse("-npm", "-npm-fresh", "-1s"); err == nil {
		t.Error("a negative fresh window was accepted")
	}
	if err := parse("-nix=false", "-goproxy=false", "-maven=false", "-npm"); err != nil {
		t.Errorf("npm alone: %v", err)
	}
	if err := parse("-nix=false", "-goproxy=false", "-maven=false"); err == nil {
		t.Error("nothing to serve was accepted")
	}
}
