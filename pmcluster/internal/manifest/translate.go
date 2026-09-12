package manifest

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/pkg/dsl"
)

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

// Translate renders a validated, interpolated *dsl.App into compose v3.9
// YAML. CALLER MUST run Parse → Interpolate → Validate first; Translate
// does no validation itself.
func Translate(app *dsl.App) ([]byte, error) {
	cf := &composeFile{
		Version:  "3.9",
		Services: map[string]*composeService{},
	}

	privateNet := app.Name + privateNetSuffix

	// Only emit shared networks at the top level if at least one service
	// references them.
	usesTraefikNet := false
	usesMonitoringNet := false

	for name, svc := range app.Services {
		cs := translateService(app, name, svc, privateNet, &usesTraefikNet, &usesMonitoringNet)
		cf.Services[name] = cs
	}

	if len(app.Volumes) > 0 {
		cf.Volumes = map[string]*composeVolume{}
		for _, v := range app.Volumes {
			cf.Volumes[v] = &composeVolume{Driver: "local"}
		}
	}

	cf.Networks = map[string]*composeNetwork{
		privateNet: {Driver: "overlay"},
	}
	if usesTraefikNet {
		cf.Networks[traefikNet] = &composeNetwork{External: true}
	}
	if usesMonitoringNet {
		cf.Networks[monitoringNet] = &composeNetwork{External: true}
	}

	// Collect every secret referenced by any service AND by the top-level
	// secrets: key.  This way secrets behave like volumes — referenced in a
	// service → auto-declared at the top compose level with external: true.
	secretSet := map[string]struct{}{}
	for _, svc := range app.Services {
		for _, s := range svc.Secrets {
			secretSet[s] = struct{}{}
		}
	}
	for _, s := range app.Secrets {
		secretSet[s] = struct{}{}
	}
	if len(secretSet) > 0 {
		cf.Secrets = map[string]*composeSecret{}
		for s := range secretSet {
			cf.Secrets[s] = &composeSecret{External: true}
		}
	}

	out, err := yaml.Marshal(cf)
	if err != nil {
		return nil, fmt.Errorf("marshal compose: %w", err)
	}
	return out, nil
}

// translateService updates the uses*Net pointers so the top-level
// networks block matches what services actually reference.
func translateService(
	app *dsl.App,
	name string,
	s *dsl.Service,
	privateNet string,
	usesTraefikNet, usesMonitoringNet *bool,
) *composeService {
	cs := &composeService{
		Image:       s.Image,
		Command:     s.Command,
		Entrypoint:  s.Entrypoint,
		Environment: s.Env,
		Volumes:     s.Volumes,
		Secrets:     s.Secrets,
	}

	// Exposed services join traefik-net (so Traefik can reach them) and
	// monitoring-net (so OTel can scrape them) on top of the private overlay.
	cs.Networks = []string{privateNet}
	if s.Expose != nil {
		cs.Networks = append(cs.Networks, traefikNet, monitoringNet)
		*usesTraefikNet = true
		*usesMonitoringNet = true
	}

	cs.Healthcheck = translateHealthcheck(s)

	cs.Deploy = translateDeploy(app, name, s)

	return cs
}

func translateHealthcheck(s *dsl.Service) *composeHealthcheck {
	if s.Healthcheck == nil {
		return nil
	}
	h := s.Healthcheck

	switch h.Type {
	case "pg_isready":
		// $$ escapes Compose's deploy-time ${VAR} interpolation so
		// POSTGRES_USER/POSTGRES_DB resolve at runtime inside the container.
		return &composeHealthcheck{
			Test:     []string{"CMD-SHELL", "pg_isready -U $$POSTGRES_USER -d $$POSTGRES_DB"},
			Interval: "10s",
			Timeout:  "5s",
			Retries:  5,
		}
	case "http":
		port := 0
		if s.Expose != nil {
			port = s.Expose.Port
		}
		path := h.Path
		if path == "" {
			path = "/"
		}
		// Use 127.0.0.1 (not "localhost"): inside containers localhost can
		// resolve to IPv6 ::1 while apps commonly bind IPv4 0.0.0.0 only, so a
		// localhost probe gets "connection refused" and the task is flagged
		// unhealthy and restart-looped by Swarm even though the app is fine.
		test := fmt.Sprintf("wget -q --spider http://127.0.0.1:%d%s", port, path)
		return &composeHealthcheck{
			Test:     []string{"CMD-SHELL", test},
			Interval: "10s",
			Timeout:  "5s",
			Retries:  5,
		}
	}

	return &composeHealthcheck{
		Test:     h.Test,
		Interval: h.Interval,
		Timeout:  h.Timeout,
		Retries:  h.Retries,
	}
}

func translateDeploy(app *dsl.App, name string, s *dsl.Service) *composeDeploy {
	d := &composeDeploy{
		Labels: standardLabels(app, name),
	}

	switch {
	case s.RunOnce:
		d.RestartPolicy = &composeRestartPolicy{Condition: "none"}
	default:
		replicas := 1
		if s.Replicas != nil {
			replicas = *s.Replicas
		}
		d.Replicas = &replicas
		d.RestartPolicy = &composeRestartPolicy{Condition: "on-failure"}
	}

	switch s.Placement {
	case "manager":
		d.Placement = &composePlacement{Constraints: []string{"node.role == manager"}}
	case "worker":
		d.Placement = &composePlacement{Constraints: []string{"node.role == worker"}}
	}

	// Defaults apply even when Update is nil so re-deploys roll cleanly;
	// run-once jobs have no update lifecycle.
	if !s.RunOnce {
		d.UpdateConfig = translateUpdate(s.Update)
	}

	if s.Expose != nil {
		addTraefikLabels(d.Labels, app, name, s.Expose)
	}

	if s.SkipFilelog {
		d.Labels[labelSkipFilelog] = "true"
	}

	return d
}

func standardLabels(app *dsl.App, serviceName string) map[string]string {
	return map[string]string{
		labelService:     serviceName,
		labelApplication: app.Name,
		labelEnvironment: app.Env,
		labelVersion:     app.Version,
	}
}

// addTraefikLabels scopes router/service names as <app>-<service> to
// avoid collisions across apps.
//
// The cluster-wide cors-default middleware (defined in the Traefik
// dynamic file provider by pmcluster cluster up) is attached by
// default. Services that own CORS themselves opt out via
// expose.cors_disabled: true.
//
// When the service declares expose.aliases (extra hostnames routing to the
// same backend), we emit one Traefik router per alias — all sharing the same
// backend service — so a customer's own domain can serve the app alongside
// the canonical ${domain} host. Because the aliases live on foreign domains
// the cluster-wide cors-default regex doesn't cover, aliases force a per-app
// CORS middleware whose origin regex spans the primary host AND every alias.
func addTraefikLabels(labels map[string]string, app *dsl.App, serviceName string, exp *dsl.Expose) {
	scope := app.Name + "-" + serviceName
	labels["traefik.enable"] = "true"
	labels["traefik.http.services."+scope+".loadbalancer.server.port"] = fmt.Sprintf("%d", exp.Port)
	labels["traefik.docker.network"] = traefikNet

	// Determine the middleware attached to every router: the per-app one when
	// there are aliases (foreign origins) and CORS isn't disabled, else the
	// cluster-wide cors-default.
	middleware := "cors-default@file"
	aliasCORS := len(exp.Aliases) > 0 && !exp.CORSDisabled
	if aliasCORS {
		corsName := scope + "-cors"
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolalloworiginlistregex"] = buildOriginRegex(exp)
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolallowcredentials"] = "true"
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolallowmethods"] = "GET,POST,PUT,PATCH,DELETE,OPTIONS"
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolallowheaders"] = "Content-Type,Authorization,X-Pmcluster-Signature,X-Request-Id"
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolmaxage"] = "600"
		labels["traefik.http.middlewares."+corsName+".headers.addvaryheader"] = "true"
		middleware = corsName + "@docker"
	}

	// Primary host router.
	labels["traefik.http.routers."+scope+".rule"] = "Host(`" + exp.Host + "`)"
	labels["traefik.http.routers."+scope+".entrypoints"] = "websecure"
	labels["traefik.http.routers."+scope+".tls"] = "true"
	if !exp.CORSDisabled {
		labels["traefik.http.routers."+scope+".middlewares"] = middleware
	}

	// One router per alias, sharing the same backend service. Names are
	// suffixed "-alias<i>" so they never collide with the primary router.
	for i, alias := range exp.Aliases {
		aliasScope := fmt.Sprintf("%s-alias%d", scope, i)
		labels["traefik.http.routers."+aliasScope+".rule"] = "Host(`" + alias + "`)"
		labels["traefik.http.routers."+aliasScope+".entrypoints"] = "websecure"
		labels["traefik.http.routers."+aliasScope+".tls"] = "true"
		if !exp.CORSDisabled {
			labels["traefik.http.routers."+aliasScope+".middlewares"] = middleware
		}
	}
}

// buildOriginRegex produces the CORS Access-Control-Allow-Origin regex for
// the primary host plus all aliases. Reverse-domains must be escaped for the
// Traefik regexp matcher (dashes are already safe in a char class-free
// alternation); we escape dots and treat each host as an exact origin match
// for https://<host>.
func buildOriginRegex(exp *dsl.Expose) string {
	hosts := append([]string{exp.Host}, exp.Aliases...)
	escaped := make([]string, 0, len(hosts))
	for _, h := range hosts {
		escaped = append(escaped, `https://`+strings.ReplaceAll(h, ".", `\.`))
	}
	return `^(` + strings.Join(escaped, `|`) + `)$`
}

func translateUpdate(u *dsl.Update) *composeUpdateConfig {
	out := &composeUpdateConfig{
		Parallelism: 1,
		Delay:       "10s",
		Order:       "start-first",
	}
	if u != nil {
		if u.Parallelism != 0 {
			out.Parallelism = u.Parallelism
		}
		if u.Delay != "" {
			out.Delay = u.Delay
		}
		if u.Order != "" {
			out.Order = u.Order
		}
	}
	return out
}
