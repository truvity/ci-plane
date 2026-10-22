package main

import (
	"context"
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
	frontendNpm:    "/npm",
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

// hostRoute is where a request for one host name is moved to.
type hostRoute struct {
	root string
	// tolerant: a path that already starts with root + "/" is left as it is.
	// Only the Nix front-end can say so unambiguously (a binary cache serves
	// /nix-cache-info, /<hash>.narinfo and /nar/..., never a path under
	// /nix/), and it is what lets a gateway that still rewrites the client's
	// root onto /nix (as it had to before hosts existed) be taken away AFTER
	// the host is set, instead of in the same moment.
	tolerant bool
}

// routes is the host -> route table of a configuration, nil when no host is
// named.
func (h *hostMap) routes() map[string]hostRoute {
	if h == nil || len(h.byKind) == 0 {
		return nil
	}
	out := make(map[string]hostRoute, len(h.byKind))
	for kind, host := range h.byKind {
		out[host] = hostRoute{root: hostRoots[kind], tolerant: kind == frontendNix}
	}
	return out
}

// withHostRouting serves a request whose Host is named in routes at the root of
// its front-end, by moving it under the front-end's path (nix.example/foo is
// served as example/nix/foo), and every other request as it is. The handler
// below sees one path layout whatever the Host was, so a front-end is written
// once. /healthz and /metrics stay on the path layout: a kubelet probes by pod
// address, never by one of these names.
func withHostRouting(routes map[string]hostRoute, next http.Handler) http.Handler {
	if len(routes) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rt, ok := routes[normalizeHost(r.Host)]; ok && (!rt.tolerant || !strings.HasPrefix(r.URL.Path, rt.root+"/")) {
			root := rt.root
			// The front-end is told the request arrived at its root, for
			// the one that has to write its own public URL into an answer.
			r2 := r.WithContext(context.WithValue(r.Context(), hostRootKey{}, true))
			u := *r.URL
			u.Path = root + ensureSlash(u.Path)
			if u.RawPath != "" {
				u.RawPath = root + ensureSlash(u.RawPath)
			}
			r2.URL = &u
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

// hostRootKey marks a request that was moved under its front-end's path
// because its Host is the front-end's own name.
type hostRootKey struct{}

// arrivedAtHostRoot reports whether the client addressed the front-end by its
// own host name, i.e. its public base URL has no path.
func arrivedAtHostRoot(r *http.Request) bool {
	v, _ := r.Context().Value(hostRootKey{}).(bool)
	return v
}

func ensureSlash(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// hostsServed names the hosts for the startup log, sorted.
func hostsServed(routes map[string]hostRoute) []string {
	out := make([]string, 0, len(routes))
	for h, rt := range routes {
		out = append(out, h+" -> "+rt.root)
	}
	sort.Strings(out)
	return out
}
