package controllers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/store"
)

// Users lists and manages console accounts and their RBAC roles. Admin-only
// (routes are mounted behind middleware.RequireRole(RoleAdmin)).
type Users struct{ *Controller }

type usersData struct {
	Users    []*store.User
	Count    int
	Error    string
	Msg      string
	Current  string // username of the signed-in admin (for self-delete guard UI)
	Disabled bool   // EDGE_LOGIN_DISABLED — CRUD hidden from nav
}

type userFormData struct {
	Username string
	Role     string
	IsEdit   bool
	ID       int64
	Action   string
	Error    string
}

// List renders the users fragment (admin-only).
func (c Users) List(g *gin.Context) {
	users, err := c.Store.ListUsers(g.Request.Context())
	d := usersData{Current: username(g), Disabled: c.Auth.LoginDisabled}
	if err != nil {
		d.Error = err.Error()
	} else {
		d.Users = users
		d.Count = len(users)
	}
	c.Views.Fragment(g, "users", d)
}

// New renders the create-user form into the modal.
func (c Users) New(g *gin.Context) {
	c.Views.Fragment(g, "userform", userFormData{
		Role:   store.RoleOperator,
		Action: WebBase + "/users/add",
	})
}

// Edit renders the edit-user form (role + optional password reset) into the modal.
func (c Users) Edit(g *gin.Context) {
	id, err := strconv.ParseInt(g.Param("id"), 10, 64)
	if err != nil {
		c.Views.Fragment(g, "userform", userFormData{Error: "Invalid user id.", Action: WebBase + "/users/add"})
		return
	}
	u, err := c.Store.GetByID(g.Request.Context(), id)
	if err != nil {
		c.Views.Fragment(g, "userform", userFormData{Error: "User not found.", Action: WebBase + "/users/add"})
		return
	}
	c.Views.Fragment(g, "userform", userFormData{
		Username: u.Username,
		Role:     u.Role,
		IsEdit:   true,
		ID:       u.ID,
		Action:   WebBase + "/users/edit",
	})
}

// Create adds a new console user. The password is required and must be at
// least 8 characters. Failures keep the modal open with the form fragment.
func (c Users) Create(g *gin.Context) {
	ctx := g.Request.Context()
	uname := g.PostForm("username")
	role := g.PostForm("role")
	pass := g.PostForm("password")
	d := userFormData{Username: uname, Role: role, Action: WebBase + "/users/add"}
	switch {
	case uname == "":
		d.Error = "Username is required."
	case len(pass) < 8:
		d.Error = "Password must be at least 8 characters."
	case role != store.RoleAdmin && role != store.RoleOperator && role != store.RoleViewer:
		d.Error = "Unknown role."
	default:
		if _, err := c.Store.CreateUser(ctx, uname, hashPassword(pass), true, role); err != nil {
			d.Error = err.Error()
		} else {
			c.Views.Fragment(g, "users", c.usersAfter(ctx, fmt.Sprintf("User %s created (%s).", uname, role)))
			return
		}
	}
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.Views.Fragment(g, "userform", d)
}

// EditSave updates a user's role and optionally resets the password. Guards:
// an admin may not demote or delete themselves, and the last admin cannot be
// demoted (enforced on the delete path via CountAdmins; here a self-demote is
// blocked explicitly).
func (c Users) EditSave(g *gin.Context) {
	ctx := g.Request.Context()
	id, err := strconv.ParseInt(g.PostForm("id"), 10, 64)
	role := g.PostForm("role")
	pass := g.PostForm("password")
	me := middleware.CurrentUser(g)
	u, gerr := c.Store.GetByID(ctx, id)

	d := userFormData{IsEdit: true, ID: id, Role: role, Action: WebBase + "/users/edit"}
	if u != nil {
		d.Username = u.Username
	}
	switch {
	case err != nil:
		d.Error = "Invalid user id."
	case gerr != nil:
		d.Error = "User not found."
	case role != store.RoleAdmin && role != store.RoleOperator && role != store.RoleViewer:
		d.Error = "Unknown role."
	case me != nil && me.Username == u.Username && role != store.RoleAdmin:
		d.Error = "You cannot demote your own account."
	case len(pass) > 0 && len(pass) < 8:
		d.Error = "Password must be at least 8 characters."
	default:
		var hash string
		if pass != "" {
			hash = hashPassword(pass)
		}
		if err := c.Store.UpdateUser(ctx, id, role, hash); err != nil {
			d.Error = err.Error()
		} else {
			c.Views.Fragment(g, "users", c.usersAfter(ctx, fmt.Sprintf("User %s updated.", d.Username)))
			return
		}
	}
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.Views.Fragment(g, "userform", d)
}

// Remove deletes a user. Guards: cannot delete yourself, and the last admin
// cannot be deleted.
func (c Users) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	id, err := strconv.ParseInt(g.Param("id"), 10, 64)
	me := middleware.CurrentUser(g)
	u, gerr := c.Store.GetByID(ctx, id)

	d := usersData{Current: username(g), Disabled: c.Auth.LoginDisabled}
	switch {
	case err != nil:
		d.Error = "Invalid user id."
	case gerr != nil:
		d.Error = "User not found."
	case me != nil && me.Username == u.Username:
		d.Error = "You cannot delete your own account."
	case u.Role == store.RoleAdmin:
		switch admins, err := c.Store.CountAdmins(ctx); {
		case err != nil:
			d.Error = err.Error()
		case admins <= 1:
			d.Error = "Cannot delete the last admin."
		default:
			d.Error = "Demote or delete admins only when another admin remains."
		}
	default:
		if err := c.Store.DeleteUser(ctx, id); err != nil {
			d.Error = err.Error()
		} else {
			d.Msg = fmt.Sprintf("Deleted user %s.", u.Username)
		}
	}
	d.Users, _ = c.Store.ListUsers(ctx)
	d.Count = len(d.Users)
	c.Views.Fragment(g, "users", d)
}

// usersAfter reloads the users fragment with a confirmation message.
func (c Users) usersAfter(ctx context.Context, msg string) usersData {
	users, _ := c.Store.ListUsers(ctx)
	return usersData{Users: users, Count: len(users), Msg: msg}
}
