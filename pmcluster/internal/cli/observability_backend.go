package cli

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/remote"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func backendObservability(cmd *cobra.Command) (observability.Reader, func(), error) {
	if rc := remoteClient(cmd); rc != nil {
		return remote.NewObservability(rc), func() {}, nil
	}
	st, cfg, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	cipher, err := credentials.Open(cfg.EncryptionKeyPath())
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	var dc runtime.Client
	if client, dockerErr := docker.New(); dockerErr == nil {
		dc = client
	}
	reader, _ := newLocalObservabilityReader(cmd.Context(), st, cipher, dc)
	return reader, func() {
		if dc != nil {
			_ = dc.Close()
		}
		_ = st.Close()
	}, nil
}

// newLocalObservabilityReader builds the daemon's source through the edge's
// reserved internal route. Parseable remains on monitoring-net; this process
// never resolves its Swarm DNS name or accepts a backend URL/password flag.
func newLocalObservabilityReader(ctx context.Context, st *store.Store, cipher *credentials.Cipher, dc observability.NodeReader) (*observability.ReadService, error) {
	reader := &observability.ReadService{Inventory: dc}
	if st == nil || cipher == nil {
		return reader, errors.New("observability edge source is not configured")
	}
	domain := strings.TrimSpace(st.SettingDefault(ctx, cluster.SettingDomain(), ""))
	if domain == "" {
		return reader, errors.New("cluster domain is not configured")
	}
	cred, err := st.Credential(ctx, "edge_api_token")
	if err != nil {
		return reader, fmt.Errorf("load edge API credential: %w", err)
	}
	plain, err := cipher.Decrypt(cred.PasswordCiphertext)
	if err != nil {
		return reader, errors.New("decrypt edge API credential")
	}
	token := strings.TrimSpace(string(plain))
	if token == "" {
		return reader, errors.New("edge API credential is empty")
	}
	_, httpsPort := cluster.LoadIngressPorts(ctx, st)
	host := "pmcluster." + domain
	httpClient, err := localIngressHTTPClient(ctx, st, host, httpsPort)
	if err != nil {
		return reader, err
	}
	source, err := observability.NewEdgeForward(observability.EdgeForwardConfig{
		BaseURL:    "https://" + host,
		EdgeToken:  token,
		HTTPClient: httpClient,
		Timeout:    observability.MaxQueryDuration,
	})
	if err != nil {
		return reader, err
	}
	reader.LogsSrc = source
	reader.Metrics = source
	reader.TracesSrc = source
	return reader, nil
}

// localIngressHTTPClient maps the public cluster hostname to the local
// Traefik listener while preserving Host and SNI. ACMΕ uses normal system-root
// verification. BYO TLS pins the operator-provided leaf and verifies its
// chain/expiry without reading the private key or disabling verification
// blindly.
func localIngressHTTPClient(ctx context.Context, st *store.Store, host string, httpsPort int) (*http.Client, error) {
	if st == nil {
		return nil, errors.New("cluster store is unavailable")
	}
	tlsConfig, err := localIngressTLS(ctx, st, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(dialCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(httpsPort)))
		},
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: observability.MaxQueryDuration,
	}
	return &http.Client{Transport: transport, Timeout: observability.MaxQueryDuration}, nil
}

func localIngressTLS(ctx context.Context, st *store.Store, host string) (*tls.Config, error) {
	mode := strings.ToLower(strings.TrimSpace(st.SettingDefault(ctx, cluster.SettingTLSMode(), "")))
	certPath := strings.TrimSpace(st.SettingDefault(ctx, cluster.SettingTLSCertPath(), ""))
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if mode == "acme" || certPath == "" {
		return config, nil
	}
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read configured TLS certificate: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("configured TLS certificate is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("configured TLS certificate is invalid")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("configured TLS certificate cannot be trusted")
	}
	pin := sha256.Sum256(leaf.Raw)
	config.RootCAs = roots
	// The operator-provided certificate can predate a domain migration and
	// therefore lack the new SNI SAN. Pin the expected leaf and verify the
	// chain/expiry manually; this is narrower than accepting any certificate.
	config.InsecureSkipVerify = true //nolint:gosec // VerifyConnection performs pin + chain + expiry checks below.
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("TLS peer did not present a certificate")
		}
		peer := state.PeerCertificates[0]
		if !bytesEqual(sha256.Sum256(peer.Raw), pin) {
			return errors.New("TLS peer certificate does not match the configured certificate")
		}
		now := time.Now()
		if now.Before(peer.NotBefore) || now.After(peer.NotAfter) {
			return errors.New("TLS peer certificate is expired or not yet valid")
		}
		intermediates := x509.NewCertPool()
		for _, cert := range state.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		if _, err := peer.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return errors.New("TLS peer certificate chain is invalid")
		}
		return nil
	}
	return config, nil
}

func bytesEqual(a, b [32]byte) bool {
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
