package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxSmall bounds every body the service holds in memory: narinfo, .info,
// .mod, sumdb tiles and answers, a not-found's text. Real ones are a few
// kilobytes; anything past this is not one of them.
const maxSmall = 8 << 20

// object is one immutable, content-addressed thing a front-end asks the
// cache for.
type object struct {
	frontend    string
	key         string // the store key, prefix included
	upstream    string // the upstream URL it is pulled through from
	contentType string
	// large objects are answered with a redirect on a hit and streamed on
	// a miss; small ones are served from memory or the store, body and all.
	large bool
	// expect, for a large object that must be verified before it is stored
	// (a NAR), looks up what its body must be: (nil, nil) means nothing is
	// known, and the object is passed through and never stored.
	expect func(ctx context.Context) (*expectation, error)
	// onBody, when set, sees the whole body of a small object each time it
	// is about to be served (a narinfo feeds the NAR index from it).
	onBody func(body []byte)
}

// expectation is the size and Nix base-32 SHA-256 a body must have to be
// committed.
type expectation struct {
	size   int64
	sha256 string
}

// verifyError is a body that arrived whole (its Content-Length held) but is
// not the one expected. It is not an upstream failure: the client has been
// served the bytes, only the store refuses them.
type verifyError struct{ msg string }

func (e *verifyError) Error() string { return e.msg }

// Cache is the shared core of both front-ends: the store lookups, the one
// upstream fetch per key, the in-memory layers and the metrics.
type Cache struct {
	store       Store
	client      *http.Client
	metrics     *Metrics
	log         *slog.Logger
	presignTTL  time.Duration
	negativeTTL time.Duration
	mutableTTL  time.Duration
	idleTimeout time.Duration

	mem     *lru // small immutable bodies
	neg     *lru // upstream not-founds, for negativeTTL
	mutable *lru // mutable answers, for mutableTTL

	fetchSem chan struct{}
	// bg is the context every pull-through runs on: NOT the request's, so
	// a client that goes away does not abort the fetch the waiting
	// requests share. Cancelled only when shutdown runs out of time.
	bg context.Context

	mu      sync.Mutex
	flights map[string]*flight
	// onFollow, when set (tests), runs as a request joins a fetch already
	// in flight.
	onFollow func()
}

// flight is one pull-through in progress. Requests for the same key that
// arrive while it runs wait for it instead of fetching again.
type flight struct {
	done   chan struct{}
	res    cached // status 0: failed (502)
	stored bool
}

type cacheOptions struct {
	PresignTTL, NegativeTTL, MutableTTL, IdleTimeout time.Duration
	MemoryBytes                                      int64
	MaxFetches                                       int
}

func newCache(bg context.Context, store Store, client *http.Client, m *Metrics, log *slog.Logger, o cacheOptions) *Cache {
	now := time.Now
	return &Cache{
		store: store, client: client, metrics: m, log: log,
		presignTTL: o.PresignTTL, negativeTTL: o.NegativeTTL, mutableTTL: o.MutableTTL, idleTimeout: o.IdleTimeout,
		mem:      newLRU(o.MemoryBytes, now),
		neg:      newLRU(o.MemoryBytes/8, now),
		mutable:  newLRU(o.MemoryBytes/8, now),
		fetchSem: make(chan struct{}, max(o.MaxFetches, 1)),
		bg:       bg,
		flights:  map[string]*flight{},
	}
}

// ServeImmutable answers a GET or HEAD for o: from memory or the store
// when it is there, by one shared upstream fetch when it is not.
func (c *Cache) ServeImmutable(w http.ResponseWriter, r *http.Request, o object) {
	head := r.Method == http.MethodHead
	if v, ok := c.neg.Get(o.key); ok {
		c.metrics.Request(o.frontend, resultNotFound)
		writeCached(w, head, v)
		return
	}
	if !o.large {
		if v, ok := c.mem.Get(o.key); ok {
			if o.onBody != nil {
				o.onBody(v.body)
			}
			c.metrics.Request(o.frontend, resultHit)
			writeCached(w, head, v)
			return
		}
	}

	ctx := r.Context()
	if o.large {
		size, err := c.store.Head(ctx, o.key)
		switch {
		case err == nil && head:
			// A presigned URL is signed for GET; a HEAD is answered here.
			w.Header().Set("Content-Type", o.contentType)
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.WriteHeader(http.StatusOK)
			c.metrics.Request(o.frontend, resultHit)
			return
		case err == nil:
			c.redirect(w, r, o)
			return
		case !errors.Is(err, ErrNotFound):
			c.storeError(w, o, "head", err)
			return
		}
		if head {
			// Not stored, and a HEAD fetches no body to store: ask upstream.
			c.proxyHead(w, r, o)
			return
		}
	} else {
		body, err := c.getSmall(ctx, o.key)
		switch {
		case err == nil:
			v := cached{status: http.StatusOK, contentType: o.contentType, body: body}
			c.mem.Put(o.key, v, 0)
			if o.onBody != nil {
				o.onBody(body)
			}
			c.metrics.Request(o.frontend, resultHit)
			writeCached(w, head, v)
			return
		case !errors.Is(err, ErrNotFound):
			c.storeError(w, o, "get", err)
			return
		}
	}
	c.pullThrough(w, r, o)
}

func (c *Cache) redirect(w http.ResponseWriter, r *http.Request, o object) {
	u, err := c.store.Presign(r.Context(), o.key, c.presignTTL)
	if err != nil {
		c.storeError(w, o, "presign", err)
		return
	}
	w.Header().Set("Location", u)
	w.Header().Set("Cache-Control", "no-store") // the URL expires; never reuse it
	w.WriteHeader(http.StatusFound)
	c.metrics.Request(o.frontend, resultRedirect)
}

func (c *Cache) storeError(w http.ResponseWriter, o object, op string, err error) {
	c.log.Error("store", "op", op, "frontend", o.frontend, "key", o.key, "err", err)
	c.metrics.Request(o.frontend, resultStoreError)
	http.Error(w, "cache store error", http.StatusBadGateway)
}

func (c *Cache) getSmall(ctx context.Context, key string) ([]byte, error) {
	rc, err := c.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return readSmall(rc)
}

// readSmall reads a stored small object whole, up to maxSmall.
func readSmall(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxSmall+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSmall {
		return nil, fmt.Errorf("stored object is over %d bytes", maxSmall)
	}
	return body, nil
}

// pullThrough fetches o from upstream once, however many requests ask
// for it meanwhile. The first request leads and is answered as the bytes
// arrive; the others wait for the outcome and are answered from it.
func (c *Cache) pullThrough(w http.ResponseWriter, r *http.Request, o object) {
	c.mu.Lock()
	if f, ok := c.flights[o.key]; ok {
		c.mu.Unlock()
		if c.onFollow != nil {
			c.onFollow()
		}
		select {
		case <-f.done:
		case <-r.Context().Done():
			return
		}
		c.follow(w, r, o, f)
		return
	}
	f := &flight{done: make(chan struct{})}
	c.flights[o.key] = f
	c.mu.Unlock()
	// Runs on a panic too (http.ErrAbortHandler below): waiting requests
	// must never be left waiting.
	defer func() {
		c.mu.Lock()
		delete(c.flights, o.key)
		c.mu.Unlock()
		close(f.done)
	}()

	select {
	case c.fetchSem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-c.fetchSem }()
	c.metrics.inFlight.Add(1)
	defer c.metrics.inFlight.Add(-1)

	if o.large {
		c.leadLarge(w, r, o, f)
	} else {
		c.leadSmall(w, r, o, f)
	}
}

// follow answers a request that waited for another's fetch of the same key.
func (c *Cache) follow(w http.ResponseWriter, r *http.Request, o object, f *flight) {
	head := r.Method == http.MethodHead
	switch {
	case f.res.status == http.StatusOK && !o.large:
		c.metrics.Request(o.frontend, resultMiss)
		writeCached(w, head, f.res)
	case f.res.status == http.StatusOK && f.stored:
		c.redirect(w, r, o)
	case f.res.status == http.StatusNotFound || f.res.status == http.StatusGone:
		c.metrics.Request(o.frontend, resultNotFound)
		writeCached(w, head, f.res)
	case f.res.status == http.StatusOK:
		// The leader's copy was passed through and not stored (no index,
		// failed verification, a store failure): this request is served
		// by a fetch of its own.
		c.pullThrough(w, r, o)
	default:
		// The shared fetch failed: this
		// request has nothing to redirect to.
		c.metrics.Request(o.frontend, resultUpstreamError)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
	}
}

// leadSmall fetches a small object whole, answers the client, then
// stores it.
func (c *Cache) leadSmall(w http.ResponseWriter, r *http.Request, o object, f *flight) {
	head := r.Method == http.MethodHead
	resp, ok := c.fetchOrAnswer(w, r, o, f)
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	fr := newFetchReader(resp, nil, nil)
	body, err := io.ReadAll(io.LimitReader(fr, maxSmall+1))
	if err == nil && len(body) > maxSmall {
		err = fmt.Errorf("upstream body is over %d bytes", maxSmall)
	}
	if err != nil {
		c.upstreamError(w, o, err)
		return
	}
	v := cached{status: http.StatusOK, contentType: o.contentType, body: body}
	f.res = v
	if o.onBody != nil {
		o.onBody(body)
	}
	c.metrics.Request(o.frontend, resultMiss)
	writeCached(w, head, v)

	n, err := c.store.Put(c.bg, o.key, bytes.NewReader(body), o.contentType)
	c.metrics.Upload(o.frontend, n, err)
	if err != nil {
		c.log.Error("store", "op", "put", "frontend", o.frontend, "key", o.key, "err", err)
		return
	}
	f.stored = true
	c.mem.Put(o.key, v, 0)
}

// leadLarge streams a large object to the client WHILE it is uploaded to
// the store. The upload reads the upstream body and the client is fed
// from what it reads, best effort: a client that goes away does not stop
// the upload the waiting requests are counting on.
func (c *Cache) leadLarge(w http.ResponseWriter, r *http.Request, o object, f *flight) {
	var want *expectation
	if o.expect != nil {
		var err error
		if want, err = o.expect(r.Context()); err != nil {
			c.storeError(w, o, "get-index", err)
			return
		}
	}
	resp, ok := c.fetchOrAnswer(w, r, o, f)
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", o.contentType)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// The client sees the response start now, not when the first 4 KiB
	// buffer fills; and a failure after this point reaches it as a broken
	// response, never as a connection closed before any answer (which an
	// HTTP client may silently retry).
	_ = rc.Flush()
	toClient := func(p []byte) error {
		_ = rc.SetWriteDeadline(time.Now().Add(c.idleTimeout))
		_, err := w.Write(p)
		return err
	}

	fr := newFetchReader(resp, want, toClient)
	start := time.Now()
	if o.expect != nil && want == nil {
		// Nothing says what this body must be (a NAR asked for without a
		// narinfo through us): it goes to the client, never to the store.
		_, err := io.Copy(io.Discard, fr)
		if err != nil {
			c.metrics.Request(o.frontend, resultUpstreamError)
			c.log.Warn("upstream", "frontend", o.frontend, "key", o.key, "err", err, "bytes", fr.n)
			panic(http.ErrAbortHandler)
		}
		f.res = cached{status: http.StatusOK}
		c.metrics.Request(o.frontend, resultNoIndex)
		c.log.Info("passed through, no index", "frontend", o.frontend, "key", o.key, "bytes", fr.n)
		return
	}
	n, err := c.store.Put(c.bg, o.key, fr, o.contentType)
	c.metrics.Upload(o.frontend, n, err)
	if err != nil && fr.err == nil {
		// The store failed and upstream did not: finish the client's copy.
		c.log.Error("store", "op", "put", "frontend", o.frontend, "key", o.key, "err", err)
		_, _ = io.Copy(io.Discard, fr)
	}
	var ve *verifyError
	if errors.As(fr.err, &ve) {
		// Whole, and the client has all of it, but not the body the
		// narinfo announced: nothing is committed.
		f.res = cached{status: http.StatusOK}
		c.metrics.Request(o.frontend, resultVerifyFailed)
		c.log.Warn("verify failed", "frontend", o.frontend, "key", o.key, "err", ve, "bytes", fr.n)
		return
	}
	if !errors.Is(fr.err, io.EOF) {
		// Truncated, stalled, too long or the wrong bytes: nothing was
		// committed, and the client must see a broken response rather
		// than a clean end it would take for the whole object.
		c.metrics.Request(o.frontend, resultUpstreamError)
		c.log.Warn("upstream", "frontend", o.frontend, "key", o.key, "err", fr.err, "bytes", fr.n)
		panic(http.ErrAbortHandler)
	}
	f.res = cached{status: http.StatusOK}
	f.stored = err == nil
	c.metrics.Request(o.frontend, resultMiss)
	c.log.Info("pulled through", "frontend", o.frontend, "key", o.key, "bytes", fr.n,
		"stored", f.stored, "seconds", time.Since(start).Seconds())
}

// fetchOrAnswer GETs o upstream. On anything but a 200 it answers the
// client (and the flight) itself and reports false.
func (c *Cache) fetchOrAnswer(w http.ResponseWriter, r *http.Request, o object, f *flight) (*http.Response, bool) {
	head := r.Method == http.MethodHead
	resp, err := c.fetch(c.bg, http.MethodGet, o.frontend, o.upstream)
	if err != nil {
		c.upstreamError(w, o, err)
		return nil, false
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp, true
	case http.StatusNotFound, http.StatusGone:
		// Passed on unchanged, never stored: GOPROXY's comma falls through
		// on exactly these two, and Nix moves to its next substituter.
		v := notFound(resp)
		c.neg.Put(o.key, v, c.negativeTTL)
		f.res = v
		c.metrics.Request(o.frontend, resultNotFound)
		writeCached(w, head, v)
		return nil, false
	default:
		_ = resp.Body.Close()
		c.upstreamError(w, o, fmt.Errorf("upstream answered %s", resp.Status))
		return nil, false
	}
}

func (c *Cache) upstreamError(w http.ResponseWriter, o object, err error) {
	c.log.Warn("upstream", "frontend", o.frontend, "url", o.upstream, "err", err)
	c.metrics.Request(o.frontend, resultUpstreamError)
	http.Error(w, "upstream fetch failed", http.StatusBadGateway)
}

// proxyHead answers a HEAD for a large object the store does not hold.
func (c *Cache) proxyHead(w http.ResponseWriter, r *http.Request, o object) {
	resp, err := c.fetch(r.Context(), http.MethodHead, o.frontend, o.upstream)
	if err != nil {
		c.upstreamError(w, o, err)
		return
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		w.Header().Set("Content-Type", o.contentType)
		if resp.ContentLength >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		}
		w.WriteHeader(http.StatusOK)
		c.metrics.Request(o.frontend, resultMiss)
	case http.StatusNotFound, http.StatusGone:
		c.metrics.Request(o.frontend, resultNotFound)
		w.WriteHeader(resp.StatusCode)
	default:
		c.upstreamError(w, o, fmt.Errorf("upstream answered %s", resp.Status))
	}
}

// ServeMutable passes a mutable answer (a version list, @latest, a sumdb
// lookup) through from upstream, remembered for mutableTTL and never
// stored.
func (c *Cache) ServeMutable(w http.ResponseWriter, r *http.Request, frontend, upstream string) {
	head := r.Method == http.MethodHead
	if v, ok := c.mutable.Get(upstream); ok {
		if v.status == http.StatusOK {
			c.metrics.Request(frontend, resultHit)
		} else {
			c.metrics.Request(frontend, resultNotFound)
		}
		writeCached(w, head, v)
		return
	}
	o := object{frontend: frontend, upstream: upstream}
	resp, err := c.fetch(r.Context(), http.MethodGet, frontend, upstream)
	if err != nil {
		c.upstreamError(w, o, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		v := notFound(resp)
		c.mutable.Put(upstream, v, c.mutableTTL)
		c.metrics.Request(frontend, resultNotFound)
		writeCached(w, head, v)
		return
	default:
		c.upstreamError(w, o, fmt.Errorf("upstream answered %s", resp.Status))
		return
	}
	body, err := io.ReadAll(io.LimitReader(newFetchReader(resp, nil, nil), maxSmall+1))
	if err == nil && len(body) > maxSmall {
		err = fmt.Errorf("upstream body is over %d bytes", maxSmall)
	}
	if err != nil {
		c.upstreamError(w, o, err)
		return
	}
	v := cached{status: http.StatusOK, contentType: resp.Header.Get("Content-Type"), body: body}
	c.mutable.Put(upstream, v, c.mutableTTL)
	c.metrics.Request(frontend, resultMiss)
	writeCached(w, head, v)
}

// notFound keeps an upstream 404/410 with the start of its text: the go
// command prints it ("not found: unknown revision ...").
func notFound(resp *http.Response) cached {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return cached{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}
}

// fetch sends one upstream request. Its body stops with an error when no
// byte has arrived for idleTimeout: a stalled upstream must fail the
// fetch, not hold every request waiting on it.
func (c *Cache) fetch(ctx context.Context, method, frontend, url string) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	start := time.Now()
	resp, err := c.client.Do(req)
	c.metrics.Upstream(frontend, time.Since(start))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &idleBody{rc: resp.Body, d: c.idleTimeout, t: time.AfterFunc(c.idleTimeout, cancel), cancel: cancel}
	return resp, nil
}

type idleBody struct {
	rc     io.ReadCloser
	d      time.Duration
	t      *time.Timer
	cancel context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.t.Reset(b.d)
	return n, err
}

func (b *idleBody) Close() error {
	b.t.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}

// fetchReader reads an upstream body and ends in io.EOF ONLY when the body
// is whole: every byte its Content-Length promised and no more, and, when
// an expectation is given, the size and SHA-256 it names (a mismatch is a
// *verifyError, after the length checks). Anything else ends in an
// error, which is what keeps Store.Put from committing the object. Bytes
// are copied to the client as they pass, best effort.
type fetchReader struct {
	r        io.Reader
	want     int64 // -1: not announced
	n        int64
	sum      hash.Hash
	wantSize int64
	wantSum  string
	toClient func([]byte) error // nil: none, or gone
	err      error              // sticky; io.EOF once the body ended whole
}

func newFetchReader(resp *http.Response, want *expectation, toClient func([]byte) error) *fetchReader {
	fr := &fetchReader{r: resp.Body, want: resp.ContentLength, toClient: toClient}
	if want != nil {
		fr.sum, fr.wantSum, fr.wantSize = sha256.New(), want.sha256, want.size
	}
	return fr
}

func (f *fetchReader) Read(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n, err := f.r.Read(p)
	if n > 0 {
		f.n += int64(n)
		if f.sum != nil {
			_, _ = f.sum.Write(p[:n])
		}
		if f.toClient != nil && f.toClient(p[:n]) != nil {
			f.toClient = nil
		}
		if f.want >= 0 && f.n > f.want {
			err = fmt.Errorf("upstream sent more than its Content-Length %d", f.want)
		}
	}
	if err == io.EOF { // returned unwrapped, by io.Reader's contract
		err = f.finish()
	}
	if err != nil {
		f.err = err
	}
	return n, err
}

func (f *fetchReader) finish() error {
	if f.want >= 0 && f.n != f.want {
		return fmt.Errorf("upstream body ended at %d of %d bytes: %w", f.n, f.want, io.ErrUnexpectedEOF)
	}
	if f.sum != nil {
		if f.n != f.wantSize {
			return &verifyError{fmt.Sprintf("upstream body is %d bytes, its narinfo says FileSize %d", f.n, f.wantSize)}
		}
		if got := nix32(f.sum.Sum(nil)); got != f.wantSum {
			return &verifyError{fmt.Sprintf("upstream body hashes to sha256:%s, its narinfo says FileHash %s", got, f.wantSum)}
		}
	}
	return io.EOF
}

// writeCached answers with v; status 0 is a failure.
func writeCached(w http.ResponseWriter, head bool, v cached) {
	if v.status == 0 {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	if v.contentType != "" {
		w.Header().Set("Content-Type", v.contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(v.body)))
	w.WriteHeader(v.status)
	if !head {
		_, _ = w.Write(v.body)
	}
}
