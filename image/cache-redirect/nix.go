package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const frontendNix = "nix"

// nixFrontend speaks the Nix binary cache protocol (the HTTP layout of
// cache.nixos.org) under /nix:
//
//	/nix-cache-info      answered here: StoreDir, WantMassQuery, Priority
//	/<hash>.narinfo      immutable, small: stored VERBATIM (its signatures
//	                     are over its content; its relative `URL: nar/...`
//	                     is left as it is and resolves against this cache)
//	/nar/<file>          immutable, large: a redirect to the store on a hit
//	everything else      404 (realisations, build logs, .ls listings)
type nixFrontend struct {
	cache    *Cache
	prefix   string
	upstream *url.URL
	priority int
	// verify checks a NAR's bytes against the SHA-256 its file name
	// carries before it is stored.
	verify bool
}

func (n *nixFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !getOrHead(w, r) {
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/nix")
	switch {
	case p == "/nix-cache-info":
		// Priority is what makes Nix pick this cache over cache.nixos.org
		// (40): LOWER wins, and the order of `substituters` does not.
		body := fmt.Sprintf("StoreDir: /nix/store\nWantMassQuery: 1\nPriority: %d\n", n.priority)
		n.cache.metrics.Request(frontendNix, resultHit)
		writeCached(w, r.Method == http.MethodHead, cached{status: http.StatusOK, contentType: "text/x-nix-cache-info", body: []byte(body)})
	case isNarinfo(p):
		n.cache.ServeImmutable(w, r, object{
			frontend: frontendNix, key: n.prefix + p[1:], upstream: joinURL(n.upstream, p),
			contentType: "text/x-nix-narinfo",
		})
	case strings.HasPrefix(p, "/nar/") && isNarFile(p[len("/nar/"):]):
		o := object{
			frontend: frontendNix, key: n.prefix + p[1:], upstream: joinURL(n.upstream, p),
			contentType: "application/x-nix-nar", large: true,
		}
		// cache.nixos.org names a NAR after its file hash:
		// nar/<sha256 in Nix base 32>.nar.<compression>.
		if stem, _, _ := strings.Cut(p[len("/nar/"):], "."); n.verify && isNix32(stem, 52) {
			o.sha256 = stem
		}
		n.cache.ServeImmutable(w, r, o)
	default:
		notFoundHere(w, n.cache.metrics, frontendNix)
	}
}

// isNarinfo matches /<32-character store hash>.narinfo.
func isNarinfo(p string) bool {
	h, ok := strings.CutSuffix(strings.TrimPrefix(p, "/"), ".narinfo")
	return ok && len(p) > 0 && p[0] == '/' && isNix32(h, 32)
}

// isNarFile admits a plain file name: letters, digits, '.', '_', '+', '-',
// not starting with a dot.
func isNarFile(f string) bool {
	if f == "" || f[0] == '.' {
		return false
	}
	for i := 0; i < len(f); i++ {
		c := f[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}

func getOrHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func notFoundHere(w http.ResponseWriter, m *Metrics, frontend string) {
	m.Request(frontend, resultNotFound)
	http.Error(w, "not found", http.StatusNotFound)
}

// joinURL appends an already validated path (no '%', '?' or '#') to an
// upstream base URL, byte for byte: RawPath keeps a module path's '!'
// case-encoding as it was requested instead of re-escaping it to %21.
func joinURL(base *url.URL, p string) string {
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + p
	u.RawPath = u.Path
	return u.String()
}
