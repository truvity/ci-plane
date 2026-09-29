// nix-worker-host-cert signs the nix worker's OWN SSH host key with an
// OpenBao SSH secrets engine, so a client's known_hosts can trust
// `@cert-authority` instead of one pinned host key per pod.
//
// A SIBLING BINARY to nix-worker-client (image/nix-worker-client), not a
// mode flag on it. That binary signs an ephemeral USER key for a runner
// pod: cert_type=user, one fixed principal (workerPrincipal), a strict
// permit-pty-only extension policy, and a fail-OPEN contract (signing
// failure degrades to local Nix builds, never fails the job). This binary
// signs the worker's own long-lived HOST key: cert_type=host, principals
// are the worker's own DNS names (rendered by the chart from the release
// namespace), no extension policy applies to host certificates at all,
// and the contract is fail-CLOSED on the first sign (sshd must not start
// presenting an unsigned or stale host identity) but fail-OPEN on renewal
// (a renewal failure logs and retries; it must never tear down a running
// sshd whose current certificate is still valid). Folding both into one
// binary behind a `-mode` flag would tangle two unrelated validation
// policies, two unrelated failure contracts and two unrelated deployment
// targets (ephemeral runner pods vs. persistent worker pods) behind one
// entrypoint; a sibling module keeps each binary's tests and env-var
// surface independent, at the cost of ~150 duplicated lines of small
// HTTP/JSON plumbing that has to stay simple exactly because it is a
// security boundary (see nix-worker-client's own comment on this).
//
// Renewal and the SIGHUP to sshd are NOT this binary's job either: they
// live in nix-worker-entrypoint.sh, which already owns the daemon and
// sshd process lifecycle. This binary does one thing — sign once, write
// the certificate, exit — and the entrypoint's loop decides what a
// success or a failure means for the running server.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	maxResponseBytes = 1 << 20

	// Sane bounds for a HOST certificate's lifetime. Unlike the user
	// certificates nix-worker-client requests, there is no known role
	// ceiling to enforce here (the estate's OpenBao role sets its own,
	// and a request above it simply gets refused server-side) — these
	// bounds only catch a configuration mistake (a TTL of seconds, or
	// of years) before it reaches OpenBao at all.
	defaultCertificateTTL = 24 * time.Hour
	minCertificateTTL     = time.Hour
	maxCertificateTTL     = 168 * time.Hour // 7 days
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type config struct {
	address             *url.URL
	namespace           string
	authMount           string
	authRole            string
	sshMount            string
	sshRole             string
	certificateTTL      string
	certificateDuration time.Duration
	principals          []string
	timeout             time.Duration
	tokenFile           string
	caFile              string
	publicKeyFile       string
	certFile            string
}

type loginResponse struct {
	Auth struct {
		ClientToken string `json:"client_token"`
	} `json:"auth"`
}

type signResponse struct {
	Data struct {
		SignedKey string `json:"signed_key"`
	} `json:"data"`
}

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatalf("nix worker host cert: %v", err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cert, err := sign(cfg)
	if err != nil {
		return err
	}
	if err := atomicWrite(cfg.certFile, cert, 0o644); err != nil {
		return fmt.Errorf("write host certificate: %w", err)
	}
	log.Printf("nix worker host cert: signed for %s, valid %s", strings.Join(cfg.principals, ","), cfg.certificateTTL)
	return nil
}

func loadConfig() (config, error) {
	var cfg config

	address, err := url.Parse(requiredEnv("NIX_WORKER_OPENBAO_ADDRESS"))
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil {
		return cfg, errors.New("NIX_WORKER_OPENBAO_ADDRESS must be an https URL without user info")
	}
	cfg.address = address
	cfg.namespace = requiredEnv("NIX_WORKER_OPENBAO_NAMESPACE")
	cfg.authMount = requiredEnv("NIX_WORKER_OPENBAO_AUTH_MOUNT")
	cfg.authRole = requiredEnv("NIX_WORKER_OPENBAO_AUTH_ROLE")
	cfg.sshMount = requiredEnv("NIX_WORKER_OPENBAO_SSH_MOUNT")
	cfg.sshRole = requiredEnv("NIX_WORKER_OPENBAO_SSH_ROLE")
	for label, value := range map[string]string{
		"auth mount": cfg.authMount,
		"auth role":  cfg.authRole,
		"SSH mount":  cfg.sshMount,
		"SSH role":   cfg.sshRole,
	} {
		if !safeName.MatchString(value) {
			return cfg, fmt.Errorf("%s contains unsupported characters", label)
		}
	}

	ttl, err := parseCertificateTTL(os.Getenv("NIX_WORKER_CERTIFICATE_TTL"))
	if err != nil {
		return cfg, err
	}
	cfg.certificateDuration = ttl
	cfg.certificateTTL = fmt.Sprintf("%ds", int64(ttl/time.Second))

	principalsRaw := requiredEnv("NIX_WORKER_HOST_PRINCIPALS")
	for _, p := range strings.Split(principalsRaw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		cfg.principals = append(cfg.principals, p)
	}
	if len(cfg.principals) == 0 {
		return cfg, errors.New("NIX_WORKER_HOST_PRINCIPALS must contain at least one name")
	}

	cfg.timeout, err = time.ParseDuration(envOr("NIX_WORKER_REQUEST_TIMEOUT", "15s"))
	if err != nil || cfg.timeout < time.Second || cfg.timeout > time.Minute {
		return cfg, errors.New("NIX_WORKER_REQUEST_TIMEOUT must be between 1s and 1m")
	}

	cfg.tokenFile = envOr("NIX_WORKER_TOKEN_FILE", "/var/run/secrets/openbao/token")
	cfg.caFile = envOr("NIX_WORKER_OPENBAO_CA_FILE", "/var/run/nix-worker-openbao/ca.crt")
	cfg.publicKeyFile = requiredEnv("NIX_WORKER_HOST_PUBLIC_KEY_FILE")
	cfg.certFile = requiredEnv("NIX_WORKER_HOST_CERT_FILE")

	return cfg, nil
}

// sign logs in on authRole with the pod's projected token and signs the
// worker's own host public key. It returns the certificate in OpenSSH
// authorized_keys form (one line, newline-terminated), validated to
// actually BE a host certificate for exactly the requested principals —
// an OpenBao role misconfigured to hand back a user certificate, or one
// bound to the wrong principals, must be refused here rather than
// installed as this worker's identity.
func sign(cfg config) ([]byte, error) {
	tokenBytes, err := readBounded(cfg.tokenFile, 64<<10)
	if err != nil {
		return nil, fmt.Errorf("read projected token: %w", err)
	}
	jwt := strings.TrimSpace(string(tokenBytes))
	if jwt == "" {
		return nil, errors.New("projected token is empty")
	}

	caPEM, err := readBounded(cfg.caFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("read OpenBao CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("OpenBao CA bundle contains no PEM certificate")
	}

	publicKeyData, err := readBounded(cfg.publicKeyFile, 64<<10)
	if err != nil {
		return nil, fmt.Errorf("read host public key: %w", err)
	}
	publicKey, _, options, rest, err := ssh.ParseAuthorizedKey(publicKeyData)
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("host public key is not one plain OpenSSH key")
	}

	client := &http.Client{
		Timeout: cfg.timeout,
		Transport: &http.Transport{
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    roots,
			},
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	var login loginResponse
	if err := postJSON(ctx, client, cfg, "auth/"+cfg.authMount+"/login", "", map[string]string{
		"role": cfg.authRole,
		"jwt":  jwt,
	}, &login); err != nil {
		return nil, fmt.Errorf("OpenBao login: %w", err)
	}
	if login.Auth.ClientToken == "" {
		return nil, errors.New("OpenBao login returned no client token")
	}

	var signed signResponse
	if err := postJSON(ctx, client, cfg, cfg.sshMount+"/sign/"+cfg.sshRole, login.Auth.ClientToken, map[string]string{
		"public_key":       string(publicKeyData),
		"cert_type":        "host",
		"valid_principals": strings.Join(cfg.principals, ","),
		"ttl":              cfg.certificateTTL,
	}, &signed); err != nil {
		return nil, fmt.Errorf("OpenBao SSH signing: %w", err)
	}
	if strings.TrimSpace(signed.Data.SignedKey) == "" {
		return nil, errors.New("OpenBao signing returned no certificate")
	}

	certData := []byte(strings.TrimSpace(signed.Data.SignedKey) + "\n")
	if err := validateHostCertificate(certData, publicKey, cfg.principals, cfg.certificateDuration); err != nil {
		return nil, fmt.Errorf("validate signed certificate: %w", err)
	}
	return certData, nil
}

// validateHostCertificate refuses anything the signing role should never
// hand back for a host: a non-host certificate, a key that is not the one
// requested, a principal set that is not EXACTLY the one requested (extra
// principals would let this cert authenticate as a host it is not), any
// critical option or extension (host certificates carry neither in this
// design — an unexpected one names a role that has drifted from what this
// worker expects), or a lifetime beyond what was requested.
func validateHostCertificate(certData []byte, publicKey ssh.PublicKey, principals []string, requestedTTL time.Duration) error {
	parsed, _, options, rest, err := ssh.ParseAuthorizedKey(certData)
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("signer returned an invalid OpenSSH certificate")
	}
	cert, ok := parsed.(*ssh.Certificate)
	if !ok || cert.CertType != ssh.HostCert {
		return errors.New("signer returned a non-host certificate")
	}
	if !bytes.Equal(cert.Key.Marshal(), publicKey.Marshal()) {
		return errors.New("certificate does not contain the worker's host key")
	}

	want := make([]string, len(principals))
	copy(want, principals)
	sort.Strings(want)
	got := make([]string, len(cert.ValidPrincipals))
	copy(got, cert.ValidPrincipals)
	sort.Strings(got)
	if len(want) != len(got) || strings.Join(want, ",") != strings.Join(got, ",") {
		return fmt.Errorf("certificate principals are %v, want exactly %v", cert.ValidPrincipals, principals)
	}
	if len(cert.CriticalOptions) != 0 {
		return fmt.Errorf("certificate carries critical options %v; want none", sortedKeys(cert.CriticalOptions))
	}
	if len(cert.Extensions) != 0 {
		return fmt.Errorf("certificate carries extensions %v; want none", sortedKeys(cert.Extensions))
	}

	now := time.Now()
	if uint64(now.Unix()) < cert.ValidAfter {
		return errors.New("certificate is not yet valid")
	}
	remaining := time.Until(time.Unix(int64(cert.ValidBefore), 0))
	if remaining <= 0 || remaining > requestedTTL+5*time.Minute {
		return fmt.Errorf("certificate remaining lifetime %s exceeds requested TTL %s", remaining.Round(time.Second), requestedTTL)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// parseCertificateTTL returns the requested certificate lifetime: 24h
// unset, and an error outside 1h..168h (a sanity bound, not a role
// ceiling — see the constants above).
func parseCertificateTTL(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultCertificateTTL, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil || ttl < minCertificateTTL || ttl > maxCertificateTTL {
		return 0, fmt.Errorf("NIX_WORKER_CERTIFICATE_TTL %q must be between %s and %s", value, minCertificateTTL, maxCertificateTTL)
	}
	if ttl%time.Second != 0 {
		return 0, fmt.Errorf("NIX_WORKER_CERTIFICATE_TTL %q must be a whole number of seconds", value)
	}
	return ttl, nil
}

func postJSON(ctx context.Context, client *http.Client, cfg config, endpoint, token string, payload any, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	u := *cfg.address
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/" + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Namespace", cfg.namespace)
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(responseBody) > maxResponseBytes {
		return errors.New("response exceeds 1 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(responseBody, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// atomicWrite writes data to path via a temp file in the same directory
// plus rename, so a renewal that fails partway through never leaves sshd
// reading a truncated certificate. It does not chown: unlike
// nix-worker-client (which hands files from an init container to a
// different container's user), this binary and sshd run in the same
// container, and the directory's ownership already governs both.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	d, err := os.Open(dir)
	if err == nil {
		defer func() { _ = d.Close() }()
		if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
			return err
		}
	}
	return nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return data, nil
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("nix worker host cert: %s is required", name)
	}
	return value
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
