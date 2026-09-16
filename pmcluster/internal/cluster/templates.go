package cluster

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
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

	// ConfigDir is ~/.pmcluster/config/. When non-empty, config loading
	// prefers a user-supplied copy from disk over the embedded default.
	// If the disk file is missing it falls back to the embedded version.
	ConfigDir string

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
	// collector pipeline (e.g. pmcluster_otel_config_v003). Substituted
	// as __OTEL_CONFIG_NAME__ in compose files.
	OTelConfigName string

	// TraefikConfigName is the versioned Docker config name for the
	// Traefik dynamic file-provider config. Substituted as
	// __TRAEFIK_CONFIG_NAME__ in compose files.
	TraefikConfigName string

	// CertSecretName is the versioned Swarm secret name for the TLS
	// certificate (e.g. cert_v001). Substituted as __CERT_SECRET__.
	CertSecretName string

	// KeySecretName is the versioned Swarm secret name for the TLS
	// private key (e.g. key_v001). Substituted as __KEY_SECRET__.
	KeySecretName string

	// EdgeImage is the pmcluster-edge container image tag used by the
	// embedded edge-stack.yml (e.g. ghcr.io/nextrum-sy/pmcluster-edge:v0.2.19).
	// Rendered via the template body; set in up/update.
	EdgeImage string
}

// readConfigFile loads a named file. When in.ConfigDir is set the disk
// copy at <ConfigDir>/<name> wins (if readable). Otherwise — or on any
// disk error — the embedded fallback is used. Applies to stacks AND
// standalone configs.
func readConfigFile(name string, in RenderInput) (string, error) {
	if in.ConfigDir != "" {
		path := filepath.Join(in.ConfigDir, name)
		if data, err := os.ReadFile(path); err == nil {
			return string(data), nil
		}
		// Disk missing/unreadable → fall through to embedded.
	}
	body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+name)
	if err != nil {
		return "", fmt.Errorf("read embedded %s: %w", name, err)
	}
	return string(body), nil
}

// LoadComposeFile renders a stack YAML: text/template first (for
// [[if .ACMEEmail]] blocks), then ${DOMAIN}/${OPENOBSERVE_ADMIN_EMAIL}
// substitution. Reads from disk first (ConfigDir), then embedded.
func LoadComposeFile(name stackName, in RenderInput) ([]byte, error) {
	fname, ok := composeFile[name]
	if !ok {
		return nil, fmt.Errorf("unknown stack %q", name)
	}
	body, err := readConfigFile(fname, in)
	if err != nil {
		return nil, err
	}
	// Custom delims so offen's runtime `{{ .Node.ID }}` template syntax
	// in backup-stack.yml is left untouched.
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
	out = strings.ReplaceAll(out, "__OTEL_CONFIG_NAME__", in.OTelConfigName)
	out = strings.ReplaceAll(out, "__TRAEFIK_CONFIG_NAME__", in.TraefikConfigName)
	out = strings.ReplaceAll(out, "__CERT_SECRET__", in.CertSecretName)
	out = strings.ReplaceAll(out, "__KEY_SECRET__", in.KeySecretName)
	return []byte(out), nil
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
// Reads from disk first (ConfigDir), then embedded.
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

// RenderTraefikDynamic renders the Traefik file-provider config.
// Reads from disk first (ConfigDir), then embedded. After the template body
// is rendered, every per-host cert in in.HostCerts is appended into the same
// doc's tls.certificates list (referencing its versioned Swarm secret) so
// Traefik serves it for the matching SNI.
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

// configVersionHeader matches the first-line version comment in every
// embedded config file. Group 1 captures the version string (e.g. "v0.1.12").
var configVersionHeader = regexp.MustCompile(`^## pmcluster-config-version: (.+)$`)

// EnsureConfigDir creates ~/.pmcluster/config/ and seeds it with the
// embedded defaults. When a file already exists on disk, its first-line
// version comment is compared against the current build version. If the
// disk copy is older (or missing a version), it's overwritten; otherwise
// operator edits are preserved.
func EnsureConfigDir(configDir string, version string) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("create config dir %s: %w", configDir, err)
	}
	for _, name := range ConfigFileNames {
		dest := filepath.Join(configDir, name)
		if data, err := os.ReadFile(dest); err == nil {
			if !shouldOverwriteConfig(data, version) {
				continue
			}
		}
		body, err := fs.ReadFile(embeddedStacks, embeddedDir+"/"+name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		// Inject the actual build version into the placeholder.
		seeded := strings.Replace(string(body), "__PMCONFIG_VERSION__", version, 1)
		if err := os.WriteFile(dest, []byte(seeded), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
	}
	return nil
}

// shouldOverwriteConfig returns true when the on-disk config is stale
// relative to the current build version. A config without a recognised
// version header is treated as stale so it's upgraded on first init
// after this feature lands.
func shouldOverwriteConfig(disk []byte, currentVersion string) bool {
	firstLine, _, _ := strings.Cut(string(disk), "\n")
	matches := configVersionHeader.FindStringSubmatch(strings.TrimSpace(firstLine))
	if len(matches) < 2 {
		return true // no version header — overwrite to add one
	}
	diskVersion := matches[1]
	return Compare(diskVersion, currentVersion) < 0
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
	// Strip leading 'v' so both "v0.1.12" and "0.1.12" compare correctly.
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
	// Take only leading digits.
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
		// Match nothing — callers should validate Domain upstream;
		// this keeps the template render-safe in the worst case.
		return `^$`
	}
	escaped := regexp.QuoteMeta(strings.ToLower(domain))
	// Each subdomain label must start with an alphanumeric (no leading
	// hyphen — RFC 1123) to avoid letting weird-but-not-quite-impossible
	// origins through.
	return `^https://([a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*\.)?` + escaped + `$`
}
