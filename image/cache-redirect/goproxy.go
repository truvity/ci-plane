package main

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const frontendGo = "goproxy"

// tilePath matches a checksum database tile: tile/<H>/<L or data>/<N>,
// N in three-digit groups (x123/456), with an optional partial width
// (.p/<W>). A tile, full or partial, never changes once served.
var tilePath = regexp.MustCompile(`^tile/[0-9]+/(data|[0-9]+)/(x[0-9]{3}/)*[0-9]{3}(\.p/[0-9]+)?$`)

// goFrontend speaks the GOPROXY protocol under /goproxy:
//
//	/<module>/@v/<version>.info|.mod   immutable, small, for a canonical version
//	/<module>/@v/<version>.zip         immutable, large: a redirect on a hit
//	/<module>/@v/list, /<module>/@latest, /<module>/@v/<query>.info
//	                                   mutable: passed through, remembered briefly
//	/sumdb/<name>/supported            200 for the one configured database
//	/sumdb/<name>/tile/...             immutable, small
//	/sumdb/<name>/lookup/..., /latest  mutable: passed through
//
// Module paths and versions stay exactly as requested, '!' case-encoding
// included: they name the store key and the upstream path both. An
// upstream 404 or 410 reaches the client unchanged, so GOPROXY=a,b,direct
// falls through to the next entry.
type goFrontend struct {
	cache         *Cache
	prefix        string
	upstream      *url.URL
	sumdbName     string
	sumdbUpstream *url.URL
}

func (g *goFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !getOrHead(w, r) {
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/goproxy")
	if !isPlainPath(p) {
		notFoundHere(w, g.cache.metrics, frontendGo)
		return
	}
	if rest, ok := strings.CutPrefix(p, "/sumdb/"); ok {
		g.sumdb(w, r, rest)
		return
	}
	if mod, ok := strings.CutSuffix(p, "/@latest"); ok && validModule(mod) {
		g.cache.ServeMutable(w, r, frontendGo, joinURL(g.upstream, p))
		return
	}
	i := strings.LastIndex(p, "/@v/")
	if i < 1 || !validModule(p[:i]) {
		notFoundHere(w, g.cache.metrics, frontendGo)
		return
	}
	file := p[i+len("/@v/"):]
	if file == "list" {
		g.cache.ServeMutable(w, r, frontendGo, joinURL(g.upstream, p))
		return
	}
	ext := path.Ext(file)
	version := strings.TrimSuffix(file, ext)
	var contentType string
	switch ext {
	case ".info":
		contentType = "application/json"
	case ".mod":
		contentType = "text/plain; charset=utf-8"
	case ".zip":
		contentType = "application/zip"
	default:
		notFoundHere(w, g.cache.metrics, frontendGo)
		return
	}
	if !canonicalVersion(version) {
		// The go command resolves a query (a branch, a commit, `latest`)
		// through <query>.info, and the answer moves with the branch: pass
		// it through. It never asks for a .mod or .zip by anything but a
		// canonical version.
		if ext == ".info" {
			g.cache.ServeMutable(w, r, frontendGo, joinURL(g.upstream, p))
			return
		}
		notFoundHere(w, g.cache.metrics, frontendGo)
		return
	}
	g.cache.ServeImmutable(w, r, object{
		frontend: frontendGo, key: g.prefix + p[1:], upstream: joinURL(g.upstream, p),
		contentType: contentType, large: ext == ".zip",
	})
}

// sumdb proxies the one checksum database it is configured for. Any other
// name is a 404, which tells the go command to contact that database
// directly.
func (g *goFrontend) sumdb(w http.ResponseWriter, r *http.Request, rest string) {
	name, sub, ok := strings.Cut(rest, "/")
	if !ok || name != g.sumdbName {
		notFoundHere(w, g.cache.metrics, frontendGo)
		return
	}
	switch {
	case sub == "supported":
		g.cache.metrics.Request(frontendGo, resultHit)
		writeCached(w, r.Method == http.MethodHead, cached{status: http.StatusOK, contentType: "text/plain; charset=utf-8"})
	case sub == "latest" || strings.HasPrefix(sub, "lookup/"):
		g.cache.ServeMutable(w, r, frontendGo, joinURL(g.sumdbUpstream, "/"+sub))
	case tilePath.MatchString(sub):
		g.cache.ServeImmutable(w, r, object{
			frontend: frontendGo, key: g.prefix + "sumdb/" + name + "/" + sub, upstream: joinURL(g.sumdbUpstream, "/"+sub),
			contentType: "application/octet-stream",
		})
	default:
		notFoundHere(w, g.cache.metrics, frontendGo)
	}
}

// validModule reports whether p ("/<escaped module path>") is a module
// path in the go command's case-encoding.
func validModule(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	_, err := module.UnescapePath(p[1:])
	return err == nil
}

// canonicalVersion reports whether an escaped version is a canonical
// semantic version (pseudo-versions and +incompatible included): the only
// versions whose .info, .mod and .zip never change.
func canonicalVersion(escaped string) bool {
	v, err := module.UnescapeVersion(escaped)
	if err != nil || !semver.IsValid(v) {
		return false
	}
	c := semver.Canonical(v)
	return v == c || v == c+"+incompatible"
}

// isPlainPath admits the characters a module path, an escaped version or a
// sumdb path can hold, and no '..' element: the path becomes a store key
// and an upstream URL, so nothing in it may be read as anything else.
func isPlainPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	for _, el := range strings.Split(p[1:], "/") {
		if el == "" || el == "." || el == ".." {
			return false
		}
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("/._~!+@-", c) >= 0) {
			return false
		}
	}
	return true
}
