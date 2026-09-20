package controllers

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// Users lists and manages console accounts and their RBAC roles. Admin-only:
// every route is mounted behind middleware.RequireRole(store.RoleAdmin).
//
// The guards exist twice on purpose. The store is the authority — it refuses a
// self-demote and a last-admin delete — and usersData recomputes the same rules
// so the table never offers a button whose request would come back refused. When
// the two ever disagree, the store wins and the operator sees the error.
//
// Every message is a dictionary key, never prose: the controller names the
// sentence and the fragment translates it. Text that a human still has to read
// in full (a raw store or upstream reply) travels beside it in ErrRaw and is
// shown only inside a <details> disclosure.
type Users struct{ *Controller }

// roleOptions is the closed set of RBAC roles, in the order the selects list
// them. The order is significant: admin first, because it is the role an
// operator is most often looking for.
var roleOptions = []string{store.RoleAdmin, store.RoleOperator, store.RoleViewer}

// minPasswordLen is the shortest password this console will store. The same
// number is quoted by users.err_password_short.
const minPasswordLen = 8

// validRole reports whether r is one of the three roles the console understands.
// A role that fails this check never reaches the store.
func validRole(r string) bool {
	for _, v := range roleOptions {
		if r == v {
			return true
		}
	}
	return false
}

// userRow is one account as the users table shows it. CanDelete and RoleLocked
// are decided here so the template cannot offer an action that would fail:
// DeleteBlockKey is the dictionary key explaining a disabled delete button, and
// is always set when CanDelete is false.
type userRow struct {
	ID             int64
	Username       string
	Role           string
	CreatedAt      int64
	IsSelf         bool
	RoleLocked     bool
	CanDelete      bool
	DeleteBlockKey string
}

// usersData is the page model.
//
// Known distinguishes "the store answered and there are no accounts" from "the
// store could not be read". A failed read must never render as an empty table —
// Known stays false, the table is replaced by the unavailable state, and the
// reason is in ErrKey/ErrRaw.
type usersData struct {
	Rows    []userRow
	Count   int
	Known   bool
	ErrKey  string
	ErrRaw  string
	MsgKey  string
	MsgArg  string
	MsgArg2 string
	Current string
	// Disabled mirrors the login-disabled flag so the page can say that no new
	// account can sign in yet.
	Disabled bool
	// Error is the pre-i18n prose field. Kept so a fragment rendered by a caller
	// this file does not own still shows something readable.
	Error string
}

// userFormData is the create/edit modal model. RoleLocked pins the role select
// when changing it would lock everyone out (own account, or the only admin);
// the controller refuses the same requests, so a locked select is not a
// cosmetic restriction.
type userFormData struct {
	TitleKey   string
	SubKey     string
	Username   string
	Role       string
	Roles      []string
	IsEdit     bool
	ID         int64
	RoleLocked bool
	Action     string
	ErrKey     string
	ErrRaw     string
	// Error is the pre-i18n prose field, kept for callers outside this rewrite.
	Error string
}

// List renders the accounts table.
func (c Users) List(g *gin.Context) {
	d := c.page(g.Request.Context(), username(g), usersData{Disabled: c.Auth.LoginDisabled})
	c.Views.Fragment(g, "users", d)
}

// New opens an empty create form. The default role is operator: it can operate
// the cluster without being able to manage accounts.
func (c Users) New(g *gin.Context) {
	c.Views.Fragment(g, "userform", userFormData{
		TitleKey: "users.form_new_title",
		SubKey:   "users.form_new_sub",
		Role:     store.RoleOperator,
		Roles:    roleOptions,
		Action:   WebBase + "/users/add",
	})
}

// Edit opens the form for one account.
func (c Users) Edit(g *gin.Context) {
	ctx := g.Request.Context()
	me := username(g)
	d := userFormData{
		TitleKey: "users.form_edit_title",
		SubKey:   "users.form_edit_sub",
		IsEdit:   true,
		Roles:    roleOptions,
		Action:   WebBase + "/users/edit",
	}
	id, err := strconv.ParseInt(g.Param("id"), 10, 64)
	if err != nil {
		d.ErrKey = "users.err_bad_id"
		c.userFormError(g, d)
		return
	}
	d.ID = id
	u, err := c.Store.GetByID(ctx, id)
	if err != nil {
		d.ErrKey, d.ErrRaw = "users.err_not_found", err.Error()
		c.userFormError(g, d)
		return
	}
	d.Username, d.Role = u.Username, u.Role

	// Pin the role select whenever changing it could remove the last way into
	// this console: the operator's own account, or the only admin there is.
	admins, adminErr := c.Store.CountAdmins(ctx)
	d.RoleLocked = u.Role == store.RoleAdmin && (adminErr != nil || admins <= 1)
	if me != "" && me == u.Username {
		d.RoleLocked = true
	}
	if adminErr != nil {
		d.ErrKey, d.ErrRaw = "users.err_admins_unknown", adminErr.Error()
	}
	c.Views.Fragment(g, "userform", d)
}

// Create adds an account. Validation failures come back as the modal fragment
// with a 422 so htmx keeps it open instead of closing on a bare error.
func (c Users) Create(g *gin.Context) {
	ctx := g.Request.Context()
	uname, role, pass := g.PostForm("username"), g.PostForm("role"), g.PostForm("password")
	d := userFormData{
		TitleKey: "users.form_new_title",
		SubKey:   "users.form_new_sub",
		Username: uname,
		Role:     role,
		Roles:    roleOptions,
		Action:   WebBase + "/users/add",
	}
	switch {
	case uname == "":
		d.ErrKey = "users.err_username"
	case len(pass) < minPasswordLen:
		d.ErrKey = "users.err_password_short"
	case !validRole(role):
		d.ErrKey = "users.err_role"
	default:
		if _, err := c.Store.CreateUser(ctx, uname, hashPassword(pass), true, role); err != nil {
			d.ErrKey, d.ErrRaw = "users.err_save", err.Error()
			break
		}
		next := c.page(ctx, username(g), usersData{Disabled: c.Auth.LoginDisabled})
		next.MsgKey, next.MsgArg, next.MsgArg2 = "users.msg_created", uname, role
		c.Views.Fragment(g, "users", next)
		return
	}
	c.userFormError(g, d)
}

// EditSave applies a role change and, when a password is supplied, a reset.
// This is also the reset-password path: an empty password leaves the stored one
// alone, which is why the modal labels the field as optional on edit.
func (c Users) EditSave(g *gin.Context) {
	ctx := g.Request.Context()
	me := middleware.CurrentUser(g)
	id, idErr := strconv.ParseInt(g.PostForm("id"), 10, 64)
	role, pass := g.PostForm("role"), g.PostForm("password")
	d := userFormData{
		TitleKey: "users.form_edit_title",
		SubKey:   "users.form_edit_sub",
		IsEdit:   true,
		ID:       id,
		Role:     role,
		Roles:    roleOptions,
		Action:   WebBase + "/users/edit",
	}
	var u *store.User
	var err error
	if idErr == nil {
		u, err = c.Store.GetByID(ctx, id)
		if u != nil {
			d.Username = u.Username
		}
	}
	switch {
	case idErr != nil:
		d.ErrKey = "users.err_bad_id"
	case err != nil:
		d.ErrKey, d.ErrRaw = "users.err_not_found", err.Error()
	case !validRole(role):
		d.ErrKey = "users.err_role"
	case me != nil && u != nil && me.Username == u.Username && role != store.RoleAdmin:
		// The store refuses this too. Answering here keeps the modal open with
		// the select back on admin rather than flashing a page-level error.
		d.ErrKey = "users.err_self_demote"
		d.Role = u.Role
		d.RoleLocked = true
	case len(pass) > 0 && len(pass) < minPasswordLen:
		d.ErrKey = "users.err_password_short"
	default:
		var hash string
		if pass != "" {
			hash = hashPassword(pass)
		}
		if err := c.Store.UpdateUser(ctx, id, role, hash); err != nil {
			d.ErrKey, d.ErrRaw = "users.err_save", err.Error()
			break
		}
		next := c.page(ctx, username(g), usersData{Disabled: c.Auth.LoginDisabled})
		next.MsgKey, next.MsgArg = "users.msg_updated", d.Username
		c.Views.Fragment(g, "users", next)
		return
	}
	c.userFormError(g, d)
}

// Remove deletes an account, unless it is the operator's own account or the last
// admin. Both refusals are re-checked here because the store's answer is the one
// that counts; the table only hides the buttons in advance.
func (c Users) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	me := middleware.CurrentUser(g)
	d := usersData{Disabled: c.Auth.LoginDisabled}
	id, idErr := strconv.ParseInt(g.Param("id"), 10, 64)
	var u *store.User
	var err error
	if idErr == nil {
		u, err = c.Store.GetByID(ctx, id)
	}
	switch {
	case idErr != nil:
		d.ErrKey = "users.err_bad_id"
	case err != nil:
		d.ErrKey, d.ErrRaw = "users.err_not_found", err.Error()
	case me != nil && u != nil && me.Username == u.Username:
		d.ErrKey = "users.err_self_delete"
	case u != nil && u.Role == store.RoleAdmin:
		// Another admin has to exist before an admin account can go, so the
		// console can never delete its way out of its own administration.
		admins, adminErr := c.Store.CountAdmins(ctx)
		switch {
		case adminErr != nil:
			d.ErrKey, d.ErrRaw = "users.err_admins_unknown", adminErr.Error()
		case admins <= 1:
			d.ErrKey = "users.err_last_admin"
		default:
			d.ErrKey = "users.err_admin_required"
		}
	default:
		if err := c.Store.DeleteUser(ctx, id); err != nil {
			d.ErrKey, d.ErrRaw = "users.err_delete", err.Error()
		} else {
			d.MsgKey, d.MsgArg = "users.msg_deleted", u.Username
		}
	}
	c.Views.Fragment(g, "users", c.page(ctx, username(g), d))
}

// page reads the accounts and the admin count into the page model. A failure
// leaves Known false and, if the caller already has a message of its own, keeps
// that message rather than hiding it behind a second error.
func (c Users) page(ctx context.Context, me string, d usersData) usersData {
	d.Current = me
	users, err := c.Store.ListUsers(ctx)
	if err != nil {
		if d.ErrKey == "" {
			d.ErrKey = "users.err_store"
		}
		if d.ErrRaw == "" {
			d.ErrRaw = err.Error()
		}
		return d
	}
	admins, adminErr := c.Store.CountAdmins(ctx)
	d.Rows, d.Count, d.Known = rowsFor(users, me, admins, adminErr), len(users), true
	if adminErr != nil {
		// Rows still render — with the role select pinned on every admin row —
		// but the count that decides the guards is not trusted.
		if d.ErrKey == "" {
			d.ErrKey = "users.err_admins_unknown"
		}
		if d.ErrRaw == "" {
			d.ErrRaw = adminErr.Error()
		}
	}
	return d
}

// rowsFor works out, per account, which destructive actions are actually
// available. adminsErr means the admin count could not be read: admin rows then
// keep their role pinned and lose the delete button, because the console cannot
// prove that removing them would leave another admin behind.
func rowsFor(users []*store.User, me string, admins int, adminsErr error) []userRow {
	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		r := userRow{
			ID:        u.ID,
			Username:  u.Username,
			Role:      u.Role,
			CreatedAt: u.CreatedAt,
			IsSelf:    me != "" && u.Username == me,
		}
		switch {
		case r.IsSelf:
			r.RoleLocked = true
			r.DeleteBlockKey = "users.self_delete_blocked"
		case u.Role == store.RoleAdmin && adminsErr != nil:
			r.RoleLocked = true
			r.DeleteBlockKey = "users.err_admins_unknown"
		case u.Role == store.RoleAdmin && admins <= 1:
			r.RoleLocked = true
			r.DeleteBlockKey = "users.last_admin_blocked"
		case u.Role == store.RoleAdmin:
			// Deleting an admin outright is refused; demote first, then delete.
			r.DeleteBlockKey = "users.err_admin_required"
		default:
			r.CanDelete = true
		}
		rows = append(rows, r)
	}
	return rows
}

// userFormError keeps the modal open on a refusal: the fragment replaces
// #modal-body and the 422 makes htmx report the request as failed, which is what
// the form's hx-on::after-request checks before switching to the table.
func (c Users) userFormError(g *gin.Context, d userFormData) {
	g.Header("HX-Retarget", "#modal-body")
	g.Status(http.StatusUnprocessableEntity)
	c.Views.Fragment(g, "userform", d)
}
