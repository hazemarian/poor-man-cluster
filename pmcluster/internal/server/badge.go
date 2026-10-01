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
// reveals aggregate health (healthy / in progress / degraded / error) — the
// same signal the console already shows — so it is safe to expose.
func BadgeMount(r chi.Router, st *store.Store, svc services.Service) {
	if st == nil || svc == nil {
		return
	}
	stackBadgeH := func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		if stack == "" {
			writeBadge(w, "pmcluster", "unknown")
			return
		}
		label, status := stackBadge(r.Context(), st, svc, stack)
		writeBadge(w, label, status)
	}
	// Combined badge: main stack health + every service in one wide SVG.
	// Registered before the {service} param route so the literal segment wins.
	servicesBadgeH := func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		segs := stackServicesBadge(r.Context(), st, svc, stack)
		writeMultiBadge(w, segs)
	}
	// Service-level badge: /api/public/badge/{stack}/{service} — one service's
	// own health, so a README (or a services table) can badge each unit.
	serviceBadgeH := func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		service := chi.URLParam(r, "service")
		if stack == "" || service == "" {
			writeBadge(w, "pmcluster", "unknown")
			return
		}
		label, status := serviceBadge(r.Context(), st, svc, stack, service)
		writeBadge(w, label, status)
	}
	// GET + HEAD. GitHub's camo proxy and some image tools preflight with HEAD;
	// chi would otherwise fall the HEAD through to the Bearer /api group → 401
	// → the badge is refused. Register both methods with the same handler.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r.Method(method, "/api/public/badge/{stack}", http.HandlerFunc(stackBadgeH))
		r.Method(method, "/api/public/badge/{stack}/services", http.HandlerFunc(servicesBadgeH))
		r.Method(method, "/api/public/badge/{stack}/{service}", http.HandlerFunc(serviceBadgeH))
	}
}

// completedRunOnce reports whether a one-shot job has finished (its task
// exited and swarm keeps a stale UpdateStatus around). Such services must
// never drive the badge to error/degraded.
func completedRunOnce(s services.ServiceSummary) bool {
	return s.RunOnce && s.Desired > 0 && s.Replicas == 0
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

	// Any service paused → error (completed run-once jobs excluded — their
	// stale paused marker is a finished-job artifact, not a failure).
	for _, s := range svcs {
		if s.UpdateState == "paused" && !completedRunOnce(s) {
			return label, "error"
		}
	}

	// Under-replicated (excluding completed run-once jobs) → degraded.
	for _, s := range svcs {
		if completedRunOnce(s) {
			continue // finished one-shot job, not a failure
		}
		if s.Desired > 0 && s.Replicas < s.Desired {
			return label, "degraded"
		}
	}

	return label, "healthy"
}

// serviceBadge derives ONE service's health. Same rules as stackBadge but
// scoped to a single service; the service label reads <stack>/<service>.
func serviceBadge(ctx context.Context, st *store.Store, svc services.Service, stack, service string) (label, status string) {
	label = stack + "/" + service

	// The stack must exist (keeps the badge honest about the cluster view).
	if _, err := st.GetStack(ctx, stack); err != nil {
		return label, "unknown"
	}

	svcs, err := svc.List(ctx, stack)
	if err != nil {
		return label, "unknown"
	}
	for i := range svcs {
		if svcs[i].Name == service {
			return label, serviceStatus(svcs[i])
		}
	}
	return label, "unknown"
}

// serviceStatus maps one service summary to its badge status using the same
// precedence as stackBadge: updating > paused (non run-once) > degraded >
// healthy; a completed run-once job always reads healthy.
func serviceStatus(s services.ServiceSummary) string {
	if s.UpdateState == "updating" {
		return "in progress"
	}
	if s.UpdateState == "paused" && !completedRunOnce(s) {
		return "error"
	}
	if completedRunOnce(s) {
		return "healthy" // finished one-shot job
	}
	if s.Desired > 0 && s.Replicas < s.Desired {
		return "degraded"
	}
	return "healthy"
}

// badgeSegment is one label/status pair of a multi-segment badge.
type badgeSegment struct {
	Label  string
	Status string
}

// stackServicesBadge builds the segment list for the combined badge: the first
// segment carries the stack's overall health, then one segment per service
// (label without the <stack>_ prefix). An unknown stack yields a single
// unknown segment.
func stackServicesBadge(ctx context.Context, st *store.Store, svc services.Service, stack string) []badgeSegment {
	_, main := stackBadge(ctx, st, svc, stack)
	segs := []badgeSegment{{Label: stack, Status: main}}

	svcs, err := svc.List(ctx, stack)
	if err != nil {
		return segs
	}
	for _, s := range svcs {
		segs = append(segs, badgeSegment{Label: strings.TrimPrefix(s.Name, stack+"_"), Status: serviceStatus(s)})
	}
	return segs
}

// newestStackFailure returns the newest recorded failure whose revision
// matches the current revision (errors-only history — a clean redeploy writes
// nothing, so absence on the current revision means healthy).
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
		"healthy":     "#44d47b",
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

// writeMultiBadge renders one wide flat-SVG badge with a segment per
// label/status pair (typically: stack main health, then one per service).
// Segments share the same visual language as writeBadge.
func writeMultiBadge(w http.ResponseWriter, segs []badgeSegment) {
	if len(segs) == 0 {
		segs = []badgeSegment{{Label: "pmcluster", Status: "unknown"}}
	}
	color := func(status string) string {
		c, ok := map[string]string{
			"healthy":     "#44d47b",
			"in progress": "#ffb454",
			"degraded":    "#ff5c5c",
			"error":       "#c62828",
			"unknown":     "#7d899a",
		}[status]
		if !ok {
			return "#7d899a"
		}
		return c
	}

	// Compute per-segment widths (label grey + status color) and total width.
	type seg struct {
		label, status string
		labelW, statW int
	}
	parsed := make([]seg, 0, len(segs))
	total := 0
	for _, s := range segs {
		lw := 8 + 6*len(s.Label)
		sw := 8 + 6*len(s.Status)
		parsed = append(parsed, seg{escapeXML(s.Label), escapeXML(s.Status), lw, sw})
		total += lw + sw
	}

	const tmpl = `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s">
  <linearGradient id="s" x2="0" y2="100%%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">%s</g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">%s</g>
</svg>`

	var rects, texts strings.Builder
	x := 0
	for _, s := range parsed {
		fmt.Fprintf(&rects, `<rect x="%d" width="%d" height="20" fill="#555"/><rect x="%d" width="%d" height="20" fill="%s"/><rect x="%d" width="%d" height="20" fill="url(#s)"/>`,
			x, s.labelW, x+s.labelW, s.statW, color(s.status), x, s.labelW+s.statW)
		fmt.Fprintf(&texts, `<text x="%d" y="14" fill="#fff" fill-opacity=".6">%s</text><text x="%d" y="14">%s</text>`,
			x+s.labelW/2, s.label, x+s.labelW+s.statW/2, s.status)
		x += s.labelW + s.statW
	}

	aria := make([]string, 0, len(parsed))
	for _, s := range parsed {
		aria = append(aria, s.label+": "+s.status)
	}
	svg := fmt.Sprintf(tmpl, total, strings.Join(aria, " · "), total, rects.String(), texts.String())

	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(svg)))
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, svg)
}
