package main

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
)

// hostRoots is the path each front-end is mounted under, for the host that
// serves it at ITS root. A client that is given a base URL of its own (a Nix
// substituter, a Maven repository, a Gradle build cache) is then given a name
// with no path, and the gateway in front of this service needs no rewrite.
var hostRoots = map[string]string{
	frontendNix:    "/nix",
	frontendMaven:  "/maven",
	frontendGradle: "/gradle/cache",
}

// hostMap is the -hosts flag: kind=host pairs, repeatable or comma-separated
// (the environment form). Only the kinds in hostRoots may be named; the Go
// proxy keeps its /goproxy path on every host, and so does any request whose
// Host is not named here.
type hostMap struct {
	byKind map[string]string // kind -> normalised host
}

func newHostMap() *hostMap { return &hostMap{byKind: map[string]string{}} }

func (h *hostMap) String() string {
	if h == nil {
		return ""
	}
	parts := make([]string, 0, len(h.byKind))
	for _, k := range sortedKeys(h.byKind) {
		parts = append(parts, k+"="+h.byKind[k])
	}
	return strings.Join(parts, ",")
}

func (h *hostMap) Set(v string) error {
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		kind, host, ok := strings.Cut(entry, "=")
		if _, known := hostRoots[kind]; !ok || !known {
			return fmt.Errorf("%q: want kind=host, the kind one of %s", entry, strings.Join(sortedKeys(hostRoots), ", "))
		}
		host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if !validHostName(host) || strings.Contains(host, ":") {
			return fmt.Errorf("%q: %q is not a host name (no scheme, path or port)", entry, host)
		}
		if _, dup := h.byKind[kind]; dup {
			return fmt.Errorf("kind %q is given a host twice", kind)
		}
		for other, oh := range h.byKind {
			if oh == host {
				return fmt.Errorf("host %q is given to both %q and %q", host, other, kind)
			}
		}
		h.byKind[kind] = host
	}
	return nil
}

// normalizeHost is the form hosts are compared in: no port, no trailing dot,
// lower case. A gateway may forward the Host header with or without the port
// it listened on.
func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	return strings.Trim(s, "[]")
}

// validHostName admits letters, digits, '.', '-' and ':' (an IPv6 literal in
// a Host header), nothing else.
func validHostName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) && strings.IndexByte(".-:", s[i]) < 0 {
			return false
		}
	}
	return true
}

// routes is the host -> mount path table of a configuration, nil when no
// host is named.
func (h *hostMap) routes() map[string]string {
	if h == nil || len(h.byKind) == 0 {
		return nil
	}
	out := make(map[string]string, len(h.byKind))
	for kind, host := range h.byKind {
		out[host] = hostRoots[kind]
	}
	return out
}

// withHostRouting serves a request whose Host is named in routes at the root of
// its front-end, by moving it under the front-end's path (nix.example/foo is
// served as example/nix/foo), and every other request as it is. The handler
// below sees one path layout whatever the Host was, so a front-end is written
// once. /healthz and /metrics stay on the path layout: a kubelet probes by pod
// address, never by one of these names.
func withHostRouting(routes map[string]string, next http.Handler) http.Handler {
	if len(routes) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if root, ok := routes[normalizeHost(r.Host)]; ok {
			r2 := *r
			u := *r.URL
			u.Path = root + ensureSlash(u.Path)
			if u.RawPath != "" {
				u.RawPath = root + ensureSlash(u.RawPath)
			}
			r2.URL = &u
			r = &r2
		}
		next.ServeHTTP(w, r)
	})
}

func ensureSlash(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// hostsServed names the hosts for the startup log, sorted.
func hostsServed(routes map[string]string) []string {
	out := make([]string, 0, len(routes))
	for h, root := range routes {
		out = append(out, h+" -> "+root)
	}
	sort.Strings(out)
	return out
}
