package main

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const frontendGradle = "gradle"

// gradleEntryType is the media type Gradle gives a build cache entry.
const gradleEntryType = "application/vnd.gradle.build-cache-entry.1"

// gradleFrontend is Gradle's remote build cache (the HttpBuildCache protocol)
// under /gradle/cache/<key>, kept in the store:
//
//	GET  /gradle/cache/<key>  302 to a presigned GET of the entry, or 404
//	PUT  /gradle/cache/<key>  stores the body; 204
//	GET  /gradle/cache/       200: the probe of a runner
//
// Unlike the other front-ends it is WRITABLE by whoever may reach it, so it is
// off unless asked for, and it must not be shared between tiers: each tier
// (preview, stable) runs its own, on its own bucket, so a reader only reads
// what its own tier's jobs wrote. With -hosts gradle=<host> it is also served
// at the root of that host (GET|PUT /<key>). What a write can do is bounded:
//
//   - only keys of 32 to 128 hexadecimal digits, which is what Gradle sends (a
//     hash of the task's inputs);
//   - a body that arrives whole or not at all (Store.Put commits on io.EOF
//     only, and a request body cut short ends in an error);
//   - a Content-Length, and no more than maxEntry bytes of it (413: Gradle
//     reads that as "too large to cache" and carries on);
//   - the first write of a key wins: a later PUT of the same key is answered
//     204 and changes nothing, so an entry already in the store cannot be
//     replaced by another job's.
//
// A hit is a redirect, so the bytes of an entry never pass through the pod.
type gradleFrontend struct {
	cache    *Cache
	prefix   string
	maxEntry int64
}

func (g *gradleFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, "/gradle/cache/")
	if !ok {
		notFoundHere(w, g.cache.metrics, frontendGradle)
		return
	}
	if key == "" {
		if !getOrHead(w, r) {
			return
		}
		g.cache.metrics.Request(frontendGradle, resultHit)
		writeCached(w, r.Method == http.MethodHead, cached{status: http.StatusOK, contentType: "text/plain; charset=utf-8", body: []byte("gradle build cache\n")})
		return
	}
	if !validGradleKey(key) {
		notFoundHere(w, g.cache.metrics, frontendGradle)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		g.get(w, r, g.prefix+key)
	case http.MethodPut:
		g.put(w, r, g.prefix+key)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (g *gradleFrontend) get(w http.ResponseWriter, r *http.Request, key string) {
	o := object{frontend: frontendGradle, key: key, contentType: gradleEntryType}
	size, err := g.cache.store.Head(r.Context(), key)
	switch {
	case errors.Is(err, ErrNotFound):
		g.cache.metrics.Request(frontendGradle, resultNotFound)
		http.Error(w, "not found", http.StatusNotFound)
	case err != nil:
		g.cache.storeError(w, o, "head", err)
	case r.Method == http.MethodHead:
		w.Header().Set("Content-Type", gradleEntryType)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		g.cache.metrics.Request(frontendGradle, resultHit)
	default:
		g.cache.redirect(w, r, o)
	}
}

func (g *gradleFrontend) put(w http.ResponseWriter, r *http.Request, key string) {
	o := object{frontend: frontendGradle, key: key}
	if r.ContentLength < 0 {
		g.cache.metrics.Request(frontendGradle, resultRejected)
		http.Error(w, "Content-Length required", http.StatusLengthRequired)
		return
	}
	if r.ContentLength > g.maxEntry {
		g.cache.metrics.Request(frontendGradle, resultRejected)
		http.Error(w, "entry too large", http.StatusRequestEntityTooLarge)
		return
	}
	switch _, err := g.cache.store.Head(r.Context(), key); {
	case err == nil:
		// First write wins; the body is not read.
		g.cache.metrics.Request(frontendGradle, resultHit)
		w.WriteHeader(http.StatusNoContent)
		return
	case !errors.Is(err, ErrNotFound):
		g.cache.storeError(w, o, "head", err)
		return
	}
	// The server bounds the body by the Content-Length and ends it in an
	// error when the client sends fewer bytes than it announced, and Put then
	// discards the write. A client that stops sending is let go after the idle
	// timeout, like an upstream that stops answering.
	body := &idleReader{r: r.Body, rc: http.NewResponseController(w), d: g.cache.idleTimeout}
	n, err := g.cache.store.Put(r.Context(), key, body, gradleEntryType)
	g.cache.metrics.Upload(frontendGradle, n, err)
	if err != nil {
		g.cache.log.Warn("store", "op", "put", "frontend", frontendGradle, "key", logSafe(key), "err", err)
		g.cache.metrics.Request(frontendGradle, resultStoreError)
		http.Error(w, "cache store error", http.StatusBadGateway)
		return
	}
	g.cache.metrics.Request(frontendGradle, resultStored)
	w.WriteHeader(http.StatusNoContent)
}

// validGradleKey admits the keys Gradle sends: the hexadecimal form of a hash
// of the task's inputs (128 bits today), never a path.
func validGradleKey(k string) bool {
	if len(k) < 32 || len(k) > 128 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// idleReader reads a request body with a deadline that moves with every read:
// a stalled client fails the upload instead of holding it open.
type idleReader struct {
	r  io.Reader
	rc *http.ResponseController
	d  time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	_ = i.rc.SetReadDeadline(time.Now().Add(i.d))
	return i.r.Read(p)
}
