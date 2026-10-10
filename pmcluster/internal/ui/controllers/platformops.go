package controllers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// The Platform controller type is declared in platform.go (the platform-services page). The
// methods below add the control-plane surfaces that used to be reachable only from a shell:
// Docker registry credentials, the bootstrap credentials the platform mints for its bundled
// components, and the daemon's own logs.
//
// Each one keeps password material on the safe side of the boundary:
//   - registries: the password is accepted on add and never read back;
//   - credentials: list + rotate, and rotation's new password is shown ONCE
//     (there is no reveal call in the API to re-read it);
//   - logs: read-only, served from a fixed filename pattern.

// logTailDefault is the line count a freshly opened log page asks for.
const logTailDefault = 200

// ---------------------------------------------------------------- registries

type registryRow struct {
	Host      string
	Username  string
	CreatedAt int64
}

type registriesData struct {
	Rows  []registryRow
	Known bool

	// Form echo, so a rejected add does not lose what the operator typed.
	FormHost string
	FormUser string

	ErrKey string
	ErrRaw string

	MsgKey string
	MsgArg string
}

func (d *registriesData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// RegistriesPage renders the registry list.
func (c Platform) RegistriesPage(g *gin.Context) {
	ctx := g.Request.Context()
	d := registriesData{}
	if !c.apiRefused(g, &d) {
		c.fillRegistries(ctx, &d)
	}
	c.Views.Fragment(g, "registries", d)
}

// RegistryAdd logs this host in to a registry and records the credential.
func (c Platform) RegistryAdd(g *gin.Context) {
	ctx := g.Request.Context()
	d := registriesData{
		FormHost: strings.TrimSpace(g.PostForm("host")),
		FormUser: strings.TrimSpace(g.PostForm("username")),
	}
	password := g.PostForm("password")
	if !c.apiRefused(g, &d) {
		if err := c.API.AddRegistry(ctx, d.FormHost, d.FormUser, password); err != nil {
			// The daemon's message names the real cause (docker login output);
			// guessing "wrong password" here would hide an unreachable host.
			d.fail("registries.err_add", err.Error())
		} else {
			d.MsgKey, d.MsgArg = "registries.msg_added", d.FormHost
			d.FormHost, d.FormUser = "", ""
			c.fillRegistries(ctx, &d)
		}
	}
	c.Views.Fragment(g, "registries", d)
}

// RegistryRemove forgets a registry and logs the host out.
func (c Platform) RegistryRemove(g *gin.Context) {
	ctx := g.Request.Context()
	host := strings.TrimSpace(g.PostForm("host"))
	d := registriesData{}
	if !c.apiRefused(g, &d) {
		if err := c.API.RemoveRegistry(ctx, host); err != nil {
			d.fail("registries.err_remove", err.Error())
		} else {
			d.MsgKey, d.MsgArg = "registries.msg_removed", host
		}
		c.fillRegistries(ctx, &d)
	}
	c.Views.Fragment(g, "registries", d)
}

// fillRegistries loads the list, keeping Known false when the read failed so
// the page can say "unknown" instead of drawing an empty table. An error
// already recorded by the mutation that triggered the reload wins.
func (c Platform) fillRegistries(ctx context.Context, d *registriesData) {
	rows, err := c.API.ListRegistries(ctx)
	if err != nil {
		if d.ErrKey == "" {
			d.fail("registries.err_list", err.Error())
		}
		return
	}
	d.Known = true
	d.Rows = make([]registryRow, 0, len(rows))
	for _, r := range rows {
		d.Rows = append(d.Rows, registryRow{Host: r.Host, Username: r.Username, CreatedAt: r.CreatedAt})
	}
}

// --------------------------------------------------------------- credentials

type credentialRow struct {
	Name            string
	Kind            string
	Username        string
	SwarmSecretName string
	CreatedAt       int64
	RotatedAt       int64
}

type credentialsData struct {
	Rows  []credentialRow
	Known bool

	// Rotated carries the one-time password from a rotation. It is shown in a
	// modal because it cannot be read back once this render is gone.
	Rotated *pmapi.RotatedCredential

	ErrKey string
	ErrRaw string

	MsgKey string
	MsgArg string
}

func (d *credentialsData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// CredentialsPage renders the bootstrap-credential list.
func (c Platform) CredentialsPage(g *gin.Context) {
	ctx := g.Request.Context()
	d := credentialsData{}
	if !c.apiRefused(g, &d) {
		c.fillCredentials(ctx, &d)
	}
	c.Views.Fragment(g, "credentials", d)
}

// CredentialRotate swaps a credential's password and reveals the new one once.
func (c Platform) CredentialRotate(g *gin.Context) {
	ctx := g.Request.Context()
	name := g.Param("name")
	d := credentialsData{}
	if !c.apiRefused(g, &d) {
		rotated, err := c.API.RotateCredential(ctx, name)
		if err != nil {
			d.fail("credentials.err_rotate", err.Error())
		} else {
			d.Rotated = rotated
			d.MsgKey, d.MsgArg = "credentials.msg_rotated", name
		}
		c.fillCredentials(ctx, &d)
	}
	c.Views.Fragment(g, "credentials", d)
}

// fillCredentials loads the credential list (metadata only — the API has no
// reveal route to call).
func (c Platform) fillCredentials(ctx context.Context, d *credentialsData) {
	rows, err := c.API.ListCredentials(ctx)
	if err != nil {
		if d.ErrKey == "" {
			d.fail("credentials.err_list", err.Error())
		}
		return
	}
	d.Known = true
	d.Rows = make([]credentialRow, 0, len(rows))
	for _, r := range rows {
		d.Rows = append(d.Rows, credentialRow{
			Name:            r.Name,
			Kind:            r.Kind,
			Username:        r.Username,
			SwarmSecretName: r.SwarmSecretName,
			CreatedAt:       r.CreatedAt,
			RotatedAt:       r.RotatedAt,
		})
	}
}

// ---------------------------------------------------------------------- logs

// logLine is one daemon log entry, parsed for display. Raw keeps the original
// JSON so an operator can still copy a line verbatim.
type logLine struct {
	Time    string
	Level   string
	Message string
	Raw     string
}

type logsData struct {
	Known bool
	Rows  []logLine

	File      string
	Files     []string
	Truncated bool

	Tail  int
	Since string
	All   bool

	ErrKey string
	ErrRaw string
}

func (d *logsData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// LogsPage renders the daemon's own logs.
func (c Platform) LogsPage(g *gin.Context) {
	c.renderLogs(g, logTailDefault, "", false, "controllogs")
}

// LogsTail re-reads the log with the toolbar's current filters (htmx polling).
// It renders the BODY fragment only, so the panel head and the toolbar are not
// replaced under the operator's cursor while they are reading.
func (c Platform) LogsTail(g *gin.Context) {
	tail := logTailDefault
	if v := strings.TrimSpace(g.Query("tail")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tail = n
		}
	}
	c.renderLogs(g, tail, strings.TrimSpace(g.Query("since")), g.Query("all") == "true", "controllogs_body")
}

func (c Platform) renderLogs(g *gin.Context, tail int, since string, all bool, fragment string) {
	ctx := g.Request.Context()
	d := logsData{Tail: tail, Since: since, All: all}
	if !c.apiRefused(g, &d) {
		page, err := c.API.ControlPlaneLogs(ctx, tail, since, all)
		if err != nil {
			d.fail("logs.err_read", err.Error())
		} else {
			d.Known = true
			d.File, d.Files, d.Truncated = page.File, page.Files, page.Truncated
			d.Rows = parseLogLines(page.Lines)
		}
	}
	c.Views.Fragment(g, fragment, d)
}

// parseLogLines turns raw zerolog JSON into display rows, keeping the original
// line so an unparsable entry is still shown rather than dropped.
func parseLogLines(lines []string) []logLine {
	rows := make([]logLine, 0, len(lines))
	for _, raw := range lines {
		row := logLine{Raw: raw}
		var entry struct {
			Time    string `json:"time"`
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(raw), &entry); err == nil {
			row.Time, row.Level, row.Message = entry.Time, entry.Level, entry.Message
		}
		if row.Message == "" && row.Time == "" {
			// Not zerolog JSON (a panic dump, a wrapped stack trace): show it
			// whole, because that is exactly the line worth reading.
			row.Message = raw
		}
		rows = append(rows, row)
	}
	return rows
}

// -------------------------------------------------------------------- verify

type verifyData struct {
	Name  string
	Value string
	Match bool
	Done  bool

	ErrKey string
	ErrRaw string
}

func (d *verifyData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// VerifyForm renders the modal that asks for a candidate value.
func (c Platform) VerifyForm(g *gin.Context) {
	c.Views.Fragment(g, "secretverify", verifyData{Name: g.Param("name")})
}

// VerifySubmit compares the candidate against the stored hash and reports only
// whether it matches — the secret's plaintext never travels to the browser.
func (c Platform) VerifySubmit(g *gin.Context) {
	ctx := g.Request.Context()
	d := verifyData{Name: g.Param("name"), Value: g.PostForm("value"), Done: true}
	if !c.apiRefused(g, &d) {
		match, err := c.API.VerifySecret(ctx, d.Name, d.Value)
		if err != nil {
			d.fail("inventory.err_verify", err.Error())
		} else {
			d.Match = match
		}
	}
	// The submitted value is never echoed back into the response.
	d.Value = ""
	c.Views.Fragment(g, "secretverify", d)
}
