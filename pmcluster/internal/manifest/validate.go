package manifest

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// nameRe enforces lowercase + digits + dashes/underscores for `app:` and
// service keys. Matches Docker Swarm stack/service naming constraints.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// hostnameRe is a permissive FQDN check. We trust the operator's `domain`
// so we just require: at least one dot, no whitespace, no obvious garbage.
var hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9*]([a-zA-Z0-9.-]*\.[a-zA-Z]{2,})?$`)

// Validate runs semantic checks against an interpolated manifest. Returns
// the first failure with a path-prefixed error so the operator sees
// `services.api.image: required` not just `required`.
func Validate(app *dsl.App) error {
	if !nameRe.MatchString(app.Name) {
		return fmt.Errorf("app: must be lowercase letters/digits/dashes/underscores, got %q", app.Name)
	}
	if app.Env == "" {
		return fmt.Errorf("env: required (e.g. production, staging)")
	}
	if app.Domain == "" {
		return fmt.Errorf("domain: required")
	}
	if len(app.Services) == 0 {
		return fmt.Errorf("services: at least one service is required")
	}
	for name, svc := range app.Services {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("services.%s: invalid service name", name)
		}
		if err := validateService(name, svc); err != nil {
			return err
		}
	}
	for i, name := range app.Secrets {
		if name == "" {
			return fmt.Errorf("secrets[%d]: empty name", i)
		}
	}
	for i, name := range app.Volumes {
		if name == "" {
			return fmt.Errorf("volumes[%d]: empty name", i)
		}
	}
	return nil
}

func validateService(name string, s *dsl.Service) error {
	prefix := "services." + name
	if s.Image == "" {
		return fmt.Errorf("%s.image: required", prefix)
	}
	if s.Replicas != nil && *s.Replicas < 0 {
		return fmt.Errorf("%s.replicas: must be ≥ 0, got %d", prefix, *s.Replicas)
	}
	if s.RunOnce && s.Replicas != nil {
		return fmt.Errorf("%s: replicas and run_once are mutually exclusive", prefix)
	}
	if s.Placement != "" && s.Placement != "manager" && s.Placement != "worker" {
		return fmt.Errorf("%s.placement: must be 'manager', 'worker', or empty (got %q)", prefix, s.Placement)
	}
	for i, v := range s.Volumes {
		if !strings.Contains(v, ":") {
			return fmt.Errorf("%s.volumes[%d]: must be 'name:path' or '/host:/container', got %q", prefix, i, v)
		}
	}
	for k, v := range s.Env {
		if malformedEnvRef(v) {
			return fmt.Errorf("%s.env.%s: malformed reference %q — expected config(name) or secrets(name)", prefix, k, v)
		}
		if ref, ok := parseEnvRef(v); ok && ref.Name == "" {
			return fmt.Errorf("%s.env.%s: empty name in %s() reference", prefix, k, ref.Kind)
		}
		if ref, ok := parseEnvRef(v); ok && ref.Kind == "secrets" {
			if !slices.Contains(s.Secrets, ref.Name) {
				return fmt.Errorf("%s.env.%s: secrets(%s) is not mounted — add %q to the service secrets: array (env refs point at /run/secrets/<name>)", prefix, k, ref.Name, ref.Name)
			}
		}
	}
	if s.Expose != nil {
		if s.Expose.Port < 1 || s.Expose.Port > 65535 {
			return fmt.Errorf("%s.expose.port: must be 1..65535, got %d", prefix, s.Expose.Port)
		}
		if s.Expose.Host == "" {
			return fmt.Errorf("%s.expose.host: required when expose is set", prefix)
		}
		if !hostnameRe.MatchString(s.Expose.Host) {
			return fmt.Errorf("%s.expose.host: invalid hostname %q", prefix, s.Expose.Host)
		}
		for i, alias := range s.Expose.Aliases {
			if alias == "" {
				return fmt.Errorf("%s.expose.aliases[%d]: empty alias", prefix, i)
			}
			if !hostnameRe.MatchString(alias) {
				return fmt.Errorf("%s.expose.aliases[%d]: invalid hostname %q", prefix, i, alias)
			}
		}
	}
	if s.Healthcheck != nil {
		if err := validateHealthcheck(prefix, s.Healthcheck); err != nil {
			return err
		}
	}
	if s.Update != nil {
		if s.Update.Order != "" && s.Update.Order != "start-first" && s.Update.Order != "stop-first" {
			return fmt.Errorf("%s.update.order: must be 'start-first' or 'stop-first', got %q", prefix, s.Update.Order)
		}
		if s.Update.Parallelism < 0 {
			return fmt.Errorf("%s.update.parallelism: must be ≥ 0, got %d", prefix, s.Update.Parallelism)
		}
	}
	return nil
}

func validateHealthcheck(prefix string, h *dsl.Healthcheck) error {
	switch h.Type {
	case "":

		if len(h.Test) == 0 && (h.Interval != "" || h.Timeout != "" || h.Retries != 0) {
			return fmt.Errorf("%s.healthcheck: full-form requires `test` when interval/timeout/retries are set", prefix)
		}
	case "pg_isready", "http":

		if len(h.Test) > 0 {
			return fmt.Errorf("%s.healthcheck: shorthand `type: %s` cannot be combined with explicit `test`", prefix, h.Type)
		}
	default:
		return fmt.Errorf("%s.healthcheck.type: must be 'pg_isready', 'http', or empty (got %q)", prefix, h.Type)
	}
	return nil
}
