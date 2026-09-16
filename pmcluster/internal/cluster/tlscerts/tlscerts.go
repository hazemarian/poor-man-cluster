// Package tlscerts validates TLS certificate/key pairs against a hostname.
//
// Every certificate flow (the cluster's own cert and per-host certs) uses
// Validate + ParseAndCheck. PEM bytes are never managed on the filesystem by
// this package — they live in versioned Swarm secrets; only validation
// helpers live here.
package tlscerts

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var hostRe = regexp.MustCompile(`^(\*\.)?[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// Validate checks that host is DNS-shaped and free of path separators or
// traversal.
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

// PairInfo is the metadata extracted from a validated cert/key pair.
type PairInfo struct {
	SANs      []string
	NotBefore time.Time
	NotAfter  time.Time
}

// ParseAndCheck validates that certPEM and keyPEM form a matching X.509 pair
// and that the leaf certificate covers host (honouring CN and wildcard SANs).
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

type certPair struct {
	leaf *x509.Certificate
}

func parsePair(certPEM, keyPEM string) (*certPair, error) {
	if certPEM == "" || keyPEM == "" {
		return nil, fmt.Errorf("both cert and key are required")
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		return nil, fmt.Errorf("cert and key do not form a valid pair: %w", err)
	}
	leaf, err := parseLeaf([]byte(certPEM))
	if err != nil {
		return nil, err
	}
	return &certPair{leaf: leaf}, nil
}

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

func hostCoveredByCert(cert *x509.Certificate, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, san := range cert.DNSNames {
		if domainMatches(strings.ToLower(san), host) {
			return true
		}
	}
	if cert.Subject.CommonName != "" {
		cn := strings.ToLower(cert.Subject.CommonName)
		if domainMatches(cn, host) {
			return true
		}
	}
	return false
}

func domainMatches(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return len(name) > len(suffix) && strings.HasSuffix(name, suffix)
	}
	return false
}
