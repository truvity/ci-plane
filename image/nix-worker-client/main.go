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
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	maxResponseBytes = 1 << 20
	workerPrincipal  = "nixremote"
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
	timeout             time.Duration
	tokenFile           string
	caFile              string
	sshCAFile           string
	knownHostsSource    string
	sshConfigSource     string
	machinesSource      string
	sshDir              string
	machinesDir         string
	podName             string
	podNamespace        string
	podUID              string
	runnerUID           int
	runnerGID           int
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
		log.Fatalf("nix builder setup: %v", err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if err := prepareOutputs(cfg); err != nil {
		return err
	}

	if err := configureRemote(cfg); err != nil {
		removeCredentials(cfg)
		log.Printf("nix builder setup: warning: remote builders disabled; local builds remain available: %v", err)
		return nil
	}

	log.Printf("nix builder setup: OpenBao certificate installed; remote builders enabled")
	return nil
}

func loadConfig() (config, error) {
	var cfg config

	address, err := url.Parse(requiredEnv("NIX_BUILDER_OPENBAO_ADDRESS"))
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil {
		return cfg, errors.New("NIX_BUILDER_OPENBAO_ADDRESS must be an https URL without user info")
	}
	cfg.address = address
	cfg.namespace = requiredEnv("NIX_BUILDER_OPENBAO_NAMESPACE")
	cfg.authMount = requiredEnv("NIX_BUILDER_OPENBAO_AUTH_MOUNT")
	cfg.authRole = requiredEnv("NIX_BUILDER_OPENBAO_AUTH_ROLE")
	cfg.sshMount = requiredEnv("NIX_BUILDER_OPENBAO_SSH_MOUNT")
	cfg.sshRole = requiredEnv("NIX_BUILDER_OPENBAO_SSH_ROLE")
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

	cfg.certificateTTL = envOr("NIX_BUILDER_CERTIFICATE_TTL", "72h")
	ttl, err := time.ParseDuration(cfg.certificateTTL)
	if err != nil || ttl < time.Hour || ttl > 7*24*time.Hour {
		return cfg, errors.New("NIX_BUILDER_CERTIFICATE_TTL must be between 1h and 168h")
	}
	cfg.certificateDuration = ttl

	cfg.timeout, err = time.ParseDuration(envOr("NIX_BUILDER_REQUEST_TIMEOUT", "15s"))
	if err != nil || cfg.timeout < time.Second || cfg.timeout > time.Minute {
		return cfg, errors.New("NIX_BUILDER_REQUEST_TIMEOUT must be between 1s and 1m")
	}

	cfg.tokenFile = envOr("NIX_BUILDER_TOKEN_FILE", "/var/run/secrets/openbao/token")
	cfg.caFile = envOr("NIX_BUILDER_OPENBAO_CA_FILE", "/var/run/nix-builder-config/openbao-ca.crt")
	cfg.sshCAFile = envOr("NIX_BUILDER_SSH_CA_FILE", "/var/run/nix-builder-config/ssh-client-ca.pub")
	cfg.knownHostsSource = envOr("NIX_BUILDER_KNOWN_HOSTS_FILE", "/var/run/nix-builder-known-hosts/known_hosts")
	cfg.sshConfigSource = envOr("NIX_BUILDER_SSH_CONFIG_FILE", "/var/run/nix-builder-config/ssh_config")
	cfg.machinesSource = envOr("NIX_BUILDER_MACHINES_TEMPLATE", "/var/run/nix-builder-config/machines.template")
	cfg.sshDir = envOr("NIX_BUILDER_SSH_DIR", "/var/run/nix-builder-ssh")
	cfg.machinesDir = envOr("NIX_BUILDER_MACHINES_DIR", "/var/run/nix-builders")
	cfg.podName = requiredEnv("POD_NAME")
	cfg.podNamespace = requiredEnv("POD_NAMESPACE")
	cfg.podUID = requiredEnv("POD_UID")

	runner, err := user.Lookup("runner")
	if err != nil {
		return cfg, fmt.Errorf("lookup runner user: %w", err)
	}
	cfg.runnerUID, err = strconv.Atoi(runner.Uid)
	if err != nil {
		return cfg, fmt.Errorf("parse runner uid: %w", err)
	}
	cfg.runnerGID, err = strconv.Atoi(runner.Gid)
	if err != nil {
		return cfg, fmt.Errorf("parse runner gid: %w", err)
	}

	return cfg, nil
}

func prepareOutputs(cfg config) error {
	if err := secureDir(cfg.sshDir, 0o700, cfg.runnerUID, cfg.runnerGID); err != nil {
		return fmt.Errorf("prepare SSH directory: %w", err)
	}
	if err := secureDir(cfg.machinesDir, 0o755, cfg.runnerUID, cfg.runnerGID); err != nil {
		return fmt.Errorf("prepare machines directory: %w", err)
	}

	// An empty machines file is the fail-open commit state: Nix retains
	// its ordinary local builders when OpenBao or the signer is unavailable.
	if err := atomicWrite(filepath.Join(cfg.machinesDir, "machines"), nil, 0o644, cfg.runnerUID, cfg.runnerGID); err != nil {
		return fmt.Errorf("publish empty machines file: %w", err)
	}

	knownHosts, err := readBounded(cfg.knownHostsSource, 64<<10)
	if err != nil || len(bytes.TrimSpace(knownHosts)) == 0 {
		return errors.New("known_hosts is missing or empty")
	}
	if err := atomicWrite(filepath.Join(cfg.sshDir, "known_hosts"), knownHosts, 0o644, cfg.runnerUID, cfg.runnerGID); err != nil {
		return fmt.Errorf("publish known_hosts: %w", err)
	}

	sshConfig, err := readBounded(cfg.sshConfigSource, 64<<10)
	if err != nil || len(bytes.TrimSpace(sshConfig)) == 0 {
		return errors.New("SSH config is missing or empty")
	}
	if err := atomicWrite(filepath.Join(cfg.sshDir, "config"), sshConfig, 0o600, cfg.runnerUID, cfg.runnerGID); err != nil {
		return fmt.Errorf("publish SSH config: %w", err)
	}

	machines, err := readBounded(cfg.machinesSource, 256<<10)
	if err != nil || len(bytes.TrimSpace(machines)) == 0 {
		return errors.New("machines template is missing or empty")
	}

	caPEM, err := readBounded(cfg.caFile, 1<<20)
	if err != nil {
		return fmt.Errorf("read OpenBao CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("OpenBao CA bundle contains no PEM certificate")
	}
	if _, err := readSSHAuthorities(cfg.sshCAFile); err != nil {
		return fmt.Errorf("read SSH client CA: %w", err)
	}

	return nil
}

func configureRemote(cfg config) error {
	tokenBytes, err := readBounded(cfg.tokenFile, 64<<10)
	if err != nil {
		return fmt.Errorf("read projected token: %w", err)
	}
	jwt := strings.TrimSpace(string(tokenBytes))
	if jwt == "" {
		return errors.New("projected token is empty")
	}

	caPEM, err := readBounded(cfg.caFile, 1<<20)
	if err != nil {
		return fmt.Errorf("read OpenBao CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("OpenBao CA bundle contains no PEM certificate")
	}
	authorities, err := readSSHAuthorities(cfg.sshCAFile)
	if err != nil {
		return fmt.Errorf("read SSH client CA: %w", err)
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

	staging, err := os.MkdirTemp(cfg.sshDir, ".setup-")
	if err != nil {
		return fmt.Errorf("create credential staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return fmt.Errorf("secure credential staging directory: %w", err)
	}

	keyPath := filepath.Join(staging, "nix-builder")
	comment := cfg.podNamespace + "/" + cfg.podName + "/" + cfg.podUID
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", keyPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("generate ephemeral SSH key: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	var login loginResponse
	if err := postJSON(ctx, client, cfg, "auth/"+cfg.authMount+"/login", "", map[string]string{
		"role": cfg.authRole,
		"jwt":  jwt,
	}, &login); err != nil {
		return fmt.Errorf("OpenBao login: %w", err)
	}
	if login.Auth.ClientToken == "" {
		return errors.New("OpenBao login returned no client token")
	}

	publicKey, err := readBounded(keyPath+".pub", 64<<10)
	if err != nil {
		return fmt.Errorf("read generated public key: %w", err)
	}

	var signed signResponse
	if err := postJSON(ctx, client, cfg, cfg.sshMount+"/sign/"+cfg.sshRole, login.Auth.ClientToken, map[string]string{
		"public_key":       string(publicKey),
		"cert_type":        "user",
		"valid_principals": workerPrincipal,
		"ttl":              cfg.certificateTTL,
	}, &signed); err != nil {
		return fmt.Errorf("OpenBao SSH signing: %w", err)
	}
	if strings.TrimSpace(signed.Data.SignedKey) == "" {
		return errors.New("OpenBao signing returned no certificate")
	}

	certPath := keyPath + "-cert.pub"
	certData := []byte(signed.Data.SignedKey + "\n")
	if err := os.WriteFile(certPath, certData, 0o600); err != nil {
		return fmt.Errorf("write staged certificate: %w", err)
	}
	if err := validateCertificate(certData, publicKey, authorities, workerPrincipal, cfg.certificateDuration); err != nil {
		return fmt.Errorf("validate signed certificate: %w", err)
	}

	for _, file := range []struct {
		source string
		name   string
		mode   os.FileMode
	}{
		{keyPath, "nix-builder", 0o600},
		{keyPath + ".pub", "nix-builder.pub", 0o644},
		{certPath, "nix-builder-cert.pub", 0o644},
	} {
		data, err := readBounded(file.source, 128<<10)
		if err != nil {
			return fmt.Errorf("read staged %s: %w", file.name, err)
		}
		if err := atomicWrite(filepath.Join(cfg.sshDir, file.name), data, file.mode, cfg.runnerUID, cfg.runnerGID); err != nil {
			return fmt.Errorf("publish %s: %w", file.name, err)
		}
	}

	machines, err := readBounded(cfg.machinesSource, 256<<10)
	if err != nil {
		return fmt.Errorf("read machines template: %w", err)
	}
	if err := atomicWrite(filepath.Join(cfg.machinesDir, "machines"), machines, 0o644, cfg.runnerUID, cfg.runnerGID); err != nil {
		return fmt.Errorf("enable remote builders: %w", err)
	}

	return nil
}

func readSSHAuthorities(path string) ([]ssh.PublicKey, error) {
	data, err := readBounded(path, 256<<10)
	if err != nil {
		return nil, err
	}

	var authorities []ssh.PublicKey
	for len(bytes.TrimSpace(data)) > 0 {
		key, _, options, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse public key: %w", err)
		}
		if len(options) != 0 {
			return nil, errors.New("SSH CA key must not carry authorized_keys options")
		}
		if key.Type() != ssh.KeyAlgoED25519 {
			return nil, fmt.Errorf("SSH CA key has type %s, want %s", key.Type(), ssh.KeyAlgoED25519)
		}
		authorities = append(authorities, key)
		data = rest
	}
	if len(authorities) == 0 {
		return nil, errors.New("SSH client CA file contains no public key")
	}
	return authorities, nil
}

func validateCertificate(certData, publicKeyData []byte, authorities []ssh.PublicKey, principal string, requestedTTL time.Duration) error {
	parsed, _, options, rest, err := ssh.ParseAuthorizedKey(certData)
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("signer returned an invalid OpenSSH certificate")
	}
	cert, ok := parsed.(*ssh.Certificate)
	if !ok || cert.CertType != ssh.UserCert {
		return errors.New("signer returned a non-user certificate")
	}

	publicKey, _, options, rest, err := ssh.ParseAuthorizedKey(publicKeyData)
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("generated public key is not one plain OpenSSH key")
	}
	if !bytes.Equal(cert.Key.Marshal(), publicKey.Marshal()) {
		return errors.New("certificate does not contain the generated public key")
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != principal {
		return fmt.Errorf("certificate principals are %v, want only %s", cert.ValidPrincipals, principal)
	}
	if len(cert.CriticalOptions) != 0 || len(cert.Extensions) != 0 {
		return errors.New("certificate carries unexpected critical options or extensions")
	}

	trustedAuthority := false
	for _, authority := range authorities {
		if bytes.Equal(cert.SignatureKey.Marshal(), authority.Marshal()) {
			trustedAuthority = true
			break
		}
	}
	if !trustedAuthority {
		return errors.New("certificate was signed by an unexpected SSH CA")
	}

	checker := ssh.CertChecker{IsUserAuthority: func(candidate ssh.PublicKey) bool {
		for _, authority := range authorities {
			if bytes.Equal(candidate.Marshal(), authority.Marshal()) {
				return true
			}
		}
		return false
	}}
	if err := checker.CheckCert(principal, cert); err != nil {
		return fmt.Errorf("certificate signature or validity: %w", err)
	}

	remaining := time.Until(time.Unix(int64(cert.ValidBefore), 0))
	if remaining <= 0 || remaining > requestedTTL+5*time.Minute {
		return fmt.Errorf("certificate remaining lifetime %s exceeds requested TTL %s", remaining.Round(time.Second), requestedTTL)
	}
	return nil
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
	defer resp.Body.Close()

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

func atomicWrite(path string, data []byte, mode os.FileMode, uid, gid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chown(uid, gid); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
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
		defer d.Close()
		if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
			return err
		}
	}
	return nil
}

func secureDir(path string, mode os.FileMode, uid, gid int) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func removeCredentials(cfg config) {
	for _, name := range []string{"nix-builder", "nix-builder.pub", "nix-builder-cert.pub"} {
		_ = os.Remove(filepath.Join(cfg.sshDir, name))
	}
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

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
		log.Fatalf("nix builder setup: %s is required", name)
	}
	return value
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
