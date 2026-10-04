package manifest

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

var envVarRe = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// Interpolate mutates app in place. Placeholders:
//
//	${app}       → app.Name
//	${env}       → app.Env
//	${version}   → app.Version  (defaults to "latest")
//	${registry}  → app.Registry
//	${domain}    → app.Domain
//	${env:VAR}   → os.Getenv("VAR") — error if unset
func Interpolate(app *dsl.App) error {
	if app.Version == "" {
		app.Version = "latest"
	}
	builtins := map[string]string{
		"${app}":      app.Name,
		"${env}":      app.Env,
		"${version}":  app.Version,
		"${registry}": app.Registry,
		"${domain}":   app.Domain,
	}
	subst := func(s string) (string, error) { return substitute(s, builtins) }

	for _, p := range []*string{&app.Domain, &app.Registry, &app.Version, &app.RepoURL, &app.EnvFile} {
		v, err := subst(*p)
		if err != nil {
			return err
		}
		*p = v
	}
	for i := range app.Secrets {
		v, err := subst(app.Secrets[i])
		if err != nil {
			return err
		}
		app.Secrets[i] = v
	}

	for name, svc := range app.Services {
		if err := interpolateService(name, svc, subst); err != nil {
			return err
		}
	}
	return nil
}

func interpolateService(name string, s *dsl.Service, subst func(string) (string, error)) error {
	wrap := func(err error, field string) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("services.%s.%s: %w", name, field, err)
	}

	v, err := subst(s.Image)
	if err != nil {
		return wrap(err, "image")
	}
	s.Image = v

	for i := range s.Command {
		v, err := subst(s.Command[i])
		if err != nil {
			return wrap(err, fmt.Sprintf("command[%d]", i))
		}
		s.Command[i] = v
	}
	for i := range s.Entrypoint {
		v, err := subst(s.Entrypoint[i])
		if err != nil {
			return wrap(err, fmt.Sprintf("entrypoint[%d]", i))
		}
		s.Entrypoint[i] = v
	}
	for k, val := range s.Env {
		v, err := subst(val)
		if err != nil {
			return wrap(err, "env."+k)
		}
		s.Env[k] = v
	}
	for i := range s.Volumes {
		v, err := subst(s.Volumes[i])
		if err != nil {
			return wrap(err, fmt.Sprintf("volumes[%d]", i))
		}
		s.Volumes[i] = v
	}
	for i := range s.Binds {
		v, err := subst(s.Binds[i])
		if err != nil {
			return wrap(err, fmt.Sprintf("binds[%d]", i))
		}
		s.Binds[i] = v
	}
	for i := range s.ExtraHosts {
		v, err := subst(s.ExtraHosts[i])
		if err != nil {
			return wrap(err, fmt.Sprintf("extra_hosts[%d]", i))
		}
		s.ExtraHosts[i] = v
	}
	for i := range s.Configs {
		v, err := subst(s.Configs[i].Source)
		if err != nil {
			return wrap(err, fmt.Sprintf("configs[%d].source", i))
		}
		s.Configs[i].Source = v
		v, err = subst(s.Configs[i].Target)
		if err != nil {
			return wrap(err, fmt.Sprintf("configs[%d].target", i))
		}
		s.Configs[i].Target = v
	}
	for k, val := range s.Labels {
		v, err := subst(val)
		if err != nil {
			return wrap(err, "labels."+k)
		}
		s.Labels[k] = v
	}
	if s.Logging != nil {
		if v, err := subst(s.Logging.Driver); err == nil {
			s.Logging.Driver = v
		} else {
			return wrap(err, "logging.driver")
		}
		for k, val := range s.Logging.Options {
			v, err := subst(val)
			if err != nil {
				return wrap(err, "logging.options."+k)
			}
			s.Logging.Options[k] = v
		}
	}
	if s.User != "" {
		v, err := subst(s.User)
		if err != nil {
			return wrap(err, "user")
		}
		s.User = v
	}
	if s.RestartDelay != "" {
		v, err := subst(s.RestartDelay)
		if err != nil {
			return wrap(err, "restart_delay")
		}
		s.RestartDelay = v
	}
	if s.Healthcheck != nil && s.Healthcheck.StartPeriod != "" {
		v, err := subst(s.Healthcheck.StartPeriod)
		if err != nil {
			return wrap(err, "healthcheck.start_period")
		}
		s.Healthcheck.StartPeriod = v
	}
	if s.Expose != nil {
		v, err := subst(s.Expose.Host)
		if err != nil {
			return wrap(err, "expose.host")
		}
		s.Expose.Host = v
		for i := range s.Expose.Aliases {
			v, err := subst(s.Expose.Aliases[i])
			if err != nil {
				return wrap(err, fmt.Sprintf("expose.aliases[%d]", i))
			}
			s.Expose.Aliases[i] = v
		}
	}
	return nil
}

func substitute(s string, builtins map[string]string) (string, error) {
	for placeholder, value := range builtins {
		s = strings.ReplaceAll(s, placeholder, value)
	}
	var unresolved []string
	out := envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[len("${env:") : len(match)-1]
		v, ok := os.LookupEnv(varName)
		if !ok {
			unresolved = append(unresolved, varName)
			return match
		}
		return v
	})
	if len(unresolved) > 0 {
		return "", fmt.Errorf("unresolved env var(s): %s", strings.Join(unresolved, ", "))
	}
	return out, nil
}
