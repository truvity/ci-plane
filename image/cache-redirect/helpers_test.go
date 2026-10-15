package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memStore is the Store of the tests: a map, committing a Put only when
// its reader ended in io.EOF -- the contract the S3 store keeps.
type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
	// err, when set, is every call's answer (an AccessDenied, say).
	err error
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (s *memStore) Head(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	b, ok := s.objs[key]
	if !ok {
		return 0, ErrNotFound
	}
	return int64(len(b)), nil
}

func (s *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	b, ok := s.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

func (s *memStore) Put(_ context.Context, key string, r io.Reader, _ string) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.objs[key] = b
	return int64(len(b)), nil
}

func (s *memStore) Presign(_ context.Context, key string, ttl time.Duration) (string, error) {
	return "https://store.example/" + key + "?expires=" + ttl.String(), nil
}

func (s *memStore) object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objs[key]
	return b, ok
}

func (s *memStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// upstream is an httptest upstream that counts requests per path.
type upstream struct {
	*httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	paths []string // RequestURI, in order
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{hits: map[string]int{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[r.URL.Path]++
		u.paths = append(u.paths, r.RequestURI)
		u.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func (u *upstream) total() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, c := range u.hits {
		n += c
	}
	return n
}

// fixture is a running cache-redirect over a memStore, with every
// upstream pointed at up.
type fixture struct {
	srv   *httptest.Server
	store *memStore
	cache *Cache
	up    *upstream
}

func newFixture(t *testing.T, up *upstream, extra ...string) *fixture {
	t.Helper()
	args := append([]string{
		"-s3-bucket", "example",
		"-nix-upstream", up.URL,
		"-goproxy-upstream", up.URL + "/proxy",
		"-goproxy-sumdb-upstream", up.URL + "/sumdb",
		"-idle-timeout", "5s",
	}, extra...)
	cfg, err := parseConfig(args, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := upstreamClient()
	h, c := newHandler(context.Background(), cfg, store, client, log)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, store: store, cache: c, up: up}
}

// noRedirect is a client that reports a redirect instead of following it.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type reply struct {
	code    int
	body    string
	header  http.Header
	readErr error
}

func (f *fixture) do(t *testing.T, method, path string) reply {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return reply{readErr: err}
	}
	defer func() { _ = resp.Body.Close() }()
	b, rerr := io.ReadAll(resp.Body)
	return reply{code: resp.StatusCode, body: string(b), header: resp.Header, readErr: rerr}
}

func (f *fixture) get(t *testing.T, path string) reply {
	t.Helper()
	return f.do(t, http.MethodGet, path)
}

func (f *fixture) metrics(t *testing.T) string { t.Helper(); return f.get(t, "/metrics").body }

// truncate answers with a Content-Length it does not honour, then drops
// the connection: what a CDN reset mid-body looks like.
func truncate(w http.ResponseWriter, body []byte, short int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body[:short])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	panic(http.ErrAbortHandler)
}

// gate holds an upstream handler until released.
type gate struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{arrived: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) wait() {
	g.once.Do(func() { close(g.arrived) })
	<-g.release
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var errDenied = errors.New("AccessDenied: 403")

// counter is a goroutine-safe count of something a hook observed.
type counter struct{ n atomic.Int64 }

func (c *counter) inc()       { c.n.Add(1) }
func (c *counter) get() int64 { return c.n.Load() }
