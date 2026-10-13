package runneridentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Documents are compared byte for byte: an unchanged grant must never be an
// update of a policy, and a policy's bytes are what the provider diffs.
// The expected strings are the documents the estate's own code rendered
// before this package existed, with placeholder names.

// expand turns the expected documents' @arn@ marker into the ARN prefix:
// the repository's leak canary refuses a literal one in any tracked file.
func expand(s string) string { return strings.ReplaceAll(s, "@arn@", "arn:"+partition) }

// fixture reads an expected document from testdata.
func fixture(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	return expand(strings.TrimSuffix(string(raw), "\n"))
}

var estate = Estate{Cluster: "example", Region: "eu-example-1", AccountID: "123456789012"}

func TestSharedPoolDocument(t *testing.T) {
	p := Pool{
		Estate:           estate,
		Registry:         RegistryShared,
		RunnerImageRepos: []string{"ci/runners/*"},
		CacheBucket:      "example-ci-cache",
		StableChartRepoARNs: []string{
			"arn:" + partition + ":ecr:eu-example-1:123456789012:repository/proj/charts/one",
			"arn:" + partition + ":ecr:eu-example-1:123456789012:repository/proj/charts/two",
		},
	}

	got, err := p.Document()
	if err != nil {
		t.Fatal(err)
	}

	want := fixture(t, "shared-pool.json")

	if got != want {
		t.Errorf("document differs:\n got %s\nwant %s", got, want)
	}
}

func TestOwnPoolDocument(t *testing.T) {
	p := Pool{
		Estate:           estate,
		Registry:         RegistryOwn,
		RunnerImageRepos: []string{"ci/runners/*"},
		CacheBucket:      "example-ci-cache",
		PullThroughRepos: []string{"docker-hub/*", "ghcr/*", "quay/*", "github/*"},
	}

	got, err := p.Document()
	if err != nil {
		t.Fatal(err)
	}

	noCache := fixture(t, "own-pool.json")

	if got != noCache {
		t.Errorf("document without the buildkit cache differs:\n got %s\nwant %s", got, noCache)
	}

	p.BuildkitCacheRepo = "ci/buildkit-cache"

	got, err = p.Document()
	if err != nil {
		t.Fatal(err)
	}

	withCache := fixture(t, "own-pool-buildkit-cache.json")

	if got != withCache {
		t.Errorf("document with the buildkit cache differs:\n got %s\nwant %s", got, withCache)
	}
}

func TestModproxyDocument(t *testing.T) {
	m := Modproxy{Estate: estate, Bucket: "example-ci-cache", Prefix: "modcache"}

	got, err := m.Document()
	if err != nil {
		t.Fatal(err)
	}

	want := fixture(t, "modproxy.json")

	if got != want {
		t.Errorf("document differs:\n got %s\nwant %s", got, want)
	}
}

// A pool that omits a required input is refused, not rendered with a hole.
func TestPoolRefusesMissingInputs(t *testing.T) {
	for name, p := range map[string]Pool{
		"no runner images":     {Estate: estate, CacheBucket: "b", Registry: RegistryOwn, PullThroughRepos: []string{"x"}},
		"no bucket":            {Estate: estate, RunnerImageRepos: []string{"r"}, Registry: RegistryOwn, PullThroughRepos: []string{"x"}},
		"shared, no charts":    {Estate: estate, RunnerImageRepos: []string{"r"}, CacheBucket: "b", Registry: RegistryShared},
		"own, no pull-through": {Estate: estate, RunnerImageRepos: []string{"r"}, CacheBucket: "b", Registry: RegistryOwn},
		"unknown registry":     {Estate: estate, RunnerImageRepos: []string{"r"}, CacheBucket: "b", Registry: 9},
	} {
		if _, err := p.Document(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	if _, err := (Modproxy{Estate: estate, Bucket: "b"}).Document(); err == nil {
		t.Error("modproxy without a prefix: want an error")
	}
}
