package controllers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// Roles defines what a console account may do. A role is a name plus a set of
// permissions from a FIXED catalogue (store.Permissions) — fixed because a
// permission is a promise a route keeps, and a checkbox for something no route
// checks would be a lie.
//
// The three built-ins ship with the permissions the console had before roles were
// data, so an untouched cluster behaves exactly as it did. They can be edited
// (a cluster may want its operators to open shells) but not deleted, and the admin
// role cannot be edited at all: a console whose admin role grants nothing has no
// way back in.
type Roles struct{ *Controller }

// roleRow is one role as the table shows it.
type roleRow struct {
	Name        string
	Description string
	Builtin     bool
	Permissions []string
	Accounts    int
}

type rolesData struct {
	Rows   []roleRow
	Known  bool
	ErrKey string
	ErrRaw string

	MsgKey string
	MsgArg string
}

// roleFormData is the create/edit form: every permission in the catalogue, with
// the ones the role already has marked.
type roleFormData struct {
	TitleKey    string
	SubKey      string
	Name        string
	Description string
	IsEdit      bool
	// Locked disables the permission checkboxes (the admin role).
	Locked bool
	Groups []permGroup
	ErrKey string
	ErrRaw string
}

type permGroup struct {
	Name  string
	Perms []permChoice
}

type permChoice struct {
	Name    string
	Summary string
	Checked bool
	// Locked keeps a permission on and unclickable (the admin role's).
	Locked bool
}

func (c Roles) List(g *gin.Context) {
	d := rolesData{}
	roles, err := c.Store.ListRoles(g.Request.Context())
	if err != nil {
		d.ErrKey, d.ErrRaw = "roles.err_list", err.Error()
		c.Views.Fragment(g, "roles", d)
		return
	}
	users, _ := c.Store.ListUsers(g.Request.Context())
	counts := map[string]int{}
	for _, u := range users {
		counts[u.Role]++
	}
	for _, r := range roles {
		d.Rows = append(d.Rows, roleRow{
			Name: r.Name, Description: r.Description, Builtin: r.Builtin,
			Permissions: r.Permissions, Accounts: counts[r.Name],
		})
	}
	d.Known = true
	c.Views.Fragment(g, "roles", d)
}

func (c Roles) New(g *gin.Context) {
	c.Views.Fragment(g, "roleform", roleFormData{
		TitleKey: "roles.form_new_title",
		Groups:   groupPermissions(nil, false),
	})
}

func (c Roles) Create(g *gin.Context) {
	ctx := g.Request.Context()
	name := strings.TrimSpace(g.PostForm("name"))
	r, err := c.Store.CreateRole(ctx, name, g.PostForm("description"), g.PostFormArray("perm"))
	if err != nil {
		c.Views.Fragment(g, "roleform", roleFormData{
			TitleKey: "roles.form_new_title", Name: name,
			Description: g.PostForm("description"),
			Groups:      groupPermissions(g.PostFormArray("perm"), false),
			ErrKey:      "roles.err_create", ErrRaw: err.Error(),
		})
		return
	}
	d := rolesData{MsgKey: "roles.msg_created", MsgArg: r.Name}
	c.reload(g, d)
}

func (c Roles) Edit(g *gin.Context) {
	name := g.Param("name")
	r, err := c.Store.RoleByName(g.Request.Context(), name)
	if err != nil {
		d := rolesData{ErrKey: "roles.err_missing", ErrRaw: name}
		c.reload(g, d)
		return
	}
	locked := r.Name == store.RoleAdmin
	c.Views.Fragment(g, "roleform", roleFormData{
		TitleKey: "roles.form_edit_title", SubKey: "roles.form_edit_sub",
		Name: r.Name, Description: r.Description, IsEdit: true, Locked: locked,
		Groups: groupPermissions(r.Permissions, locked),
	})
}

func (c Roles) EditSave(g *gin.Context) {
	ctx := g.Request.Context()
	name := strings.TrimSpace(g.PostForm("name"))
	if name == store.RoleAdmin {
		// A console whose last admin role grants nothing has no way back in; the
		// role that guarantees access is not editable.
		d := rolesData{ErrKey: "roles.err_admin_fixed", ErrRaw: name}
		c.reload(g, d)
		return
	}
	if err := c.Store.UpdateRole(ctx, name, g.PostForm("description"), g.PostFormArray("perm")); err != nil {
		c.Views.Fragment(g, "roleform", roleFormData{
			TitleKey: "roles.form_edit_title", Name: name, IsEdit: true,
			Description: g.PostForm("description"),
			Groups:      groupPermissions(g.PostFormArray("perm"), false),
			ErrKey:      "roles.err_save", ErrRaw: err.Error(),
		})
		return
	}
	c.reload(g, rolesData{MsgKey: "roles.msg_saved", MsgArg: name})
}

func (c Roles) Remove(g *gin.Context) {
	name := g.Param("name")
	d := rolesData{}
	if err := c.Store.DeleteRole(g.Request.Context(), name); err != nil {
		d.ErrKey = "roles.err_remove"
		d.ErrRaw = err.Error()
		c.reload(g, d)
		return
	}
	c.reload(g, rolesData{MsgKey: "roles.msg_removed", MsgArg: name})
}

// reload renders the roles page with the current rows and the message/diagnostic
// the caller collected.
func (c Roles) reload(g *gin.Context, d rolesData) {
	if len(d.Rows) == 0 && d.ErrKey == "" {
		if roles, err := c.Store.ListRoles(g.Request.Context()); err == nil {
			users, _ := c.Store.ListUsers(g.Request.Context())
			counts := map[string]int{}
			for _, u := range users {
				counts[u.Role]++
			}
			for _, r := range roles {
				d.Rows = append(d.Rows, roleRow{
					Name: r.Name, Description: r.Description, Builtin: r.Builtin,
					Permissions: r.Permissions, Accounts: counts[r.Name],
				})
			}
			d.Known = true
		}
	}
	c.Views.Fragment(g, "roles", d)
}

// groupPermissions turns the catalogue into the form's checkbox groups, marking
// the role's current permissions and (for the admin role) locking them on.
func groupPermissions(held []string, lockAll bool) []permGroup {
	has := map[string]bool{}
	for _, p := range held {
		has[p] = true
	}
	order := []string{}
	byGroup := map[string][]permChoice{}
	for _, p := range store.Permissions() {
		if _, seen := byGroup[p.Group]; !seen {
			order = append(order, p.Group)
		}
		byGroup[p.Group] = append(byGroup[p.Group], permChoice{
			Name: p.Name, Summary: p.Summary,
			Checked: has[p.Name] || lockAll,
			Locked:  lockAll,
		})
	}
	out := make([]permGroup, 0, len(order))
	for _, name := range order {
		out = append(out, permGroup{Name: name, Perms: byGroup[name]})
	}
	return out
}

// Can renders a permission check for templates (nav visibility).
func (c Roles) Can(g *gin.Context, perm string) bool {
	u := middleware.CurrentUser(g)
	if u == nil {
		return false
	}
	if u.Role == store.RoleAdmin {
		return true
	}
	r, err := c.Store.RoleByName(g.Request.Context(), u.Role)
	if err != nil {
		return false
	}
	return r.Has(perm)
}

// keep http imported for the interface shape used by other controllers.
var _ = http.StatusOK
