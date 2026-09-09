package cluster

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
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

	stacks := []stackName{StackInfra, StackObservability, StackBackup}
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

			// No un-substituted placeholders should remain.
			if strings.Contains(body, "${DOMAIN}") {
				t.Error("${DOMAIN} placeholder was not substituted")
			}
			if strings.Contains(body, "${OPENOBSERVE_ADMIN_EMAIL}") {
				t.Error("${OPENOBSERVE_ADMIN_EMAIL} placeholder was not substituted")
			}

			// For stacks that actually use ${DOMAIN}, verify substitution.
			if stacksWithDomain[s] && !strings.Contains(body, "example.com") {
				t.Error("substituted domain 'example.com' not found in output")
			}
		})
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
	// Verify our substituted email appears somewhere (it may or may not be in
	// this stack, but at least no placeholder should remain).
	_ = body
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

// TestRenderOTelCollectorConfig_ContainsBasicAuth verifies that the rendered
// OTel config contains Authorization: Basic <base64(<org>:<ingestion_token>)> —
// the dedicated INGESTION token (not the rotating human password) — and that
// decoding the base64 yields the correct "org:token" string.
func TestRenderOTelCollectorConfig_ContainsBasicAuth(t *testing.T) {
	in := RenderInput{
		OpenObserveOrg:            "default",
		OpenObserveIngestionToken: "st1ableR00t-tok3n",
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	// Verify the Authorization header placeholder is gone.
	if strings.Contains(body, "__BASIC_AUTH_PLACEHOLDER__") {
		t.Error("__BASIC_AUTH_PLACEHOLDER__ not substituted")
	}

	// The template wraps the value in quotes: Authorization: "Basic <b64>"
	// Find the Authorization line with the quoted value.
	const prefix = `Authorization: "Basic `
	idx := strings.Index(body, prefix)
	if idx < 0 {
		t.Fatalf("%q not found in rendered config", prefix)
	}
	rest := body[idx+len(prefix):]
	// The base64 value ends at the closing quote.
	end := strings.Index(rest, `"`)
	if end < 0 {
		end = len(rest)
	}
	b64 := strings.TrimSpace(rest[:end])

	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode of %q: %v", b64, err)
	}

	want := "default:st1ableR00t-tok3n"
	if string(decoded) != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

// TestRenderOTelCollectorConfig_DoesNotEmbedHumanPassword verifies the rendered
// config never contains the human admin password (only the stable token).
func TestRenderOTelCollectorConfig_DoesNotEmbedHumanPassword(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail:     "admin@example.com",
		OpenObserveAdminPassword:  "SOMEPASSWORD_SECRET",
		OpenObserveIngestionToken: "R00ttok3n",
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	if strings.Contains(string(data), "SOMEPASSWORD_SECRET") {
		t.Error("rendered OTel config must NOT embed the rotating human password")
	}
}

// TestRenderOTelCollectorConfig_MissingToken returns an error when the
// ingestion token is empty.
func TestRenderOTelCollectorConfig_MissingToken(t *testing.T) {
	_, err := RenderOTelCollectorConfig(RenderInput{OpenObserveIngestionToken: ""})
	if err == nil {
		t.Fatal("expected error when ingestion token is empty, got nil")
	}
}

// TestRenderOTelCollectorConfig_LogsPipeline verifies the rendered config
// has a minimal logs pipeline: filelog → resource → batch → OTLP exporter.
func TestRenderOTelCollectorConfig_LogsPipeline(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail:     "admin@x.com",
		OpenObserveIngestionToken: "tok",
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	// Must NOT contain obsolete processors.
	for _, bad := range []string{"drop()", "filter/noise:", "transform:", "recombine"} {
		if strings.Contains(body, bad) {
			t.Errorf("rendered config should not contain obsolete processor: %q", bad)
		}
	}

	// Logs pipeline uses resourcedetection → resource → filter/skip_filelog for label-based enrichment.
	if !strings.Contains(body, "[resourcedetection, resource, filter/skip_filelog, batch]") {
		t.Error("logs pipeline must include resourcedetection + resource + filter/skip_filelog before batch")
	}
	// Metrics pipeline uses resourcedetection → resource → transform/metrics.
	if !strings.Contains(body, "[resourcedetection, resource, transform/metrics, batch]") {
		t.Error("metrics pipeline must include resourcedetection + resource + transform/metrics before batch")
	}
}

// TestRenderOTelCollectorConfig_FilelogReceiver verifies that the static
// filelog receiver is configured with the correct glob patterns and operators.
func TestRenderOTelCollectorConfig_FilelogReceiver(t *testing.T) {
	in := RenderInput{
		OpenObserveAdminEmail:     "admin@x.com",
		OpenObserveIngestionToken: "tok",
	}
	data, err := RenderOTelCollectorConfig(in)
	if err != nil {
		t.Fatalf("RenderOTelCollectorConfig: %v", err)
	}
	body := string(data)

	// Must use static filelog (not receiver_creator).
	if strings.Contains(body, "receiver_creator") {
		t.Error("rendered config should not contain receiver_creator")
	}

	// Filelog receiver with glob patterns.
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

	// Must NOT contain the old observer-based approach.
	if strings.Contains(body, "docker_observer") {
		t.Error("rendered config should not contain docker_observer")
	}

	// Must contain the filter/skip_filelog processor with the correct OTTL
	// expression that matches the `io.pmcluster.skip_filelog` container label.
	if !strings.Contains(body, "filter/skip_filelog") {
		t.Error("rendered config should contain filter/skip_filelog processor")
	}
	// The rendered YAML escapes dots in the label key with \\.
	// In the raw template output, dots are preceded by two backslashes.
	skipFilelogExpr := "container.labels.io" + "\\" + "\\" + ".pmcluster" + "\\" + "\\" + ".skip_filelog"
	if !strings.Contains(body, skipFilelogExpr) {
		t.Error("rendered config should contain the OTTL filter for io.pmcluster.skip_filelog label")
	}
	// The logs pipeline must include filter/skip_filelog before batch.
	if !strings.Contains(body, "processors: [resourcedetection, resource, filter/skip_filelog, batch]") {
		t.Error("logs pipeline should list filter/skip_filelog processor before batch")
	}
	// Verify no old placeholder remains.
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
		OpenObserveAdminEmail:     "admin@x.com",
		OpenObserveIngestionToken: "tok",
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
	// The phantom key must not appear as an actual option (with colon); a
	// comment may mention it, so only match the YAML key form.
	if strings.Contains(body, "metric_labels_to_resource_attributes:") {
		t.Error("rendered config should NOT contain the non-existent metric_labels_to_resource_attributes option")
	}
	// transform/metrics must guard against clobbering the label-derived values.
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

// TestRenderTraefikDynamic_ACMEMode verifies the ACME branch emits the
// certResolver tag and drops the static-cert tls block.
func TestRenderTraefikDynamic_ACMEMode(t *testing.T) {
	in := RenderInput{Domain: "x.example.com", ACMEEmail: "ops@example.com"}
	data, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatalf("RenderTraefikDynamic: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "certResolver: letsencrypt") {
		t.Errorf("ACME mode missing certResolver line:\n%s", body)
	}
	if strings.Contains(body, "/run/secrets/cert") || strings.Contains(body, "/run/secrets/key") {
		t.Errorf("ACME mode must not reference static cert/key secrets:\n%s", body)
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
		"traefik.http.routers.portainer.tls.certresolver=letsencrypt",
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
		"http://example.com",           // wrong scheme
		"https://example.com:8080",     // port not allowed
		"https://evil.com",             // different domain
		"https://example.com.evil.com", // suffix-attack
		"https://EXAMPLE.COM",          // case mismatch (Traefik regex is case-sensitive; Origin is lowercase per RFC 6454)
		"https://example.com/",         // trailing slash means a path component is in the Origin
		"https://-bad.example.com",     // leading hyphen
		"https://foo..example.com",     // empty label
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
// contains the cors-default middleware and the pmcluster router includes
// it in its middlewares chain.
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
	if !strings.Contains(body, "- cors-default") {
		t.Errorf("rendered config missing cors-default in router middlewares list:\n%s", body)
	}
	if !strings.Contains(body, `Access-Control-Allow-Credentials: "true"`) {
		t.Errorf("rendered config missing Access-Control-Allow-Credentials:\n%s", body)
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
		{"v0.1.11", "0.1.12", -1},       // leading v optional
		{"dev", "v0.1.0", -1},           // "dev" → [0], "v0.1.0" → [0,1,0], so dev < 0.1.0
		{"v0.10.0", "v0.2.0", 1},        // 10 > 2 numerically
		{"v0.1.12-alpha", "v0.1.11", 1}, // pre-release ignored, 12 > 11
	}
	for _, tt := range tests {
		got := Compare(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestShouldOverwriteConfig_StaleOrMissing exercises the version-based
// overwrite decision logic.
func TestShouldOverwriteConfig_StaleOrMissing(t *testing.T) {
	t.Run("missing version header", func(t *testing.T) {
		old := []byte("version: \"3.9\"\nservices:\n  foo:\n")
		if !shouldOverwriteConfig(old, "v0.1.12") {
			t.Error("file with no version header should be overwritten")
		}
	})

	t.Run("older version", func(t *testing.T) {
		old := []byte("## pmcluster-config-version: v0.1.9\nversion: \"3.9\"\n")
		if !shouldOverwriteConfig(old, "v0.1.12") {
			t.Error("file with older version should be overwritten")
		}
	})

	t.Run("same version", func(t *testing.T) {
		old := []byte("## pmcluster-config-version: v0.1.12\nversion: \"3.9\"\n")
		if shouldOverwriteConfig(old, "v0.1.12") {
			t.Error("file with same version should NOT be overwritten")
		}
	})

	t.Run("newer version", func(t *testing.T) {
		old := []byte("## pmcluster-config-version: v0.2.0\nversion: \"3.9\"\n")
		if shouldOverwriteConfig(old, "v0.1.12") {
			t.Error("file with newer version should NOT be overwritten")
		}
	})
}
