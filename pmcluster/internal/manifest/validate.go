package manifest

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/refs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// nameRe enforces lowercase + digits + dashes/underscores for `app:` and
// service keys. Matches Docker Swarm stack/service naming constraints.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// hostnameRe is a permissive FQDN check. We trust the operator's `domain`
// so we just require: at least one dot, no whitespace, no obvious garbage.
var hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9*]([a-zA-Z0-9.-]*\.[a-zA-Z]{2,})?$`)

// nodeNameRe accepts Docker node hostnames and node IDs: letters, digits,
// dots, dashes, underscores — no whitespace or characters that could break
// out of the rendered placement constraint.
var nodeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// cpusRe matches Docker cpu quantities (decimal strings like "0.25", "1.5").
var cpusRe = regexp.MustCompile(`^\d+(\.\d+)?$`)

// memRe matches Docker memory quantities (e.g. "128M", "512M", "1G", "1024Mi").
// The unit is a letter (k/K/m/M/g/G/t/T) with an OPTIONAL i (binary) prefix
// and an OPTIONAL b/B suffix: 128M, 512m, 1G, 1Gi, 1024Mi, 512MiB all pass.
var memRe = regexp.MustCompile(`^\d+(\.\d+)?[kKmMgGtT]i?[bB]?$`)

// durationRe matches Docker duration strings (e.g. "5s", "10s", "30s", "1m").
// Used by healthcheck timing and restart-policy delay fields.
var durationRe = regexp.MustCompile(`^\d+[a-zA-Z]+$`)

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
	if s.RunOnce && s.Mode == "global" {
		return fmt.Errorf("%s: run_once and mode: global are mutually exclusive", prefix)
	}
	if s.Replicas != nil && s.Mode == "global" {
		return fmt.Errorf("%s: replicas and mode: global are mutually exclusive", prefix)
	}
	if s.Mode != "" && s.Mode != "global" {
		return fmt.Errorf("%s.mode: must be 'global' or empty, got %q", prefix, s.Mode)
	}
	if s.Restart != "" && s.Restart != "any" && s.Restart != "on-failure" && s.Restart != "none" {
		return fmt.Errorf("%s.restart: must be 'any', 'on-failure', 'none', or empty, got %q", prefix, s.Restart)
	}
	if s.RestartDelay != "" && !durationRe.MatchString(s.RestartDelay) {
		return fmt.Errorf("%s.restart_delay: invalid duration %q (e.g. 5s)", prefix, s.RestartDelay)
	}
	for i, c := range s.Constraints {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("%s.constraints[%d]: empty constraint", prefix, i)
		}
		if strings.ContainsAny(c, "\r\n") {
			return fmt.Errorf("%s.constraints[%d]: constraint must be single-line", prefix, i)
		}
	}
	for i, p := range s.Ports {
		if p.Target < 1 || p.Target > 65535 {
			return fmt.Errorf("%s.ports[%d].target: must be 1..65535, got %d", prefix, i, p.Target)
		}
		if p.Published != 0 && (p.Published < 1 || p.Published > 65535) {
			return fmt.Errorf("%s.ports[%d].published: must be 1..65535 or 0 (defaults to target), got %d", prefix, i, p.Published)
		}
		if p.Protocol != "" && p.Protocol != "tcp" && p.Protocol != "udp" {
			return fmt.Errorf("%s.ports[%d].protocol: must be 'tcp' or 'udp', got %q", prefix, i, p.Protocol)
		}
		if p.Mode != "" && p.Mode != "ingress" && p.Mode != "host" {
			return fmt.Errorf("%s.ports[%d].mode: must be 'ingress' or 'host', got %q", prefix, i, p.Mode)
		}
	}
	for i, c := range s.Configs {
		if strings.TrimSpace(c.Source) == "" {
			return fmt.Errorf("%s.configs[%d].source: required", prefix, i)
		}
		if strings.TrimSpace(c.Target) == "" {
			return fmt.Errorf("%s.configs[%d].target: required", prefix, i)
		}
	}
	for i, b := range s.Binds {
		if !strings.Contains(b, ":") {
			return fmt.Errorf("%s.binds[%d]: must be '/host:/container[:ro]', got %q", prefix, i, b)
		}
	}
	for i, eh := range s.ExtraHosts {
		if !strings.Contains(eh, ":") {
			return fmt.Errorf("%s.extra_hosts[%d]: must be 'host:ip', got %q", prefix, i, eh)
		}
	}
	if s.Resources != nil {
		if s.Resources.Reservations != nil {
			if s.Resources.Reservations.CPUs != "" && !cpusRe.MatchString(s.Resources.Reservations.CPUs) {
				return fmt.Errorf("%s.resources.reservations.cpus: invalid quantity %q", prefix, s.Resources.Reservations.CPUs)
			}
			if s.Resources.Reservations.Memory != "" && !memRe.MatchString(s.Resources.Reservations.Memory) {
				return fmt.Errorf("%s.resources.reservations.memory: invalid quantity %q", prefix, s.Resources.Reservations.Memory)
			}
		}
		if s.Resources.Limits != nil {
			if s.Resources.Limits.CPUs != "" && !cpusRe.MatchString(s.Resources.Limits.CPUs) {
				return fmt.Errorf("%s.resources.limits.cpus: invalid quantity %q", prefix, s.Resources.Limits.CPUs)
			}
			if s.Resources.Limits.Memory != "" && !memRe.MatchString(s.Resources.Limits.Memory) {
				return fmt.Errorf("%s.resources.limits.memory: invalid quantity %q", prefix, s.Resources.Limits.Memory)
			}
		}
	}
	if s.Logging != nil {
		if strings.TrimSpace(s.Logging.Driver) == "" {
			return fmt.Errorf("%s.logging.driver: required", prefix)
		}
	}
	if s.Placement != "" && s.Placement != "manager" && s.Placement != "worker" {
		// Any other value is a node-hostname pin (node.hostname == <value>).
		// Keep it a sane Docker hostname: letters, digits, dots and hyphens,
		// no spaces or slash — the constraint value lands verbatim in the
		// rendered Compose so it must not be able to smuggle YAML.
		if !nodeNameRe.MatchString(s.Placement) {
			return fmt.Errorf("%s.placement: must be 'manager', 'worker', a node hostname, or empty (got %q)", prefix, s.Placement)
		}
	}
	for i, v := range s.Volumes {
		if !strings.Contains(v, ":") {
			return fmt.Errorf("%s.volumes[%d]: must be 'name:path' or '/host:/container', got %q", prefix, i, v)
		}
	}
	for k, v := range s.Env {
		if refs.MalformedEnvRef(v) {
			return fmt.Errorf("%s.env.%s: malformed reference %q — expected config(name), secrets(name) or settings(name)", prefix, k, v)
		}
		if ref, ok := refs.ParseEnvRef(v); ok && ref.Name == "" {
			return fmt.Errorf("%s.env.%s: empty name in %s() reference", prefix, k, ref.Kind)
		}
		if ref, ok := refs.ParseEnvRef(v); ok && ref.Kind == "secrets" {
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
	if h.StartPeriod != "" && !durationRe.MatchString(h.StartPeriod) {
		return fmt.Errorf("%s.healthcheck.start_period: invalid duration %q (e.g. 10s)", prefix, h.StartPeriod)
	}
	return nil
}
