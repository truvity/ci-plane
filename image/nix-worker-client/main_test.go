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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type testCA struct {
	signer ssh.Signer
	public ssh.PublicKey
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
	return testCA{signer: signer, public: signer.PublicKey()}
}

func newUserKey(t *testing.T) ssh.PublicKey {
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
	principals      []string
	lifetime        time.Duration
	extensions      map[string]string
	criticalOptions map[string]string
}

// sign issues what the signing role issues by default: a user certificate
// for the one principal, one hour, carrying permit-pty.
func (ca testCA) sign(t *testing.T, key ssh.PublicKey, opts certOptions) []byte {
	t.Helper()
	if opts.principals == nil {
		opts.principals = []string{workerPrincipal}
	}
	if opts.lifetime == 0 {
		opts.lifetime = time.Hour
	}
	if opts.extensions == nil {
		opts.extensions = map[string]string{permitPTY: ""}
	}
	now := time.Now()
	cert := &ssh.Certificate{
		Key:             key,
		Serial:          1,
		CertType:        ssh.UserCert,
		KeyId:           "system:serviceaccount:runners:runner",
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

func TestValidateCertificate(t *testing.T) {
	ca := newTestCA(t)
	otherCA := newTestCA(t)
	key := newUserKey(t)
	publicKey := ssh.MarshalAuthorizedKey(key)
	authorities := []ssh.PublicKey{ca.public}

	cases := []struct {
		name    string
		cert    []byte
		wantErr string
	}{
		{
			name: "permit-pty only, as the signing role always issues",
			cert: ca.sign(t, key, certOptions{}),
		},
		{
			name: "no extensions",
			cert: ca.sign(t, key, certOptions{extensions: map[string]string{}}),
		},
		{
			name:    "another extension",
			cert:    ca.sign(t, key, certOptions{extensions: map[string]string{"permit-port-forwarding": ""}}),
			wantErr: "only permit-pty is accepted",
		},
		{
			name: "permit-pty plus another extension",
			cert: ca.sign(t, key, certOptions{extensions: map[string]string{
				permitPTY:                 "",
				"permit-agent-forwarding": "",
			}}),
			wantErr: "only permit-pty is accepted",
		},
		{
			name:    "permit-pty with a value",
			cert:    ca.sign(t, key, certOptions{extensions: map[string]string{permitPTY: "yes"}}),
			wantErr: "only permit-pty is accepted",
		},
		{
			name:    "critical option force-command",
			cert:    ca.sign(t, key, certOptions{criticalOptions: map[string]string{"force-command": "/bin/sh"}}),
			wantErr: "critical options [force-command]",
		},
		{
			name:    "critical option source-address with no extensions",
			cert:    ca.sign(t, key, certOptions{extensions: map[string]string{}, criticalOptions: map[string]string{"source-address": "10.0.0.0/8"}}),
			wantErr: "critical options [source-address]",
		},
		{
			name:    "unexpected CA",
			cert:    otherCA.sign(t, key, certOptions{}),
			wantErr: "unexpected SSH CA",
		},
		{
			name:    "extra principal",
			cert:    ca.sign(t, key, certOptions{principals: []string{workerPrincipal, "root"}}),
			wantErr: "principals",
		},
		{
			name:    "lifetime beyond the requested TTL",
			cert:    ca.sign(t, key, certOptions{lifetime: 72 * time.Hour}),
			wantErr: "exceeds requested TTL",
		},
		{
			name:    "someone else's key",
			cert:    ca.sign(t, newUserKey(t), certOptions{}),
			wantErr: "does not contain the generated public key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCertificate(tc.cert, publicKey, authorities, workerPrincipal, time.Hour)
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
		{"", time.Hour},
		{"1h", time.Hour},
		{"60m", time.Hour},
		{"3600s", time.Hour},
		{"1h0m0s", time.Hour},
		{"30m", 30 * time.Minute},
		{"5m", 5 * time.Minute},
	} {
		got, err := parseCertificateTTL(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseCertificateTTL(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"61m", "1h1s", "2h", "72h", "168h", "4m59s", "0", "-1h", "1.5s", "soon"} {
		if got, err := parseCertificateTTL(in); err == nil {
			t.Errorf("parseCertificateTTL(%q) = %v; want an error", in, got)
		}
	}
}

// fakeOpenBao answers the two calls the setup makes: the JWT login and
// the SSH signing request, signing with ca and adding extensions the way
// the signing role does.
func fakeOpenBao(t *testing.T, ca testCA, extensions map[string]string, gotTTL *string) *httptest.Server {
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
			if body["role"] != "runner-example" || body["jwt"] != "projected-jwt" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "client-token"}})
		case "/v1/ssh/sign/user":
			if r.Header.Get("X-Vault-Token") != "client-token" || body["valid_principals"] != workerPrincipal {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			*gotTTL = body["ttl"]
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(body["public_key"]))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			signed := ca.sign(t, key, certOptions{extensions: extensions})
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"signed_key": strings.TrimSpace(string(signed))}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func setupConfig(t *testing.T, server *httptest.Server, ca testCA) config {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
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
	cfg := config{
		address:             address,
		namespace:           "env",
		authMount:           "jwt-env",
		authRole:            "runner-example",
		sshMount:            "ssh",
		sshRole:             "user",
		certificateTTL:      "3600s",
		certificateDuration: time.Hour,
		timeout:             10 * time.Second,
		tokenFile:           write("token", []byte("projected-jwt\n")),
		caFile:              write("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})),
		sshCAFile:           write("ssh-user-ca.pub", ssh.MarshalAuthorizedKey(ca.public)),
		knownHostsSource:    write("known_hosts", []byte("worker ssh-ed25519 AAAATEST\n")),
		sshConfigSource:     write("ssh_config", []byte("Host worker\n  BatchMode yes\n")),
		machinesSource:      write("machines.template", []byte("ssh://nixremote@worker x86_64-linux /home/runner/.ssh/nix-builder 2 1 - -\n")),
		sshDir:              filepath.Join(dir, "ssh"),
		machinesDir:         filepath.Join(dir, "machines"),
		podName:             "runner-0",
		podNamespace:        "runners",
		podUID:              "uid",
		runnerUID:           os.Getuid(),
		runnerGID:           os.Getgid(),
	}
	return cfg
}

func TestSetupEnablesRemoteWithPermitPTYCertificate(t *testing.T) {
	ca := newTestCA(t)
	var gotTTL string
	server := fakeOpenBao(t, ca, map[string]string{permitPTY: ""}, &gotTTL)
	defer server.Close()
	cfg := setupConfig(t, server, ca)

	if err := prepareOutputs(cfg); err != nil {
		t.Fatal(err)
	}
	if err := configureRemote(cfg); err != nil {
		t.Fatalf("configureRemote: %v", err)
	}
	if gotTTL != "3600s" {
		t.Errorf("requested ttl %q, want 3600s", gotTTL)
	}
	machines, err := os.ReadFile(filepath.Join(cfg.machinesDir, "machines"))
	if err != nil || len(machines) == 0 {
		t.Fatalf("machines file not published: %q, %v", machines, err)
	}
	for _, name := range []string{"nix-builder", "nix-builder.pub", "nix-builder-cert.pub"} {
		if _, err := os.Stat(filepath.Join(cfg.sshDir, name)); err != nil {
			t.Errorf("%s not published: %v", name, err)
		}
	}
}

func TestSetupRefusesUnexpectedExtensionAndStaysLocal(t *testing.T) {
	ca := newTestCA(t)
	var gotTTL string
	server := fakeOpenBao(t, ca, map[string]string{permitPTY: "", "permit-port-forwarding": ""}, &gotTTL)
	defer server.Close()
	cfg := setupConfig(t, server, ca)

	if err := prepareOutputs(cfg); err != nil {
		t.Fatal(err)
	}
	err := configureRemote(cfg)
	if err == nil || !strings.Contains(err.Error(), "only permit-pty is accepted") {
		t.Fatalf("configureRemote: want the extension refused, got %v", err)
	}
	removeCredentials(cfg)
	machines, err := os.ReadFile(filepath.Join(cfg.machinesDir, "machines"))
	if err != nil || len(machines) != 0 {
		t.Fatalf("machines file must stay empty (local builds): %q, %v", machines, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.sshDir, "nix-builder-cert.pub")); !os.IsNotExist(err) {
		t.Errorf("certificate must not be published: %v", err)
	}
}
