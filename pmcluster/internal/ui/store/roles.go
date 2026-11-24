package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Permission is one thing a console role may do. The set is fixed in code on
// purpose: a permission is a promise the routes keep, so inventing one in a form
// that no route checks would be a lie with a checkbox next to it.
type Permission struct {
	Name  string
	Group string
	// Summary is one sentence for the role editor, in plain words.
	Summary string
}

// Permission names, as constants: a permission is checked in routes, in
// templates and in the role editor, and a typo in any of those three would
// silently grant or deny the wrong thing.
const (
	PermView           = "console.view"
	PermDeploy         = "stacks.deploy"
	PermOperate        = "stacks.operate"
	PermSecretsWrite   = "secrets.write"
	PermBackups        = "backups.manage"
	PermShells         = "shells.open"
	PermPlatformConfig = "platform.configure"
	PermSettingsWrite  = "settings.write"
	PermKeys           = "keys.manage"
	PermUsers          = "users.manage"
	PermRoles          = "roles.manage"
)

// The catalogue. Order here is the order the role editor shows them in.
var permissions = []Permission{
	{PermView, "Read", "See stacks, services, logs, backups and cluster state."},
	{PermDeploy, "Stacks", "Deploy a stack, sync it with its repo, roll it back, move it."},
	{PermOperate, "Stacks", "Restart or scale services, acknowledge a failover, remove a stack."},
	{PermSecretsWrite, "Stacks", "Create and edit a stack's configs and secrets."},
	{PermBackups, "Backups", "Take a backup, restore one, run the restore drill."},
	{PermShells, "Terminal", "Open an interactive shell in a stack's network."},
	{PermPlatformConfig, "Platform", "Rotate TLS certificates, manage webhooks and registries."},
	{PermSettingsWrite, "Platform", "Change cluster settings — the ones that embed credentials."},
	{PermKeys, "Access", "Mint and revoke API keys, including master keys."},
	{PermUsers, "Access", "Approve people, assign roles, revoke console accounts."},
	{PermRoles, "Access", "Create and edit roles and their permissions."},
}

// Permissions returns the catalogue.
func Permissions() []Permission { return append([]Permission(nil), permissions...) }

// PermissionExists reports whether name is in the catalogue.
func PermissionExists(name string) bool {
	for _, p := range permissions {
		if p.Name == name {
			return true
		}
	}
	return false
}

// AllPermissions is the set an admin role starts with.
func AllPermissions() []string {
	out := make([]string, 0, len(permissions))
	for _, p := range permissions {
		out = append(out, p.Name)
	}
	return out
}

// Role is a named set of permissions. Users point at it by name.
type Role struct {
	ID          int64
	Name        string
	Description string
	Permissions []string
	// Builtin roles ship with the console; deleting one would strand every user
	// that holds it.
	Builtin   bool
	CreatedAt int64
}

// Has reports whether the role grants perm. The admin role is a superset by
// definition: a cluster that edits the admin role down to nothing would have no
// way back in through the console.
func (r *Role) Has(perm string) bool {
	if r == nil {
		return false
	}
	for _, p := range r.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// errRoleNotFound / errRoleInUse are reported to the role editor verbatim.
var (
	ErrRoleNotFound = errors.New("role not found")
	ErrRoleUnknown  = errors.New("no such role")
	ErrRoleInUse    = errors.New("role is assigned to at least one account")
	ErrRoleBuiltin  = errors.New("built-in roles cannot be deleted")
)

// ensureRolesSchema creates the roles table and seeds the three built-ins with
// the permissions the console had before roles were data.
func ensureRolesSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS roles (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			permissions TEXT NOT NULL DEFAULT '',
			builtin     INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL
		)`); err != nil {
		return err
	}
	seeds := []struct {
		name, desc string
		perms      []string
	}{
		{RoleViewer, "Read-only: sees everything, changes nothing.", []string{PermView}},
		{RoleOperator, "Runs the cluster: deploys, restarts, backs up, opens shells.",
			[]string{PermView, PermDeploy, PermOperate, PermSecretsWrite, PermBackups, PermShells, PermPlatformConfig}},
		{RoleAdmin, "Everything, including accounts, keys and settings.", AllPermissions()},
	}
	for _, s := range seeds {
		if _, err := db.Exec(
			`INSERT INTO roles (name, description, permissions, builtin, created_at)
			 VALUES (?, ?, ?, 1, ?)
			 ON CONFLICT(name) DO NOTHING`,
			s.name, s.desc, strings.Join(s.perms, ","), time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

func scanRole(scan func(...any) error) (*Role, error) {
	var (
		r     Role
		perms string
		built int
	)
	if err := scan(&r.ID, &r.Name, &r.Description, &perms, &built, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.Builtin = built == 1
	if perms != "" {
		r.Permissions = strings.Split(perms, ",")
	}
	return &r, nil
}

const roleCols = `id, name, description, permissions, builtin, created_at`

// ListRoles returns every role, built-ins first then alphabetical.
func (s *Store) ListRoles(ctx context.Context) ([]*Role, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+roleCols+` FROM roles ORDER BY builtin DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Role
	for rows.Next() {
		r, err := scanRole(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoleExists answers the question a form has to ask before storing a role name:
// a permission typo fails closed (nobody holds it), but a role typo would store an
// account that can do nothing at all and cannot be fixed by the account holder.
func (s *Store) RoleExists(ctx context.Context, name string) bool {
	_, err := s.RoleByName(ctx, name)
	return err == nil
}

// RoleByName returns a role or ErrRoleNotFound.
func (s *Store) RoleByName(ctx context.Context, name string) (*Role, error) {
	r, err := scanRole(s.db.QueryRowContext(ctx,
		`SELECT `+roleCols+` FROM roles WHERE name = ?`, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRoleNotFound
	}
	return r, err
}

// CreateRole adds a role. Unknown permission names are refused rather than
// stored: a typo would otherwise sit in the table granting nothing.
func (s *Store) CreateRole(ctx context.Context, name, description string, perms []string) (*Role, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("role name is required")
	}
	if strings.ContainsAny(name, " ,") {
		return nil, errors.New("role name cannot contain spaces or commas")
	}
	clean, err := validatePermissions(perms)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO roles (name, description, permissions, builtin, created_at) VALUES (?, ?, ?, 0, ?)`,
		name, strings.TrimSpace(description), strings.Join(clean, ","), time.Now().Unix()); err != nil {
		return nil, err
	}
	return s.RoleByName(ctx, name)
}

// UpdateRole replaces a role's description and permissions. Renaming is not
// offered: users point at the name, so a rename is a migration, not an edit.
func (s *Store) UpdateRole(ctx context.Context, name, description string, perms []string) error {
	clean, err := validatePermissions(perms)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE roles SET description = ?, permissions = ? WHERE name = ?`,
		strings.TrimSpace(description), strings.Join(clean, ","), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// DeleteRole removes a custom role. Built-ins and roles in use are refused: the
// first would strand every install that expects them, the second would leave
// accounts pointing at nothing.
func (s *Store) DeleteRole(ctx context.Context, name string) error {
	r, err := s.RoleByName(ctx, name)
	if err != nil {
		return err
	}
	if r.Builtin {
		return ErrRoleBuiltin
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w (%d account(s))", ErrRoleInUse, n)
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM roles WHERE name = ?`, name)
	return err
}

// RoleNames is what the user form's select needs.
func (s *Store) RoleNames(ctx context.Context) ([]string, error) {
	roles, err := s.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out, nil
}

func validatePermissions(perms []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if !PermissionExists(p) {
			return nil, fmt.Errorf("unknown permission %q", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
