package manifest

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update, when true, writes the actual translator output to the golden files
// instead of comparing against them. Usage (must use -args to pass flags to
// the test binary rather than go test itself):
//
//	go test -run TestTranslate_Golden ./internal/manifest/ -args -update
var update = flag.Bool("update", false, "update golden files instead of comparing")

// goldenDir is the base directory holding translator test cases relative to
// this file's package directory.
const goldenDir = "testdata/translator"

// TestTranslate_Golden runs the full Parse → Interpolate → Validate → Translate
// pipeline on each case's dsl.yaml and compares (or writes) the expected
// compose.yaml golden file.
func TestTranslate_Golden(t *testing.T) {
	cases := []struct {
		name string

		expectedContains []string
	}{
		{
			name: "minimal",
			expectedContains: []string{
				`version: "3.9"`,
				"minimal-app-net",
				"busybox:latest",
				"condition: on-failure",
				"order: start-first",
			},
		},
		{
			name: "with-expose",
			expectedContains: []string{
				"traefik-net",
				"monitoring-net",
				"external: true",
				"traefik.enable",
				"traefik.http.routers.web-app-api.rule",
				"api.web-app.example.com",
				"traefik.http.routers.web-app-api.middlewares: cors-default@file",
			},
		},
		{
			name: "donation-campaign",
			expectedContains: []string{
				"donation-campaign-net",
				"traefik-net",
				"monitoring-net",
				"donation_campaign_db_password",
				"db_data",
				"pg_isready",
				"wget -q --spider",
				"api.donation-campaign.example.com",
				"condition: none",
				"replicas: 2",
				"traefik.http.routers.donation-campaign-api.middlewares: cors-default@file",
			},
		},
		{
			name: "with-volumes-secrets",
			expectedContains: []string{
				"vault-app-net",
				"vault_token",
				"data_vol",
				"vault status",
				"node.role == manager",
			},
		},
		{
			name: "service-only-secrets",
			expectedContains: []string{
				"abbas-net",
				"traefik-net",
				"monitoring-net",
				"abbas_session_secret",
				"abbas.nextrum-sy.com",
				"wget -q --spider",
				"external: true",
				"node.role == manager",
			},
		},
		{
			name: "runonce-job",
			expectedContains: []string{
				"batch-job-net",
				"condition: none",
			},
		},
		{
			name: "skip-filelog",
			expectedContains: []string{
				"io.pmcluster.skip_filelog",
			},
		},
		{
			name: "aliases",
			expectedContains: []string{
				"traefik.http.routers.bookfair-api.rule: Host(`idlib-book-fair.nextrum-sy.com`)",
				"traefik.http.routers.bookfair-api-alias0.rule: Host(`idlibookfair.com`)",
				"traefik.http.routers.bookfair-api-alias1.rule: Host(`www.nextrum-sy.com`)",

				"traefik.http.services.bookfair-api.loadbalancer.server.port: \"8080\"",

				"traefik.http.middlewares.bookfair-api-cors.headers.accesscontrolalloworiginlistregex: ^(https://idlib-book-fair\\.nextrum-sy\\.com|https://idlibookfair\\.com|https://www\\.nextrum-sy\\.com)$$",
				"traefik.http.middlewares.bookfair-api-cors.headers.accesscontrolallowcredentials: \"true\"",
				"traefik.http.routers.bookfair-api.middlewares: bookfair-api-cors@swarm",
				"traefik.http.routers.bookfair-api-alias0.middlewares: bookfair-api-cors@swarm",
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dslPath := filepath.Join(goldenDir, tc.name, "dsl.yaml")
			goldenPath := filepath.Join(goldenDir, tc.name, "compose.yaml")

			dslBytes, err := os.ReadFile(dslPath)
			if err != nil {
				t.Fatalf("read dsl.yaml: %v", err)
			}

			app, err := Parse(dslBytes)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if err := Interpolate(app); err != nil {
				t.Fatalf("Interpolate: %v", err)
			}
			if err := Validate(app); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			got, err := Translate(app)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}

			if *update {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("updated %s", goldenPath)
			}

			gotStr := string(got)
			for _, substr := range tc.expectedContains {
				if !containsSubstr(gotStr, substr) {
					t.Errorf("output missing expected substring %q\n--- got ---\n%s", substr, gotStr)
				}
			}

			if tc.name == "runonce-job" {
				if containsSubstr(gotStr, "update_config") {
					t.Errorf("runonce job should not have update_config:\n%s", gotStr)
				}
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				if os.IsNotExist(err) {
					t.Fatalf("golden file missing; run with -update to create: %s", goldenPath)
				}
				t.Fatalf("read golden: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("output differs from golden %s\n--- want ---\n%s\n--- got ---\n%s",
					goldenPath, want, got)
			}
		})
	}
}

// containsSubstr is a simple substring check (avoids importing strings in
// this file — we use the package-internal one instead for zero extra deps).
func containsSubstr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}
