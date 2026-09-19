package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// stacksWithDomain lists the bundled stacks that actually contain ${DOMAIN}
// substitution points (backup-stack.yml has none).
var stacksWithDomain = map[stackName]bool{
	StackInfra:         true,
	StackObservability: false,
	StackBackup:        false,
}

// TestLoadComposeFile_KnownStacks verifies each known stack returns non-empty
// YAML and that all placeholder tokens have been removed.
func TestLoadComposeFile_KnownStacks(t *testing.T) {
	in := RenderInput{
		Domain:                "example.com",
		OpenObserveAdminEmail: "ops@example.com",
	}

	stacks := []stackName{StackInfra, StackEdge, StackObservability, StackBackup}
	for _, s := range stacks {
		t.Run(string(s), func(t *testing.T) {
			data, err := LoadComposeFile(s, in)
			if err != nil {
				t.Fatalf("LoadComposeFile(%q): %v", s, err)
			}
			if len(data) == 0 {
				t.Fatal("returned empty YAML")
			}
			body := string(data)

			if strings.Contains(body, "${DOMAIN}") {
				t.Error("${DOMAIN} placeholder was not substituted")
			}
			if strings.Contains(body, "${OPENOBSERVE_ADMIN_EMAIL}") {
				t.Error("${OPENOBSERVE_ADMIN_EMAIL} placeholder was not substituted")
			}

			if stacksWithDomain[s] && !strings.Contains(body, "example.com") {
				t.Error("substituted domain 'example.com' not found in output")
			}
		})
	}
}

// TestLoadComposeFile_BackupControlPlane verifies the backup stack renders
// the manager-only control-plane backup agent when DataDir is set, and omits
// it (with no dangling placeholder) when it isn't.
func TestLoadComposeFile_BackupControlPlane(t *testing.T) {

	in := RenderInput{Domain: "example.com", DataDir: "/root/.pmcluster"}
	data, err := LoadComposeFile(StackBackup, in)
	if err != nil {
		t.Fatalf("LoadComposeFile(backup): %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"control-plane-backup",
		"/root/.pmcluster:/backup/pmcluster:ro",
		"BACKUP_SOURCES=/backup/pmcluster",
		"pmcluster-ctlplane-",
		"node.role == manager",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered backup stack missing %q", want)
		}
	}
	if strings.Contains(body, "${DATA_DIR}") {
		t.Error("${DATA_DIR} placeholder was not substituted")
	}

	in2 := RenderInput{Domain: "example.com"}
	data2, err := LoadComposeFile(StackBackup, in2)
	if err != nil {
		t.Fatalf("LoadComposeFile(backup, no DataDir): %v", err)
	}
	body2 := string(data2)
	if strings.Contains(body2, "control-plane-backup") {
		t.Error("control-plane-backup should be omitted when DataDir is empty")
	}
	if strings.Contains(body2, "${DATA_DIR}") {
		t.Error("dangling ${DATA_DIR} placeholder in no-DataDir render")
	}
}

// TestLoadComposeFile_SubstitutesOpenObserveEmail verifies the email
// substitution specifically in the observability stack, which uses it.
func TestLoadComposeFile_SubstitutesOpenObserveEmail(t *testing.T) {
	in := RenderInput{
		Domain:                "prod.example.com",
		OpenObserveAdminEmail: "admin@company.org",
	}
	data, err := LoadComposeFile(StackObservability, in)
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "${OPENOBSERVE_ADMIN_EMAIL}") {
		t.Error("${OPENOBSERVE_ADMIN_EMAIL} placeholder was not substituted in observability stack")
	}

	_ = body
}

// TestLoadComposeFile_EscapesDollarInOpenObservePassword verifies that a
// generated OpenObserve root password containing "$" (RandomPassword's special
// charset includes "!@#$%^&*") is doubled to "$$" in the rendered compose file
// so `docker stack deploy` interpolation emits a literal "$" — otherwise the
// env value would be truncated and OO's root password would no longer match
// the stored credential (HTTP 401 on provisioning).
func TestLoadComposeFile_EscapesDollarInOpenObservePassword(t *testing.T) {
	in := RenderInput{
		Domain:                   "example.com",
		OpenObserveAdminEmail:    "ops@example.com",
		OpenObserveAdminPassword: "ab$cd",
	}
	data, err := LoadComposeFile(StackObservability, in)
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "ab$cd") {
		t.Error("raw '$' was not escaped in ZO_ROOT_USER_PASSWORD (docker stack deploy would interpolate it)")
	}
	if !strings.Contains(body, "ab$$cd") {
		t.Error("expected 'ab$$cd' in rendered observability stack after $-escaping")
	}
}

// TestLoadComposeFile_ObservabilityOmitsRootToken verifies that fresh-install
// observability does NOT bake ZO_ROOT_USER_TOKEN into the OpenObserve data
// volume — the OTel collector instead uses a dedicated ingestion token created
// via the OO API and rendered into the collector config, so credentials stay in
// pmcluster config rather than the OO volume.
func TestLoadComposeFile_ObservabilityOmitsRootToken(t *testing.T) {
	in := RenderInput{
		Domain:                "example.com",
		OpenObserveAdminEmail: "ops@example.com",
	}
	data, err := LoadComposeFile(StackObservability, in)
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "ZO_ROOT_USER_TOKEN") {
		t.Error("observability stack must NOT set ZO_ROOT_USER_TOKEN (baked into OO volume on first boot)")
	}
	if !strings.Contains(body, "ZO_ROOT_USER_EMAIL=ops@example.com") {
		t.Error("observability stack should still set ZO_ROOT_USER_EMAIL")
	}
}

// TestLoadComposeFile_UnknownStack verifies that an unknown stack name returns
// an error.
func TestLoadComposeFile_UnknownStack(t *testing.T) {
	_, err := LoadComposeFile("nonexistent", RenderInput{Domain: "x.com"})
	if err == nil {
		t.Fatal("expected error for unknown stack, got nil")
	}
}

// TestLoadComposeFile_SSOStack verifies the sso-stack renders the oauth2-proxy
// sidecar with the sso.<domain> Traefik router and the matching callback URL.
func TestLoadComposeFile_SSOStack(t *testing.T) {
	in := RenderInput{
		Domain:          "example.com",
		SSOEnabled:      true,
		SSOClientID:     "client-id",
		SSOClientSecret: "client-secret",
		SSOCookieSecret: "cookie-secret",
		SSOGitHubOrg:    "nextrum-s",
	}
	data, err := LoadComposeFile(StackSSO, in)
	if err != nil {
		t.Fatalf("LoadComposeFile(StackSSO): %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"Host(`sso.example.com`)",
		"traefik.http.services.sso.loadbalancer.server.port=4180",
		"OAUTH2_PROXY_REDIRECT_URL: \"https://sso.example.com/oauth2/callback\"",
		"OAUTH2_PROXY_CLIENT_ID: \"client-id\"",
		"OAUTH2_PROXY_CLIENT_SECRET: \"client-secret\"",
		"OAUTH2_PROXY_GITHUB_ORG: \"nextrum-s\"",
		"OAUTH2_PROXY_COOKIE_SECRET: \"cookie-secret\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sso-stack render missing %q", want)
		}
	}
	for _, bad := range []string{"pmcluster.example.com/oauth2", "__SSO", "[[."} {
		if strings.Contains(body, bad) {
			t.Errorf("sso-stack render should not contain %q", bad)
		}
	}
}

// TestRenderOTelCollectorConfig_ContainsBasicAuth verifies that the rendered
// OTel config contains Authorization: Basic <base64(admin email:admin password)> —
// the ROOT admin credentials (the same ones OpenObserve itself runs with), and
// that decoding the base64 yields the correct "email:password" string.
func TestRenderOTelCollectorConfig_ContainsBasicAuth(t *testing.T) {
	in := RenderInput{
		OpenObserveBasicAuth: openObserveBasicAuth("admin@example.com", "s3cret-Pass"),
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	if strings.Contains(body, "__BASIC_AUTH_PLACEHOLDER__") {
		t.Error("__BASIC_AUTH_PLACEHOLDER__ not substituted")
	}

	const prefix = `Authorization: "Basic `
	idx := strings.Index(body, prefix)
	if idx < 0 {
		t.Fatalf("%q not found in rendered config", prefix)
	}
	rest := body[idx+len(prefix):]

	end := strings.Index(rest, `"`)
	if end < 0 {
		end = len(rest)
	}
	b64 := strings.TrimSpace(rest[:end])

	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode of %q: %v", b64, err)
	}

	want := "admin@example.com:s3cret-Pass"
	if string(decoded) != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

// TestRenderOTelCollectorConfig_EmbedsAdminPassword verifies the rendered
// config embeds the ROOT admin password (base64) — by design the collector
// authenticates with the same credentials OpenObserve runs with, so there is
// one password everywhere and no provisioning API calls.
func TestRenderOTelCollectorConfig_EmbedsAdminPassword(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail:    "admin@example.com",
		OpenObserveAdminPassword: "SOMEPASSWORD_SECRET",
		OpenObserveBasicAuth:     openObserveBasicAuth("admin@example.com", "SOMEPASSWORD_SECRET"),
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("admin@example.com:SOMEPASSWORD_SECRET"))
	if !strings.Contains(string(data), b64) {
		t.Errorf("rendered OTel config must embed the admin basic auth %q", b64)
	}
	if strings.Contains(string(data), "SOMEPASSWORD_SECRET") {
		t.Error("rendered OTel config must NOT embed the raw admin password (only base64)")
	}
}

// TestRenderOTelCollectorConfig_MissingBasicAuth returns an error when the
// admin basic auth value is empty.
func TestRenderOTelCollectorConfig_MissingBasicAuth(t *testing.T) {
	_, err := RenderOTelCollectorConfig(RenderInput{})
	if err == nil {
		t.Fatal("expected error when admin basic auth is empty, got nil")
	}
}

// TestRenderOTelCollectorConfig_LogsPipeline verifies the rendered config
// has a minimal logs pipeline: filelog → resource → batch → OTLP exporter.
func TestRenderOTelCollectorConfig_LogsPipeline(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail: "admin@x.com",
		OpenObserveBasicAuth:  openObserveBasicAuth("admin@x.com", "pw"),
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	for _, bad := range []string{"drop()", "filter/noise:", "transform:", "recombine"} {
		if strings.Contains(body, bad) {
			t.Errorf("rendered config should not contain obsolete processor: %q", bad)
		}
	}

	if !strings.Contains(body, "[resourcedetection, resource, filter/skip_filelog, batch]") {
		t.Error("logs pipeline must include resourcedetection + resource + filter/skip_filelog before batch")
	}

	if !strings.Contains(body, "[resourcedetection, resource, transform/metrics, batch]") {
		t.Error("metrics pipeline must include resourcedetection + resource + transform/metrics before batch")
	}
}

// TestRenderOTelCollectorConfig_FilelogReceiver verifies that the static
// filelog receiver is configured with the correct glob patterns and operators.
func TestRenderOTelCollectorConfig_FilelogReceiver(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail: "admin@x.com",
		OpenObserveBasicAuth:  openObserveBasicAuth("admin@x.com", "pw"),
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	if strings.Contains(body, "receiver_creator") {
		t.Error("rendered config should not contain receiver_creator")
	}

	for _, want := range []string{
		`/var/lib/docker/containers/*/*-json.log`,
		`exclude:`,
		`otel-collector`,
		`openobserve`,
		`json_parser`,
		`error_mode: ignore`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered config missing expected content: %q", want)
		}
	}

	if strings.Contains(body, "docker_observer") {
		t.Error("rendered config should not contain docker_observer")
	}

	if !strings.Contains(body, "filter/skip_filelog") {
		t.Error("rendered config should contain filter/skip_filelog processor")
	}

	skipFilelogExpr := "container.labels.io" + "\\" + "\\" + ".pmcluster" + "\\" + "\\" + ".skip_filelog"
	if !strings.Contains(body, skipFilelogExpr) {
		t.Error("rendered config should contain the OTTL filter for io.pmcluster.skip_filelog label")
	}

	if !strings.Contains(body, "processors: [resourcedetection, resource, filter/skip_filelog, batch]") {
		t.Error("logs pipeline should list filter/skip_filelog processor before batch")
	}

	if strings.Contains(body, "__FILELOG_EXCLUDE_PATTERNS__") {
		t.Error("rendered config should NOT contain __FILELOG_EXCLUDE_PATTERNS__ (label-based approach replaces it)")
	}
}

// TestRenderOTelCollectorConfig_DockerStatsLabels verifies the docker_stats
// receiver promotes Swarm container labels to metric datapoint attributes via
// the (real) container_labels_to_metric_labels option, and no longer relies on
// the non-existent metric_labels_to_resource_attributes key.
func TestRenderOTelCollectorConfig_DockerStatsLabels(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail: "admin@x.com",
		OpenObserveBasicAuth:  openObserveBasicAuth("admin@x.com", "pw"),
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"container_labels_to_metric_labels",
		`com.docker.swarm.service.name: "service_name"`,
		`com.docker.stack.namespace:    "stack_name"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered config missing expected docker_stats mapping: %q", want)
		}
	}

	if strings.Contains(body, "metric_labels_to_resource_attributes:") {
		t.Error("rendered config should NOT contain the non-existent metric_labels_to_resource_attributes option")
	}

	for _, want := range []string{`conditions:`, `resource.attributes["service.name"] != nil`} {
		if !strings.Contains(body, want) {
			t.Errorf("transform/metrics missing guard condition: %q", want)
		}
	}
}

// TestRenderTraefikDynamic_SubstitutesDomain verifies that __DOMAIN__ is
// replaced with the supplied Domain value.
func TestRenderTraefikDynamic_SubstitutesDomain(t *testing.T) {
	in := RenderInput{Domain: "myapp.example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)

	if strings.Contains(body, "__DOMAIN__") {
		t.Error("__DOMAIN__ placeholder was not substituted")
	}
	if !strings.Contains(body, "myapp.example.com") {
		t.Error("substituted domain not found in output")
	}
}

// TestRenderTraefikDynamic_EmptyDomain returns an error when Domain is empty.
func TestRenderTraefikDynamic_EmptyDomain(t *testing.T) {
	_, err := RenderTraefikDynamic(RenderInput{Domain: ""})
	if err == nil {
		t.Fatal("expected error for empty Domain, got nil")
	}
}

// TestRenderTraefikDynamic_ACMEMode verifies the ACME branch drops the
// static-cert tls block and declares no file-provider router (the
// pmcluster.<domain> router + its letsencrypt certResolver are mounted via
// edge-stack.yml labels).
func TestRenderTraefikDynamic_ACMEMode(t *testing.T) {
	in := RenderInput{Domain: "x.example.com", ACMEEmail: "ops@example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "/run/secrets/cert") || strings.Contains(body, "/run/secrets/key") {
		t.Errorf("ACME mode must not reference static cert/key secrets:\n%s", body)
	}
	if strings.Contains(body, "routers:") {
		t.Errorf("ACME mode file-provider should declare no routers (edge labels own the router):\n%s", body)
	}
	if !strings.Contains(body, "cors-default:") {
		t.Errorf("ACME mode must still ship the shared middlewares:\n%s", body)
	}
}

// TestRenderTraefikDynamic_BYOMode verifies the legacy path stays intact
// when ACMEEmail is empty.
func TestRenderTraefikDynamic_BYOMode(t *testing.T) {
	in := RenderInput{Domain: "x.example.com", CertSecretName: "cert_v001", KeySecretName: "key_v001"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "certResolver") {
		t.Errorf("BYO mode must not emit certResolver:\n%s", body)
	}
	if !strings.Contains(body, "/run/secrets/cert_v001") || !strings.Contains(body, "/run/secrets/key_v001") {
		t.Errorf("BYO mode must reference static cert/key secrets:\n%s", body)
	}
}

// selfSignedForHost generates a throwaway self-signed ECDSA cert/key for
// host, used only to prove the dynamic-config render appends host certs.
func selfSignedForHost(t *testing.T, host string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// TestRenderTraefikDynamic_AppendsHostCerts verifies that per-host certs
// recorded in the DB (as versioned Swarm secrets) are merged into the dynamic
// config's tls.certificates (referencing /run/secrets/<name>) WITHOUT
// disturbing the cluster's own BYO default cert block, and that an empty
// list leaves the rendered body untouched.
func TestRenderTraefikDynamic_AppendsHostCerts(t *testing.T) {
	cfgDir := t.TempDir()

	in := RenderInput{
		Domain:         "x.example.com",
		ConfigDir:      cfgDir,
		CertSecretName: "cert_v001",
		KeySecretName:  "key_v001",
		HostCerts: []HostCertEntry{
			{Host: "idlibookfair.com", CertSecret: "hostcert-idlibookfair-com_v001", KeySecret: "hostkey-idlibookfair-com_v001"},
			{Host: "abbas.example.com", CertSecret: "hostcert-abbas-example-com_v007", KeySecret: "hostkey-abbas-example-com_v007"},
		},
	}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "certFile: /run/secrets/hostcert-idlibookfair-com_v001") {
		t.Errorf("host cert certFile missing:\n%s", body)
	}
	if !strings.Contains(body, "keyFile: /run/secrets/hostkey-idlibookfair-com_v001") {
		t.Errorf("host cert keyFile missing:\n%s", body)
	}
	if !strings.Contains(body, "certFile: /run/secrets/hostcert-abbas-example-com_v007") {
		t.Errorf("second host cert certFile missing:\n%s", body)
	}

	if !strings.Contains(body, "/run/secrets/cert_v001") || !strings.Contains(body, "/run/secrets/key_v001") {
		t.Errorf("BYO default cert references dropped:\n%s", body)
	}

	if strings.Count(body, "tls:") != 1 {
		t.Errorf("expected exactly one `tls:` key, got %d:\n%s", strings.Count(body, "tls:"), body)
	}

	in.HostCerts = nil
	data2, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic (no hosts): %v", err)
	}
	if !strings.Contains(string(data2), "cors-default") {
		t.Errorf("no-hosts render lost middlewares:\n%s", string(data2))
	}
	if strings.Contains(string(data2), "traefik/hosts") || strings.Contains(string(data2), "hostcert-") {
		t.Errorf("no-hosts render should not reference host certs:\n%s", string(data2))
	}
}

// TestLoadComposeFile_InfraACMEMode verifies the infra stack picks up the
// ACME-specific Traefik flags + volume in ACME mode and drops the cert/key
// secret declarations.
func TestLoadComposeFile_InfraACMEMode(t *testing.T) {
	body, err := LoadComposeFile(StackInfra, RenderInput{
		Domain:    "x.example.com",
		ACMEEmail: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"certificatesresolvers.letsencrypt.acme.email=ops@example.com",
		"--certificatesresolvers.letsencrypt.acme.httpchallenge=true",
		"traefik_acme:/letsencrypt",
		"traefik.http.routers.traefik.tls.certresolver=letsencrypt",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ACME-mode infra stack missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"  cert:\n    external: true",
		"  key:\n    external: true",
	} {
		if strings.Contains(s, unwanted) {
			t.Errorf("ACME-mode infra stack should NOT contain %q", unwanted)
		}
	}
}

// TestCORSOriginRegex_MatchesSubdomains exercises the regex against a set
// of origin strings to confirm only same-domain HTTPS origins match.
func TestCORSOriginRegex_MatchesSubdomains(t *testing.T) {
	pat := CORSOriginRegex("example.com")
	re := mustCompile(t, pat)

	allow := []string{
		"https://example.com",
		"https://traefik.example.com",
		"https://api.foo.example.com",
		"https://a.b.c.example.com",
	}
	for _, o := range allow {
		if !re.MatchString(o) {
			t.Errorf("expected match for %q with pattern %s", o, pat)
		}
	}

	deny := []string{
		"http://example.com",
		"https://example.com:8080",
		"https://evil.com",
		"https://example.com.evil.com",
		"https://EXAMPLE.COM",
		"https://example.com/",
		"https://-bad.example.com",
		"https://foo..example.com",
	}
	for _, o := range deny {
		if re.MatchString(o) {
			t.Errorf("expected NO match for %q with pattern %s", o, pat)
		}
	}
}

// TestCORSOriginRegex_RejectsBadDomain falls back to a never-match pattern
// when the input doesn't look like a host.
func TestCORSOriginRegex_RejectsBadDomain(t *testing.T) {
	for _, bad := range []string{
		"", "no-dot", "http://example.com",
		"example.com/path", "example.com:80",
		".example.com", "example..com",
	} {
		got := CORSOriginRegex(bad)
		if got != `^$` {
			t.Errorf("CORSOriginRegex(%q) = %q, want fallback %q", bad, got, `^$`)
		}
	}
}

// TestRenderTraefikDynamic_CORSWired verifies the rendered dynamic config
// contains the cors-default middleware, its domain-based origin, and the
// Access-Control-Allow-Credentials header. The shared middlewares live here
// (referenced by routers mounted via stack labels, e.g. the edge console's
// pmcluster.<domain> router in edge-stack.yml), so no router is declared in
// this file itself.
func TestRenderTraefikDynamic_CORSWired(t *testing.T) {
	in := RenderInput{Domain: "example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "cors-default:") {
		t.Errorf("rendered config missing cors-default middleware:\n%s", body)
	}
	if !strings.Contains(body, `Access-Control-Allow-Origin: "https://example.com"`) {
		t.Errorf("rendered config missing domain-based origin in cors-default:\n%s", body)
	}
	if !strings.Contains(body, `Access-Control-Allow-Credentials: "true"`) {
		t.Errorf("rendered config missing Access-Control-Allow-Credentials:\n%s", body)
	}

	if strings.Contains(body, "routers:") {
		t.Errorf("file-provider should declare no routers (edge owns pmcluster.<domain>):\n%s", body)
	}
}

// TestRenderTraefikDynamic_AutoAuthMiddleware verifies the openobserve-auto-auth
// middleware is rendered when OpenObserveBasicAuth is set (Traefik injects the
// OO root Basic header AFTER admin-auth lets the operator through) and is
// OMITTED when empty (no OO credentials yet — avoids a public root credential).
func TestRenderTraefikDynamic_AutoAuthMiddleware(t *testing.T) {
	in := RenderInput{Domain: "example.com", OpenObserveBasicAuth: "Basic dXNlckBleGFtcGxlLmNvbTpzZWNyZXQ="}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "customRequestHeaders") {
		t.Errorf("rendered config missing openobserve-auto-auth middleware:\n%s", body)
	}
	if !strings.Contains(body, `Authorization: "Basic dXNlckBleGFtcGxlLmNvbTpzZWNyZXQ="`) {
		t.Errorf("rendered config missing injected Authorization header:\n%s", body)
	}

	inEmpty := RenderInput{Domain: "example.com"}
	dataEmpty, err := RenderTraefikDynamic(inEmpty)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic (empty auth): %v", err)
	}
	if strings.Contains(string(dataEmpty), "customRequestHeaders") || strings.Contains(string(dataEmpty), `Authorization:`) {
		t.Errorf("empty OpenObserveBasicAuth must OMIT the auto-auth middleware:\n%s", string(dataEmpty))
	}
}

// TestOpenObserveBasicAuth verifies the base64 header computation: empty email
// or password yields an empty string (middleware omitted), otherwise
// "Basic base64(email:password)".
func TestOpenObserveBasicAuth(t *testing.T) {
	if got := openObserveBasicAuth("", "pw"); got != "" {
		t.Errorf("empty email: got %q, want empty", got)
	}
	if got := openObserveBasicAuth("admin@example.com", ""); got != "" {
		t.Errorf("empty password: got %q, want empty", got)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin@example.com:secret"))
	if got := openObserveBasicAuth("admin@example.com", "secret"); got != want {
		t.Errorf("openObserveBasicAuth = %q, want %q", got, want)
	}
}

// TestRenderTraefikDynamic_CORSOverride verifies that changing the domain
// updates the Access-Control-Allow-Origin header.
func TestRenderTraefikDynamic_CORSOverride(t *testing.T) {
	in := RenderInput{Domain: "other.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `Access-Control-Allow-Origin: "https://other.com"`) {
		t.Errorf("override origin not present:\n%s", body)
	}
	if strings.Contains(body, `Access-Control-Allow-Origin: "https://example.com"`) {
		t.Errorf("old domain should not appear")
	}
}

func mustCompile(t *testing.T, pat string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pat)
	if err != nil {
		t.Fatalf("compile %q: %v", pat, err)
	}
	return re
}

// TestLoadComposeFile_InfraBYOMode verifies the legacy operator-cert path.
func TestLoadComposeFile_InfraBYOMode(t *testing.T) {
	body, err := LoadComposeFile(StackInfra, RenderInput{
		Domain:         "x.example.com",
		CertSecretName: "cert_v001",
		KeySecretName:  "key_v001",
	})
	if err != nil {
		t.Fatalf("LoadComposeFile: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "certificatesresolvers.letsencrypt") {
		t.Errorf("BYO-mode infra stack must not contain ACME flags")
	}
	if strings.Contains(s, "traefik_acme") {
		t.Errorf("BYO-mode infra stack must not declare the ACME volume")
	}
	if !strings.Contains(s, "  {}\n") {
		t.Errorf("BYO-mode infra stack must render an empty volumes mapping (not volumes: null):\n%s", s)
	}
	for _, want := range []string{
		"  cert_v001:\n    external: true",
		"  key_v001:\n    external: true",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("BYO-mode infra stack missing %q", want)
		}
	}
}

// TestCompare_Versions exercises the semver comparison used by
// EnsureConfigDir to decide whether a disk config is stale.
func TestCompare_Versions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v0.1.12", "v0.1.12", 0},
		{"v0.1.12", "v0.1.11", 1},
		{"v0.1.11", "v0.1.12", -1},
		{"v0.2.0", "v0.1.99", 1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.1.11", "0.1.12", -1},
		{"dev", "v0.1.0", -1},
		{"v0.10.0", "v0.2.0", 1},
		{"v0.1.12-alpha", "v0.1.11", 1},
	}
	for _, tt := range tests {
		got := Compare(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestEdgeImageFor(t *testing.T) {
	t.Setenv(EdgeImageEnv, "")

	t.Run("defaults to the release version tag", func(t *testing.T) {
		origVersion := buildinfo.Version
		t.Cleanup(func() { buildinfo.Version = origVersion })
		buildinfo.Version = "v0.2.30"
		if got := EdgeImageFor(); got != EdgeImageBase+":v0.2.30" {
			t.Errorf("EdgeImageFor() = %q, want %q", got, EdgeImageBase+":v0.2.30")
		}
	})

	t.Run("dev builds fall back to latest", func(t *testing.T) {
		origVersion := buildinfo.Version
		t.Cleanup(func() { buildinfo.Version = origVersion })
		buildinfo.Version = "dev"
		if got := EdgeImageFor(); got != EdgeImageBase+":latest" {
			t.Errorf("EdgeImageFor() = %q, want %q", got, EdgeImageBase+":latest")
		}
	})

	t.Run("env pin uses tag suffix", func(t *testing.T) {
		t.Setenv(EdgeImageEnv, "v0.2.21")
		if got := EdgeImageFor(); got != EdgeImageBase+":v0.2.21" {
			t.Errorf("EdgeImageFor() = %q, want %q", got, EdgeImageBase+":v0.2.21")
		}
	})

	t.Run("env pin accepts full image ref", func(t *testing.T) {
		t.Setenv(EdgeImageEnv, "registry.example.com/pmcluster-edge:edge-1.2")
		if got := EdgeImageFor(); got != "registry.example.com/pmcluster-edge:edge-1.2" {
			t.Errorf("EdgeImageFor() = %q, want the custom ref", got)
		}
	})
}

func TestSyncClusterConfigs(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "sync.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Seed a stale infra-stack row exactly like the server's v0.2.31 leftovers
	// (contains Portainer, stamped with an old version).
	staleContent := "services:\n  portainer:\n    image: portainer/portainer-ce:2.39.5\n"
	if _, err := s.CreateConfig(context.Background(), "cluster", "", "infra-stack", "template", staleContent, "v0.2.31"); err != nil {
		t.Fatalf("create stale row: %v", err)
	}

	res, err := SyncPlatformConfigs(context.Background(), s, "v0.2.45")
	if err != nil {
		t.Fatalf("SyncPlatformConfigs: %v", err)
	}

	rows, err := s.ListConfigs(context.Background(), "cluster", "")
	if err != nil {
		t.Fatalf("list cluster rows: %v", err)
	}
	if len(rows) != len(ConfigFileNames) {
		t.Fatalf("got %d cluster rows, want %d", len(rows), len(ConfigFileNames))
	}
	for _, r := range rows {
		if r.Version != "v0.2.45" {
			t.Errorf("row %s version = %s, want v0.2.45", r.Name, r.Version)
		}
		if r.Kind != "template" {
			t.Errorf("row %s kind = %s, want template", r.Name, r.Kind)
		}
	}
	infra, err := s.GetConfig(context.Background(), "infra-stack")
	if err != nil {
		t.Fatalf("get infra-stack: %v", err)
	}
	if strings.Contains(infra.Content, "portainer") {
		t.Error("stale portainer content survived the sync")
	}
	if len(res.Updated) != 1 || res.Updated[0] != "infra-stack" {
		t.Errorf("res.Updated = %v, want exactly [infra-stack]", res.Updated)
	}
	if len(res.Created) != len(ConfigFileNames)-1 {
		t.Errorf("res.Created = %v, want %d rows created", res.Created, len(ConfigFileNames)-1)
	}

	// Re-run: everything is current → nothing churned, all preserved.
	res2, err := SyncPlatformConfigs(context.Background(), s, "v0.2.45")
	if err != nil {
		t.Fatalf("second SyncPlatformConfigs: %v", err)
	}
	if len(res2.Created)+len(res2.Updated) != 0 {
		t.Errorf("second sync churned rows: created=%v updated=%v", res2.Created, res2.Updated)
	}
	if len(res2.Preserved) != len(ConfigFileNames) {
		t.Errorf("res2.Preserved = %v, want all %d rows", res2.Preserved, len(ConfigFileNames))
	}
}

func TestSyncClusterConfigs_ConsoleEditPreserved(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "edit.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// First sync seeds all rows at the current version.
	if _, err := SyncPlatformConfigs(context.Background(), s, "v0.2.45"); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	// Simulate a console edit: row is updated and re-stamped with the same
	// current version.
	custom := "version: \"3.9\"\nservices:\n  edge:\n    image: custom/edge:1.0\n"
	if _, err := s.UpdateConfig(context.Background(), "edge-stack", custom, "v0.2.45"); err != nil {
		t.Fatalf("console edit: %v", err)
	}

	res, err := SyncPlatformConfigs(context.Background(), s, "v0.2.45")
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if len(res.Created)+len(res.Updated) != 0 {
		t.Errorf("sync churned console-edited row: created=%v updated=%v", res.Created, res.Updated)
	}
	// The edit must survive (DB authoritative at the current version).
	row, err := s.GetConfig(context.Background(), "edge-stack")
	if err != nil {
		t.Fatalf("get edge-stack: %v", err)
	}
	if row.Content != custom {
		t.Errorf("console edit lost: %q", row.Content)
	}
}

func TestSyncPlatformConfigs_NilStore(t *testing.T) {
	res, err := SyncPlatformConfigs(context.Background(), nil, "v0.2.45")
	if err != nil {
		t.Fatalf("SyncPlatformConfigs(nil store): %v", err)
	}
	if len(res.Created)+len(res.Updated)+len(res.Preserved) != 0 {
		t.Errorf("nil-store result should be empty, got %+v", res)
	}
}

func TestPruneCandidates(t *testing.T) {
	compose := []byte("version: \"3.9\"\nservices:\n  web:\n    image: nginx\n  db:\n    image: postgres\n")

	t.Run("drops services missing from the compose", func(t *testing.T) {
		remove, err := pruneCandidates("infra", compose, []string{"infra_web", "infra_db", "infra_portainer", ""})
		if err != nil {
			t.Fatalf("pruneCandidates: %v", err)
		}
		want := []string{"infra_portainer"}
		if strings.Join(remove, ",") != strings.Join(want, ",") {
			t.Errorf("remove = %v, want %v", remove, want)
		}
	})

	t.Run("no-op when everything matches", func(t *testing.T) {
		remove, err := pruneCandidates("infra", compose, []string{"infra_web", "infra_db"})
		if err != nil {
			t.Fatalf("pruneCandidates: %v", err)
		}
		if len(remove) != 0 {
			t.Errorf("remove = %v, want none", remove)
		}
	})

	t.Run("empty compose never prunes", func(t *testing.T) {
		remove, err := pruneCandidates("infra", []byte("services: {}\n"), []string{"infra_web"})
		if err != nil {
			t.Fatalf("pruneCandidates: %v", err)
		}
		if len(remove) != 0 {
			t.Errorf("remove = %v, want none (safety)", remove)
		}
	})

	t.Run("invalid yaml errors", func(t *testing.T) {
		if _, err := pruneCandidates("infra", []byte("services: notamap\n"), nil); err == nil {
			t.Error("expected parse error")
		}
	})
}

// TestRenderRefResolver verifies the cluster-side resolver maps the logical
// config()/secrets() names to the versioned Docker artifact names from the
// render, and rejects unknown references.
func TestRenderRefResolver(t *testing.T) {
	r := &renderRefResolver{render: RenderInput{
		OTelConfigName:    "pmcluster_otel_config_v046",
		TraefikConfigName: "pmcluster_traefik_dynamic_v044",
		CertSecretName:    "cert_v042",
		KeySecretName:     "key_v042",
	}}

	for name, want := range map[string]string{
		"pmcluster_otel_config":     "pmcluster_otel_config_v046",
		"pmcluster_traefik_dynamic": "pmcluster_traefik_dynamic_v044",
	} {
		got, err := r.ResolveConfig(context.Background(), name)
		if err != nil {
			t.Errorf("ResolveConfig(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveConfig(%q) = %q, want %q", name, got, want)
		}
	}

	for name, want := range map[string]string{
		"cert": "cert_v042",
		"key":  "key_v042",
	} {
		got, err := r.ResolveSecret(context.Background(), name)
		if err != nil {
			t.Errorf("ResolveSecret(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveSecret(%q) = %q, want %q", name, got, want)
		}
	}

	if _, err := r.ResolveConfig(context.Background(), "nope"); err == nil {
		t.Error("ResolveConfig(unknown) should error")
	}
	if _, err := r.ResolveSecret(context.Background(), "nope"); err == nil {
		t.Error("ResolveSecret(unknown) should error")
	}
}

// TestLoadComposeFile_ResolvesConfigSecretRefs verifies the infra and
// observability stacks substitute config()/secrets() refs with the versioned
// artifact names and leave no raw references behind.
func TestLoadComposeFile_ResolvesConfigSecretRefs(t *testing.T) {
	in := RenderInput{
		Domain:                "example.com",
		OpenObserveAdminEmail: "ops@example.com",
		OTelConfigName:        "pmcluster_otel_config_v046",
		TraefikConfigName:     "pmcluster_traefik_dynamic_v044",
		CertSecretName:        "cert_v042",
		KeySecretName:         "key_v042",
	}

	for _, s := range []stackName{StackInfra, StackObservability} {
		data, err := LoadComposeFile(s, in)
		if err != nil {
			t.Fatalf("LoadComposeFile(%q): %v", s, err)
		}
		body := string(data)
		for _, unresolved := range []string{"config(", "secrets(", "__OTEL_CONFIG_NAME__", "__TRAEFIK_CONFIG_NAME__", "__CERT_SECRET__", "__KEY_SECRET__"} {
			if strings.Contains(body, unresolved) {
				t.Errorf("%s: unresolved %q in output:\n%s", s, unresolved, body)
			}
		}
	}

	infra, err := LoadComposeFile(StackInfra, in)
	if err != nil {
		t.Fatal(err)
	}
	infraBody := string(infra)
	for _, want := range []string{"pmcluster_traefik_dynamic_v044", "cert_v042", "key_v042"} {
		if !strings.Contains(infraBody, want) {
			t.Errorf("infra-stack missing %q in output", want)
		}
	}

	obs, err := LoadComposeFile(StackObservability, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(obs), "pmcluster_otel_config_v046") {
		t.Errorf("observability-stack missing otel config name in output")
	}
}
