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
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views/i18n"
)

//go:embed templates/*.html
var files embed.FS

//go:embed all:static
var StaticFS embed.FS

// Cookie names owned by the console UI.
const (
	CookieLang  = "pmc_lang"
	CookieTheme = "pmc_theme"
)

// Lang resolves the request language: explicit ?lang= wins, then the persisted
// cookie, then the browser's Accept-Language, then English.
func Lang(c *gin.Context) i18n.Lang {
	if q := c.Query("lang"); q != "" {
		return i18n.Parse(q)
	}
	if ck, err := c.Cookie(CookieLang); err == nil && ck != "" {
		return i18n.Parse(ck)
	}
	return i18n.Parse(c.GetHeader("Accept-Language"))
}

// Localize returns the localizer for the current request.
func Localize(c *gin.Context) *i18n.Localizer { return i18n.New(Lang(c)) }

// Theme resolves the persisted theme. Dark is the default because the console
// is an operations surface: it is used at night, next to a terminal.
func Theme(c *gin.Context) string {
	if ck, err := c.Cookie(CookieTheme); err == nil && (ck == "light" || ck == "dark") {
		return ck
	}
	return "dark"
}

// CrumbKey maps a console path to its navigation key, so the breadcrumb, the
// document title and the sidebar agree without every controller repeating it.
func CrumbKey(path string) string {
	seg := strings.TrimPrefix(path, "/web/")
	if i := strings.IndexAny(seg, "/?"); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case "overview", "stacks", "services", "deploy", "webhooks", "tls",
		"backups", "users", "apikeys", "settings":
		return "nav." + seg
	default:
		return "nav.overview"
	}
}

// ShellBase returns the fields the app shell needs beyond its page payload —
// where the operator is, and what to call that place.
func ShellBase(c *gin.Context) gin.H {
	l := Localize(c)
	key := CrumbKey(c.Request.URL.Path)
	return gin.H{
		"Theme": Theme(c),
		"Path":  c.Request.URL.Path,
		"Crumb": key,
		"Title": l.T(key),
	}
}

// StaticBase is the URL prefix the console's own assets are served from.
const StaticBase = "/web/static"

// StaticHandler serves the console's own CSS and fonts. htmx itself is loaded
// from its CDN (https://unpkg.com/htmx.org@1.9.12) in app.html; only the CSS
// and font stack are self-hosted so the design renders on any host.
func StaticHandler() gin.HandlerFunc {
	sub, err := fs.Sub(StaticFS, "static")
	if err != nil {
		panic(err)
	}
	server := http.FileServer(http.FS(sub))
	return func(c *gin.Context) {
		// FileServer resolves against the request path, so drop the mount
		// prefix before delegating.
		c.Request.URL.Path = strings.TrimPrefix(c.Request.URL.Path, StaticBase)
		c.Header("Cache-Control", "public, max-age=3600")
		server.ServeHTTP(c.Writer, c.Request)
	}
}

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
	// Request-bound funcs are registered here as inert placeholders and rebound
	// per request by withRequest. They must exist at parse time because
	// text/template validates function names while parsing, not while
	// executing.
	t, err := template.New("ui").Funcs(template.FuncMap{
		"ts":   formatTime,
		"tb":   toBytes,
		"sp":   statusPill,
		"icon": icon,
		"T":    func(key string) string { return key },
		"TN":   func(key string) string { return "" },
		"TF":   func(key string, args ...any) string { return key },
		// Numbers arrive as int, int64, uint64 or float64 depending on the
		// data source, so every numeric helper takes any and coerces.
		"P":     func(count any, key string, dualCase ...i18n.Case) string { return key },
		"N":     func(v any) string { return strconv.FormatInt(i18n.Num(v), 10) },
		"N0":    func(v any) string { return strconv.FormatInt(i18n.Num(v), 10) },
		"NF":    func(v any, decimals int) string { return strconv.FormatFloat(i18n.Float(v), 'f', decimals, 64) },
		"DIR":   func() string { return "ltr" },
		"LANG":  func() string { return string(i18n.Default) },
		"THEME": func() string { return "dark" },
		"RTL":   func() bool { return false },
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Renderer{tmpl: t}, nil
}

// withRequest returns a template set whose translation funcs are bound to this
// request's language.
//
// The funcs are bound per request rather than injected into page data because
// controllers pass typed structs — a per-request clone is the only way a
// template can call {{T "key"}} no matter what data shape it was given.
func (r *Renderer) withRequest(c *gin.Context) *template.Template {
	t, err := r.tmpl.Clone()
	if err != nil {
		return r.tmpl
	}
	l := Localize(c)
	return t.Funcs(template.FuncMap{
		"T":  l.T,
		"TN": l.TN,
		"TF": l.TF,
		// Templated call sites pass int, int64 or float64; coerce so a data
		// source never decides whether a page renders.
		"P": func(count any, key string, dualCase ...i18n.Case) string {
			return l.P(int(i18n.Num(count)), key, dualCase...)
		},
		"N": func(v any) string { return l.N(i18n.Num(v)) },
		// N0 is the ungrouped integer form of N: revision IDs and other opaque
		// identifiers are never comma-grouped ("1,789,849,639" is a sum, not an
		// ID). Counts and sizes use N (grouped) and NF.
		"N0":    func(v any) string { return strconv.FormatInt(i18n.Num(v), 10) },
		"NF":    func(v any, decimals int) string { return l.NF(i18n.Float(v), decimals) },
		"DIR":   l.Dir,
		"LANG":  l.Lang,
		"RTL":   l.IsRTL,
		"THEME": func() string { return Theme(c) },
	})
}

// iconNameOK reports whether name is a safe sprite token: lowercase letters,
// digits and dashes only. Anything else is a typo or an injection attempt.
func iconNameOK(name string) bool {
	for _, r := range name {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !lower && !digit && r != '-' {
			return false
		}
	}
	return true
}

// icon renders a sprite reference to one of the inline SVG symbols defined in
// frag_icons.html. Tokens are validated so a template cannot inject markup.
func icon(name string) template.HTML {
	if !iconNameOK(name) {
		return ""
	}
	return template.HTML(`<svg class="ic" aria-hidden="true" focusable="false"><use href="#i-` + template.HTMLEscapeString(name) + `"></use></svg>`)
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
		if err := r.withRequest(c).ExecuteTemplate(&buf, name, data); err != nil {
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
	if err := r.withRequest(c).ExecuteTemplate(c.Writer, name, data); err != nil {
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

// isolate wraps a value that mixes digits with Latin letters ("7.7 GiB") in First
// Strong Isolates. In an RTL paragraph the space between the digit run and the letter
// run resolves to RTL — bidi rule N1 treats EN digits as R for neutral resolution — so
// the two runs lay out right-to-left and the value renders "GiB 7.7" (measured in the
// live DOM of the Arabic overview). The isolate holds the value in reading order in an
// RTL paragraph and is a no-op in an LTR one.
func isolate(s string) string { return "\u2068" + s + "\u2069" }

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
		return isolate(fmt.Sprintf("%d B", n))
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return isolate(fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp]))
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
