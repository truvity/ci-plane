package main

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const frontendMaven = "maven"

// defaultMavenRepos are the upstreams of the Maven front-end unless
// -maven-repo names others: what a Gradle or Maven build declares as
// mavenCentral(), google() and gradlePluginPortal(). A repository is one
// upstream or a chain of them, URLs separated by '|', asked in order while the
// answer is a 404.
//
// The Plugin Portal's repository serves everything a plugin needs from Maven
// Central too, which is every transitive dependency of every plugin, and it
// rate limits (429) a client that asks for hundreds of them at once. So the
// chain asks Maven Central first and the Portal only for what Central does not
// have: the plugin markers and the artifacts that are only on the Portal.
var defaultMavenRepos = map[string]string{
	"central":        "https://repo.maven.apache.org/maven2",
	"google":         "https://dl.google.com/dl/android/maven2",
	"gradle-plugins": "https://repo.maven.apache.org/maven2|https://plugins.gradle.org/m2",
}

// splitUpstreams is the URLs of a repository's chain, in order.
func splitUpstreams(v string) []string {
	var out []string
	for _, u := range strings.Split(v, "|") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// mavenRepos is the -maven-repo flag: name=URL[|URL...] pairs, repeatable or
// comma-separated (the environment form). The first use drops the defaults,
// so naming one repository means exactly the ones named.
type mavenRepos struct {
	set   bool
	repos map[string]string
}

func newMavenRepos() *mavenRepos {
	m := &mavenRepos{repos: map[string]string{}}
	for k, v := range defaultMavenRepos {
		m.repos[k] = v
	}
	return m
}

func (m *mavenRepos) String() string {
	if m == nil {
		return ""
	}
	parts := make([]string, 0, len(m.repos))
	for _, k := range sortedKeys(m.repos) {
		parts = append(parts, k+"="+m.repos[k])
	}
	return strings.Join(parts, ",")
}

func (m *mavenRepos) Set(v string) error {
	if !m.set {
		m.set = true
		m.repos = map[string]string{}
	}
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, upstream, ok := strings.Cut(entry, "=")
		if !ok || !validRepoName(name) {
			return fmt.Errorf("%q: want name=URL, the name made of letters, digits, '.', '_' and '-'", entry)
		}
		if _, dup := m.repos[name]; dup {
			return fmt.Errorf("repository %q is named twice", name)
		}
		m.repos[name] = upstream
	}
	return nil
}

func validRepoName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) && strings.IndexByte("._-", s[i]) < 0 {
			return false
		}
	}
	return true
}

// mavenFrontend speaks the Maven repository layout under /maven/<repo>/:
//
//	/<group path>/<artifact>/<version>/<file>   immutable: stored; large ones (archives)
//	                                            are a redirect on a hit, the rest are served
//	/<group path>/<artifact>/maven-metadata.xml mutable: passed through, remembered briefly
//	/<anything>/<x>-SNAPSHOT/<file>             mutable
//
// <repo> is one of the configured upstreams. One path per upstream, never a
// merged view: the client keeps the order and the meaning of the
// repositories it declared, and an artifact is never answered by another
// upstream than the one asked. An upstream 404 or 410 reaches the client
// unchanged, so the client moves on to its next repository.
//
// GET /maven/ answers 200 with the repository names: the probe a runner uses
// to learn the front-end is there before it points a build at it.
type mavenFrontend struct {
	cache  *Cache
	prefix string
	repos  map[string][]*url.URL // each repository's chain of upstreams, in order
}

func (m *mavenFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !getOrHead(w, r) {
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/maven")
	if p == "/" || p == "" {
		m.cache.metrics.Request(frontendMaven, resultHit)
		body := "maven: " + strings.Join(sortedKeys(m.repos), " ") + "\n"
		writeCached(w, r.Method == http.MethodHead, cached{status: http.StatusOK, contentType: "text/plain; charset=utf-8", body: []byte(body)})
		return
	}
	repo, rest, ok := strings.Cut(p[1:], "/")
	chain := m.repos[repo]
	if !ok || len(chain) == 0 || !isPlainPath("/"+rest) {
		notFoundHere(w, m.cache.metrics, frontendMaven)
		return
	}
	file := path.Base(rest)
	urls := make([]string, len(chain))
	for i, u := range chain {
		urls[i] = joinURL(u, "/"+rest)
	}
	if mutableMavenPath(rest) {
		m.cache.ServeMutable(w, r, frontendMaven, urls[0], urls[1:]...)
		return
	}
	// An archive is large: a redirect on a hit. Descriptors (pom, Gradle
	// module metadata), checksums and signatures are a few kilobytes.
	ext := strings.ToLower(path.Ext(file))
	ctype, small := mavenContentType(ext)
	m.cache.ServeImmutable(w, r, object{
		frontend: frontendMaven, key: m.prefix + repo + "/" + rest, upstream: urls[0], fallbacks: urls[1:],
		contentType: ctype, large: !small,
	})
}

// mutableMavenPath reports whether the answer for a repository path can
// change: the metadata files that list versions (and their checksums and
// signatures), the group indexes of Google's repository, and everything in
// the directory of a snapshot version.
func mutableMavenPath(rest string) bool {
	els := strings.Split(rest, "/")
	file := els[len(els)-1]
	for _, el := range els[:len(els)-1] {
		if strings.HasSuffix(el, "-SNAPSHOT") {
			return true
		}
	}
	switch {
	case strings.HasPrefix(file, "maven-metadata"), // maven-metadata.xml, -<repo>.xml and their .sha1, .md5, .asc
		strings.HasSuffix(file, "-index.xml"), // master-index.xml, group-index.xml
		strings.HasPrefix(file, "archetype-catalog"):
		return true
	}
	return false
}

// mavenContentType is the media type of a repository file by extension, and
// whether the file is small enough to serve from memory.
func mavenContentType(ext string) (string, bool) {
	switch ext {
	case ".pom", ".xml":
		return "application/xml", true
	case ".module", ".json":
		return "application/json", true
	case ".sha1", ".sha256", ".sha512", ".md5", ".asc", ".txt":
		return "text/plain; charset=utf-8", true
	case ".jar":
		return "application/java-archive", false
	}
	return "application/octet-stream", false
}
