// cache-redirect is a pull-through cache for CI downloads that are
// immutable and content-addressed -- Nix NARs, Go module zips, Maven
// artifacts, npm tarballs -- whose bytes never pass through it on a hit, and, when asked
// for, the Gradle remote build cache on the same store.
//
// The index is an S3 (or S3-compatible) bucket. A hit on a large object is
// answered with a 302 to a short-lived presigned GET of the bucket, so the
// runner downloads from the object store directly and this service only
// ever handles small requests; small objects (narinfo, .info, .mod, sumdb
// tiles) are served directly. A miss is fetched from upstream ONCE however
// many requests ask for it, streamed to the first client while it is
// uploaded, and committed only when the upstream body arrived whole.
//
//	cache-redirect -s3-bucket NAME [-s3-region R] [-listen :8080] ...
//
// Every flag can also be set as an environment variable,
// CACHE_REDIRECT_<FLAG> with dashes as underscores (CACHE_REDIRECT_S3_BUCKET);
// a flag on the command line wins. Credentials come from the default AWS
// SDK chain: EKS Pod Identity, IRSA, or AWS_ACCESS_KEY_ID and friends for
// a store without a pod identity.
//
//	/nix/...      the Nix binary cache front-end   (substituter: <base>/nix)
//	/goproxy/...  the GOPROXY front-end            (GOPROXY:     <base>/goproxy)
//	/maven/...    the Maven repositories           (url:         <base>/maven/<repo>)
//	/gradle/...   the Gradle build cache, OFF by default (url:   <base>/gradle/cache/)
//	/npm/...      the npm registry                 (registry:    <base>/npm)
//
// Each of nix, maven, gradle and npm can also be served at the root of a host name
// of its own (-hosts nix=nix.cache.example,maven=maven.cache.example), so a
// gateway routes by name and rewrites nothing; any other Host keeps the layout
// above.
//
//	/healthz      200 while serving
//	/metrics      Prometheus text format
//
// The service keeps nothing on local disk. See docs/architecture.md
// ("cache-redirect") for the design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// settings is every setting, from flags and the environment.
type settings struct {
	Listen          string
	S3              s3Options
	PresignTTL      time.Duration
	NegativeTTL     time.Duration
	MutableTTL      time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	MemoryBytes     int64
	MaxFetches      int
	RetryBackoff    time.Duration
	LogLevel        slog.Level

	Nix         bool
	NixPrefix   string
	NixUpstream string
	NixPriority int

	Go              bool
	GoPrefix        string
	GoUpstream      string
	GoSumdb         string
	GoSumdbUpstream string

	Maven       bool
	MavenPrefix string
	MavenRepos  *mavenRepos

	Gradle         bool
	GradlePrefix   string
	GradleMaxEntry int64

	Npm       bool
	NpmPrefix string
	NpmReg    string
	NpmFresh  time.Duration
	NpmMemory int64

	Hosts *hostMap
}

const envPrefix = "CACHE_REDIRECT_"

func parseConfig(args []string, getenv func(string) string, out io.Writer) (*settings, error) {
	c := &settings{}
	fs := flag.NewFlagSet("cache-redirect", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&c.Listen, "listen", ":8080", "address to serve on")
	fs.StringVar(&c.S3.Bucket, "s3-bucket", "", "the bucket the cache lives in (required)")
	fs.StringVar(&c.S3.Region, "s3-region", "", "the bucket's region (default: the SDK's, e.g. AWS_REGION)")
	fs.StringVar(&c.S3.Endpoint, "s3-endpoint", "", "endpoint URL of an S3-compatible store (empty: AWS)")
	fs.BoolVar(&c.S3.PathStyle, "s3-path-style", false, "address the bucket as <endpoint>/<bucket>/<key>")
	fs.IntVar(&c.S3.PartSize, "s3-part-size", 8<<20, "multipart upload part size in bytes (at least 5 MiB); smaller objects are one PUT")
	fs.IntVar(&c.S3.Concurrency, "s3-upload-concurrency", 2, "parts of one upload in flight at once")
	fs.DurationVar(&c.PresignTTL, "presign-ttl", 5*time.Minute, "how long a redirect's presigned URL stays valid")
	fs.DurationVar(&c.NegativeTTL, "negative-ttl", time.Minute, "how long an upstream 404/410 for an immutable path is remembered in memory")
	fs.DurationVar(&c.MutableTTL, "mutable-ttl", 30*time.Second, "how long a mutable answer (@v/list, @latest, sumdb lookup) is remembered in memory")
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", time.Minute, "an upstream body that sends nothing for this long fails")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 25*time.Second, "how long in-flight requests get to finish on SIGTERM")
	fs.Int64Var(&c.MemoryBytes, "memory-cache-bytes", 64<<20, "in-memory cache budget for small objects")
	fs.IntVar(&c.MaxFetches, "max-fetches", 16, "upstream pull-throughs at once; more wait")
	fs.DurationVar(&c.RetryBackoff, "retry-backoff", time.Second,
		"the Maven and npm front-ends repeat an upstream request answered 429, 502, 503 or 504, up to 4 attempts, "+
			"waiting this long times the attempt (or the upstream's Retry-After)")
	fs.TextVar(&c.LogLevel, "log-level", slog.LevelInfo, "debug, info, warn or error")

	fs.BoolVar(&c.Nix, "nix", true, "serve the Nix binary cache front-end under /nix")
	fs.StringVar(&c.NixPrefix, "nix-prefix", "nix/", "store key prefix of the Nix front-end")
	fs.StringVar(&c.NixUpstream, "nix-upstream", "https://cache.nixos.org", "the Nix binary cache pulled through")
	fs.IntVar(&c.NixPriority, "nix-priority", 30, "Priority in /nix-cache-info (lower wins; cache.nixos.org says 40)")

	fs.BoolVar(&c.Go, "goproxy", true, "serve the GOPROXY front-end under /goproxy")
	fs.StringVar(&c.GoPrefix, "goproxy-prefix", "goproxy/", "store key prefix of the GOPROXY front-end")
	fs.StringVar(&c.GoUpstream, "goproxy-upstream", "https://proxy.golang.org", "the module proxy pulled through")
	fs.StringVar(&c.GoSumdb, "goproxy-sumdb", "sum.golang.org", "the checksum database proxied under /sumdb/<name> (empty: none, the go command goes direct)")
	fs.StringVar(&c.GoSumdbUpstream, "goproxy-sumdb-upstream", "https://sum.golang.org", "where that checksum database is fetched from")

	fs.BoolVar(&c.Maven, "maven", true, "serve the Maven repositories under /maven/<repo>")
	fs.StringVar(&c.MavenPrefix, "maven-prefix", "maven/", "store key prefix of the Maven front-end")
	c.MavenRepos = newMavenRepos()
	fs.Var(c.MavenRepos, "maven-repo", "a Maven upstream as name=URL (repeatable, or comma-separated); naming any replaces the defaults: "+c.MavenRepos.String())

	fs.BoolVar(&c.Gradle, "gradle", false, "serve the Gradle remote build cache under /gradle/cache/ -- WRITABLE by every client that can reach the service")
	fs.StringVar(&c.GradlePrefix, "gradle-prefix", "gradle/", "store key prefix of the Gradle build cache")
	fs.Int64Var(&c.GradleMaxEntry, "gradle-max-entry-bytes", 512<<20, "the largest build cache entry a client may store (413 above it)")

	fs.BoolVar(&c.Npm, "npm", false, "serve the npm registry under /npm (read-only)")
	fs.StringVar(&c.NpmPrefix, "npm-prefix", "npm/", "store key prefix of the npm front-end")
	fs.StringVar(&c.NpmReg, "npm-registry", "https://registry.npmjs.org", "the npm registry pulled through")
	fs.DurationVar(&c.NpmFresh, "npm-fresh", 2*time.Minute, "how long a packument is served without asking the registry again; then it is revalidated with its ETag")
	fs.Int64Var(&c.NpmMemory, "npm-memory-bytes", 128<<20, "in-memory budget for packuments (a single one may be up to 64 MiB and half of this)")

	c.Hosts = newHostMap()
	fs.Var(c.Hosts, "hosts", "serve a front-end at the ROOT of a host name, as kind=host (kind: nix, maven, gradle or npm; repeatable, or comma-separated), "+
		"so the gateway needs no path rewrite; a request for any other Host keeps the /nix, /goproxy, /maven, /gradle/cache, /npm layout. "+
		"The port in the Host header is ignored")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	// The environment fills every flag the command line did not set.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || envErr != nil {
			return
		}
		name := envPrefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v := getenv(name); v != "" {
			if err := fs.Set(f.Name, v); err != nil {
				envErr = fmt.Errorf("%s: %w", name, err)
			}
		}
	})
	if envErr != nil {
		return nil, envErr
	}
	c.S3.PresignTTL = c.PresignTTL
	return c, c.validate()
}

func (c *settings) validate() error {
	var errs []error
	if c.S3.Bucket == "" {
		errs = append(errs, errors.New("-s3-bucket is required"))
	}
	if !c.Nix && !c.Go && !c.Maven && !c.Gradle && !c.Npm {
		errs = append(errs, errors.New("all front-ends are off: nothing to serve"))
	}
	// SigV4 caps a presigned URL at seven days.
	if c.PresignTTL < time.Second || c.PresignTTL > 7*24*time.Hour {
		errs = append(errs, fmt.Errorf("-presign-ttl %s: must be between 1s and 168h", c.PresignTTL))
	}
	if c.S3.PartSize < 5<<20 {
		errs = append(errs, fmt.Errorf("-s3-part-size %d: S3 requires at least 5 MiB", c.S3.PartSize))
	}
	if c.S3.Concurrency < 1 || c.MaxFetches < 1 {
		errs = append(errs, errors.New("-s3-upload-concurrency and -max-fetches must be at least 1"))
	}
	if c.IdleTimeout <= 0 {
		errs = append(errs, errors.New("-idle-timeout must be positive"))
	}
	type prefixed struct {
		on        bool
		flag, val string
	}
	var prefixes []prefixed
	for _, p := range []prefixed{
		{c.Nix, "-nix-prefix", c.NixPrefix}, {c.Go, "-goproxy-prefix", c.GoPrefix},
		{c.Maven, "-maven-prefix", c.MavenPrefix}, {c.Gradle, "-gradle-prefix", c.GradlePrefix},
		{c.Npm, "-npm-prefix", c.NpmPrefix},
	} {
		if !p.on {
			continue
		}
		if p.val == "" {
			errs = append(errs, errors.New("a front-end's key prefix must not be empty"))
			continue
		}
		prefixes = append(prefixes, p)
	}
	for i, a := range prefixes {
		for _, b := range prefixes[i+1:] {
			if strings.HasPrefix(a.val, b.val) || strings.HasPrefix(b.val, a.val) {
				errs = append(errs, fmt.Errorf("%s %q and %s %q overlap: one front-end's keys would land in the other's", a.flag, a.val, b.flag, b.val))
			}
		}
	}
	for flagName, u := range map[string]string{"-nix-upstream": c.NixUpstream, "-goproxy-upstream": c.GoUpstream, "-goproxy-sumdb-upstream": c.GoSumdbUpstream} {
		if _, err := parseUpstream(u); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", flagName, err))
		}
	}
	if c.Maven {
		if len(c.MavenRepos.repos) == 0 {
			errs = append(errs, errors.New("-maven-repo: the Maven front-end has no repository to serve"))
		}
		for name, chain := range c.MavenRepos.repos {
			ups := splitUpstreams(chain)
			if len(ups) == 0 {
				errs = append(errs, fmt.Errorf("-maven-repo %s: no URL", name))
			}
			for _, u := range ups {
				if _, err := parseUpstream(u); err != nil {
					errs = append(errs, fmt.Errorf("-maven-repo %s: %w", name, err))
				}
			}
		}
	}
	for kind, host := range c.Hosts.byKind {
		on := map[string]bool{frontendNix: c.Nix, frontendMaven: c.Maven, frontendGradle: c.Gradle, frontendNpm: c.Npm}[kind]
		if !on {
			errs = append(errs, fmt.Errorf("-hosts %s=%s: the %s front-end is off", kind, host, kind))
		}
	}
	if c.Gradle && c.GradleMaxEntry < 1 {
		errs = append(errs, errors.New("-gradle-max-entry-bytes must be positive"))
	}
	if c.Npm {
		if _, err := parseUpstream(c.NpmReg); err != nil {
			errs = append(errs, fmt.Errorf("-npm-registry: %w", err))
		}
		if c.NpmFresh < 0 {
			errs = append(errs, errors.New("-npm-fresh must not be negative"))
		}
		if c.NpmMemory < 2*maxSmall {
			errs = append(errs, fmt.Errorf("-npm-memory-bytes %d: at least %d", c.NpmMemory, 2*maxSmall))
		}
	}
	return errors.Join(errs...)
}

func parseUpstream(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%q is not an http(s) base URL", s)
	}
	return u, nil
}

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	cfg, err := parseConfig(args, os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cache-redirect: %v\n", err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	store, err := newS3Store(ctx, cfg.S3)
	if err != nil {
		log.Error("store", "err", err)
		return 1
	}
	bg, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()

	handler, _ := newHandler(bg, cfg, store, upstreamClient(), log)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("serving", "listen", cfg.Listen, "bucket", cfg.S3.Bucket, "nix", cfg.Nix, "goproxy", cfg.Go,
		"maven", cfg.Maven, "maven_repos", mavenRepoNamesOf(cfg), "gradle", cfg.Gradle, "npm", cfg.Npm, "hosts", hostsServed(cfg.Hosts.routes()))

	select {
	case err := <-errc:
		log.Error("server", "err", err)
		return 1
	case <-ctx.Done():
	}
	// Stop accepting, let in-flight requests -- and the pull-throughs they
	// lead -- finish; past the deadline, abort the uploads still running
	// (each is then discarded, never committed half-written).
	log.Info("shutting down", "timeout", cfg.ShutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("shutdown deadline passed; aborting in-flight fetches", "err", err)
		cancelBg()
		_ = srv.Close()
	}
	return 0
}

// newHandler wires the front-ends, /healthz and /metrics around one Cache.
func newHandler(bg context.Context, cfg *settings, store Store, client *http.Client, log *slog.Logger) (http.Handler, *Cache) {
	var frontends []string
	if cfg.Nix {
		frontends = append(frontends, frontendNix)
	}
	if cfg.Go {
		frontends = append(frontends, frontendGo)
	}
	if cfg.Maven {
		frontends = append(frontends, frontendMaven)
	}
	if cfg.Gradle {
		frontends = append(frontends, frontendGradle)
	}
	if cfg.Npm {
		frontends = append(frontends, frontendNpm)
	}
	m := newMetrics(frontends...)
	c := newCache(bg, store, client, m, log, cacheOptions{
		PresignTTL: cfg.PresignTTL, NegativeTTL: cfg.NegativeTTL, MutableTTL: cfg.MutableTTL,
		IdleTimeout: cfg.IdleTimeout, MemoryBytes: cfg.MemoryBytes, MaxFetches: cfg.MaxFetches,
		RetryFrontends: []string{frontendMaven, frontendNpm}, RetryBackoff: cfg.RetryBackoff,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.Write(w)
	})
	if cfg.Nix {
		up, _ := parseUpstream(cfg.NixUpstream) // validated
		mux.Handle("/nix/", &nixFrontend{cache: c, prefix: cfg.NixPrefix, upstream: up, priority: cfg.NixPriority,
			index: &narIndex{store: store, prefix: cfg.NixPrefix, mem: newLRU(cfg.MemoryBytes/8, time.Now), bg: bg, log: log}})
	}
	if cfg.Go {
		up, _ := parseUpstream(cfg.GoUpstream)
		sumUp, _ := parseUpstream(cfg.GoSumdbUpstream)
		mux.Handle("/goproxy/", &goFrontend{cache: c, prefix: cfg.GoPrefix, upstream: up, sumdbName: cfg.GoSumdb, sumdbUpstream: sumUp})
	}
	if cfg.Maven {
		repos := map[string][]*url.URL{}
		for name, chain := range cfg.MavenRepos.repos {
			for _, u := range splitUpstreams(chain) {
				pu, _ := parseUpstream(u) // validated
				repos[name] = append(repos[name], pu)
			}
		}
		mux.Handle("/maven/", &mavenFrontend{cache: c, prefix: cfg.MavenPrefix, repos: repos})
	}
	if cfg.Gradle {
		mux.Handle("/gradle/", &gradleFrontend{cache: c, prefix: cfg.GradlePrefix, maxEntry: cfg.GradleMaxEntry})
	}
	if cfg.Npm {
		up, _ := parseUpstream(cfg.NpmReg) // validated
		mux.Handle("/npm/", newNpmFrontend(c, cfg.NpmPrefix, up, cfg.NpmFresh, cfg.NpmMemory))
	}
	return withHostRouting(cfg.Hosts.routes(), mux), c
}

// mavenRepoNamesOf is the names of the configured Maven repositories, for the
// startup log; empty while the front-end is off.
func mavenRepoNamesOf(cfg *settings) []string {
	if !cfg.Maven {
		return nil
	}
	return sortedKeys(cfg.MavenRepos.repos)
}

// upstreamClient talks to the upstream caches. Compression is off: the
// bytes stored must be the bytes upstream serves, and a transparently
// decompressed body has neither upstream's length nor its hash. Redirects
// are followed (the default), so a CDN in front of an upstream is fine.
func upstreamClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   64,
		DisableCompression:    true,
	}}
}
