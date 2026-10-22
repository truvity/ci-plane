package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const frontendNpm = "npm"

const (
	// maxPackument bounds a packument held in memory, decoded. The full
	// document of a busy package (every version's manifest, readme and
	// `time`) is tens of megabytes, far past the 8 MiB maxSmall that bounds
	// every other small object.
	maxPackument = 64 << 20
	// maxNpmPost bounds the body of an audit request: a name and the versions
	// of every package in a lockfile.
	maxNpmPost = 32 << 20
	// abbreviatedType is the media type of the install-time document, the
	// packument without readmes, scripts and the like, that npm, yarn and pnpm
	// ask for first.
	abbreviatedType = "application/vnd.npm.install-v1+json"
	// failureWindow is how long a packument whose refresh failed is served
	// from the copy held, before upstream is asked again: not a wait of the
	// full retry back-off for every request while the registry is down.
	failureWindow = 15 * time.Second
)

// npmFrontend speaks the npm registry protocol under /npm, read-only:
//
//	/<pkg>                       packument, mutable: two variants by Accept (full, and the
//	                             abbreviated install document); kept for a short while, then
//	                             revalidated with its ETag; the last copy lives in the store
//	                             and is served when upstream fails
//	/<pkg>/-/<base>-<ver>.tgz    tarball, immutable, large: a redirect on a hit
//	/-/ping                      200, the probe `npm ping` sends
//	/-/package/<pkg>/dist-tags   mutable, passed through
//	/-/v1/search                 passed through
//	/-/npm/v1/security/...       POST, passed through (npm audit, yarn npm audit)
//
// <pkg> arrives as `name`, `@scope/name` or `@scope%2fname` and is the same
// package in each form. Every `dist.tarball` of a packument is rewritten to
// the address the client reached the service by, because yarn and pnpm fetch
// the URL the packument names when it is not the one they compute from the
// registry: without the rewrite they would bypass the cache, and record
// registry.npmjs.org in their lockfiles. The npm command is the exception: it
// replaces the host itself, and keeps the original URL in package-lock.json
// (see isNpmCLI).
//
// Nothing a client sends writes anything: publish, unpublish, dist-tag and
// login are 405s, and no credential is forwarded upstream.
type npmFrontend struct {
	cache    *Cache
	prefix   string
	upstream *url.URL
	fresh    time.Duration
	mem      *lru // packuments: raw upstream bodies, with their validators

	flights flightGroup
}

// flightGroup runs one refresh per key however many requests ask for it.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*npmFlight
}

type npmFlight struct {
	done chan struct{}
	res  npmResult
}

// npmResult is the outcome of a refresh: status 200 with a body, a 404/410
// (its text in the body), or 0 for a failure with nothing to serve.
type npmResult struct {
	entry  cached
	result string // the metrics result of the request(s) it answers
}

func newNpmFrontend(c *Cache, prefix string, upstream *url.URL, fresh time.Duration, memBytes int64) *npmFrontend {
	m := newLRU(memBytes, time.Now)
	m.entryMax = min(maxPackument, memBytes/2)
	return &npmFrontend{cache: c, prefix: prefix, upstream: upstream, fresh: fresh, mem: m,
		flights: flightGroup{m: map[string]*npmFlight{}}}
}

func (n *npmFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/npm")
	switch {
	case p == "" || p == "/":
		if !getOrHead(w, r) {
			return
		}
		n.cache.metrics.Request(frontendNpm, resultHit)
		body := "npm: " + n.upstream.Host + "\n"
		writeCached(w, r.Method == http.MethodHead, cached{status: http.StatusOK, contentType: "text/plain; charset=utf-8", body: []byte(body)})
		return
	case p == "/-/ping":
		if !getOrHead(w, r) {
			return
		}
		n.cache.metrics.Request(frontendNpm, resultHit)
		writeCached(w, r.Method == http.MethodHead, cached{status: http.StatusOK, contentType: "application/json", body: []byte("{}")})
		return
	case p == "/-/v1/search":
		if getOrHead(w, r) {
			n.proxy(w, r, p)
		}
		return
	case p == "/-/npm/v1/security/advisories/bulk", p == "/-/npm/v1/security/audits", p == "/-/npm/v1/security/audits/quick":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		n.proxy(w, r, p)
		return
	}
	if rest, ok := strings.CutPrefix(p, "/-/package/"); ok {
		name, tail, valid := splitNpmName(rest)
		switch {
		case !valid || len(tail) != 1 || tail[0] != "dist-tags":
			notFoundHere(w, n.cache.metrics, frontendNpm)
		case getOrHead(w, r):
			n.cache.ServeMutable(w, r, frontendNpm, n.upstreamURL("/-/package/"+npmWireName(name)+"/dist-tags"))
		}
		return
	}
	if strings.HasPrefix(p, "/-/") {
		notFoundHere(w, n.cache.metrics, frontendNpm)
		return
	}
	name, tail, ok := splitNpmName(p[1:])
	if !ok {
		notFoundHere(w, n.cache.metrics, frontendNpm)
		return
	}
	if !getOrHead(w, r) {
		return
	}
	switch {
	case len(tail) == 0:
		n.packument(w, r, name)
	case len(tail) == 2 && tail[0] == "-" && validTarballFile(name, tail[1]):
		n.cache.ServeImmutable(w, r, object{
			frontend: frontendNpm, key: n.prefix + name + "/-/" + tail[1],
			upstream: n.upstreamURL("/" + name + "/-/" + tail[1]), contentType: "application/octet-stream", large: true,
		})
	default:
		notFoundHere(w, n.cache.metrics, frontendNpm)
	}
}

// splitNpmName takes the leading package name off a path (without its leading
// slash): `name`, or `@scope/name` (the scope and the name are two path
// elements once '%2f' is decoded). The rest of the path is returned as elements.
func splitNpmName(p string) (name string, tail []string, ok bool) {
	els := strings.Split(p, "/")
	n := 1
	if strings.HasPrefix(els[0], "@") {
		n = 2
	}
	if len(els) < n {
		return "", nil, false
	}
	for i, el := range els[:n] {
		if i < n-1 {
			el = strings.TrimPrefix(el, "@") // the scope
		}
		if !validNpmNameElement(el) {
			return "", nil, false
		}
	}
	return strings.Join(els[:n], "/"), els[n:], true
}

// validNpmNameElement admits what a package name (or a scope) can hold, the
// legacy names with capitals and punctuation included, and nothing that is a
// path element of its own or a way out of the store prefix. Names do not
// begin with '.', '_' or '-'.
func validNpmNameElement(s string) bool {
	if s == "" || s[0] == '.' || s[0] == '_' || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) && strings.IndexByte("._~-!*'()", s[i]) < 0 {
			return false
		}
	}
	return true
}

// validTarballFile reports whether file is `<basename>-<version>.tgz` for the
// package, the only name a registry gives a tarball.
func validTarballFile(name, file string) bool {
	base := name[strings.LastIndex(name, "/")+1:]
	version, ok := strings.CutSuffix(strings.TrimPrefix(file, base+"-"), ".tgz")
	if !strings.HasPrefix(file, base+"-") || !ok || version == "" {
		return false
	}
	for i := 0; i < len(version); i++ {
		if !isAlnum(version[i]) && strings.IndexByte("._+-", version[i]) < 0 {
			return false
		}
	}
	return true
}

// npmWireName is the form of a name in the path of a packument request:
// '@scope%2fname', which is the only form the registry accepts everywhere.
func npmWireName(name string) string { return strings.Replace(name, "/", "%2f", 1) }

// upstreamURL appends an already validated path, '%2f' of a scoped name
// included, to the registry's base URL.
func (n *npmFrontend) upstreamURL(p string) string {
	u := *n.upstream
	raw := strings.TrimSuffix(n.upstream.EscapedPath(), "/") + p
	u.RawPath = raw
	u.Path, _ = url.PathUnescape(raw)
	return u.String()
}

// packumentVariant is which packument the client wants: the abbreviated install
// document when it accepts that media type (npm, yarn and pnpm do, first), the
// full one otherwise (`npm view`, a pnpm or yarn minimum release age, which
// need `time`).
func packumentVariant(r *http.Request) (name, accept string) {
	if strings.Contains(r.Header.Get("Accept"), abbreviatedType) {
		return "abbreviated", abbreviatedType
	}
	return "full", "application/json"
}

// publicBase is the address the client reached this front-end by, as it is
// to write it into a tarball URL: the registry URL it was given, which for a
// host-root name has no path and otherwise ends in /npm. The Host header is
// the client's own claim and is checked before it is written into a document;
// ok is false when it is not a host name.
func publicBase(r *http.Request) (string, bool) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])); p == "http" || p == "https" {
		scheme = p
	}
	host := strings.ToLower(r.Host)
	if h, port, ok := strings.Cut(host, ":"); ok && (scheme == "https" && port == "443" || scheme == "http" && port == "80") {
		host = h
	}
	if !validHostName(host) {
		return "", false
	}
	base := scheme + "://" + host
	if !arrivedAtHostRoot(r) {
		base += "/npm"
	}
	return base, true
}

func (n *npmFrontend) packument(w http.ResponseWriter, r *http.Request, name string) {
	vname, accept := packumentVariant(r)
	key := n.prefix + "_meta/" + name + "." + vname
	head := r.Method == http.MethodHead
	if v, ok := n.cache.neg.Get(key); ok {
		n.cache.metrics.Request(frontendNpm, resultNotFound)
		writeCached(w, head, v)
		return
	}
	res := npmResult{result: resultHit}
	e, have := n.mem.Get(key)
	if !have || time.Since(e.fetched) >= n.fresh {
		var ok bool
		if res, ok = n.refresh(r.Context(), key, name, accept, e, have); !ok {
			return
		}
		e = res.entry
	}
	switch {
	case e.status == 0:
		n.cache.metrics.Request(frontendNpm, resultUpstreamError)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
	case e.status != http.StatusOK:
		n.cache.metrics.Request(frontendNpm, resultNotFound)
		writeCached(w, head, e)
	default:
		n.cache.metrics.Request(frontendNpm, res.result)
		n.writePackument(w, r, e)
	}
}

// writePackument answers with e, its tarball URLs rewritten to the address the
// request came to, compressed when the client accepts that.
func (n *npmFrontend) writePackument(w http.ResponseWriter, r *http.Request, e cached) {
	body := e.body
	base, ok := publicBase(r)
	rewritten := ok && !isNpmCLI(r)
	if rewritten {
		body = rewriteTarballs(body, n.upstream, base)
	}
	h := w.Header()
	h.Set("Content-Type", e.contentType)
	h.Add("Vary", "Accept")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(n.fresh/time.Second)))
	if e.etag != "" {
		// The answer depends on the address it was asked at, so does its validator.
		h.Set("ETag", `W/"`+hashHex(e.etag+"|"+base+"|"+strconv.FormatBool(rewritten))+`"`)
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == h.Get("ETag") {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = buf.Bytes()
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// isNpmCLI reports whether the client is the npm command. It replaces the
// registry host of a tarball URL by the registry it was given (replace-registry-host,
// "npmjs" by default), and it writes the URL into package-lock.json as it was in the
// document: a rewritten URL would put this service's address in every lockfile
// generated through it, where it breaks `npm ci` for anyone who cannot reach it.
// Yarn and pnpm compute the URL from the registry, and use the document's only
// when it differs, so they need the rewrite (and then keep their lockfiles clean).
func isNpmCLI(r *http.Request) bool { return strings.HasPrefix(r.Header.Get("User-Agent"), "npm/") }

// rewriteTarballs points every `"tarball":"<registry>/` of a packument at
// base. A byte replacement of the key and the start of its value on the raw
// document, not a parse: it is tens of megabytes, and a README or a
// description that holds the same text has the quotes around it escaped, so
// only a real key matches.
func rewriteTarballs(body []byte, upstream *url.URL, base string) []byte {
	reg := strings.TrimSuffix(upstream.String(), "/")
	from := []string{`"tarball":"` + reg + `/`}
	if strings.HasPrefix(reg, "https://") { // old documents carry http://
		from = append(from, `"tarball":"http://`+strings.TrimPrefix(reg, "https://")+`/`)
	}
	to := []byte(`"tarball":"` + base + `/`)
	for _, f := range from {
		if bytes.Contains(body, []byte(f)) {
			body = bytes.ReplaceAll(body, []byte(f), to)
		}
	}
	return body
}

// refresh brings a packument up to date once, however many requests ask for it,
// and returns what each of them is to be answered with. have/prev are the copy
// in memory, fresh or not. ok is false when the request was abandoned.
func (n *npmFrontend) refresh(ctx context.Context, key, name, accept string, prev cached, have bool) (npmResult, bool) {
	n.flights.mu.Lock()
	f, running := n.flights.m[key]
	if !running {
		f = &npmFlight{done: make(chan struct{})}
		n.flights.m[key] = f
		go func() {
			f.res = n.fetchPackument(key, name, accept, prev, have)
			n.flights.mu.Lock()
			delete(n.flights.m, key)
			n.flights.mu.Unlock()
			close(f.done)
		}()
	}
	n.flights.mu.Unlock()
	select {
	case <-f.done:
		return f.res, true
	case <-ctx.Done():
		return npmResult{}, false
	}
}

// fetchPackument asks upstream, with the validator of the copy it has (from
// memory, or from the store when this pod has none), and keeps the answer. On
// a failure it falls back to the copy: a registry outage must not break
// installs of what has been installed before.
func (n *npmFrontend) fetchPackument(key, name, accept string, prev cached, have bool) npmResult {
	c := n.cache
	select {
	case c.fetchSem <- struct{}{}:
	case <-c.bg.Done():
		return npmResult{}
	}
	defer func() { <-c.fetchSem }()
	c.metrics.inFlight.Add(1)
	defer c.metrics.inFlight.Add(-1)

	if !have {
		var err error
		if prev, have, err = n.load(key); err != nil {
			c.log.Error("store", "op", "get", "frontend", frontendNpm, "key", logSafe(key), "err", err)
		}
	}
	hdr := http.Header{"Accept": {accept}, "Accept-Encoding": {"gzip"}}
	if have && prev.etag != "" {
		hdr.Set("If-None-Match", prev.etag)
	}
	upURL := n.upstreamURL("/" + npmWireName(name))
	resp, err := c.fetchHeader(c.bg, http.MethodGet, frontendNpm, upURL, hdr)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		switch resp.StatusCode {
		case http.StatusNotModified:
			if have {
				prev.fetched = time.Now()
				n.mem.Put(key, prev, 0)
				return npmResult{entry: prev, result: resultMiss}
			}
			err = errors.New("upstream answered 304 to a request that was not conditional")
		case http.StatusOK:
			var e cached
			if e, err = readPackument(resp); err == nil {
				e.fetched = time.Now()
				e.contentType = resp.Header.Get("Content-Type")
				if e.contentType == "" {
					e.contentType = "application/json"
				}
				n.mem.Put(key, e, 0)
				if !have || prev.etag != e.etag || e.etag == "" && !bytes.Equal(prev.body, e.body) {
					go n.save(key, e)
				}
				return npmResult{entry: e, result: resultMiss}
			}
		case http.StatusNotFound, http.StatusGone:
			v := notFound(resp)
			c.neg.Put(key, v, c.negativeTTL)
			return npmResult{entry: v, result: resultNotFound}
		default:
			err = fmt.Errorf("upstream answered %s", resp.Status)
		}
	}
	c.log.Warn("upstream", "frontend", frontendNpm, "url", logSafe(upURL), "err", err)
	if !have {
		return npmResult{}
	}
	// Serve the copy, and do not ask again for a moment: with the registry
	// down, every request would otherwise wait out the retries first.
	c.log.Warn("serving the last copy of a packument", "key", logSafe(key))
	prev.fetched = time.Now().Add(failureWindow - n.fresh)
	n.mem.Put(key, prev, 0)
	return npmResult{entry: prev, result: resultStale}
}

// readPackument reads an upstream 200 whole, decoded, to maxPackument.
func readPackument(resp *http.Response) (cached, error) {
	raw, err := io.ReadAll(io.LimitReader(newFetchReader(resp, nil, nil), maxPackument+1))
	if err != nil {
		return cached{}, err
	}
	if len(raw) > maxPackument {
		return cached{}, fmt.Errorf("upstream body is over %d bytes", maxPackument)
	}
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return cached{}, err
		}
		if raw, err = io.ReadAll(io.LimitReader(zr, maxPackument+1)); err != nil {
			return cached{}, err
		}
		if len(raw) > maxPackument {
			return cached{}, fmt.Errorf("upstream body is over %d bytes", maxPackument)
		}
	}
	return cached{status: http.StatusOK, body: raw, etag: resp.Header.Get("ETag")}, nil
}

// The stored copy of a packument: a line with the validator and the media type,
// then the document gzip-compressed (it is stored once per change and read when
// a pod has none, so the bytes are worth the CPU).
const storedMagic = "cr-npm1 "

func (n *npmFrontend) save(key string, e cached) {
	var buf bytes.Buffer
	buf.WriteString(storedMagic + strings.ReplaceAll(e.etag, "\n", " ") + "\t" + e.contentType + "\n")
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(e.body)
	_ = zw.Close()
	nb, err := n.cache.store.Put(n.cache.bg, key, &buf, "application/octet-stream")
	n.cache.metrics.Upload(frontendNpm, nb, err)
	if err != nil {
		n.cache.log.Error("store", "op", "put", "frontend", frontendNpm, "key", logSafe(key), "err", err)
	}
}

// load reads the stored copy of a packument; have is false when there is none.
func (n *npmFrontend) load(key string) (e cached, have bool, err error) {
	rc, err := n.cache.store.Get(n.cache.bg, key)
	if errors.Is(err, ErrNotFound) {
		return cached{}, false, nil
	}
	if err != nil {
		return cached{}, false, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxPackument+1))
	if err != nil {
		return cached{}, false, err
	}
	line, rest, ok := bytes.Cut(raw, []byte("\n"))
	meta, magic := strings.CutPrefix(string(line), storedMagic)
	if !ok || !magic {
		return cached{}, false, fmt.Errorf("%s is not a stored packument", key)
	}
	etag, ctype, _ := strings.Cut(meta, "\t")
	zr, err := gzip.NewReader(bytes.NewReader(rest))
	if err != nil {
		return cached{}, false, err
	}
	body, err := io.ReadAll(io.LimitReader(zr, maxPackument+1))
	if err != nil || len(body) > maxPackument {
		return cached{}, false, fmt.Errorf("%s: unreadable stored packument: %v", key, err)
	}
	// fetched stays zero: how old the stored copy is, is not known, so it is
	// revalidated before it is served unless upstream is down.
	return cached{status: http.StatusOK, contentType: ctype, body: body, etag: etag}, true, nil
}

// proxy passes a request through to the registry as it is: the audit POSTs
// (with their Content-Encoding) and a search. Nothing is stored or retried, no
// credential goes upstream, and a request body is bounded.
func (n *npmFrontend) proxy(w http.ResponseWriter, r *http.Request, p string) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxNpmPost)
	}
	rp := &httputil.ReverseProxy{
		Transport: n.cache.client.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u, _ := url.Parse(n.upstreamURL(p))
			u.RawQuery = pr.In.URL.RawQuery
			pr.Out.URL = u
			pr.Out.Host = u.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("User-Agent", userAgent)
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 500 {
				n.cache.metrics.Request(frontendNpm, resultUpstreamError)
			} else {
				n.cache.metrics.Request(frontendNpm, resultMiss)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			n.cache.log.Warn("upstream", "frontend", frontendNpm, "path", logSafe(p), "err", err)
			n.cache.metrics.Request(frontendNpm, resultUpstreamError)
			http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
