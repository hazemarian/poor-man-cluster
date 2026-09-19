package manifest

import (
	"strings"
	"testing"
)

// donationCampaignDSL is the higher-level form of the user's working
// compose stack from the original conversation. The pipeline (parse →
// interpolate → validate → translate) should accept this end-to-end.
const donationCampaignDSL = `
app: donation-campaign
env: production
domain: example.com
registry: ghcr.io/nextrum-sy
version: latest
secrets:
  - donation_campaign_db_password
services:
  db:
    image: postgres:14-alpine
    placement: manager
    volumes: [db_data:/var/lib/postgresql/data]
    env:
      POSTGRES_DB: donation_campaign
      POSTGRES_USER: user
    secrets: [donation_campaign_db_password]
    healthcheck:
      type: pg_isready
  migration:
    image: ${registry}/${app}:${version}
    command: [./migrate]
    run_once: true
  api:
    image: ${registry}/${app}:${version}
    replicas: 2
    expose:
      port: 8080
      host: api.${app}.${domain}
    healthcheck:
      type: http
      path: /health
    update:
      parallelism: 1
      delay: 10s
      order: start-first
`

// TestPipeline_DonationCampaign validates the smoke end-to-end of the
// non-translate stages on the user's example. Sub-agent (Phase 3.F) will
// expand with: each validation failure mode, full-form healthcheck,
// ${env:VAR} substitution, unknown-key rejection.
func TestPipeline_DonationCampaign(t *testing.T) {
	app, err := Parse([]byte(donationCampaignDSL))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if app.Name != "donation-campaign" {
		t.Errorf("app.Name = %q", app.Name)
	}
	if got := len(app.Services); got != 3 {
		t.Errorf("len(services) = %d, want 3", got)
	}

	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	api := app.Services["api"]
	if api == nil {
		t.Fatal("missing services.api after parse")
	}
	wantImage := "ghcr.io/nextrum-sy/donation-campaign:latest"
	if api.Image != wantImage {
		t.Errorf("api.image after interpolate = %q, want %q", api.Image, wantImage)
	}
	wantHost := "api.donation-campaign.example.com"
	if api.Expose == nil || api.Expose.Host != wantHost {
		t.Errorf("api.expose.host after interpolate = %q, want %q", api.Expose.Host, wantHost)
	}

	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestTranslate_DonationCampaignSmoke is a structural sanity check that
// the translator emits a valid-looking compose YAML for the example.
// Sub-agent (Phase 3.F) will write proper golden-file tests.
func TestTranslate_DonationCampaignSmoke(t *testing.T) {
	app, err := Parse([]byte(donationCampaignDSL))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := Translate(app)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	body := string(out)

	mustContain := func(substr string) {
		t.Helper()
		if !strings.Contains(body, substr) {
			t.Errorf("expected output to contain %q\n--- got ---\n%s", substr, body)
		}
	}

	mustContain(`version: "3.9"`)
	mustContain(`donation-campaign-net`)
	mustContain(`traefik-net`)
	mustContain(`monitoring-net`)
	mustContain(`external: true`)
	mustContain(`donation_campaign_db_password`)
	mustContain(`db_data`)
	mustContain(`pg_isready`)
	mustContain(`wget -q --spider`)
	mustContain(`api.donation-campaign.example.com`)
	mustContain(`traefik.http.routers.donation-campaign-api.rule`)
	mustContain(`condition: none`)
	mustContain(`condition: on-failure`)
	mustContain(`order: start-first`)
	mustContain(`replicas: 2`)
	mustContain(`application: donation-campaign`)
	mustContain(`environment: production`)
	mustContain(`version: latest`)

	t.Logf("translated compose:\n%s", body)
}

// TestParse_RejectsUnknownKeys is a smoke for the strict YAML mode.
// TestTranslate_CORSDisabledOptsOut verifies the cors-default middleware
// label is omitted from the exposed router when cors_disabled: true is
// set in the manifest.
func TestTranslate_CORSDisabledOptsOut(t *testing.T) {
	src := `
app: my-app
env: production
domain: example.com
services:
  api:
    image: example/api:latest
    expose:
      port: 8080
      host: api.${app}.${domain}
      cors_disabled: true
`
	app, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Interpolate(app); err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := Translate(app)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if strings.Contains(string(out), "cors-default@file") {
		t.Errorf("cors_disabled: true should omit cors-default middleware, got:\n%s", out)
	}

	if !strings.Contains(string(out), "traefik.http.routers.my-app-api.rule") {
		t.Errorf("expected Traefik router rule label, got:\n%s", out)
	}
}

func TestParse_RejectsUnknownKeys(t *testing.T) {
	bad := `
app: donation-campaign
env: production
domain: example.com
typo_at_top: hello
services:
  api:
    image: foo
`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected Parse to reject unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "typo_at_top") {
		t.Errorf("error should mention the unknown key, got: %v", err)
	}
}
