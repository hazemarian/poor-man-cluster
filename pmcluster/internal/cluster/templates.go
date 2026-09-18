package cluster

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// embeddedStacks holds the source-of-truth bundled compose and config files.
//
//go:embed embeds
var embeddedStacks embed.FS

const embeddedDir = "embeds"

// ConfigFileNames lists the bundled configs shipped on init to
// ~/.pmcluster/config/ so operators can customise them.
var ConfigFileNames = []string{
	"infra-stack.yml",
	"observability-stack.yml",
	"backup-stack.yml",
	"edge-stack.yml",
	"otel-collector-config.yml",
	"traefik-dynamic.yml",
}

type stackName string

const (
	StackInfra         stackName = "infra"
	StackObservability stackName = "observability"
	StackBackup        stackName = "backup"
	StackEdge          stackName = "edge"
)

var composeFile = map[stackName]string{
	StackInfra:         "infra-stack.yml",
	StackObservability: "observability-stack.yml",
	StackBackup:        "backup-stack.yml",
	StackEdge:          "edge-stack.yml",
}

// EdgeImageBase is the image registry/repo prefix for the pmcluster-edge
// container. The edge image is published alongside pmcluster (the release
// pipeline builds + pushes it for every tag), so the edge stack pins the
// matching release tag by default.
const EdgeImageBase = "ghcr.io/nextrum-sy/pmcluster-edge"

// EdgeImageEnv overrides the edge image reference in the rendered edge stack.
// It's a way to pin a specific release (e.g. PMCLUSTER_EDGE_IMAGE=v0.2.21, or a
// full custom ref like registry.example.com/pmcluster-edge:edge-1.2). When
// unset, the stack uses EdgeImageBase:<buildinfo.Version>.
const EdgeImageEnv = "PMCLUSTER_EDGE_IMAGE"

// EdgeImageFor returns the pmcluster-edge image the edge stack should deploy:
// EdgeImageBase:<release version> by default (dev builds fall back to
// EdgeImageBase:latest), or the value of EdgeImageEnv when set. A bare value
// is treated as a tag suffix; a value containing a "/" is used as a
// fully-qualified image reference.
//
// Pinning the release tag (instead of a mutable :latest) keeps the edge image
// in lock-step with the pmcluster binary that rendered the stack: a binary
// upgrade renders a new edge stack with the new version tag, so `cluster
// update` re-deploys the edge even on nodes whose local :latest cache is
// stale.
func EdgeImageFor() string {
	if v := strings.TrimSpace(os.Getenv(EdgeImageEnv)); v != "" {
		if strings.Contains(v, "/") {
			return v
		}
		return EdgeImageBase + ":" + v
	}
	if tag := buildinfo.Version; tag != "" && tag != "dev" {
		return EdgeImageBase + ":" + tag
	}
	return EdgeImageBase + ":latest"
}

type RenderInput struct {
	Domain                   string
	OpenObserveAdminEmail    string
	OpenObserveAdminPassword string

	// OpenObserveOrg is the OpenObserve organization segment used in the
	// collector's ingestion basic-auth header (Basic base64(<org>:<token>)).
	// Defaults to "default".
	OpenObserveOrg string

	// OpenObserveIngestionToken is the dedicated OpenObserve ingestion token
	// (created via the OO API, prefix o2oi_) the OTel collector uses to
	// authenticate ingestion (openobserve:5081). It is separate from the human
	// admin password and never rotates, so the rendered collector config is
	// stable across password rotations. Plaintext; read from the
	// openobserve_token managed credential in up/update.
	OpenObserveIngestionToken string

	// ACMEEmail enables Let's Encrypt automation when non-empty. Mutually
	// exclusive with operator-supplied cert/key (see cluster.UpInput).
	ACMEEmail string

	// CORSOriginRegex is the Traefik accessControlAllowOriginListRegex
	// pattern wired into the cors-default middleware. When empty,
	// RenderTraefikDynamic derives it from Domain via CORSOriginRegex().
	CORSOriginRegex string

	// ConfigDir is ~/.pmcluster/config/. It is no longer consulted for config
	// loading (the DB is the only store) but remains in RenderInput so callers
	// can derive DataDir via filepath.Dir(ConfigDir).
	ConfigDir string

	// ConfigStore is the daemon's store. When set, cluster-scope template
	// configs recorded in the DB (scope "cluster", kind "template", stamped
	// with the current build version) are the authoritative source for the
	// platform config files — they are consulted BEFORE the disk copy and the
	// embedded default, so editing a cluster config in the console and running
	// `cluster update` re-renders + re-deploys the affected stack. Rows whose
	// version does not match buildinfo.Version are stale and skipped.
	ConfigStore *store.Store

	// DataDir is ~/.pmcluster/ — the parent of ConfigDir. Substituted as
	// ${DATA_DIR} in compose files (the backup stack bind-mounts it into the
	// control-plane backup agent so pmcluster's own state rides along in the
	// nightly archive). Derived by callers via filepath.Dir(ConfigDir).
	DataDir string

	// HostCerts lists the per-host (bring-your-own) certificates recorded in
	// the DB. RenderTraefikDynamic appends each one's versioned Swarm secret
	// names to the dynamic config's tls.certificates so Traefik serves them
	// for the matching SNI. Per-host certs are never read from the filesystem
	// (they live only in Swarm secrets + DB metadata).
	HostCerts []HostCertEntry

	// OTelConfigName is the versioned Docker config name for the OTel
	// collector pipeline (e.g. pmcluster_otel_config_v003). Resolved from
	// `config(pmcluster_otel_config)` in compose files.
	OTelConfigName string

	// TraefikConfigName is the versioned Docker config name for the
	// Traefik dynamic file-provider config. Resolved from
	// `config(pmcluster_traefik_dynamic)` in compose files.
	TraefikConfigName string

	// CertSecretName is the versioned Swarm secret name for the TLS
	// certificate (e.g. cert_v001). Resolved from `secrets(cert)`.
	CertSecretName string

	// KeySecretName is the versioned Swarm secret name for the TLS
	// private key (e.g. key_v001). Resolved from `secrets(key)`.
	KeySecretName string

	// EdgeImage is the pmcluster-edge container image tag used by the
	// embedded edge-stack.yml (e.g. ghcr.io/nextrum-sy/pmcluster-edge:v0.2.19).
	// Rendered via the template body; set in up/update.
	EdgeImage string
}

// readConfigFile loads a named file. Resolution order:
//  1. A cluster-scope template config in the DB whose name matches the file
//     (basename without ".yml"), scope is "cluster", kind is "template" and
//     version equals the current build version (ConfigStore set + row found).
//  2. The embedded fallback.
//
// The database is the single source of truth for platform configs — there is
// no disk copy to consult (config files were removed; the store's rendered
// snapshots carry the applied state). Applies to stacks AND standalone configs.
func readConfigFile(name string, in RenderInput) (string, error) {
	if in.ConfigStore != nil {
		cfgName := strings.TrimSuffix(name, ".yml")
		if row, err := in.ConfigStore.GetConfig(context.Background(), cfgName); err == nil &&
			row.Scope == "cluster" && row.Kind == "template" && row.Version == buildinfo.Version {
			return row.Content, nil
		}
	}
	body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+name)
	if err != nil {
		return "", fmt.Errorf("read embedded %s: %w", name, err)
	}
	return string(body), nil
}

// renderRefResolver resolves config()/secrets() references in platform stack
// templates to the versioned Docker artifact names computed by up/update. It
// implements manifest.RefResolver so the platform templates use the SAME
// config(name)/secrets(name) reference syntax as the DSL env values — one
// mechanism for replacing configs and secrets everywhere.
type renderRefResolver struct {
	render RenderInput
}

// configNameAliases maps a logical config() name to the RenderInput field
// holding that config's versioned Docker config name.
var configNameAliases = map[string]func(RenderInput) string{
	"pmcluster_otel_config":     func(r RenderInput) string { return r.OTelConfigName },
	"pmcluster_traefik_dynamic": func(r RenderInput) string { return r.TraefikConfigName },
}

// secretNameAliases maps a logical secrets() name to the RenderInput field
// holding that secret's versioned Swarm secret name.
var secretNameAliases = map[string]func(RenderInput) string{
	"cert": func(r RenderInput) string { return r.CertSecretName },
	"key":  func(r RenderInput) string { return r.KeySecretName },
}

func (r *renderRefResolver) ResolveConfig(_ context.Context, name string) (string, error) {
	fn, ok := configNameAliases[name]
	if !ok {
		return "", fmt.Errorf("resolve config(%s): no such platform config (known: pmcluster_otel_config, pmcluster_traefik_dynamic)", name)
	}
	return fn(r.render), nil
}

func (r *renderRefResolver) ResolveSecret(_ context.Context, name string) (string, error) {
	fn, ok := secretNameAliases[name]
	if !ok {
		return "", fmt.Errorf("resolve secrets(%s): no such platform secret (known: cert, key)", name)
	}
	return fn(r.render), nil
}

// LoadComposeFile renders a stack YAML: text/template first (for
// [[if .ACMEEmail]] blocks), then ${DOMAIN}/${OPENOBSERVE_ADMIN_EMAIL}
// substitution, then config()/secrets() reference resolution. Reads the
// template from the store when a current-version cluster-scope row exists,
// else the embedded default.
func LoadComposeFile(name stackName, in RenderInput) ([]byte, error) {
	fname, ok := composeFile[name]
	if !ok {
		return nil, fmt.Errorf("unknown stack %q", name)
	}
	body, err := readConfigFile(fname, in)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New(string(name)).Delims("[[", "]]").Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s template: %w", fname, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, in); err != nil {
		return nil, fmt.Errorf("execute %s template: %w", fname, err)
	}
	out := strings.ReplaceAll(rendered.String(), "${DOMAIN}", in.Domain)
	out = strings.ReplaceAll(out, "${OPENOBSERVE_ADMIN_EMAIL}", in.OpenObserveAdminEmail)
	out = strings.ReplaceAll(out, "${DATA_DIR}", in.DataDir)
	out = strings.ReplaceAll(out, "__OPENOBSERVE_PASSWORD__", escapeComposeDollar(in.OpenObserveAdminPassword))
	resolved, err := manifest.ReplaceRefs(context.Background(), out, &renderRefResolver{render: in})
	if err != nil {
		return nil, fmt.Errorf("resolve config()/secrets() in %s: %w", fname, err)
	}
	return []byte(resolved), nil
}

// escapeComposeDollar doubles every "$" so Docker Compose/Swarm variable
// interpolation (which `docker stack deploy -c` performs on the rendered file)
// emits a literal "$". Generated passwords include the special charset
// "!@#$%^&*", so a raw "$" would otherwise be treated as an interpolation
// sigil and silently truncate the value — e.g. a ZO_ROOT_USER_PASSWORD of
// "ab$cd" would be rendered as "ab", leaving OpenObserve with a root password
// that no longer matches the stored credential (401 on provisioning).
func escapeComposeDollar(s string) string {
	return strings.ReplaceAll(s, "$", "$$")
}

// ensureEdgeConfig renders the edge-stack.yml (with the version-keyed image tag)
// and ensures a versioned Docker config holding that body. The config is a pure
// "has the edge input moved" content fingerprint — the edge stack never mounts
// it. `cluster update` reads the returned created flag to decide whether the
// edge stack needs to re-deploy, mirroring the OTel/Traefik content-aware path.
func ensureEdgeConfig(ctx context.Context, d docker.Client, version string, render RenderInput) (string, bool, error) {
	edgeYAML, err := LoadComposeFile(StackEdge, render)
	if err != nil {
		return "", false, err
	}
	return EnsureConfig(ctx, d, "pmcluster_edge", edgeYAML, version)
}

// RenderOTelCollectorConfig fills in the OpenObserve `Authorization: Basic <b64>`
// header value. The credential is the dedicated INGESTION token (created via the
// OO API, prefix o2oi_), NOT the rotating human password, so the rendered config
// stays content-stable across password rotations. OpenObserve authenticates OTLP
// ingestion with Basic <base64(<org>:<ingestion_token>)>.
func RenderOTelCollectorConfig(in RenderInput) ([]byte, error) {
	org := in.OpenObserveOrg
	if org == "" {
		org = "default"
	}
	if org == "" || in.OpenObserveIngestionToken == "" {
		return nil, fmt.Errorf("RenderOTelCollectorConfig: OpenObserve org and ingestion token are required")
	}
	cred := org + ":" + in.OpenObserveIngestionToken
	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))

	body, err := readConfigFile("otel-collector-config.yml", in)
	if err != nil {
		return nil, err
	}
	rendered := strings.ReplaceAll(body, "__BASIC_AUTH_PLACEHOLDER__", basicAuth)
	return []byte(rendered), nil
}

// RenderTraefikDynamic renders the Traefik file-provider config. After the
// template body is rendered, every per-host cert in in.HostCerts is appended
// into the same doc's tls.certificates list (referencing its versioned Swarm
// secret) so Traefik serves it for the matching SNI.
func RenderTraefikDynamic(in RenderInput) ([]byte, error) {
	if in.Domain == "" {
		return nil, fmt.Errorf("RenderTraefikDynamic: Domain is required")
	}

	body, err := readConfigFile("traefik-dynamic.yml", in)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("traefik-dynamic").Delims("[[", "]]").Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse traefik dynamic template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, in); err != nil {
		return nil, fmt.Errorf("execute traefik dynamic template: %w", err)
	}
	return appendHostCertSecrets(out.Bytes(), in.HostCerts)
}

// appendHostCertSecrets merges every per-host cert's versioned Swarm secret
// into the rendered Traefik dynamic config's tls.certificates list. Traefik
// reads Swarm secrets as files under /run/secrets/<name>, so each entry
// references /run/secrets/<certSecret> + /run/secrets/<keySecret>. It uses a
// YAML round-trip so there is exactly one `tls:` block and one
// `tls.certificates` list — never a duplicate YAML key. With no host certs the
// body is returned untouched. The cluster's own default cert block (from the
// template) is preserved verbatim in the list; host certs are additional
// entries.
func appendHostCertSecrets(body []byte, entries []HostCertEntry) ([]byte, error) {
	if len(entries) == 0 {
		return body, nil
	}
	additions := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		additions = append(additions, map[string]string{
			"certFile": "/run/secrets/" + e.CertSecret,
			"keyFile":  "/run/secrets/" + e.KeySecret,
		})
	}

	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal rendered traefik dynamic config: %w", err)
	}
	tls, _ := doc["tls"].(map[string]any)
	if tls == nil {
		tls = map[string]any{}
		doc["tls"] = tls
	}
	certs, _ := tls["certificates"].([]any)
	for _, a := range additions {
		certs = append(certs, a)
	}
	tls["certificates"] = certs

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal traefik dynamic config with host certs: %w", err)
	}
	return out, nil
}

// ConfigSyncResult reports which cluster-scope template rows were reconciled
// by SyncClusterConfigs, so callers can surface it in workflow output.
type ConfigSyncResult struct {
	Created   []string // rows inserted (fresh install / first sync)
	Updated   []string // stale rows overwritten with the current build content
	Preserved []string // current-version rows left alone (console edits)
}

// SyncPlatformConfigs reconciles the cluster-scope template rows in the store
// with the current build version. It is the single entry point `cluster up`
// and `cluster update` call so the DB always carries the current build's
// configs (modulo operator edits at the current version). The DB is the only
// store for platform configs — no files are written or mirrored to disk. With
// a nil store it degrades to a no-op.
func SyncPlatformConfigs(ctx context.Context, st *store.Store, version string) (*ConfigSyncResult, error) {
	if st == nil {
		return &ConfigSyncResult{}, nil
	}
	return SyncClusterConfigs(ctx, st, version)
}

// SyncClusterConfigs reconciles the cluster-scope template rows (one per
// ConfigFileNames entry) in the store with the current build version, k8s
// style: the embedded template is the desired state, the DB row is the live
// state, and this loop brings the two together.
//
//   - a missing row is created from the embedded default (desired state);
//   - a row stamped with the current build version is authoritative — it may
//     hold a console edit and is preserved as-is;
//   - a stale row (older build version) is refreshed from the embedded default
//     so platform configs always track the shipped templates.
//
// Rows whose scope/kind is not cluster/template are left untouched.
func SyncClusterConfigs(ctx context.Context, st *store.Store, version string) (*ConfigSyncResult, error) {
	res := &ConfigSyncResult{}
	for _, name := range ConfigFileNames {
		cfgName := strings.TrimSuffix(name, ".yml")

		row, err := st.GetConfig(ctx, cfgName)
		switch {
		case err == nil && row.Scope == "cluster" && row.Kind == "template":
			if row.Version == version {
				// DB is current + authoritative (possibly a console edit).
				res.Preserved = append(res.Preserved, cfgName)
				continue
			}
			// Stale row → adopt the embedded default and rewrite the DB row.
			content, err := embeddedConfigContent(name)
			if err != nil {
				return nil, err
			}
			if _, err := st.UpdateConfig(ctx, cfgName, content, version); err != nil {
				return nil, fmt.Errorf("update cluster config %s: %w", cfgName, err)
			}
			res.Updated = append(res.Updated, cfgName)
		case err == nil:
			// Name is taken by a non-cluster config (e.g. a service config).
			// Never clobber a user's config.
			continue
		case errors.Is(err, store.ErrConfigNotFound):
			content, err := embeddedConfigContent(name)
			if err != nil {
				return nil, err
			}
			if _, err := st.CreateConfig(ctx, "cluster", "", cfgName, "template", content, version); err != nil {
				return nil, fmt.Errorf("create cluster config %s: %w", cfgName, err)
			}
			res.Created = append(res.Created, cfgName)
		default:
			return nil, fmt.Errorf("read cluster config %s: %w", cfgName, err)
		}
	}
	return res, nil
}

// embeddedConfigContent returns the embedded default body for a config file.
// Templates carry no version header — the DB row's version column is the only
// version signal.
func embeddedConfigContent(name string) (string, error) {
	body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+name)
	if err != nil {
		return "", fmt.Errorf("read embedded %s: %w", name, err)
	}
	return string(body), nil
}

// validDomain matches a DNS host: dot-separated labels of letters,
// digits, and hyphens (no scheme, no port, no path). Used so callers
// can't smuggle regex metachars through Domain into the template.
var validDomain = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)

// CORSOriginRegex returns the Traefik origin-list regex that allows
// https://<domain> and https://<any-subdomain>.<domain>. Falls back to
// a never-match pattern when domain is empty or malformed so the
// middleware never inadvertently opens up "*".
// Compare compares two npm-like semver strings (vM.m.p or M.m.p).
// Returns -1 if a < b, 0 if equal, 1 if a > b. Pre-release tags and
// build metadata are not handled — we only ship tagged releases.
func Compare(a, b string) int {

	normalize := strings.TrimPrefix(a, "v")
	normalizeB := strings.TrimPrefix(b, "v")

	partsA := strings.Split(normalize, ".")
	partsB := strings.Split(normalizeB, ".")

	maxLen := len(partsA)
	if len(partsB) > maxLen {
		maxLen = len(partsB)
	}

	for i := 0; i < maxLen; i++ {
		var numA, numB int
		if i < len(partsA) {
			numA = parseSegment(partsA[i])
		}
		if i < len(partsB) {
			numB = parseSegment(partsB[i])
		}
		if numA < numB {
			return -1
		}
		if numA > numB {
			return 1
		}
	}
	return 0
}

// parseSegment converts a version segment to an int, ignoring any
// trailing pre-release suffix (e.g. "12-alpha" → 12).
func parseSegment(s string) int {

	var n int
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func CORSOriginRegex(domain string) string {
	if !validDomain.MatchString(domain) {

		return `^$`
	}
	escaped := regexp.QuoteMeta(strings.ToLower(domain))

	return `^https://([a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*\.)?` + escaped + `$`
}
