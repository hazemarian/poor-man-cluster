package edgeproxy

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type ctxKeyIP struct{}

// realIP returns the client IP attached to the request context by RealIP
// middleware, or "" if none.
func realIP(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyIP{}).(string)
	return s
}

// RealIP returns middleware that resolves the true client IP by trusting
// X-Forwarded-For only from known proxy CIDRs (Traefik and the overlay
// networks). This prevents a remote caller from spoofing the header: we walk
// the X-Forwarded-For chain right-to-left, accepting each hop whose right
// neighbour (the connecting proxy) is trusted, and stop at the first
// untrusted hop.
func RealIP(trusted []string) func(http.Handler) http.Handler {
	trustedNets := parseNets(trusted)
	isTrusted := func(ip string) bool {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return false
		}
		for _, n := range trustedNets {
			if n.Contains(parsed) {
				return true
			}
		}
		return false
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			client := hostOnly(r.RemoteAddr)
			for _, hop := range reverse(xff(r)) {
				if isTrusted(client) {
					client = hop
					continue
				}
				break
			}
			if client == "" {
				client = hostOnly(r.RemoteAddr)
			}
			ctx := context.WithValue(r.Context(), ctxKeyIP{}, client)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// xff returns the X-Forwarded-For chain split into hop IPs.
func xff(r *http.Request) []string {
	var hops []string
	for _, part := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		if p := strings.TrimSpace(part); p != "" {
			hops = append(hops, p)
		}
	}
	return hops
}

// reverse returns a reversed copy of xs.
func reverse(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[len(xs)-1-i] = x
	}
	return out
}

// hostOnly strips the port from a "host:port" string.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSpace(hostport)
}

// parseNets parses CIDR strings into *net.IPNet, ignoring invalid entries.
func parseNets(cidrs []string) []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}
