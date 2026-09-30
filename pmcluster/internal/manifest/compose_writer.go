package manifest

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"sigs.k8s.io/yaml"
)

// ComposeWriter renders an IR into Docker Compose v3.9 YAML — the swarm
// backend's artifact. This is the ONLY writer today; Terraform/Helm writers
// can be added against the same IR without touching the translator.
//
// VolumeRoot is the single host directory every container volume is forced
// under: named volumes are declared with a bind driver_opts pointing at
// <root>/<app>/<name> (so Docker's first-use ownership copy still runs for
// DB images), and bind mounts are relocated to <root>/<app>/<basename>.
// Empty means the default /var/stack/data.
type ComposeWriter struct {
	VolumeRoot string
	// CertResolver names the ACME resolver to attach to every exposed Traefik
	// router (and its aliases) as traefik.http.routers.<scope>.tls.certresolver.
	// Empty means no certresolver label is emitted — required for BYO-cert
	// clusters that never define a letsencrypt resolver. Traefik's
	// entrypoint-default certresolver does NOT cascade to swarm-label routers,
	// so ACME clusters MUST set this to their resolver name (e.g.
	// "letsencrypt") or no certificate is ever issued for app domains.
	CertResolver string
	// PinNode names the default node hostname that stateful services (any
	// service with a volume mount) are pinned to when the manifest leaves
	// placement empty. Set from the platform_node cluster setting. An empty
	// value disables the auto-pin (services with volumes then schedule
	// anywhere, exactly as today).
	PinNode string
}

// DefaultVolumeRoot is where every container volume lands unless the
// operator overrides the volume_root setting.
const DefaultVolumeRoot = "/var/stack/data"

// Write implements Writer.
func (w ComposeWriter) Write(ctx context.Context, ir *IR) ([]byte, error) {
	root := w.VolumeRoot
	if root == "" {
		root = DefaultVolumeRoot
	}
	cf := &composeFile{
		Version:  "3.9",
		Services: map[string]*composeService{},
	}

	privateNet := privateNetName

	usesTraefikNet := false
	usesMonitoringNet := false

	app := irApp{name: ir.Name, env: ir.Env, version: ir.Version}

	for i := range ir.Services {
		s := &ir.Services[i]
		cs, err := composeServiceFromIR(s, app, privateNet, root, w.CertResolver, w.PinNode, &usesTraefikNet, &usesMonitoringNet)
		if err != nil {
			return nil, err
		}
		cf.Services[s.Name] = cs
	}

	if len(ir.Volumes) > 0 {
		cf.Volumes = map[string]*composeVolume{}
		for _, v := range ir.Volumes {
			cf.Volumes[v] = &composeVolume{
				Driver: "local",
				DriverOpts: map[string]string{
					"type":   "none",
					"o":      "bind",
					"device": root + "/" + ir.Name + "/" + v,
				},
			}
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

	if len(ir.Secrets) > 0 {
		cf.Secrets = map[string]*composeSecret{}
		for _, s := range ir.Secrets {
			cf.Secrets[s] = &composeSecret{External: true}
		}
	}

	out, err := yaml.Marshal(cf)
	if err != nil {
		return nil, fmt.Errorf("marshal compose: %w", err)
	}
	return out, nil
}

// composeServiceFromIR maps one IR service to its compose block.
func composeServiceFromIR(
	s *IRService,
	app irApp,
	privateNet, volumeRoot, certResolver, pinNode string,
	usesTraefikNet, usesMonitoringNet *bool,
) (*composeService, error) {
	volumes := relocateVolumes(volumeRoot, app.name, s.Volumes)
	cs := &composeService{
		Image:       s.Image,
		Command:     s.Command,
		Entrypoint:  s.Entrypoint,
		Environment: s.Env,
		Volumes:     volumes,
		Secrets:     s.Secrets,
	}

	cs.Networks = []string{privateNet}
	if s.Expose != nil {
		cs.Networks = append(cs.Networks, traefikNet, monitoringNet)
		*usesTraefikNet = true
		*usesMonitoringNet = true
	}

	cs.Healthcheck = composeHealthcheckFromIR(s)
	cs.Deploy = composeDeployFromIR(app, s, certResolver, pinNode, len(volumes) > 0)

	// depends_on is emitted for compose parity ONLY. docker stack deploy
	// parses and ignores it — the Swarm scheduler has no dependency graph.
	// Startup ordering is enforced by the CONTROL PLANE, not by a rendered
	// artifact: the deploy pipeline topologically sorts the stack's services
	// into depends_on levels and deploys + waits for each level before the
	// next (see internal/manifest.ServiceLevels and the stacks deploy path).
	if len(s.DependsOn) > 0 {
		cs.DependsOn = s.DependsOn
	}

	return cs, nil
}

// relocateVolumes forces every container volume under the single volume
// root. A source starting with '/' is a host bind mount and is relocated to
// <root>/<app>/<basename>; anything else is a named volume (declared by the
// writer with a bind driver_opts) and is left untouched in the service —
// the mount source is resolved by the volume declaration.
func relocateVolumes(root, app string, volumes []string) []string {
	out := make([]string, len(volumes))
	for i, v := range volumes {
		src, rest, ok := strings.Cut(v, ":")
		if ok && strings.HasPrefix(src, "/") {
			out[i] = root + "/" + app + "/" + path.Base(src) + ":" + rest
			continue
		}
		out[i] = v
	}
	return out
}

// irApp is the minimal app identity the compose writer stamps onto services.
// It is populated by TranslateWithResolver before writing.
type irApp struct {
	name    string
	env     string
	version string
}

func composeHealthcheckFromIR(s *IRService) *composeHealthcheck {
	if s.Healthcheck == nil {
		return nil
	}
	h := s.Healthcheck

	switch h.Type {
	case "pg_isready":
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

func composeDeployFromIR(app irApp, s *IRService, certResolver, pinNode string, stateful bool) *composeDeploy {
	d := &composeDeploy{
		Labels: standardLabels(app, s.Name),
	}

	switch {
	case s.RunOnce && len(s.DependsOn) > 0:
		// A one-shot job waiting on dependencies: the wait loop blocks until
		// the dependency DNS resolves, but the job itself may still fail
		// transiently (dependency accepted connections then died, a bad
		// migration is retried before the operator intervenes). Bound the
		// retries so the job eventually stops instead of churning forever.
		d.RestartPolicy = &composeRestartPolicy{Condition: "on-failure", MaxAttempts: 3}
	case s.RunOnce:
		d.RestartPolicy = &composeRestartPolicy{Condition: "none"}
	default:
		replicas := s.Replicas
		if replicas == 0 {
			replicas = 1
		}
		d.Replicas = &replicas
		d.RestartPolicy = &composeRestartPolicy{Condition: "on-failure"}
	}

	switch s.Placement {
	case "manager":
		d.Placement = &composePlacement{Constraints: []string{"node.role == manager"}}
	case "worker":
		d.Placement = &composePlacement{Constraints: []string{"node.role == worker"}}
	case "":
		// A stateful service (one holding a volume) with no explicit
		// placement is auto-pinned to the cluster's designated platform node
		// when one is configured. Its data lives under /var/stack/data and
		// only exists on that node — letting Swarm schedule it anywhere would
		// either strand the data or start a fresh empty volume.
		if stateful && pinNode != "" {
			d.Placement = &composePlacement{Constraints: []string{"node.hostname == " + pinNode}}
		}
	default:
		// Any other value is a node-hostname pin: the operator keeps a
		// stateful service (with a volume) on one specific node so its
		// data never has to migrate.
		d.Placement = &composePlacement{Constraints: []string{"node.hostname == " + s.Placement}}
	}

	if !s.RunOnce {
		d.UpdateConfig = translateUpdate(s.Update)
		// A stateful service should never be updated start-first: the old
		// task's shutdown races the new task's startup on the same data dir
		// (postgres deletes the freshly written postmaster.pid and the new
		// instance immediately shuts down — every redeploy crashes the DB).
		// Stop the old task fully before starting the replacement.
		if stateful && d.UpdateConfig.Order == "start-first" {
			d.UpdateConfig.Order = "stop-first"
		}
	}

	if s.Expose != nil {
		addTraefikLabels(d.Labels, app, s.Name, s.Expose, certResolver)
	}

	if s.SkipFilelog {
		d.Labels[labelSkipFilelog] = "true"
	}

	return d
}

func standardLabels(app irApp, serviceName string) map[string]string {
	return map[string]string{
		labelService:     serviceName,
		labelApplication: app.name,
		labelEnvironment: app.env,
		labelVersion:     app.version,
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
func addTraefikLabels(labels map[string]string, app irApp, serviceName string, exp *IRExpose, certResolver string) {
	scope := app.name + "-" + serviceName
	labels["traefik.enable"] = "true"
	labels["traefik.http.services."+scope+".loadbalancer.server.port"] = fmt.Sprintf("%d", exp.Port)
	labels["traefik.docker.network"] = traefikNet

	middleware := "cors-default@file"
	aliasCORS := len(exp.Aliases) > 0 && !exp.CORSDisabled
	if aliasCORS {
		corsName := scope + "-cors"

		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolalloworiginlistregex"] = escapeCompose(buildOriginRegex(exp))
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolallowcredentials"] = "true"
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolallowmethods"] = "GET,POST,PUT,PATCH,DELETE,OPTIONS"
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolallowheaders"] = "Content-Type,Authorization,X-Pmcluster-Signature,X-Request-Id"
		labels["traefik.http.middlewares."+corsName+".headers.accesscontrolmaxage"] = "600"
		labels["traefik.http.middlewares."+corsName+".headers.addvaryheader"] = "true"
		middleware = corsName + "@swarm"
	}

	labels["traefik.http.routers."+scope+".rule"] = "Host(`" + exp.Host + "`)"
	labels["traefik.http.routers."+scope+".entrypoints"] = "websecure"
	labels["traefik.http.routers."+scope+".tls"] = "true"
	// Explicit certresolver, ONLY for ACME clusters: the platform stacks carry
	// this label and the entrypoint-default resolver does not reliably cascade
	// to swarm-label routers — without it ACME never issues a certificate for
	// the app's domain and Traefik serves its self-signed default cert. But a
	// BYO-cert cluster (no letsencrypt resolver exists) must NOT reference it.
	if certResolver != "" {
		labels["traefik.http.routers."+scope+".tls.certresolver"] = certResolver
	}
	if !exp.CORSDisabled {
		labels["traefik.http.routers."+scope+".middlewares"] = middleware
	}

	for i, alias := range exp.Aliases {
		aliasScope := fmt.Sprintf("%s-alias%d", scope, i)
		labels["traefik.http.routers."+aliasScope+".rule"] = "Host(`" + alias + "`)"
		labels["traefik.http.routers."+aliasScope+".entrypoints"] = "websecure"
		labels["traefik.http.routers."+aliasScope+".tls"] = "true"
		if certResolver != "" {
			labels["traefik.http.routers."+aliasScope+".tls.certresolver"] = certResolver
		}
		if !exp.CORSDisabled {
			labels["traefik.http.routers."+aliasScope+".middlewares"] = middleware
		}
	}
}

// escapeCompose doubles every literal `$` so the value survives docker stack
// deploy's interpolation pass unchanged (e.g. a trailing CORS regex `$`).
func escapeCompose(s string) string {
	return strings.ReplaceAll(s, "$", "$$")
}

// buildOriginRegex produces the CORS Access-Control-Allow-Origin regex for
// the primary host plus all aliases. Reverse-domains must be escaped for the
// Traefik regexp matcher (dashes are already safe in a char class-free
// alternation); we escape dots and treat each host as an exact origin match
// for https://<host>.
func buildOriginRegex(exp *IRExpose) string {
	hosts := append([]string{exp.Host}, exp.Aliases...)
	escaped := make([]string, 0, len(hosts))
	for _, h := range hosts {
		escaped = append(escaped, `https://`+strings.ReplaceAll(h, ".", `\.`))
	}
	return `^(` + strings.Join(escaped, `|`) + `)$`
}

func translateUpdate(u *IRUpdate) *composeUpdateConfig {
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

// volumeDir is the subset of the compose volumes block the storage-dir
// enforcer needs: each named volume's bind target on the host.
type volumeDir struct {
	DriverOpts map[string]string `json:"driver_opts,omitempty"`
}

// EnsureVolumeDirs creates the host directories every named volume in a
// rendered compose binds to. The volume root design declares each named
// volume with driver_opts {type:none, o:bind, device:<root>/<app>/<name>};
// Docker refuses to start a task when the bind source does not exist
// ('failed to populate volume: no such file or directory' → update paused).
// cluster up / join create the roots, but per-app subdirs only exist after
// the first deploy — so the deploy pipeline calls this before DeployStack.
func EnsureVolumeDirs(composeYAML []byte, mkdirAll func(string, os.FileMode) error) error {
	var doc struct {
		Volumes map[string]volumeDir `json:"volumes"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return fmt.Errorf("parse rendered compose volumes: %w", err)
	}
	for name, v := range doc.Volumes {
		device := v.DriverOpts["device"]
		if device == "" {
			continue
		}
		if err := mkdirAll(device, 0o755); err != nil {
			return fmt.Errorf("ensure volume dir for %q (%s): %w", name, device, err)
		}
	}
	return nil
}
