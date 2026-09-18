// Package views renders the HTMX templates that power the UI.
//
// Controllers (MVC "C") hand a template name + typed data to these helpers,
// which execute the embedded templates (MVC "V"). Fragments are partials meant
// to be swapped into the app shell by HTMX; pages are standalone documents
// (login, setup, the app shell itself).
package views

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed templates/*.html
var files embed.FS

// Renderer executes HTML templates from the embedded FS.
type Renderer struct {
	tmpl *template.Template
	// ShellData builds the data object for the app shell (sidebar + styles)
	// when a fragment route is hit with a full page load — a browser refresh
	// or deep link. When nil, fragment routes always render bare partials
	// (used by login/setup, which have their own documents).
	ShellData func(c *gin.Context) gin.H
}

// NewRenderer parses all embedded templates and their helper funcs.
func NewRenderer() (*Renderer, error) {
	t, err := template.New("ui").Funcs(template.FuncMap{
		"ts": formatTime,
		"tb": toBytes,
		"sp": statusPill,
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Renderer{tmpl: t}, nil
}

// Page renders a standalone document (login/setup/app).
func (r *Renderer) Page(c *gin.Context, name string, data any) {
	r.execute(c, name, data)
}

// Fragment renders a partial to be swapped in by HTMX. When the request is a
// full page load (no HX-Request header — a browser refresh or deep link on a
// fragment URL) and ShellData is set, it renders the app shell with the
// fragment embedded in #view so navigation styles survive a refresh.
func (r *Renderer) Fragment(c *gin.Context, name string, data any) {
	if r.ShellData != nil && c.GetHeader("HX-Request") == "" {
		var buf bytes.Buffer
		if err := r.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
			http.Error(c.Writer, "template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		shell := r.ShellData(c)
		shell["ViewContent"] = template.HTML(buf.String())
		r.execute(c, "app", shell)
		return
	}
	r.execute(c, name, data)
}

func (r *Renderer) execute(c *gin.Context, name string, data any) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := r.tmpl.ExecuteTemplate(c.Writer, name, data); err != nil {
		http.Error(c.Writer, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

// formatTime renders a Unix epoch (seconds) as a local, human-readable time.
// A 0/nil timestamp renders as an em-dash.
func formatTime(epoch any) string {
	var sec int64
	switch v := epoch.(type) {
	case int64:
		sec = v
	case int:
		sec = int64(v)
	case nil:
		return "—"
	default:
		return "—"
	}
	if sec == 0 {
		return "—"
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04")
}

// toBytes humanizes a byte count (memory, sizes).
func toBytes(b any) string {
	var n int64
	switch v := b.(type) {
	case int64:
		n = v
	case int:
		n = int64(v)
	default:
		return "—"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// statusPill maps an arbitrary status string to a CSS pill class ("good" /
// "warn" / "bad") for the dark theme, plus a fallback.
func statusPill(s string) string {
	switch s {
	case "running", "active", "callable", "ready", "success", "completed", "leader":
		return "good"
	case "pending", "starting", "syncing", "worker", "released", "created":
		return "warn"
	case "error", "failed", "crash-loop", "down", "unreachable", "drain":
		return "bad"
	default:
		return "warn"
	}
}
