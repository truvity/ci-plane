package main

import (
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const narinfoHash = "0mdqa9w1p6cmli6976v4wi0sw9r4p5pr" // 32 characters

// A narinfo exactly as an upstream signs it, odd spacing and all: the
// signature covers a fingerprint of these fields, so the stored copy must
// be these bytes, not a re-rendering.
const narinfoBody = "StorePath: /nix/store/0mdqa9w1p6cmli6976v4wi0sw9r4p5pr-hello-2.12\n" +
	"URL: nar/1b8m03r63zqhnjf7l5wnldhh7c134ap5vpj0850ymkq1iyzicy5s.nar.xz\n" +
	"Compression: xz\nFileHash: sha256:1b8m03r63zqhnjf7l5wnldhh7c134ap5vpj0850ymkq1iyzicy5s\n" +
	"FileSize: 3\nNarHash: sha256:0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73\nNarSize: 0\n" +
	"References:  \nSig: cache.nixos.org-1:dGVzdA==\nSig: other-1:c2lnbmF0dXJl\n"

// narFile names a body after its own file hash, as cache.nixos.org does.
func narFile(body []byte) string {
	sum := sha256.Sum256(body)
	return nix32(sum[:]) + ".nar.zst"
}

func TestNix32(t *testing.T) {
	for in, want := range map[string]string{
		"":    "0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73",
		"abc": "1b8m03r63zqhnjf7l5wnldhh7c134ap5vpj0850ymkq1iyzicy5s",
	} {
		sum := sha256.Sum256([]byte(in))
		if got := nix32(sum[:]); got != want {
			t.Errorf("nix32(sha256(%q)) = %s, want %s (nix-hash --to-base32)", in, got, want)
		}
	}
}

func TestNixCacheInfo(t *testing.T) {
	f := newFixture(t, newUpstream(t, func(http.ResponseWriter, *http.Request) {}), "-nix-priority", "25")
	r := f.get(t, "/nix/nix-cache-info")
	if r.code != 200 || r.body != "StoreDir: /nix/store\nWantMassQuery: 1\nPriority: 25\n" {
		t.Fatalf("nix-cache-info = %d %q", r.code, r.body)
	}
	if f.up.total() != 0 {
		t.Error("nix-cache-info went upstream")
	}
}

func TestNarinfoMissThenHitPreservesBytes(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(narinfoBody)) })
	f := newFixture(t, up)
	path := "/nix/" + narinfoHash + ".narinfo"

	for i := range 3 {
		r := f.get(t, path)
		if r.code != 200 || r.body != narinfoBody {
			t.Fatalf("request %d: %d %q", i, r.code, r.body)
		}
		if ct := r.header.Get("Content-Type"); ct != "text/x-nix-narinfo" {
			t.Errorf("Content-Type %q", ct)
		}
	}
	if n := up.count("/" + narinfoHash + ".narinfo"); n != 1 {
		t.Errorf("upstream fetched %d times, want 1", n)
	}
	stored, ok := f.store.object("nix/" + narinfoHash + ".narinfo")
	if !ok || string(stored) != narinfoBody {
		t.Errorf("stored narinfo = %q, want the upstream bytes verbatim", stored)
	}

	// A fresh process (memory empty) serves it from the store.
	f2 := newFixture(t, up)
	f2.store.objs = f.store.objs
	if r := f2.get(t, path); r.code != 200 || r.body != narinfoBody {
		t.Fatalf("store hit: %d %q", r.code, r.body)
	}
	if n := up.count("/" + narinfoHash + ".narinfo"); n != 1 {
		t.Errorf("a store hit went upstream")
	}
	m := f.metrics(t)
	for _, want := range []string{
		`cache_redirect_requests_total{frontend="nix",result="miss"} 1`,
		`cache_redirect_requests_total{frontend="nix",result="hit"} 2`,
		`cache_redirect_uploads_total{frontend="nix",result="ok"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestNarMissStreamsThenRedirects(t *testing.T) {
	body := []byte(strings.Repeat("nar bytes ", 10000))
	file := narFile(body)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	f := newFixture(t, up, "-presign-ttl", "2m")

	r := f.get(t, "/nix/nar/"+file)
	if r.code != 200 || r.body != string(body) || r.readErr != nil {
		t.Fatalf("miss: %d, %d bytes, %v", r.code, len(r.body), r.readErr)
	}
	if got, _ := f.store.object("nix/nar/" + file); string(got) != string(body) {
		t.Fatalf("stored %d bytes, want %d", len(got), len(body))
	}

	r = f.get(t, "/nix/nar/"+file)
	if r.code != http.StatusFound || r.header.Get("Location") != "https://store.example/nix/nar/"+file+"?expires=2m0s" {
		t.Fatalf("hit: %d Location %q, want a 302 to the presigned URL", r.code, r.header.Get("Location"))
	}

	// HEAD on a hit: a presigned GET cannot answer a HEAD, so it is
	// answered here, with the stored size.
	r = f.do(t, http.MethodHead, "/nix/nar/"+file)
	if r.code != 200 || r.header.Get("Content-Length") != "100000" {
		t.Fatalf("HEAD hit: %d Content-Length %q", r.code, r.header.Get("Content-Length"))
	}
	if n := up.count("/nar/" + file); n != 1 {
		t.Errorf("upstream fetched %d times, want 1", n)
	}
	if m := f.metrics(t); !strings.Contains(m, `cache_redirect_requests_total{frontend="nix",result="redirect"} 1`) ||
		!strings.Contains(m, `cache_redirect_upload_bytes_total{frontend="nix"} 100000`) {
		t.Errorf("metrics:\n%s", m)
	}
}

func TestNarHashMismatchIsNeverStored(t *testing.T) {
	body := []byte("these are not the bytes the name promises")
	file := narFile([]byte("other bytes"))
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	f := newFixture(t, up)

	r := f.get(t, "/nix/nar/"+file)
	if r.readErr == nil && r.code == 200 {
		t.Fatal("a body that does not hash to its name was answered as a clean 200")
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}

	// The check can be turned off for an upstream that names NARs otherwise.
	f = newFixture(t, up, "-nix-verify-nar-hash=false")
	if r := f.get(t, "/nix/nar/"+file); r.code != 200 || r.readErr != nil {
		t.Fatalf("unverified: %d %v", r.code, r.readErr)
	}
	if _, ok := f.store.object("nix/nar/" + file); !ok {
		t.Error("unverified NAR not stored")
	}
}

func TestTruncatedUpstreamIsNeverCommitted(t *testing.T) {
	body := []byte(strings.Repeat("x", 4096))
	file := narFile(body)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { truncate(w, body, 1000) })
	f := newFixture(t, up)

	for _, path := range []string{"/nix/nar/" + file, "/nix/" + narinfoHash + ".narinfo"} {
		r := f.get(t, path)
		if r.readErr == nil && r.code == 200 && len(r.body) == len(body) {
			t.Fatalf("%s: a truncated upstream body reached the client as a whole one", path)
		}
		if r.code == 200 && r.readErr == nil {
			t.Errorf("%s: truncated body ended cleanly (%d bytes); the client must see the break", path, len(r.body))
		}
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Fatalf("a truncated object was committed: %v", keys)
	}
	// Nothing remembered: the next request tries upstream again.
	f.get(t, "/nix/nar/"+file)
	if n := up.count("/nar/" + file); n != 2 {
		t.Errorf("upstream fetched %d times, want 2", n)
	}
	if m := f.metrics(t); !strings.Contains(m, `cache_redirect_uploads_total{frontend="nix",result="failed"} 2`) {
		t.Errorf("metrics:\n%s", m)
	}
}

func TestConcurrentMissesShareOneFetch(t *testing.T) {
	body := []byte(strings.Repeat("shared ", 50000))
	file := narFile(body)
	var g atomic.Pointer[gate]
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		g.Load().wait()
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			_, _ = w.Write([]byte(narinfoBody))
			return
		}
		_, _ = w.Write(body)
	})
	f := newFixture(t, up)
	var joined counter
	f.cache.onFollow = joined.inc

	const n = 8
	for _, tc := range []struct {
		path, upPath string
		large        bool
	}{
		{"/nix/nar/" + file, "/nar/" + file, true},
		{"/nix/" + narinfoHash + ".narinfo", "/" + narinfoHash + ".narinfo", false},
	} {
		gt := newGate()
		g.Store(gt)
		joined.n.Store(0)
		replies := make([]reply, n)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); replies[0] = f.get(t, tc.path) }()
		select {
		case <-gt.arrived:
		case <-time.After(5 * time.Second):
			wg.Wait()
			t.Fatalf("%s: the first request never reached upstream: %+v", tc.path, replies[0])
		}
		for i := 1; i < n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); replies[i] = f.get(t, tc.path) }()
		}
		waitFor(t, "the followers to join", func() bool { return joined.get() == n-1 })
		close(gt.release)
		wg.Wait()

		if c := up.count(tc.upPath); c != 1 {
			t.Errorf("%s: %d upstream fetches for %d concurrent misses, want 1", tc.path, c, n)
		}
		for i, r := range replies {
			switch {
			case r.readErr != nil:
				t.Errorf("%s reply %d: %v", tc.path, i, r.readErr)
			case i == 0 && (r.code != 200 || len(r.body) == 0):
				t.Errorf("%s leader: %d, %d bytes", tc.path, r.code, len(r.body))
			case i > 0 && tc.large && r.code != http.StatusFound:
				t.Errorf("%s follower %d: %d, want a 302 to the stored object", tc.path, i, r.code)
			case i > 0 && !tc.large && (r.code != 200 || r.body != narinfoBody):
				t.Errorf("%s follower %d: %d %q", tc.path, i, r.code, r.body)
			}
		}
	}
}

func TestNixNotFoundPassesThroughAndIsNotStored(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "404", http.StatusNotFound) })
	f := newFixture(t, up)
	path := "/nix/" + narinfoHash + ".narinfo"
	for range 2 {
		if r := f.get(t, path); r.code != http.StatusNotFound {
			t.Fatalf("got %d, want 404", r.code)
		}
	}
	if n := up.count("/" + narinfoHash + ".narinfo"); n != 1 {
		t.Errorf("upstream asked %d times; the not-found is remembered for -negative-ttl", n)
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("a not-found was stored: %v", keys)
	}
}

func TestNixHeadMiss(t *testing.T) {
	body := []byte("a nar")
	file := narFile(body)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			_, _ = w.Write([]byte(narinfoBody))
			return
		}
		_, _ = w.Write(body)
	})
	f := newFixture(t, up)

	// A narinfo HEAD is answered and the narinfo stored on the way.
	r := f.do(t, http.MethodHead, "/nix/"+narinfoHash+".narinfo")
	if r.code != 200 || r.body != "" || r.header.Get("Content-Length") != strconv.Itoa(len(narinfoBody)) {
		t.Fatalf("narinfo HEAD: %d %q %q", r.code, r.body, r.header.Get("Content-Length"))
	}
	if _, ok := f.store.object("nix/" + narinfoHash + ".narinfo"); !ok {
		t.Error("narinfo HEAD miss did not store it")
	}

	// A NAR HEAD is asked of upstream and stores nothing.
	r = f.do(t, http.MethodHead, "/nix/nar/"+file)
	if r.code != 200 || r.header.Get("Content-Length") != "5" {
		t.Fatalf("nar HEAD: %d %q", r.code, r.header.Get("Content-Length"))
	}
	if _, ok := f.store.object("nix/nar/" + file); ok {
		t.Error("a HEAD stored a NAR")
	}
}

func TestNixUnservedPaths(t *testing.T) {
	f := newFixture(t, newUpstream(t, func(http.ResponseWriter, *http.Request) {}))
	for _, p := range []string{
		"/nix/realisations/sha256:abc!out.doi",
		"/nix/log/" + narinfoHash + "-hello.drv",
		"/nix/" + narinfoHash + ".ls",
		"/nix/UPPERCASEUPPERCASEUPPERCASEUPPE.narinfo",
		"/nix/nar/.hidden",
		"/nix/nar/",
	} {
		if r := f.get(t, p); r.code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, r.code)
		}
	}
	if r := f.do(t, http.MethodPut, "/nix/"+narinfoHash+".narinfo"); r.code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d, want 405", r.code)
	}
	if f.up.total() != 0 {
		t.Error("an unserved path went upstream")
	}
}

func TestStoreErrorIsNotAMiss(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(narinfoBody)) })
	f := newFixture(t, up)
	f.store.err = errDenied
	for _, p := range []string{"/nix/" + narinfoHash + ".narinfo", "/nix/nar/" + narFile([]byte("x"))} {
		if r := f.get(t, p); r.code != http.StatusBadGateway {
			t.Errorf("%s: %d, want 502", p, r.code)
		}
	}
	if up.total() != 0 {
		t.Error("a store that denies access was treated as a miss and went upstream")
	}
	if m := f.metrics(t); !strings.Contains(m, `cache_redirect_requests_total{frontend="nix",result="store_error"} 2`) {
		t.Errorf("metrics:\n%s", m)
	}
}
