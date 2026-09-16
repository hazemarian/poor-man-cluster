// Package tlscerts validates TLS certificate/key pairs and provides the
// legacy per-host certificate file manager.
//
// Per-host certificates are now stored as versioned Swarm secrets with a
// site_certs DB row — the same table and flow as the cluster's own
// certificate — and are never read from the manager's filesystem. This
// package's Manager/HostsDir remain only as the one-time migration path that
// imports certs left in the legacy <configDir>/hosts/ directory into
// secrets + DB. Validate + ParseAndCheck are used by every certificate flow.
//
// Paths are deliberately ops-invisible: the caller (CLI, daemon API, edge UI)
// supplies cert + key as text; the manager writes, validates, lists and removes
// them. A config refresh (see cluster.RefreshHostCerts) makes Traefik pick the
// new cert up via its file-provider hot reload.
package tlscerts

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Subdir is the per-host cert directory name relative to the pmcluster config
// dir, e.g. <configDir>/hosts/<host>/.
const Subdir = "hosts"

// HostsDir resolves the per-host certs base directory from a pmcluster config
// dir. Keys and certs live at <configDir>/hosts/<host>/{cert.pem,key.pem}.
func HostsDir(configDir string) string { return filepath.Join(configDir, Subdir) }

// EnsureHostsDir guarantees the per-host certs base directory exists. The
// infra stack bind-mounts this directory read-only into Traefik, and Docker
// rejects service bind mounts whose source path is missing — so cluster
// up/update must create it before `docker stack deploy`.
func EnsureHostsDir(configDir string) error {
	return os.MkdirAll(HostsDir(configDir), 0o700)
}

// hostRe bounds the folder name we derive from a host so no path traversal or
// garbage can escape the hosts dir. It mirrors a DNS hostname (plus optional
// leading '*.' for wildcard certs).
var hostRe = regexp.MustCompile(`^(\*\.)?[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// HostCert is the immutable on-disk state of one per-host cert.
type HostCert struct {
	Host      string    `json:"host"`
	SANs      []string  `json:"sans"`
	NotAfter  time.Time `json:"not_after"`
	NotBefore time.Time `json:"not_before"`
	CertFile  string    `json:"cert_file"`
	KeyFile   string    `json:"key_file"`
}

// Manager is a tiny filesystem wrapper over <hostsDir>/. It owns name
// sanitisation, PEM validation and atomic writes. All methods are idempotent
// and safe to call concurrently.
type Manager struct {
	dir string
}

// New returns a Manager rooted at the hosts base directory created from
// configDir. The directory (and parents) are created on demand.
func New(configDir string) *Manager { return &Manager{dir: HostsDir(configDir)} }

// Dir returns the hosts base directory this manager writes to.
func (m *Manager) Dir() string { return m.dir }

// HostDir returns the per-host directory for host.
func (m *Manager) HostDir(host string) string { return filepath.Join(m.dir, m.sanitize(host)) }

// sanitize validates host and lowercases it for use as a folder name. It
// returns an error via Validate otherwise.
func (m *Manager) sanitize(host string) string { return normalizeHost(host) }

// normalizeHost lowercases a validated host. Callers must Validate first.
func normalizeHost(host string) string { return strings.ToLower(host) }

// Validate checks that host is safe to use as a directory name (DNS-shaped,
// no path separators or traversal) and returns a wrapped error otherwise.
func Validate(host string) error {
	if host == "" {
		return fmt.Errorf("host is required")
	}
	if strings.ContainsAny(host, "/\\") || strings.Contains(host, "..") {
		return fmt.Errorf("invalid host %q: must not contain path separators or '..'", host)
	}
	if !hostRe.MatchString(host) {
		return fmt.Errorf("invalid host %q", host)
	}
	return nil
}

// Put validates certPEM + keyPEM as a matching pair, checks the certificate
// covers host, atomically writes them to disk (0600) and returns the stored
// HostCert. The existing cert/key for the same host are replaced.
func (m *Manager) Put(ctx context.Context, host, certPEM, keyPEM string) (HostCert, error) {
	if err := Validate(host); err != nil {
		return HostCert{}, err
	}
	host = normalizeHost(host)

	parsed, err := parsePair(certPEM, keyPEM)
	if err != nil {
		return HostCert{}, err
	}
	if !hostCoveredByCert(parsed.leaf, host) {
		return HostCert{}, fmt.Errorf("certificate for %q does not cover host %q (SANs: %s)",
			host, host, strings.Join(parsed.leaf.DNSNames, ", "))
	}

	dir := m.HostDir(host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return HostCert{}, fmt.Errorf("create host dir: %w", err)
	}
	// Write key first, then cert, always 0600.
	if err := writeFile(dir, "key.pem", keyPEM); err != nil {
		return HostCert{}, err
	}
	if err := writeFile(dir, "cert.pem", certPEM); err != nil {
		return HostCert{}, err
	}
	return HostCert{
		Host:      host,
		SANs:      append([]string(nil), parsed.leaf.DNSNames...),
		NotAfter:  parsed.leaf.NotAfter,
		NotBefore: parsed.leaf.NotBefore,
		CertFile:  filepath.Join(dir, "cert.pem"),
		KeyFile:   filepath.Join(dir, "key.pem"),
	}, nil
}

// writeFile writes data to <dir>/<name> with 0600 perms, replacing any prior
// content atomically via rename.
func writeFile(dir, name, data string) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}

// List returns every on-disk per-host cert, sorted by host. A host whose
// cert.pem is unreadable/parseable is skipped from the result.
func (m *Manager) List(ctx context.Context) ([]HostCert, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hosts dir %s: %w", m.dir, err)
	}
	var out []HostCert
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		certPath := filepath.Join(m.dir, e.Name(), "cert.pem")
		keyPath := filepath.Join(m.dir, e.Name(), "key.pem")
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			continue // incomplete / not a cert host
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			continue
		}
		parsed, err := parsePair(string(certPEM), string(keyPEM))
		if err != nil {
			continue
		}
		out = append(out, HostCert{
			Host:      e.Name(),
			SANs:      append([]string(nil), parsed.leaf.DNSNames...),
			NotAfter:  parsed.leaf.NotAfter,
			NotBefore: parsed.leaf.NotBefore,
			CertFile:  certPath,
			KeyFile:   keyPath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// Remove deletes the on-disk folder for host. It returns whether anything was
// removed (false when the host had no stored cert).
func (m *Manager) Remove(ctx context.Context, host string) (bool, error) {
	if err := Validate(host); err != nil {
		return false, err
	}
	host = normalizeHost(host)
	dir := m.HostDir(host)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat host dir: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("remove host dir: %w", err)
	}
	return true, nil
}

// certPair holds a parsed leaf cert and its validated keypair.
type certPair struct {
	leaf *x509.Certificate
}

// PairInfo is the metadata extracted from a validated cert/key pair — the
// fields pmcluster persists for expiry monitoring and display.
type PairInfo struct {
	SANs      []string
	NotBefore time.Time
	NotAfter  time.Time
}

// ParseAndCheck validates that certPEM and keyPEM form a matching X.509 pair
// and that the leaf certificate covers host (honouring CN and wildcard SANs).
// It returns the extracted metadata on success. Exported for the site-cert
// path (cluster's own domain) which does not use the per-host file manager.
func ParseAndCheck(certPEM, keyPEM, host string) (PairInfo, error) {
	parsed, err := parsePair(certPEM, keyPEM)
	if err != nil {
		return PairInfo{}, err
	}
	if !hostCoveredByCert(parsed.leaf, host) {
		return PairInfo{}, fmt.Errorf("certificate for %q does not cover host %q (SANs: %s)",
			host, host, strings.Join(parsed.leaf.DNSNames, ", "))
	}
	return PairInfo{
		SANs:      append([]string(nil), parsed.leaf.DNSNames...),
		NotBefore: parsed.leaf.NotBefore,
		NotAfter:  parsed.leaf.NotAfter,
	}, nil
}

// parsePair validates that certPEM and keyPEM form a matching X.509 pair and
// returns the parsed leaf certificate.
func parsePair(certPEM, keyPEM string) (*certPair, error) {
	if certPEM == "" || keyPEM == "" {
		return nil, fmt.Errorf("both cert and key are required")
	}
	_, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("cert and key do not form a valid pair: %w", err)
	}
	leaf, err := parseLeaf([]byte(certPEM))
	if err != nil {
		return nil, err
	}
	return &certPair{leaf: leaf}, nil
}

// parseLeaf parses the first PEM block of certPEM as an X.509 certificate.
func parseLeaf(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

// hostCoveredByCert reports whether the certificate covers host, honouring
// CN and wildcard DNS SANs (e.g. *.example.com covers api.example.com).
func hostCoveredByCert(cert *x509.Certificate, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, san := range cert.DNSNames {
		if domainMatches(strings.ToLower(san), host) {
			return true
		}
	}
	// Fall back to CN for single-name certs without SANs.
	if cert.Subject.CommonName != "" {
		cn := strings.ToLower(cert.Subject.CommonName)
		if domainMatches(cn, host) {
			return true
		}
	}
	return false
}

// domainMatches reports whether pattern (possibly "*.example.com") matches
// the fully-qualified name.
func domainMatches(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return len(name) > len(suffix) && strings.HasSuffix(name, suffix)
	}
	return false
}
