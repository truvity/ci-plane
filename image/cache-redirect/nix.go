package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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
//
// A NAR's file name is NOT the hash of its (compressed) body on
// cache.nixos.org, so a NAR is verified against the narinfo that names it
// (URL, FileHash, FileSize): serving a narinfo records those in an index
// record in the store (see narIndex). A NAR with no record is passed
// through and never stored.
//
//	everything else      404 (realisations, build logs, .ls listings)
type nixFrontend struct {
	cache    *Cache
	prefix   string
	upstream *url.URL
	priority int
	index    *narIndex
}

// narIndex is what ties a NAR to the narinfo that announced it. The
// narinfo request and the NAR request can reach different replicas, so
// the record is kept in the store, under
//
//	<nix prefix>nar-index/<URL path>.json     e.g. nix/nar-index/nar/<file>.json
//
// holding {"url":"nar/<file>","fileHash":"<nix base 32>","fileSize":N}, and
// in memory as a copy. Records are small and immutable in effect (a NAR's
// URL names one body), written when a narinfo is served.
type narIndex struct {
	store  Store
	prefix string
	mem    *lru
	bg     context.Context
	log    *slog.Logger
}

// narRecord is one index record.
type narRecord struct {
	URL      string `json:"url"`
	FileHash string `json:"fileHash"` // Nix base 32, no "sha256:"
	FileSize int64  `json:"fileSize"`
}

func (x *narIndex) key(urlPath string) string { return x.prefix + "nar-index/" + urlPath + ".json" }

// parseNarinfo extracts the NAR record from a narinfo; ok is false when it
// lacks a usable URL, FileHash or FileSize.
func parseNarinfo(body []byte) (rec narRecord, ok bool) {
	var size string
	var hash string
	for _, line := range strings.Split(string(body), "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "URL":
			rec.URL = v
		case "FileHash":
			hash = v
		case "FileSize":
			size = v
		}
	}
	sum, hasPrefix := strings.CutPrefix(hash, "sha256:")
	n, err := strconv.ParseInt(size, 10, 64)
	if !hasPrefix || !isNix32(sum, 52) || err != nil || n < 0 {
		return rec, false
	}
	f, isNar := strings.CutPrefix(rec.URL, "nar/")
	if !isNar || !isNarFile(f) {
		return rec, false
	}
	rec.FileHash, rec.FileSize = sum, n
	return rec, true
}

// record remembers rec in memory and writes it to the store unless this
// replica already holds an identical one.
func (x *narIndex) record(rec narRecord) {
	b, _ := json.Marshal(rec)
	if v, ok := x.mem.Get(rec.URL); ok && string(v.body) == string(b) {
		return
	}
	if _, err := x.store.Put(x.bg, x.key(rec.URL), strings.NewReader(string(b)), "application/json"); err != nil {
		x.log.Error("store", "op", "put", "frontend", frontendNix, "key", logSafe(x.key(rec.URL)), "err", err)
		// Memory still serves this replica's own NAR requests.
	}
	x.mem.Put(rec.URL, cached{status: http.StatusOK, body: b}, 0)
}

// lookup finds the record of a NAR: memory, then the store. (nil, nil):
// there is none. A store error other than a miss is returned.
func (x *narIndex) lookup(ctx context.Context, urlPath string) (*expectation, error) {
	if v, ok := x.mem.Get(urlPath); ok {
		if rec, ok := decodeRecord(v.body); ok && rec.URL == urlPath {
			return &expectation{size: rec.FileSize, sha256: rec.FileHash}, nil
		}
	}
	raw, err := cacheGet(ctx, x.store, x.key(urlPath))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec, ok := decodeRecord(raw)
	if !ok || rec.URL != urlPath {
		return nil, nil // unreadable: as good as none, the next narinfo rewrites it
	}
	x.mem.Put(urlPath, cached{status: http.StatusOK, body: raw}, 0)
	return &expectation{size: rec.FileSize, sha256: rec.FileHash}, nil
}

func decodeRecord(b []byte) (narRecord, bool) {
	var rec narRecord
	if json.Unmarshal(b, &rec) != nil || !isNix32(rec.FileHash, 52) || rec.FileSize < 0 {
		return rec, false
	}
	return rec, true
}

// cacheGet reads a small object whole.
func cacheGet(ctx context.Context, s Store, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return readSmall(rc)
}

// recordNarinfo is the hook run on every narinfo this front-end serves.
func (n *nixFrontend) recordNarinfo(body []byte) {
	if rec, ok := parseNarinfo(body); ok {
		n.index.record(rec)
	}
}

func (n *nixFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !getOrHead(w, r) {
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/nix")
	switch {
	case p == "/nix-cache-info":
		// Priority is what makes Nix pick this cache over cache.nixos.org
		// (40): LOWER wins, whatever order the client lists caches in.
		body := fmt.Sprintf("StoreDir: /nix/store\nWantMassQuery: 1\nPriority: %d\n", n.priority)
		n.cache.metrics.Request(frontendNix, resultHit)
		writeCached(w, r.Method == http.MethodHead, cached{status: http.StatusOK, contentType: "text/x-nix-cache-info", body: []byte(body)})
	case isNarinfo(p):
		n.cache.ServeImmutable(w, r, object{
			frontend: frontendNix, key: n.prefix + p[1:], upstream: joinURL(n.upstream, p),
			contentType: "text/x-nix-narinfo",
			onBody:      n.recordNarinfo,
		})
	case strings.HasPrefix(p, "/nar/") && isNarFile(p[len("/nar/"):]):
		o := object{
			frontend: frontendNix, key: n.prefix + p[1:], upstream: joinURL(n.upstream, p),
			contentType: "application/x-nix-nar", large: true,
		}
		urlPath := p[1:] // nar/<file>, what a narinfo's URL: says
		o.expect = func(ctx context.Context) (*expectation, error) { return n.index.lookup(ctx, urlPath) }
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
		if !isAlnum(f[i]) && strings.IndexByte("._+-", f[i]) < 0 {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
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
