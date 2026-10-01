package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// BadgeMount registers the PUBLIC stack-status badge route. It is mounted
// outside the Bearer-authenticated /api group on purpose: README badges are
// fetched by GitHub's camo proxy and browsers with no credentials. It only
// reveals aggregate health (deployed / in progress / degraded / error) — the
// same signal the console already shows — so it is safe to expose.
func BadgeMount(r chi.Router, st *store.Store, svc services.Service) {
	if st == nil || svc == nil {
		return
	}
	r.Get("/api/public/badge/{stack}", func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		if stack == "" {
			writeBadge(w, "pmcluster", "unknown")
			return
		}
		label, status := stackBadge(r.Context(), st, svc, stack)
		writeBadge(w, label, status)
	})
}

// stackBadge derives a stack's health status from its service replicas
// (update state wins) and its recorded deploy errors (a failure on the
// current revision trumps everything).
func stackBadge(ctx context.Context, st *store.Store, svc services.Service, stack string) (label, status string) {
	label = stack

	// Deploy error on the current revision → error (dark red).
	stRow, err := st.GetStack(ctx, stack)
	if err == nil {
		if errMsg := newestStackFailure(store.ParseStackErrors(stRow.LastError), stRow.CurrentRevision); errMsg != "" {
			return label, "error"
		}
	}

	// No stack row → unknown.
	if err != nil {
		if !errors.Is(err, store.ErrStackNotFound) {
			return label, "unknown"
		}
		return label, "unknown"
	}

	svcs, err := svc.List(ctx, stack)
	if err != nil {
		return label, "unknown"
	}
	if len(svcs) == 0 {
		return label, "unknown"
	}

	// Any service mid-update → in progress.
	for _, s := range svcs {
		if s.UpdateState == "updating" {
			return label, "in progress"
		}
	}

	// Any service paused → error.
	for _, s := range svcs {
		if s.UpdateState == "paused" {
			return label, "error"
		}
	}

	// Under-replicated (excluding completed run-once jobs) → degraded.
	for _, s := range svcs {
		if s.RunOnce && s.Desired > 0 && s.Replicas == 0 {
			continue // finished one-shot job, not a failure
		}
		if s.Desired > 0 && s.Replicas < s.Desired {
			return label, "degraded"
		}
	}

	return label, "deployed"
}

// newestStackFailure returns the newest recorded failure whose revision
// matches the current revision (errors-only history — a clean redeploy writes
// nothing, so absence on the current revision means deployed).
func newestStackFailure(errors []store.StackErrorEntry, currentRevision int64) string {
	for _, e := range errors {
		if e.Error != "" && e.Revision == currentRevision {
			return e.Error
		}
	}
	return ""
}

// writeBadge renders a shields.io-style flat SVG badge.
func writeBadge(w http.ResponseWriter, label, status string) {
	color := map[string]string{
		"deployed":    "#44d47b",
		"in progress": "#ffb454",
		"degraded":    "#ff5c5c",
		"error":       "#c62828",
		"unknown":     "#7d899a",
	}[status]
	if color == "" {
		color = "#7d899a"
	}

	const tmpl = `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">
  <title>%s: %s</title>
  <linearGradient id="s" x2="0" y2="100%%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="%d" height="20" fill="#555"/>
    <rect x="%d" width="%d" height="20" fill="%s"/>
    <rect width="%d" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="%d" y="14" fill="#fff" fill-opacity=".6">%s</text>
    <text x="%d" y="14">%s</text>
  </g>
</svg>`

	lw := 8 + 6*len(label)  // approx label width
	sw := 8 + 6*len(status) // approx status width
	total := lw + sw
	textLabel := escapeXML(label)
	textStatus := escapeXML(status)

	svg := fmt.Sprintf(tmpl,
		total, textLabel, textStatus,
		textLabel, textStatus,
		total,
		lw, lw, sw, color,
		total,
		lw/2, textLabel,
		lw+sw/2, textStatus,
	)

	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(svg)))
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, svg)
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
