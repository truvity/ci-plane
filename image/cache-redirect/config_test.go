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
		"nothing to serve":        {"-s3-bucket", "b", "-nix=false", "-goproxy=false"},
		"not an http(s) base URL": {"-s3-bucket", "b", "-nix-upstream", "cache.nixos.org"},
	} {
		_, err := parseConfig(args, none, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want one saying %q", args, err, want)
		}
	}
}
