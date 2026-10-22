package main

import (
	"net/http"
	"strings"
	"testing"
)

// mavenUpstream serves a tiny repository: a pom, a jar, Gradle module
// metadata, a version list, and a snapshot.
func mavenUpstream(t *testing.T) *upstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case strings.HasSuffix(p, ".pom"):
			_, _ = w.Write([]byte("<project/>"))
		case strings.HasSuffix(p, ".module"):
			_, _ = w.Write([]byte(`{"formatVersion":"1.1"}`))
		case strings.HasSuffix(p, ".jar"):
			_, _ = w.Write([]byte("PK jar bytes"))
		case strings.HasSuffix(p, ".sha1"):
			_, _ = w.Write([]byte("da39a3ee5e6b4b0d3255bfef95601890afd80709"))
		case strings.HasSuffix(p, "maven-metadata.xml"), strings.Contains(p, "-SNAPSHOT/"):
			_, _ = w.Write([]byte("<metadata/>"))
		default:
			http.NotFound(w, r)
		}
	})
}

func newMavenFixture(t *testing.T, up *upstream) *fixture {
	t.Helper()
	return newFixture(t, up, "-maven-repo", "central="+up.URL+"/central,google="+up.URL+"/google")
}

func TestMavenImmutableHitMissRedirect(t *testing.T) {
	up := mavenUpstream(t)
	f := newMavenFixture(t, up)
	base := "/maven/central/org/example/lib/1.2.3/lib-1.2.3"

	for _, ext := range []string{".pom", ".module", ".jar", ".jar.sha1"} {
		if r := f.get(t, base+ext); r.code != 200 || r.body == "" {
			t.Fatalf("%s miss: %d %q", ext, r.code, r.body)
		}
	}
	// A pom, module metadata and a checksum are small: served directly.
	for _, ext := range []string{".pom", ".module", ".jar.sha1"} {
		if r := f.get(t, base+ext); r.code != 200 || r.body == "" {
			t.Errorf("%s hit: %d %q", ext, r.code, r.body)
		}
	}
	// The jar is an archive: a redirect to the store.
	r := f.get(t, base+".jar")
	if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://store.example/maven/central/org/example/lib/1.2.3/lib-1.2.3.jar?") {
		t.Fatalf(".jar hit: %d %q", r.code, r.header.Get("Location"))
	}
	if n := up.total(); n != 4 {
		t.Errorf("%d upstream fetches, want 4 (one per file)", n)
	}
	if got := up.paths[0]; !strings.HasPrefix(got, "/central/org/example/lib/1.2.3/") {
		t.Errorf("upstream asked for %s, want it under the repository's own path", got)
	}
	if _, ok := f.store.object("maven/central/org/example/lib/1.2.3/lib-1.2.3.jar"); !ok {
		t.Errorf("jar not stored; store holds %v", f.store.keys())
	}
	if r := f.do(t, http.MethodHead, base+".jar"); r.code != 200 {
		t.Errorf("HEAD of a stored jar: %d", r.code)
	}
}

func TestMavenRepositoriesAreSeparatePaths(t *testing.T) {
	up := mavenUpstream(t)
	f := newMavenFixture(t, up)

	if r := f.get(t, "/maven/google/com/example/x/1/x-1.pom"); r.code != 200 {
		t.Fatalf("google: %d", r.code)
	}
	if got := up.paths[0]; got != "/google/com/example/x/1/x-1.pom" {
		t.Errorf("upstream asked for %s, want the google repository's path", got)
	}
	if _, ok := f.store.object("maven/google/com/example/x/1/x-1.pom"); !ok {
		t.Errorf("stored under %v, want the repository name in the key", f.store.keys())
	}

	// A repository that is not configured, a path with no file, and paths that
	// would walk out of the repository are never served. (The mux itself
	// redirects a path with '..' or '//' to its cleaned form, which is then
	// judged on its own.)
	for _, bad := range []string{
		"/maven/unknown/org/example/x/1/x-1.pom",
		"/maven/central",
		"/maven/central/org/example/x/1/x-1.pom%3Fa=b",
		"/maven/central/org/example/x/1/x-1.pom%2F..%2F",
	} {
		if r := f.get(t, bad); r.code != http.StatusNotFound && r.code != http.StatusMovedPermanently {
			t.Errorf("%s: %d, want 404", bad, r.code)
		}
	}
	for _, walk := range []string{"/maven/central/org/../../etc/passwd", "/maven/central/org//x.pom"} {
		if r := f.get(t, walk); r.code == http.StatusOK {
			t.Errorf("%s: 200", walk)
		}
	}
	if n := up.total(); n != 1 {
		t.Errorf("%d upstream fetches, want only the one for the google pom", n)
	}
}

func TestMavenUpstreamNotFoundPassesThroughAndIsRemembered(t *testing.T) {
	up := mavenUpstream(t)
	f := newMavenFixture(t, up)
	p := "/maven/central/org/example/missing/1/missing-1.module"
	// The upstream's handler answers 404 for a path it has no rule for.
	p = strings.Replace(p, ".module", ".nope", 1)
	for i := 0; i < 3; i++ {
		if r := f.get(t, p); r.code != http.StatusNotFound {
			t.Fatalf("attempt %d: %d, want 404", i, r.code)
		}
	}
	if n := up.total(); n != 1 {
		t.Errorf("%d upstream fetches for a path asked for 3 times, want 1 (the 404 is remembered)", n)
	}
	if len(f.store.keys()) != 0 {
		t.Errorf("a 404 was stored: %v", f.store.keys())
	}
}

func TestMavenMutablePathsAreNotStored(t *testing.T) {
	up := mavenUpstream(t)
	f := newMavenFixture(t, up)
	for _, p := range []string{
		"/maven/central/org/example/lib/maven-metadata.xml",
		"/maven/central/org/example/lib/maven-metadata.xml.sha1",
		"/maven/central/org/example/lib/1.0-SNAPSHOT/lib-1.0-20260101.000000-1.jar",
		"/maven/central/org/example/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.pom",
	} {
		if r := f.get(t, p); r.code != 200 || r.body == "" {
			t.Errorf("%s: %d %q", p, r.code, r.body)
		}
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("mutable answers were stored: %v", keys)
	}
}

func TestMutableMavenPath(t *testing.T) {
	for p, want := range map[string]bool{
		"org/example/lib/maven-metadata.xml":              true,
		"org/example/lib/1.0-SNAPSHOT/maven-metadata.xml": true,
		"org/example/lib/1.0-SNAPSHOT/lib-1.0-2026-1.jar": true,
		"org/example/lib/1.0/maven-metadata-central.xml":  true,
		"master-index.xml":                                        true,
		"androidx/core/group-index.xml":                           true,
		"org/example/lib/1.0/lib-1.0.pom":                         false,
		"org/example/lib/1.0/lib-1.0.jar":                         false,
		"org/example/lib/1.0.0-SNAPSHOTS/lib-1.0.0-SNAPSHOTS.jar": false,
		"org/example/lib-SNAPSHOT-thing/1.0/lib-1.0.pom":          false,
	} {
		if got := mutableMavenPath(p); got != want {
			t.Errorf("mutableMavenPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestMavenProbe(t *testing.T) {
	f := newMavenFixture(t, mavenUpstream(t))
	r := f.get(t, "/maven/")
	if r.code != 200 || r.body != "maven: central google\n" {
		t.Errorf("GET /maven/: %d %q, want 200 listing the repositories", r.code, r.body)
	}
	if r := f.do(t, http.MethodPost, "/maven/central/x"); r.code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d, want 405: the front-end is read-only", r.code)
	}
}

func TestMavenOffIsNotThere(t *testing.T) {
	f := newFixture(t, mavenUpstream(t), "-maven=false")
	if r := f.get(t, "/maven/"); r.code != http.StatusNotFound {
		t.Errorf("GET /maven/ with the front-end off: %d, want 404 (the probe of a runner must fail)", r.code)
	}
}

func TestMavenStoreErrorIsNotAMiss(t *testing.T) {
	up := mavenUpstream(t)
	f := newMavenFixture(t, up)
	f.store.err = errDenied
	if r := f.get(t, "/maven/central/org/example/lib/1/lib-1.jar"); r.code != http.StatusBadGateway {
		t.Errorf("jar with a failing store: %d, want 502", r.code)
	}
	if n := up.total(); n != 0 {
		t.Errorf("%d upstream fetches: a store that refuses must not turn every request into one", n)
	}
}

func TestMavenChainAsksTheNextUpstreamOnA404(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/first/org/example/lib/1/lib-1.pom":
			_, _ = w.Write([]byte("<from-first/>"))
		case "/second/org/example/lib/1/lib-1.pom", "/second/org/example/only/1/only-1.pom":
			_, _ = w.Write([]byte("<from-second/>"))
		case "/second/org/example/lib/1/lib-1.jar":
			_, _ = w.Write([]byte("PK second"))
		default:
			http.NotFound(w, r)
		}
	})
	f := newFixture(t, up, "-maven-repo", "chain="+up.URL+"/first|"+up.URL+"/second")

	if r := f.get(t, "/maven/chain/org/example/lib/1/lib-1.pom"); r.code != 200 || r.body != "<from-first/>" {
		t.Errorf("a file the first upstream has: %d %q, want its own answer", r.code, r.body)
	}
	if r := f.get(t, "/maven/chain/org/example/only/1/only-1.pom"); r.code != 200 || r.body != "<from-second/>" {
		t.Errorf("a file only the second has: %d %q", r.code, r.body)
	}
	// A large file and a HEAD of it go down the chain too.
	if r := f.get(t, "/maven/chain/org/example/lib/1/lib-1.jar"); r.code != 200 || r.body != "PK second" {
		t.Errorf("a jar only the second has: %d %q", r.code, r.body)
	}
	if _, ok := f.store.object("maven/chain/org/example/only/1/only-1.pom"); !ok {
		t.Errorf("stored %v, want the key to carry the repository's name, not the upstream's", f.store.keys())
	}
	if r := f.get(t, "/maven/chain/org/example/none/1/none-1.pom"); r.code != http.StatusNotFound {
		t.Errorf("a file neither has: %d, want 404", r.code)
	}
	if r := f.do(t, http.MethodHead, "/maven/chain/org/example/lib/2/lib-2.jar"); r.code != http.StatusNotFound {
		t.Errorf("HEAD of a jar neither has: %d, want 404", r.code)
	}
	// A hit is answered from the store: no upstream is asked again.
	before := up.total()
	f.get(t, "/maven/chain/org/example/only/1/only-1.pom")
	if up.total() != before {
		t.Errorf("a stored file went back upstream")
	}
}

func TestMavenRetriesARateLimitedUpstream(t *testing.T) {
	var asked counter
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.inc()
		if asked.get() <= 2 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("<project/>"))
	})
	f := newFixture(t, up, "-maven-repo", "central="+up.URL, "-retry-backoff", "1ms")
	if r := f.get(t, "/maven/central/org/example/lib/1/lib-1.pom"); r.code != 200 || r.body != "<project/>" {
		t.Fatalf("after two 429s: %d %q, want the artifact", r.code, r.body)
	}
	if asked.get() != 3 {
		t.Errorf("%d upstream requests, want 3", asked.get())
	}
}

func TestMavenGivesUpOnAnUpstreamThatKeepsFailing(t *testing.T) {
	var asked counter
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.inc()
		http.Error(w, "busy", http.StatusServiceUnavailable)
	})
	f := newFixture(t, up, "-maven-repo", "central="+up.URL, "-retry-backoff", "1ms")
	if r := f.get(t, "/maven/central/org/example/lib/1/lib-1.pom"); r.code != http.StatusBadGateway {
		t.Fatalf("an upstream that stays 503: %d, want 502", r.code)
	}
	if asked.get() != maxAttempts {
		t.Errorf("%d upstream requests, want %d", asked.get(), maxAttempts)
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("stored %v", keys)
	}
}

// Only the Maven front-end repeats: the others keep their behaviour.
func TestOtherFrontendsDoNotRetry(t *testing.T) {
	var asked counter
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.inc()
		http.Error(w, "busy", http.StatusServiceUnavailable)
	})
	f := newFixture(t, up, "-retry-backoff", "1ms")
	if r := f.get(t, "/goproxy/example.com/mod/@v/v1.0.0.mod"); r.code != http.StatusBadGateway {
		t.Fatalf("goproxy with a failing upstream: %d, want 502", r.code)
	}
	if asked.get() != 1 {
		t.Errorf("%d upstream requests from the Go front-end, want 1", asked.get())
	}
}

func TestUpstreamRequestsNameTheirSender(t *testing.T) {
	var seen string
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("<project/>"))
	})
	f := newFixture(t, up, "-maven-repo", "central="+up.URL)
	f.get(t, "/maven/central/org/example/lib/1/lib-1.pom")
	if !strings.HasPrefix(seen, "cache-redirect ") {
		t.Errorf("User-Agent %q: Go's default is answered with 429 by Maven Central's CDN", seen)
	}
}
