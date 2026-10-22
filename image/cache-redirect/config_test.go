package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnvironment(t *testing.T) {
	env := map[string]string{
		"CACHE_REDIRECT_S3_BUCKET":   "from-env",
		"CACHE_REDIRECT_PRESIGN_TTL": "90s",
		"CACHE_REDIRECT_NIX":         "false",
		"CACHE_REDIRECT_LISTEN":      ":1",
	}
	c, err := parseConfig([]string{"-listen", ":9"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.S3.Bucket != "from-env" || c.PresignTTL != 90*time.Second || c.S3.PresignTTL != 90*time.Second || c.Nix || !c.Go {
		t.Errorf("environment not applied: %+v", c)
	}
	if c.Listen != ":9" {
		t.Errorf("listen %q: a flag on the command line must win over the environment", c.Listen)
	}
}

func TestConfigRefusals(t *testing.T) {
	none := func(string) string { return "" }
	for want, args := range map[string][]string{
		"-s3-bucket is required":  {},
		"overlap":                 {"-s3-bucket", "b", "-nix-prefix", "cache/", "-goproxy-prefix", "cache/go/"},
		"must not be empty":       {"-s3-bucket", "b", "-nix-prefix", ""},
		"-presign-ttl":            {"-s3-bucket", "b", "-presign-ttl", "200h"},
		"at least 5 MiB":          {"-s3-bucket", "b", "-s3-part-size", "1024"},
		"nothing to serve":        {"-s3-bucket", "b", "-nix=false", "-goproxy=false", "-maven=false"},
		"not an http(s) base URL": {"-s3-bucket", "b", "-nix-upstream", "cache.nixos.org"},
	} {
		_, err := parseConfig(args, none, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want one saying %q", args, err, want)
		}
	}
}

func TestConfigMavenRepositories(t *testing.T) {
	none := func(string) string { return "" }
	c, err := parseConfig([]string{"-s3-bucket", "b"}, none, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := c.MavenRepos.String()
	for _, want := range []string{
		"central=https://repo.maven.apache.org/maven2",
		"google=https://dl.google.com/dl/android/maven2",
		"gradle-plugins=https://repo.maven.apache.org/maven2|https://plugins.gradle.org/m2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("default repositories %q lack %q", got, want)
		}
	}
	if !c.Maven || c.Gradle {
		t.Errorf("maven %v, gradle %v: want the Maven front-end on and the writable Gradle cache off by default", c.Maven, c.Gradle)
	}

	// Naming a repository replaces the defaults; both the repeated flag and the
	// comma-separated environment form work.
	c, err = parseConfig([]string{"-s3-bucket", "b", "-maven-repo", "a=https://a.example/m", "-maven-repo", "b=https://b.example"}, none, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MavenRepos.String(); got != "a=https://a.example/m,b=https://b.example" {
		t.Errorf("repositories %q, want only the two named", got)
	}
	env := map[string]string{"CACHE_REDIRECT_S3_BUCKET": "b", "CACHE_REDIRECT_MAVEN_REPO": "x=https://x.example, y=https://y.example"}
	c, err = parseConfig(nil, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MavenRepos.String(); got != "x=https://x.example,y=https://y.example" {
		t.Errorf("repositories from the environment %q", got)
	}

	for want, args := range map[string][]string{
		"want name=URL":                {"-s3-bucket", "b", "-maven-repo", "https://a.example"},
		"no URL":                       {"-s3-bucket", "b", "-maven-repo", "a=|"},
		"named twice":                  {"-s3-bucket", "b", "-maven-repo", "a=https://a.example", "-maven-repo", "a=https://b.example"},
		"not an http(s) base URL":      {"-s3-bucket", "b", "-maven-repo", "a=repo.example"},
		"want name=URL, the name made": {"-s3-bucket", "b", "-maven-repo", "a/b=https://a.example"},
		"overlap":                      {"-s3-bucket", "b", "-gradle", "-gradle-prefix", "x/", "-maven-prefix", "x/maven/"},
		"must be positive":             {"-s3-bucket", "b", "-gradle", "-gradle-max-entry-bytes", "0"},
	} {
		_, err := parseConfig(args, none, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want one saying %q", args, err, want)
		}
	}
}
