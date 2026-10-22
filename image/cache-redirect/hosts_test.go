package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

const (
	nixHost    = "nix.cache.example.test"
	mavenHost  = "maven.cache.example.test"
	gradleHost = "gradle.cache.example.test"
	goHost     = "golang.cache.example.test"
)

// hostFlags names a host for each of the three front-ends that can have one.
var hostFlags = []string{"-gradle", "-hosts", "nix=" + nixHost + ",maven=" + mavenHost, "-hosts", "gradle=" + gradleHost}

// onHost sends a request with the given Host header (the connection is to the
// test server whatever it says, as through a gateway).
func (f *fixture) onHost(t *testing.T, method, host, path string, body []byte) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, rerr := io.ReadAll(resp.Body)
	return reply{code: resp.StatusCode, body: string(b), header: resp.Header, readErr: rerr}
}

func TestHostNixServedAtRoot(t *testing.T) {
	f := newFixture(t, newUpstream(t, http.NotFound), hostFlags...)

	r := f.onHost(t, http.MethodGet, nixHost, "/nix-cache-info", nil)
	if r.code != 200 || !strings.Contains(r.body, "StoreDir: /nix/store") {
		t.Fatalf("/nix-cache-info on the nix host: %d %q", r.code, r.body)
	}
	// The same Host with the port a gateway may leave on it, or in capitals.
	for _, h := range []string{nixHost + ":443", strings.ToUpper(nixHost), nixHost + "."} {
		if r := f.onHost(t, http.MethodGet, h, "/nix-cache-info", nil); r.code != 200 {
			t.Errorf("Host %q: %d, want the nix host", h, r.code)
		}
	}
	// A narinfo is looked up at the root, and asked of the upstream at its own.
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	f = newFixture(t, up, hostFlags...)
	if r := f.onHost(t, http.MethodGet, nixHost, "/0123456789abcdfghijklmnpqrsvwxyz.narinfo", nil); r.code != http.StatusNotFound {
		t.Errorf("narinfo miss: %d, want the upstream's 404", r.code)
	}
	if len(up.paths) != 1 || up.paths[0] != "/0123456789abcdfghijklmnpqrsvwxyz.narinfo" {
		t.Errorf("upstream asked for %v, want /<hash>.narinfo (the host is the only prefix)", up.paths)
	}
	// A gateway that still rewrites the client's root onto /nix (the way
	// before hosts existed) sends /nix/nix-cache-info: served as it is, not
	// moved under /nix a second time, so the rewrite can be taken away after
	// the host is set.
	if r := f.onHost(t, http.MethodGet, nixHost, "/nix/nix-cache-info", nil); r.code != 200 || !strings.Contains(r.body, "StoreDir") {
		t.Errorf("/nix/nix-cache-info on the nix host: %d %q, want it served as it is", r.code, r.body)
	}
	const other = "1123456789abcdfghijklmnpqrsvwxyz.narinfo"
	r = f.onHost(t, http.MethodGet, nixHost, "/nix/"+other, nil)
	if r.code != http.StatusNotFound || len(up.paths) != 2 || up.paths[1] != "/"+other {
		t.Errorf("/nix/<hash>.narinfo on the nix host: %d, upstream asked %v, want the same upstream path as the root form", r.code, up.paths)
	}
	// Other kinds are not served on the nix host.
	if r := f.onHost(t, http.MethodGet, nixHost, "/central/org/x/1/x-1.pom", nil); r.code != http.StatusNotFound {
		t.Errorf("a Maven path on the nix host: %d, want 404", r.code)
	}
}

func TestHostMavenServedAtRoot(t *testing.T) {
	up := mavenUpstream(t)
	f := newFixture(t, up, append([]string{"-maven-repo", "central=" + up.URL + "/central,google=" + up.URL + "/google"}, hostFlags...)...)

	r := f.onHost(t, http.MethodGet, mavenHost, "/", nil)
	if r.code != 200 || !strings.Contains(r.body, "central") || !strings.Contains(r.body, "google") {
		t.Fatalf("index on the maven host: %d %q", r.code, r.body)
	}
	base := "/central/org/example/lib/1.2.3/lib-1.2.3"
	if r := f.onHost(t, http.MethodGet, mavenHost+":8080", base+".pom", nil); r.code != 200 || r.body != "<project/>" {
		t.Fatalf("pom on the maven host: %d %q", r.code, r.body)
	}
	if got := up.paths[len(up.paths)-1]; got != "/central/org/example/lib/1.2.3/lib-1.2.3.pom" {
		t.Errorf("upstream asked for %s", got)
	}
	if _, ok := f.store.object("maven/central/org/example/lib/1.2.3/lib-1.2.3.pom"); !ok {
		t.Errorf("not stored under the front-end's prefix; store holds %v", f.store.keys())
	}
	if r := f.onHost(t, http.MethodGet, mavenHost, base+".jar", nil); r.code != 200 {
		t.Fatalf("jar miss: %d", r.code)
	}
	r = f.onHost(t, http.MethodGet, mavenHost, base+".jar", nil)
	if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://store.example/maven/central/") {
		t.Errorf("jar hit: %d %q, want a redirect to the store", r.code, r.header.Get("Location"))
	}
	if r := f.onHost(t, http.MethodGet, mavenHost, "/nope/x.pom", nil); r.code != http.StatusNotFound {
		t.Errorf("unknown repository: %d, want 404", r.code)
	}
}

func TestHostGradleServedAtRoot(t *testing.T) {
	f := newFixture(t, newUpstream(t, http.NotFound), hostFlags...)

	// The probe of a runner: GET / is 200.
	if r := f.onHost(t, http.MethodGet, gradleHost, "/", nil); r.code != 200 {
		t.Fatalf("GET / on the gradle host: %d, want 200", r.code)
	}
	if r := f.onHost(t, http.MethodGet, gradleHost, "/"+gradleKey, nil); r.code != http.StatusNotFound {
		t.Fatalf("GET before PUT: %d, want 404", r.code)
	}
	if r := f.onHost(t, http.MethodPut, gradleHost+":443", "/"+gradleKey, []byte("entry bytes")); r.code != http.StatusNoContent {
		t.Fatalf("PUT: %d, want 204", r.code)
	}
	if b, ok := f.store.object("gradle/" + gradleKey); !ok || string(b) != "entry bytes" {
		t.Fatalf("stored %q (%v); store holds %v", b, ok, f.store.keys())
	}
	r := f.onHost(t, http.MethodGet, gradleHost, "/"+gradleKey, nil)
	if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://store.example/gradle/"+gradleKey+"?") {
		t.Errorf("GET after PUT: %d %q, want a redirect to the store", r.code, r.header.Get("Location"))
	}
	// Only Nix is tolerant of its own prefix: a Maven repository may be named
	// "maven", so /maven/... on the maven host is a repository path.
	if r := f.onHost(t, http.MethodGet, mavenHost, "/maven/", nil); r.code != http.StatusNotFound {
		t.Errorf("/maven/ on the maven host: %d, want 404 (an unknown repository)", r.code)
	}
	// Not a key: refused whatever the Host.
	if r := f.onHost(t, http.MethodPut, gradleHost, "/not-a-key", []byte("x")); r.code != http.StatusNotFound {
		t.Errorf("PUT of a non-key: %d, want 404", r.code)
	}
	// The nix host does not write gradle entries.
	if r := f.onHost(t, http.MethodPut, nixHost, "/"+gradleKey, []byte("x")); r.code == http.StatusNoContent {
		t.Errorf("PUT on the nix host stored an entry")
	}
}

// Any other Host keeps the path layout, so the Go name and the in-cluster
// Service work as they did before hosts existed.
func TestHostOthersKeepPathRouting(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mod") {
			_, _ = w.Write([]byte("module example.com/mod\n"))
			return
		}
		http.NotFound(w, r)
	})
	f := newFixture(t, up, hostFlags...)

	for _, host := range []string{goHost, "cache-redirect.example.svc", "cache-redirect.example.svc.cluster.local:80", ""} {
		r := f.onHost(t, http.MethodGet, host, "/goproxy/example.com/mod/@v/v1.6.0.mod", nil)
		if r.code != 200 || !strings.Contains(r.body, "module example.com/mod") {
			t.Errorf("Host %q, /goproxy: %d %q", host, r.code, r.body)
		}
		if r := f.onHost(t, http.MethodGet, host, "/nix/nix-cache-info", nil); r.code != 200 {
			t.Errorf("Host %q, /nix/nix-cache-info: %d, want 200 (the path layout, as before)", host, r.code)
		}
		if r := f.onHost(t, http.MethodGet, host, "/maven/", nil); r.code != 200 {
			t.Errorf("Host %q, /maven/: %d, want 200", host, r.code)
		}
		if r := f.onHost(t, http.MethodGet, host, "/gradle/cache/", nil); r.code != 200 {
			t.Errorf("Host %q, /gradle/cache/: %d, want 200", host, r.code)
		}
		if r := f.onHost(t, http.MethodGet, host, "/healthz", nil); r.code != 200 {
			t.Errorf("Host %q, /healthz: %d, want 200", host, r.code)
		}
	}
}

func TestConfigHosts(t *testing.T) {
	none := func(string) string { return "" }
	args := []string{"-s3-bucket", "b", "-gradle", "-hosts", "nix=Nix.Example.TEST,maven=m.example.test", "-hosts", "gradle=g.example.test"}
	c, err := parseConfig(args, none, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Hosts.String(); got != "gradle=g.example.test,maven=m.example.test,nix=nix.example.test" {
		t.Errorf("hosts %q: want them normalised (lower case, no port)", got)
	}
	env := map[string]string{"CACHE_REDIRECT_S3_BUCKET": "b", "CACHE_REDIRECT_HOSTS": "nix=a.example.test, maven=b.example.test"}
	c, err = parseConfig(nil, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Hosts.String(); got != "maven=b.example.test,nix=a.example.test" {
		t.Errorf("hosts from the environment %q", got)
	}
	if c, _ = parseConfig([]string{"-s3-bucket", "b"}, none, io.Discard); len(c.Hosts.routes()) != 0 {
		t.Errorf("no -hosts must mean no routes, got %v", c.Hosts.routes())
	}

	for want, args := range map[string][]string{
		"want kind=host":     {"-s3-bucket", "b", "-hosts", "a.example.test"},
		"the kind one of":    {"-s3-bucket", "b", "-hosts", "go=a.example.test"},
		"not a host name":    {"-s3-bucket", "b", "-hosts", "nix=https://a.example.test/x"},
		"given a host twice": {"-s3-bucket", "b", "-hosts", "nix=a.example.test", "-hosts", "nix=b.example.test"},
		"given to both":      {"-s3-bucket", "b", "-hosts", "nix=a.example.test,maven=a.example.test"},
		"front-end is off":   {"-s3-bucket", "b", "-hosts", "gradle=a.example.test"},
	} {
		_, err := parseConfig(args, none, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want one saying %q", args, err, want)
		}
	}
}
