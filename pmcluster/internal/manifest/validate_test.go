package manifest

import (
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// baseApp returns a minimal valid *dsl.App (already Interpolated — no
// placeholders to expand). Each test mutates the relevant field.
func baseApp() *dsl.App {
	return &dsl.App{
		Name:   "my-app",
		Env:    "production",
		Domain: "example.com",
		Services: map[string]*dsl.Service{
			"api": {Image: "nginx:latest"},
		},
	}
}

// ptr is a small helper to get a pointer to an int literal.
func ptr(n int) *int { return &n }

// mustFail calls Validate and asserts it returns an error containing substr.
func mustFail(t *testing.T, app *dsl.App, substr string) {
	t.Helper()
	err := Validate(app)
	if err == nil {
		t.Fatalf("expected Validate to fail with %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error %q should contain %q", err.Error(), substr)
	}
}

// mustPass calls Validate and asserts it returns nil.
func mustPass(t *testing.T, app *dsl.App) {
	t.Helper()
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: unexpected error: %v", err)
	}
}

func TestValidate_ValidMinimal(t *testing.T) {
	mustPass(t, baseApp())
}

func TestValidate_AppNameLowercase(t *testing.T) {
	a := baseApp()
	a.Name = "MyApp"
	mustFail(t, a, "app:")
}

func TestValidate_AppNameEmpty(t *testing.T) {
	a := baseApp()
	a.Name = ""
	mustFail(t, a, "app:")
}

func TestValidate_MissingEnv(t *testing.T) {
	a := baseApp()
	a.Env = ""
	mustFail(t, a, "env: required")
}

func TestValidate_MissingDomain(t *testing.T) {
	a := baseApp()
	a.Domain = ""
	mustFail(t, a, "domain: required")
}

func TestValidate_NoServices(t *testing.T) {
	a := baseApp()
	a.Services = map[string]*dsl.Service{}
	mustFail(t, a, "services:")
}

func TestValidate_MissingImage(t *testing.T) {
	a := baseApp()
	a.Services["api"].Image = ""
	mustFail(t, a, "services.api.image: required")
}

func TestValidate_ReplicasAndRunOnceMutex(t *testing.T) {
	a := baseApp()
	a.Services["api"].RunOnce = true
	a.Services["api"].Replicas = ptr(2)
	mustFail(t, a, "replicas and run_once are mutually exclusive")
}

func TestValidate_InvalidPlacement(t *testing.T) {
	a := baseApp()
	a.Services["api"].Placement = "somewhere"
	mustFail(t, a, "placement:")
}

func TestValidate_ValidPlacementManager(t *testing.T) {
	a := baseApp()
	a.Services["api"].Placement = "manager"
	mustPass(t, a)
}

func TestValidate_ValidPlacementWorker(t *testing.T) {
	a := baseApp()
	a.Services["api"].Placement = "worker"
	mustPass(t, a)
}

func TestValidate_ExposePortZero(t *testing.T) {
	a := baseApp()
	a.Services["api"].Expose = &dsl.Expose{Port: 0, Host: "api.example.com"}
	mustFail(t, a, "expose.port:")
}

func TestValidate_ExposePortTooHigh(t *testing.T) {
	a := baseApp()
	a.Services["api"].Expose = &dsl.Expose{Port: 65536, Host: "api.example.com"}
	mustFail(t, a, "expose.port:")
}

func TestValidate_ExposeHostEmpty(t *testing.T) {
	a := baseApp()
	a.Services["api"].Expose = &dsl.Expose{Port: 8080, Host: ""}
	mustFail(t, a, "expose.host: required")
}

func TestValidate_ExposeHostInvalidHostname(t *testing.T) {
	a := baseApp()
	// A hostname with spaces is clearly invalid.
	a.Services["api"].Expose = &dsl.Expose{Port: 8080, Host: "not valid hostname"}
	mustFail(t, a, "expose.host: invalid hostname")
}

func TestValidate_ExposeValid(t *testing.T) {
	a := baseApp()
	a.Services["api"].Expose = &dsl.Expose{Port: 8080, Host: "api.example.com"}
	mustPass(t, a)
}

func TestValidate_AliasesEmpty(t *testing.T) {
	a := baseApp()
	a.Services["api"].Expose = &dsl.Expose{
		Port:    8080,
		Host:    "api.example.com",
		Aliases: []string{""},
	}
	mustFail(t, a, "expose.aliases[0]")
}

func TestValidate_AliasesInvalidHostname(t *testing.T) {
	a := baseApp()
	a.Services["api"].Expose = &dsl.Expose{
		Port:    8080,
		Host:    "api.example.com",
		Aliases: []string{"not a hostname"},
	}
	mustFail(t, a, "expose.aliases[0]")
}

func TestValidate_AliasesValid(t *testing.T) {
	a := baseApp()
	a.Services["api"].Expose = &dsl.Expose{
		Port:    8080,
		Host:    "api.example.com",
		Aliases: []string{"idlibookfair.com", "shop.example.org"},
	}
	mustPass(t, a)
}

func TestValidate_HealthcheckShorthandAndTestMutex(t *testing.T) {
	a := baseApp()
	a.Services["api"].Healthcheck = &dsl.Healthcheck{
		Type: "http",
		Test: []string{"CMD", "curl", "-f", "http://localhost"},
	}
	mustFail(t, a, "shorthand")
}

func TestValidate_HealthcheckFullFormWithoutTest(t *testing.T) {
	a := baseApp()
	a.Services["api"].Healthcheck = &dsl.Healthcheck{
		Interval: "10s",
		Timeout:  "5s",
		Retries:  3,
	}
	mustFail(t, a, "full-form requires")
}

func TestValidate_HealthcheckFullFormWithTest(t *testing.T) {
	a := baseApp()
	a.Services["api"].Healthcheck = &dsl.Healthcheck{
		Test:     []string{"CMD-SHELL", "curl -f http://localhost"},
		Interval: "10s",
		Timeout:  "5s",
		Retries:  3,
	}
	mustPass(t, a)
}

func TestValidate_HealthcheckInvalidType(t *testing.T) {
	a := baseApp()
	a.Services["api"].Healthcheck = &dsl.Healthcheck{Type: "redis_ping"}
	mustFail(t, a, "healthcheck.type:")
}

func TestValidate_UpdateOrderInvalid(t *testing.T) {
	a := baseApp()
	a.Services["api"].Update = &dsl.Update{Order: "sideways"}
	mustFail(t, a, "update.order:")
}

func TestValidate_UpdateOrderValid(t *testing.T) {
	a := baseApp()
	a.Services["api"].Update = &dsl.Update{Order: "stop-first"}
	mustPass(t, a)
}

func TestValidate_UpdateParallelismNegative(t *testing.T) {
	a := baseApp()
	a.Services["api"].Update = &dsl.Update{Parallelism: -1}
	mustFail(t, a, "update.parallelism:")
}

func TestValidate_SecretEmptyName(t *testing.T) {
	a := baseApp()
	a.Secrets = []string{"valid-secret", ""}
	mustFail(t, a, "secrets[1]: empty name")
}

func TestValidate_VolumeEmptyName(t *testing.T) {
	a := baseApp()
	a.Volumes = []string{""}
	mustFail(t, a, "volumes[0]: empty name")
}
