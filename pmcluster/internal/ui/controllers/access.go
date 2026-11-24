package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// Access is the page a pending identity may see, and nothing else.
//
// The approval model only works if "not yet approved" is a real place with a real
// sentence: an identity that oauth2-proxy authenticated but that no admin has
// given a role to reaches this page, and every other route redirects here. That
// is why this controller has no data calls — a pending user gets no fragments, no
// counts, no stack names.
type Access struct{ *Controller }

// Pending renders the waiting page with the identity that is waiting, so the
// operator can tell the admin exactly which account to approve.
func (c Access) Pending(g *gin.Context) {
	u := middleware.CurrentUser(g)
	// This page belongs to people who are waiting. An account that has been
	// approved and then refreshes a stale tab (or bookmarks it) is not waiting for
	// anything — send it where it was trying to go instead of showing it a page
	// that says its access is pending.
	if u == nil || u.Status != store.StatusPending {
		redirect(g, WebBase+"/")
		return
	}
	page := accessPage{Version: c.Version, SSO: c.Auth.SSO}
	if u != nil {
		page.Email = u.Email
		if page.Email == "" {
			page.Email = u.Username
		}
	}
	c.Views.Page(g, "pending", page)
}

// accessPage is the whole model of the waiting page.
type accessPage struct {
	Version string
	// Email is the identity waiting for a role (SSO) — the string an admin needs
	// in order to approve the right person.
	Email string
	// SSO picks the sign-out affordance: the proxy's session cannot be cleared by
	// the console's own logout form.
	SSO bool
}
