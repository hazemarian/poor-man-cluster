package manifest

import (
	"context"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/refs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// EnvResolver resolves `config(<name>)` references in service env values
// against the DB-backed config store. Implemented by internal/stacks with a
// *store.Store; nil means config() refs are rejected at translate time.
//
// Resolutions are STACK-SCOPED: the caller passes the app (stack) being
// translated, and implementations must never return a row tagged to a
// different stack — cross-stack configs must not leak into another stack's
// env. Rows with an empty stack are shared and usable by any stack.
type EnvResolver interface {
	ResolveConfig(ctx context.Context, stack, name string) (string, error)
}

// SettingsResolver is implemented by EnvResolvers that can also resolve
// `settings(<name>)` references against the cluster settings store. When an
// env value is exactly `settings(name)`, the translator substitutes the live
// cluster setting value at deploy/update time. Resolvers that don't
// implement this reject settings() refs at translate time.
type SettingsResolver interface {
	ResolveSetting(ctx context.Context, stack, name string) (string, error)
}

// ConfigPathResolver is implemented by EnvResolvers that can resolve a
// config_path(<name>) mount to its in-container target path. When a resolver
// does not implement it (or returns an empty path), the config file mounts at
// the default refs.DefaultConfigMountPath (currently /etc/<name>).
type ConfigPathResolver interface {
	ResolveConfigPath(ctx context.Context, stack, name string) (string, error)
}

// Shared external networks ensured by `pmcluster cluster up`; the
// translator references them with `external: true`.
const (
	traefikNet    = "traefik-net"
	monitoringNet = "monitoring-net"
)

// privateNetName is the compose-internal name of the per-app inline overlay.
// It stays FIXED (not "<app>-net") because `docker stack deploy` prefixes
// every compose network with the stack name — an app-named network would
// render as "<app>_<app>-net" (the app twice) and push long app names over
// Docker's 63-character network-name limit. The deployed network is simply
// "<app>_net", mirroring the "<app>_<service>" service convention.
const privateNetName = "net"

const (
	labelService     = "service"
	labelApplication = "application"
	labelEnvironment = "environment"
	labelVersion     = "version"

	// labelSkipFilelog is set on Swarm deploy labels when a service
	// opts out of filelog scraping (skip_filelog: true). The OTel
	// collector's filter processor drops these logs.
	labelSkipFilelog = "io.pmcluster.skip_filelog"

	// labelNode re-uses runtime.NodeLabel: the hostname the service's
	// placement pin targets (an auto-resolved storage node). Kept here as a
	// local alias so the compose backend can reference it without reaching
	// into runtime everywhere.
	labelNode = runtime.NodeLabel

	// labelPlatform is stamped on every service of a platform-managed stack
	// (app.platform: true) so the UI/CLI/down/--purge can differentiate
	// platform stacks (infra/edge/observability/backup/sso) from user app
	// stacks.
	labelPlatform = "io.pmcluster.platform"
)

// defaultWriter is the backend targeted when no writer is supplied: the
// swarm backend (Docker Compose v3.9). Other writers (Terraform, Helm) are
// future backends plugged in at the call site.
var defaultWriter Writer = ComposeWriter{}

// Translate renders a validated, interpolated *dsl.App into compose v3.9
// YAML (the swarm backend) via the default Writer. CALLER MUST run
// Parse → Interpolate → Validate first; Translate does no validation
// itself. config() env references require a resolver; pass nil to reject
// them at translate time.
func Translate(app *dsl.App) ([]byte, error) {
	return TranslateWithResolver(context.Background(), app, nil)
}

// TranslateWithResolver is Translate with DB-backed config resolution for
// `env: X: config(name)` values.
func TranslateWithResolver(ctx context.Context, app *dsl.App, res EnvResolver) ([]byte, error) {
	return TranslateIR(ctx, app, res, defaultWriter)
}

// TranslateIR builds the neutral IR for a validated *dsl.App and renders it
// through the given Writer. Callers with a non-default backend pass their
// own writer; nil falls back to the compose writer.
func TranslateIR(ctx context.Context, app *dsl.App, res EnvResolver, w Writer) ([]byte, error) {
	ir, err := BuildIR(ctx, app, res)
	if err != nil {
		return nil, err
	}
	if w == nil {
		w = defaultWriter
	}
	return w.Write(ctx, ir)
}

// BuildIR translates a validated, interpolated *dsl.App into the neutral
// intermediate representation consumed by Writers. config() env references
// require a resolver; pass nil to reject them at translate time.
func BuildIR(ctx context.Context, app *dsl.App, res EnvResolver) (*IR, error) {
	ir := &IR{
		Name:     app.Name,
		Env:      app.Env,
		Version:  app.Version,
		Platform: app.Platform,
		Networks: app.Networks,
		Services: make([]IRService, 0, len(app.Services)),
	}

	// App-level volumes are declared verbatim (plain named volumes, never
	// bind-relocated under the volume root). Platform stacks rely on this to
	// keep their data volumes (openobserve_data, traefik_acme, pmui-data)
	// exactly where they are across the L4 pipeline unification.
	if len(app.Volumes) > 0 {
		ir.PlainVolumes = make(map[string]string, len(app.Volumes))
		for name, decl := range app.Volumes {
			ir.PlainVolumes[name] = decl
		}
	}

	for name, svc := range app.Services {
		is, err := translateService(ctx, app, name, svc, res)
		if err != nil {
			return nil, err
		}
		ir.Services = append(ir.Services, *is)
	}

	// Named volumes are auto-collected from the services — the app-level
	// volumes list was removed from the DSL (service volumes are the single
	// source of truth; the writer declares + relocates them).
	namedSet := map[string]struct{}{}
	for _, svc := range app.Services {
		for _, v := range svc.Volumes {
			src := v
			if i := strings.IndexByte(src, ':'); i >= 0 {
				src = v[:i]
			}
			if src != "" && !strings.HasPrefix(src, "/") {
				namedSet[src] = struct{}{}
			}
		}
	}
	for n := range namedSet {
		ir.Volumes = append(ir.Volumes, n)
	}

	secretSet := map[string]struct{}{}
	for _, svc := range app.Services {
		for _, s := range svc.Secrets {
			secretSet[s] = struct{}{}
		}
	}
	for _, s := range app.Secrets {
		secretSet[s] = struct{}{}
	}
	for s := range secretSet {
		ir.Secrets = append(ir.Secrets, s)
	}

	configSet := map[string]struct{}{}
	for _, svc := range app.Services {
		for _, c := range svc.Configs {
			if name, ok := refs.ParseConfigPath(c); ok && name != "" {
				configSet[name] = struct{}{}
			}
		}
	}
	for c := range configSet {
		ir.Configs = append(ir.Configs, c)
	}

	return ir, nil
}

// appendDistinct returns base followed by extras, dropping empties and
// duplicates while preserving first-seen order.
func appendDistinct(base []string, extras ...string) []string {
	seen := make(map[string]bool, len(base)+len(extras))
	out := make([]string, 0, len(base)+len(extras))
	for _, s := range base {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range extras {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// translateService maps one DSL service to its IR entry, resolving
// config()/secrets() env references.
func translateService(ctx context.Context, app *dsl.App, name string, s *dsl.Service, res EnvResolver) (*IRService, error) {
	env, err := resolveServiceEnv(ctx, app.Name, s.Env, res)
	if err != nil {
		return nil, fmt.Errorf("services.%s: %w", name, err)
	}

	is := &IRService{
		Name:          name,
		Image:         s.Image,
		Command:       s.Command,
		Entrypoint:    s.Entrypoint,
		Env:           env,
		Volumes:       s.Volumes,
		Secrets:       s.Secrets,
		RunOnce:       s.RunOnce,
		Placement:     s.Placement,
		SkipFilelog:   s.SkipFilelog,
		DependsOn:     s.DependsOn,
		Mode:          s.Mode,
		Restart:       s.Restart,
		RestartDelay:  s.RestartDelay,
		Constraints:   s.Constraints,
		Binds:         s.Binds,
		ExtraHosts:    s.ExtraHosts,
		User:          s.User,
		Labels:        s.Labels,
		ExtraNetworks: appendDistinct(app.Networks, s.Networks...),
	}

	if len(s.Ports) > 0 {
		for _, p := range s.Ports {
			is.Ports = append(is.Ports, IRPort{
				Target:    p.Target,
				Published: p.Published,
				Mode:      p.Mode,
			})
		}
	}

	if len(s.Configs) > 0 {
		for _, c := range s.Configs {
			name, ok := refs.ParseConfigPath(c)
			if !ok {
				return nil, fmt.Errorf("services.%s.configs: expected config_path(<name>), got %q", name, c)
			}
			target := refs.DefaultConfigMountPath(name)
			if cpr, ok := res.(ConfigPathResolver); ok {
				if p, err := cpr.ResolveConfigPath(ctx, app.Name, name); err == nil && p != "" {
					target = p
				}
			}
			is.Configs = append(is.Configs, IRConfigMount{Source: name, Target: target})
		}
	}

	if s.Resources != nil {
		is.Resources = &IRResources{}
		if s.Resources.Reservations != nil {
			is.Resources.Reservations = &IRResourceSpec{CPUs: s.Resources.Reservations.CPUs, Memory: s.Resources.Reservations.Memory}
		}
		if s.Resources.Limits != nil {
			is.Resources.Limits = &IRResourceSpec{CPUs: s.Resources.Limits.CPUs, Memory: s.Resources.Limits.Memory}
		}
	}

	if s.Expose != nil {
		is.Expose = &IRExpose{
			Port:         s.Expose.Port,
			Host:         s.Expose.Host,
			External:     s.Expose.External,
			Mode:         s.Expose.Mode,
			Aliases:      s.Expose.Aliases,
			CORSDisabled: s.Expose.CORSDisabled,
		}
	}

	if s.Logging != nil {
		lg := &IRLogging{Driver: s.Logging.Driver}
		if len(s.Logging.Options) > 0 {
			lg.Options = make(map[string]string, len(s.Logging.Options))
			for k, v := range s.Logging.Options {
				lg.Options[k] = v
			}
		}
		is.Logging = lg
	}

	is.Healthcheck = translateHealthcheck(s)

	if !s.RunOnce {
		replicas := 1
		if s.Replicas != nil {
			replicas = *s.Replicas
		}
		is.Replicas = replicas

		if s.Update != nil {
			u := &IRUpdate{
				Parallelism: 1,
				Delay:       "10s",
				Order:       "start-first",
			}
			if s.Update.Parallelism != 0 {
				u.Parallelism = s.Update.Parallelism
			}
			if s.Update.Delay != "" {
				u.Delay = s.Update.Delay
			}
			if s.Update.Order != "" {
				u.Order = s.Update.Order
			}
			is.Update = u
		}
	}

	return is, nil
}

// resolveServiceEnv resolves config()/secrets() env references for the given
// stack. Returns the resolved env map.  It does NOT mount anything:
// validation guarantees the operator listed every secrets(name) used in env
// in the service's own `secrets:` array (so the /run/secrets/<name> path
// actually exists).
//
//   - config(name): value = config content from the DB (via EnvResolver).
//     Multi-line content is rejected — compose environment values must be
//     single-line; file-style content belongs in configs mounted as files.
//   - secrets(name): value = /run/secrets/<name> (the mounted file path).
//     The secret must already be mounted via the service secrets: array.
func resolveServiceEnv(ctx context.Context, stack string, env map[string]string, res EnvResolver) (map[string]string, error) {
	if len(env) == 0 {
		return env, nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		ref, ok := refs.ParseEnvRef(v)
		if !ok {
			out[k] = v
			continue
		}
		switch ref.Kind {
		case "secrets":
			if ref.Name == "" {
				return nil, fmt.Errorf("env.%s: secrets() name is empty", k)
			}
			out[k] = refs.SecretMountPath(ref.Name)
		case "config":
			if res == nil {
				return nil, fmt.Errorf("env.%s: config(%s) requires config resolution (not available)", k, ref.Name)
			}
			content, err := res.ResolveConfig(ctx, stack, ref.Name)
			if err != nil {
				return nil, fmt.Errorf("env.%s: resolve config(%s): %w", k, ref.Name, err)
			}
			if strings.ContainsAny(content, "\r\n") {
				return nil, fmt.Errorf("env.%s: config(%s) contains newlines — configs injected into env must be single-line; use a file config for multi-line content", k, ref.Name)
			}
			out[k] = content
		case "settings":
			sr, ok := res.(SettingsResolver)
			if !ok || sr == nil {
				return nil, fmt.Errorf("env.%s: settings(%s) requires settings resolution (not available)", k, ref.Name)
			}
			value, err := sr.ResolveSetting(ctx, stack, ref.Name)
			if err != nil {
				return nil, fmt.Errorf("env.%s: resolve settings(%s): %w", k, ref.Name, err)
			}
			out[k] = value
		default:
			return nil, fmt.Errorf("env.%s: unknown reference kind %q", k, ref.Kind)
		}
	}
	return out, nil
}

// translateHealthcheck produces the IR liveness probe for a DSL healthcheck.
// The backend-specific probe strings (CMD-SHELL, wget) are emitted by the
// Writer from the IR — see ComposeWriter.
func translateHealthcheck(s *dsl.Service) *IRHealthcheck {
	if s.Healthcheck == nil {
		return nil
	}
	h := s.Healthcheck

	switch h.Type {
	case "pg_isready":
		return &IRHealthcheck{
			Type:     "pg_isready",
			Interval: "10s",
			Timeout:  "5s",
			Retries:  5,
		}
	case "http":
		path := h.Path
		if path == "" {
			path = "/"
		}
		return &IRHealthcheck{
			Type:     "http",
			Path:     path,
			Interval: "10s",
			Timeout:  "5s",
			Retries:  5,
		}
	}

	return &IRHealthcheck{
		Type:        "",
		Test:        h.Test,
		Interval:    h.Interval,
		Timeout:     h.Timeout,
		StartPeriod: h.StartPeriod,
		Retries:     h.Retries,
	}
}
