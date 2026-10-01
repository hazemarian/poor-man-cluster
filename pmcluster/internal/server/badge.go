package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// BadgeMount registers the PUBLIC stack-status badge route. It is mounted
// outside the Bearer-authenticated /api group on purpose: README badges are
// fetched by GitHub's camo proxy and browsers with no credentials. It only
// reveals aggregate health (healthy / in progress / degraded / error) — the
// same signal the console already shows — so it is safe to expose.
//
// The badge reads status ONLY from the DB snapshot the control loop writes
// (stack_status): no live Docker queries per request, so README traffic never
// stresses the swarm. A stack with no snapshot yet reads "unknown".
func BadgeMount(r chi.Router, st *store.Store) {
	if st == nil {
		return
	}
	stackBadgeH := func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		if stack == "" {
			writeBadge(w, "pmcluster", "unknown")
			return
		}
		label, status := stackBadge(r.Context(), st, stack)
		writeBadge(w, label, status)
	}
	// Combined badge: main stack health + every service in one wide SVG.
	// Registered before the {service} param route so the literal segment wins.
	servicesBadgeH := func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		segs := stackServicesBadge(r.Context(), st, stack)
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
		label, status := serviceBadge(r.Context(), st, stack, service)
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

// stackBadge derives a stack's health from the DB status snapshot the control
// loop writes. No live Docker queries. Unknown when no snapshot exists yet.
func stackBadge(ctx context.Context, st *store.Store, stack string) (label, status string) {
	label = stack
	snap, err := st.GetStackStatus(ctx, stack)
	if err != nil {
		return label, "unknown"
	}
	if snap.Status == "" {
		return label, "unknown"
	}
	return label, snap.Status
}

// serviceBadge derives ONE service's health from the DB snapshot's per-service
// map (keyed by the unqualified service name — the loop strips the stack_
// prefix). Unknown when the stack or service has no snapshot entry.
func serviceBadge(ctx context.Context, st *store.Store, stack, service string) (label, status string) {
	label = stack + "/" + service
	snap, err := st.GetStackStatus(ctx, stack)
	if err != nil {
		return label, "unknown"
	}
	if status, ok := snap.Services[service]; ok && status != "" {
		return label, status
	}
	return label, "unknown"
}

// badgeSegment is one label/status pair of a multi-segment badge.
type badgeSegment struct {
	Label  string
	Status string
}

// stackServicesBadge builds the segment list for the combined badge: the first
// segment carries the stack's overall health, then one segment per service
// (label without the <stack>_ prefix), from the DB snapshot only.
func stackServicesBadge(ctx context.Context, st *store.Store, stack string) []badgeSegment {
	_, main := stackBadge(ctx, st, stack)
	segs := []badgeSegment{{Label: stack, Status: main}}

	snap, err := st.GetStackStatus(ctx, stack)
	if err != nil {
		return segs
	}
	for name, status := range snap.Services {
		segs = append(segs, badgeSegment{Label: strings.TrimPrefix(name, stack+"_"), Status: status})
	}
	return segs
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
