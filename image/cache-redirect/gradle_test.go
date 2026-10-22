package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const gradleKey = "e50a3988ab413e352f1ee2f4dcc2bdf1"

func newGradleFixture(t *testing.T, extra ...string) *fixture {
	t.Helper()
	return newFixture(t, newUpstream(t, http.NotFound), append([]string{"-gradle"}, extra...)...)
}

func (f *fixture) put(t *testing.T, path string, body []byte) reply {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, f.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return reply{code: resp.StatusCode, header: resp.Header}
}

func TestGradlePutThenGetRedirects(t *testing.T) {
	f := newGradleFixture(t)
	path := "/gradle/cache/" + gradleKey

	if r := f.get(t, path); r.code != http.StatusNotFound {
		t.Fatalf("GET before PUT: %d, want 404 (a miss)", r.code)
	}
	if r := f.put(t, path, []byte("entry bytes")); r.code != http.StatusNoContent {
		t.Fatalf("PUT: %d, want 204", r.code)
	}
	if b, ok := f.store.object("gradle/" + gradleKey); !ok || string(b) != "entry bytes" {
		t.Fatalf("stored %q (%v), want the body under gradle/<key>; store holds %v", b, ok, f.store.keys())
	}
	r := f.get(t, path)
	if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://store.example/gradle/"+gradleKey+"?") {
		t.Fatalf("GET after PUT: %d %q, want a redirect to the store", r.code, r.header.Get("Location"))
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q on a redirect, want no-store", r.header.Get("Cache-Control"))
	}
	if r := f.do(t, http.MethodHead, path); r.code != 200 || r.header.Get("Content-Length") != "11" {
		t.Errorf("HEAD: %d length %q, want 200 and 11", r.code, r.header.Get("Content-Length"))
	}
}

func TestGradleFirstWriteWins(t *testing.T) {
	f := newGradleFixture(t)
	path := "/gradle/cache/" + gradleKey
	f.put(t, path, []byte("the first"))
	if r := f.put(t, path, []byte("the second, from another job")); r.code != http.StatusNoContent {
		t.Fatalf("second PUT: %d, want 204 (Gradle must not see an error)", r.code)
	}
	if b, _ := f.store.object("gradle/" + gradleKey); string(b) != "the first" {
		t.Errorf("stored %q, want the first write to stay", b)
	}
}

func TestGradleRefusals(t *testing.T) {
	f := newGradleFixture(t, "-gradle-max-entry-bytes", "10")

	bad := []string{
		"short", strings.Repeat("g", 32), strings.ToUpper(gradleKey), strings.Repeat(gradleKey, 4) + "0",
		"..%2F" + gradleKey, "a/" + gradleKey,
	}
	for _, key := range bad {
		if r := f.put(t, "/gradle/cache/"+key, []byte("x")); r.code != http.StatusNotFound {
			t.Errorf("PUT of key %q: %d, want 404", key, r.code)
		}
	}
	if r := f.put(t, "/gradle/cache/"+gradleKey, []byte("eleven bytes")); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("PUT over the cap: %d, want 413", r.code)
	}
	if r := f.do(t, http.MethodDelete, "/gradle/cache/"+gradleKey); r.code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: %d, want 405", r.code)
	}
	if r := f.do(t, http.MethodPost, "/gradle/cache/"+gradleKey); r.code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d, want 405", r.code)
	}
	if r := f.get(t, "/gradle/other/"+gradleKey); r.code != http.StatusNotFound {
		t.Errorf("GET outside /gradle/cache/: %d, want 404", r.code)
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("a refused write left %v in the store", keys)
	}
	m := f.metrics(t)
	if !strings.Contains(m, `cache_redirect_requests_total{frontend="gradle",result="rejected"} 1`) {
		t.Errorf("the 413 is not counted as rejected:\n%s", m)
	}
}

func TestGradlePutNeedsAContentLength(t *testing.T) {
	f := newGradleFixture(t)
	conn := dial(t, f)
	_, _ = fmt.Fprintf(conn, "PUT /gradle/cache/%s HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n", gradleKey)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusLengthRequired {
		t.Errorf("chunked PUT: %d, want 411", resp.StatusCode)
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("stored %v", keys)
	}
}

// A body cut short must not become a hit: the entry would then be served,
// truncated, to every later build.
func TestGradleTruncatedPutIsNotCommitted(t *testing.T) {
	f := newGradleFixture(t)
	conn := dial(t, f)
	_, _ = fmt.Fprintf(conn, "PUT /gradle/cache/%s HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nonly ten b", gradleKey)
	_ = conn.Close()
	waitFor(t, "the upload to be given up", func() bool {
		return strings.Contains(f.metrics(t), `cache_redirect_uploads_total{frontend="gradle",result="failed"} 1`)
	})
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("a truncated body was committed: %v", keys)
	}
	if r := f.get(t, "/gradle/cache/"+gradleKey); r.code != http.StatusNotFound {
		t.Errorf("GET after a truncated PUT: %d, want 404", r.code)
	}
}

func TestGradleStoreErrorIsNotAMiss(t *testing.T) {
	f := newGradleFixture(t)
	f.store.err = errDenied
	if r := f.get(t, "/gradle/cache/"+gradleKey); r.code != http.StatusBadGateway {
		t.Errorf("GET with a failing store: %d, want 502", r.code)
	}
	if r := f.put(t, "/gradle/cache/"+gradleKey, []byte("x")); r.code != http.StatusBadGateway {
		t.Errorf("PUT with a failing store: %d, want 502", r.code)
	}
}

func TestGradleProbeAndOffByDefault(t *testing.T) {
	f := newGradleFixture(t)
	if r := f.get(t, "/gradle/cache/"); r.code != 200 {
		t.Errorf("probe: %d, want 200", r.code)
	}
	off := newFixture(t, newUpstream(t, http.NotFound))
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		if r := off.do(t, m, "/gradle/cache/"+gradleKey); r.code != http.StatusNotFound {
			t.Errorf("%s with the front-end off (the default): %d, want 404 and nothing written", m, r.code)
		}
	}
	if keys := off.store.keys(); len(keys) != 0 {
		t.Errorf("a disabled build cache stored %v", keys)
	}
}

func TestValidGradleKey(t *testing.T) {
	for k, want := range map[string]bool{
		gradleKey:                        true,
		strings.Repeat("0", 64):          true,
		strings.Repeat("f", 128):         true,
		strings.Repeat("f", 129):         false,
		strings.Repeat("f", 31):          false,
		"":                               false,
		strings.Repeat("F", 32):          false,
		strings.Repeat("a", 31) + "/":    false,
		strings.Repeat("a", 31) + "\x00": false,
		strings.Repeat("a", 30) + "%2f":  false,
	} {
		if got := validGradleKey(k); got != want {
			t.Errorf("validGradleKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func dial(t *testing.T, f *fixture) net.Conn {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn
}
