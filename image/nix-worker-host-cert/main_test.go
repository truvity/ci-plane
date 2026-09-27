package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type testCA struct {
	signer ssh.Signer
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{signer: signer}
}

func newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type certOptions struct {
	certType        uint32
	principals      []string
	lifetime        time.Duration
	extensions      map[string]string
	criticalOptions map[string]string
}

// sign issues what a correctly configured host-signing role issues by
// default: a host certificate for exactly the requested principals, one
// day, no extensions, no critical options.
func (ca testCA) sign(t *testing.T, key ssh.PublicKey, opts certOptions) []byte {
	t.Helper()
	if opts.certType == 0 {
		opts.certType = ssh.HostCert
	}
	if opts.lifetime == 0 {
		opts.lifetime = 24 * time.Hour
	}
	now := time.Now()
	cert := &ssh.Certificate{
		Key:             key,
		Serial:          1,
		CertType:        opts.certType,
		KeyId:           "nix-worker-amd64.ci-cache.svc.cluster.local",
		ValidPrincipals: opts.principals,
		ValidAfter:      uint64(now.Add(-30 * time.Second).Unix()),
		ValidBefore:     uint64(now.Add(opts.lifetime).Unix()),
		Permissions: ssh.Permissions{
			CriticalOptions: opts.criticalOptions,
			Extensions:      opts.extensions,
		},
	}
	if err := cert.SignCert(rand.Reader, ca.signer); err != nil {
		t.Fatal(err)
	}
	return ssh.MarshalAuthorizedKey(cert)
}

func TestValidateHostCertificate(t *testing.T) {
	ca := newTestCA(t)
	key := newHostKey(t)
	principals := []string{"nix-worker-amd64.ci-cache.svc.cluster.local"}

	cases := []struct {
		name    string
		cert    []byte
		wantErr string
	}{
		{
			name: "exactly the requested principal, no extensions or critical options",
			cert: ca.sign(t, key, certOptions{principals: principals}),
		},
		{
			name:    "a user certificate instead of a host certificate",
			cert:    ca.sign(t, key, certOptions{certType: ssh.UserCert, principals: principals}),
			wantErr: "non-host certificate",
		},
		{
			name:    "an extra principal",
			cert:    ca.sign(t, key, certOptions{principals: append(append([]string{}, principals...), "some-other-host")}),
			wantErr: "principals",
		},
		{
			name:    "missing the requested principal",
			cert:    ca.sign(t, key, certOptions{principals: []string{"some-other-host"}}),
			wantErr: "principals",
		},
		{
			name:    "no principals at all",
			cert:    ca.sign(t, key, certOptions{}),
			wantErr: "principals",
		},
		{
			name:    "an extension host certificates should never carry",
			cert:    ca.sign(t, key, certOptions{principals: principals, extensions: map[string]string{"permit-pty": ""}}),
			wantErr: "extensions",
		},
		{
			name:    "a critical option",
			cert:    ca.sign(t, key, certOptions{principals: principals, criticalOptions: map[string]string{"force-command": "/bin/sh"}}),
			wantErr: "critical options",
		},
		{
			name:    "someone else's key",
			cert:    ca.sign(t, newHostKey(t), certOptions{principals: principals}),
			wantErr: "does not contain the worker's host key",
		},
		{
			name:    "lifetime beyond the requested TTL",
			cert:    ca.sign(t, key, certOptions{principals: principals, lifetime: 30 * 24 * time.Hour}),
			wantErr: "exceeds requested TTL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHostCertificate(tc.cert, key, principals, 24*time.Hour)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseCertificateTTL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"1h", time.Hour},
		{"168h", 168 * time.Hour},
	} {
		got, err := parseCertificateTTL(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseCertificateTTL(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"59m", "169h", "0", "-1h", "1.5s", "soon"} {
		if got, err := parseCertificateTTL(in); err == nil {
			t.Errorf("parseCertificateTTL(%q) = %v; want an error", in, got)
		}
	}
}

// fakeOpenBao answers the two calls sign makes: the JWT login and the SSH
// signing request, signing with ca and returning the extensions/type the
// test wants — including a MISCONFIGURED role that hands back a user
// certificate, so the client-side refusal in validateHostCertificate is
// exercised end to end, not just against a hand-built certificate.
func fakeOpenBao(t *testing.T, ca testCA, opts certOptions) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Namespace") != "env" {
			http.Error(w, "namespace", http.StatusBadRequest)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/auth/jwt-env/login":
			if body["role"] != "nix-worker" || body["jwt"] != "projected-jwt" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "client-token"}})
		case "/v1/ssh/sign/host":
			if r.Header.Get("X-Vault-Token") != "client-token" || body["cert_type"] != "host" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(body["public_key"]))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			o := opts
			if o.principals == nil {
				o.principals = strings.Split(body["valid_principals"], ",")
			}
			signed := ca.sign(t, key, o)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"signed_key": strings.TrimSpace(string(signed))}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func writeConfig(t *testing.T, server *httptest.Server, hostKey ssh.PublicKey) config {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return config{
		address:             address,
		namespace:           "env",
		authMount:           "jwt-env",
		authRole:            "nix-worker",
		sshMount:            "ssh",
		sshRole:             "host",
		certificateTTL:      "86400s",
		certificateDuration: 24 * time.Hour,
		principals:          []string{"nix-worker-amd64.ci-cache.svc.cluster.local"},
		timeout:             10 * time.Second,
		tokenFile:           write("token", []byte("projected-jwt\n")),
		caFile:              write("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})),
		publicKeyFile:       write("ssh_host_ed25519_key.pub", ssh.MarshalAuthorizedKey(hostKey)),
		certFile:            filepath.Join(dir, "ssh_host_ed25519_key-cert.pub"),
	}
}

func TestSignWritesAValidatedHostCertificate(t *testing.T) {
	ca := newTestCA(t)
	hostKey := newHostKey(t)
	server := fakeOpenBao(t, ca, certOptions{})
	defer server.Close()
	cfg := writeConfig(t, server, hostKey)

	cert, err := sign(cfg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if len(cert) == 0 {
		t.Fatal("sign returned no certificate")
	}
	if err := atomicWrite(cfg.certFile, cert, 0o644); err != nil {
		t.Fatalf("write host certificate: %v", err)
	}
	written, err := os.ReadFile(cfg.certFile)
	if err != nil || len(written) == 0 {
		t.Fatalf("certificate not written: %q, %v", written, err)
	}
}

func TestSignRefusesAMisconfiguredRole(t *testing.T) {
	ca := newTestCA(t)
	hostKey := newHostKey(t)
	// A role that hands back a USER certificate for a host-cert request:
	// exactly the drift validateHostCertificate exists to catch.
	server := fakeOpenBao(t, ca, certOptions{certType: ssh.UserCert, principals: []string{"nix-worker-amd64.ci-cache.svc.cluster.local"}})
	defer server.Close()
	cfg := writeConfig(t, server, hostKey)

	if _, err := sign(cfg); err == nil || !strings.Contains(err.Error(), "non-host certificate") {
		t.Fatalf("sign: want the non-host certificate refused, got %v", err)
	}
}

func TestSignRefusesAnUnexpectedPrincipal(t *testing.T) {
	ca := newTestCA(t)
	hostKey := newHostKey(t)
	server := fakeOpenBao(t, ca, certOptions{principals: []string{"some-other-host"}})
	defer server.Close()
	cfg := writeConfig(t, server, hostKey)

	if _, err := sign(cfg); err == nil || !strings.Contains(err.Error(), "principals") {
		t.Fatalf("sign: want the unexpected principal refused, got %v", err)
	}
}
