package main

import (
	"crypto/sha256"
	"encoding/json"
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

// narFile is a NAR's file name as cache.nixos.org really has it: NOT the
// hash of the (compressed) body, whatever it looks like.
func narFile(body []byte) string {
	sum := sha256.Sum256(append([]byte("not the body: "), body...))
	return nix32(sum[:]) + ".nar.zst"
}

// narinfoFor is the narinfo of a NAR whose body is body: its real FileHash
// (Nix base 32 SHA-256) and FileSize, at URL nar/<file>.
func narinfoFor(file string, body []byte) string {
	sum := sha256.Sum256(body)
	return "StorePath: /nix/store/" + narinfoHash + "-go-1.27.1\nURL: nar/" + file + "\nCompression: zstd\n" +
		"FileHash: sha256:" + nix32(sum[:]) + "\nFileSize: " + strconv.Itoa(len(body)) + "\n" +
		"NarHash: sha256:0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73\nNarSize: 0\nReferences: \nSig: cache.nixos.org-1:dGVzdA==\n"
}

// nixUpstream serves narinfo for narinfoHash and body for nar/<file>.
func nixUpstream(t *testing.T, file string, info string, body []byte) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			_, _ = w.Write([]byte(info))
			return
		}
		_, _ = w.Write(body)
	})
}

// seedIndex writes the index record of file's narinfo into f's store, as
// another replica would have.
func seedIndex(t *testing.T, f *fixture, file string, info string) {
	t.Helper()
	rec, ok := parseNarinfo([]byte(info))
	if !ok {
		t.Fatal("test narinfo unparsable")
	}
	b, _ := json.Marshal(rec)
	f.store.mu.Lock()
	f.store.objs["nix/nar-index/nar/"+file+".json"] = b
	f.store.mu.Unlock()
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
	up := nixUpstream(t, file, narinfoFor(file, body), body)
	f := newFixture(t, up, "-presign-ttl", "2m")

	if r := f.get(t, "/nix/"+narinfoHash+".narinfo"); r.code != 200 {
		t.Fatalf("narinfo: %d", r.code)
	}
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
		!strings.Contains(m, `cache_redirect_upload_bytes_total{frontend="nix"} `+strconv.Itoa(len(body)+len(narinfoFor(file, body)))) {
		t.Errorf("metrics:\n%s", m)
	}
}

func TestNarVerificationAgainstTheNarinfo(t *testing.T) {
	body := []byte(strings.Repeat("compressed nar ", 1000))
	file := narFile(body)
	good := narinfoFor(file, body)
	other := []byte(strings.Repeat("some other  nar", 1000)) // same size, other bytes
	otherSum := sha256.Sum256(other)

	for name, tc := range map[string]struct {
		info string
		want string // metrics line
	}{
		"hash mismatch": {
			strings.Replace(good, "FileHash: sha256:"+nix32(func() []byte { s := sha256.Sum256(body); return s[:] }()), "FileHash: sha256:"+nix32(otherSum[:]), 1),
			`cache_redirect_requests_total{frontend="nix",result="verify_failed"} 1`,
		},
		"size mismatch": {
			strings.Replace(good, "FileSize: "+strconv.Itoa(len(body)), "FileSize: "+strconv.Itoa(len(body)+1), 1),
			`cache_redirect_requests_total{frontend="nix",result="verify_failed"} 1`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			up := nixUpstream(t, file, tc.info, body)
			f := newFixture(t, up)
			if r := f.get(t, "/nix/"+narinfoHash+".narinfo"); r.code != 200 {
				t.Fatalf("narinfo: %d", r.code)
			}
			// The client still gets every byte, cleanly.
			r := f.get(t, "/nix/nar/"+file)
			if r.code != 200 || r.readErr != nil || r.body != string(body) {
				t.Fatalf("nar: %d, %d bytes, %v; the client must get the pass-through bytes", r.code, len(r.body), r.readErr)
			}
			if _, ok := f.store.object("nix/nar/" + file); ok {
				t.Fatal("a NAR that does not match its narinfo was committed")
			}
			m := f.metrics(t)
			if !strings.Contains(m, tc.want) {
				t.Errorf("metrics lack %s:\n%s", tc.want, m)
			}
			if strings.Contains(m, `result="upstream_error"} 1`) {
				t.Errorf("a verify failure was counted as an upstream error:\n%s", m)
			}
			// Nothing was remembered as stored: the next request fetches again.
			f.get(t, "/nix/nar/"+file)
			if n := up.count("/nar/" + file); n != 2 {
				t.Errorf("upstream fetched %d times, want 2", n)
			}
		})
	}
}

func TestNarNameIsNotItsHash(t *testing.T) {
	body := []byte(strings.Repeat("zstd bytes ", 5000))
	file := narFile(body) // the name's hash is NOT the body's
	up := nixUpstream(t, file, narinfoFor(file, body), body)
	f := newFixture(t, up)

	f.get(t, "/nix/"+narinfoHash+".narinfo")
	if r := f.get(t, "/nix/nar/"+file); r.code != 200 || r.readErr != nil || r.body != string(body) {
		t.Fatalf("miss: %d %v", r.code, r.readErr)
	}
	if got, ok := f.store.object("nix/nar/" + file); !ok || string(got) != string(body) {
		t.Fatal("a NAR whose narinfo matches was not committed")
	}
	if r := f.get(t, "/nix/nar/"+file); r.code != http.StatusFound {
		t.Fatalf("next request: %d, want a 302", r.code)
	}
	if _, ok := f.store.object("nix/nar-index/nar/" + file + ".json"); !ok {
		t.Errorf("no index record in the store; keys %v", f.store.keys())
	}
}

func TestNarWithoutIndexIsPassedThroughNotStored(t *testing.T) {
	body := []byte(strings.Repeat("orphan ", 1000))
	file := narFile(body)
	up := nixUpstream(t, file, narinfoFor(file, body), body)
	f := newFixture(t, up)

	for range 2 {
		r := f.get(t, "/nix/nar/"+file)
		if r.code != 200 || r.readErr != nil || r.body != string(body) {
			t.Fatalf("%d %v", r.code, r.readErr)
		}
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}
	m := f.metrics(t)
	if !strings.Contains(m, `cache_redirect_requests_total{frontend="nix",result="no_index"} 2`) ||
		!strings.Contains(m, `cache_redirect_requests_total{frontend="nix",result="upstream_error"} 0`) {
		t.Errorf("metrics:\n%s", m)
	}
}

func TestNarIndexSharedBetweenReplicas(t *testing.T) {
	body := []byte(strings.Repeat("replica ", 4000))
	file := narFile(body)
	up := nixUpstream(t, file, narinfoFor(file, body), body)
	a := newFixture(t, up)
	a.get(t, "/nix/"+narinfoHash+".narinfo") // replica A sees the narinfo

	b := newFixture(t, up) // replica B: fresh memory, the same bucket
	b.store.mu.Lock()
	a.store.mu.Lock()
	b.store.objs = a.store.objs
	a.store.mu.Unlock()
	b.store.mu.Unlock()

	if r := b.get(t, "/nix/nar/"+file); r.code != 200 || r.readErr != nil || r.body != string(body) {
		t.Fatalf("replica B miss: %d %v", r.code, r.readErr)
	}
	if _, ok := b.store.object("nix/nar/" + file); !ok {
		t.Fatal("replica B did not commit the NAR from replica A's index record")
	}
	if r := a.get(t, "/nix/nar/"+file); r.code != http.StatusFound {
		t.Errorf("replica A after B's commit: %d, want 302", r.code)
	}
}

func TestTruncatedUpstreamIsNeverCommitted(t *testing.T) {
	body := []byte(strings.Repeat("x", 4096))
	file := narFile(body)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { truncate(w, body, 1000) })
	f := newFixture(t, up)
	seedIndex(t, f, file, narinfoFor(file, body))

	for _, path := range []string{"/nix/nar/" + file, "/nix/" + narinfoHash + ".narinfo"} {
		r := f.get(t, path)
		if r.readErr == nil && r.code == 200 && len(r.body) == len(body) {
			t.Fatalf("%s: a truncated upstream body reached the client as a whole one", path)
		}
		if r.code == 200 && r.readErr == nil {
			t.Errorf("%s: truncated body ended cleanly (%d bytes); the client must see the break", path, len(r.body))
		}
	}
	for _, k := range f.store.keys() {
		if !strings.HasPrefix(k, "nix/nar-index/") {
			t.Fatalf("a truncated object was committed: %v", f.store.keys())
		}
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
	seedIndex(t, f, file, narinfoFor(file, body))
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
