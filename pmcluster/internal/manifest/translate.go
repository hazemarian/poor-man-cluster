package manifest

import (
	"context"
	"fmt"
	"strings"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/refs"
	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

// EnvResolver resolves `config(<name>)` references in service env values
// against the DB-backed config store. Implemented by internal/stacks with a
// *store.Store; nil means config() refs are rejected at translate time.
type EnvResolver interface {
	ResolveConfig(ctx context.Context, name string) (string, error)
}

// Shared external networks ensured by `pmcluster cluster up`; the
// translator references them with `external: true`.
const (
	traefikNet    = "traefik-net"
	monitoringNet = "monitoring-net"
)

// privateNetSuffix forms the per-app inline overlay (e.g.
// "donation-campaign-net").
const privateNetSuffix = "-net"

const (
	labelService     = "service"
	labelApplication = "application"
	labelEnvironment = "environment"
	labelVersion     = "version"

	// labelSkipFilelog is set on Swarm deploy labels when a service
	// opts out of filelog scraping (skip_filelog: true). The OTel
	// collector's filter processor drops these logs.
	labelSkipFilelog = "io.pmcluster.skip_filelog"
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
		Services: make([]IRService, 0, len(app.Services)),
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

	return ir, nil
}

// translateService maps one DSL service to its IR entry, resolving
// config()/secrets() env references.
func translateService(ctx context.Context, app *dsl.App, name string, s *dsl.Service, res EnvResolver) (*IRService, error) {
	env, err := resolveServiceEnv(ctx, s.Env, res)
	if err != nil {
		return nil, fmt.Errorf("services.%s: %w", name, err)
	}

	is := &IRService{
		Name:        name,
		Image:       s.Image,
		Command:     s.Command,
		Entrypoint:  s.Entrypoint,
		Env:         env,
		Volumes:     s.Volumes,
		Secrets:     s.Secrets,
		RunOnce:     s.RunOnce,
		Placement:   s.Placement,
		SkipFilelog: s.SkipFilelog,
	}

	if s.Expose != nil {
		is.Expose = &IRExpose{
			Port:         s.Expose.Port,
			Host:         s.Expose.Host,
			Aliases:      s.Expose.Aliases,
			CORSDisabled: s.Expose.CORSDisabled,
		}
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

// resolveServiceEnv resolves config()/secrets() env references. Returns the
// resolved env map.  It does NOT mount anything: validation guarantees the
// operator listed every secrets(name) used in env in the service's own
// `secrets:` array (so the /run/secrets/<name> path actually exists).
//
//   - config(name): value = config content from the DB (via EnvResolver).
//     Multi-line content is rejected — compose environment values must be
//     single-line; file-style content belongs in configs mounted as files.
//   - secrets(name): value = /run/secrets/<name> (the mounted file path).
//     The secret must already be mounted via the service secrets: array.
func resolveServiceEnv(ctx context.Context, env map[string]string, res EnvResolver) (map[string]string, error) {
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
			content, err := res.ResolveConfig(ctx, ref.Name)
			if err != nil {
				return nil, fmt.Errorf("env.%s: resolve config(%s): %w", k, ref.Name, err)
			}
			if strings.ContainsAny(content, "\r\n") {
				return nil, fmt.Errorf("env.%s: config(%s) contains newlines — configs injected into env must be single-line; use a file config for multi-line content", k, ref.Name)
			}
			out[k] = content
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
		Type:     "",
		Test:     h.Test,
		Interval: h.Interval,
		Timeout:  h.Timeout,
		Retries:  h.Retries,
	}
}
