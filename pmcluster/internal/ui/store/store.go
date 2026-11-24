// Package store persists the UI's own data (users + settings) in SQLite.
//
// This is deliberately separate from the pmcluster daemon's store: the UI can
// live anywhere (in or out of the swarm) and only ever talks to the cluster
// through the pmcluster API. Here we keep only what the UI itself needs — who
// may log in, and the operator's preferred API URL/token overrides.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a user or setting doesn't exist.
var ErrNotFound = errors.New("not found")

// Identity errors. They exist so a handler can say WHICH thing was wrong instead
// of turning every refusal into "could not save".
var (
	ErrInvalidEmail    = errors.New("not an email address")
	ErrAccountDisabled = errors.New("that account is disabled")
	ErrUsernameTaken   = errors.New("another account already uses that name")
	ErrEmailTaken      = errors.New("another account already uses that address")
)

// Settings keys for the console's pmcluster API override (the defaults come
// from env / mounted Swarm secrets; these let the operator override in the UI,
// or are seeded once on first boot).
const (
	KeyAPIURL = "pmcluster_api_url"
	KeyToken  = "pmcluster_api_token"
	// KeySessionSecret persists the auto-generated console session-secret so a
	// login-enabled console started without PMCLUSTER_UI_SECRET keeps a stable,
	// non-forgeable secret across reloads.
	KeySessionSecret = "pmcluster_ui_session_secret"
)

// Store wraps the SQLite connection. All access is serialized by SQLite's
// default; a single admin UI client makes contention negligible.
type Store struct {
	db *sql.DB
}

// Roles define what a console user may do (RBAC). Roles are hierarchical:
// admin can do everything, operator everything except user/api-key
// management, viewer read-only.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Account status. A console user is only worth a role once it is active: an SSO
// identity that nobody has approved sits in pending and reaches exactly one page.
const (
	StatusActive   = "active"
	StatusPending  = "pending"
	StatusDisabled = "disabled"
)

// How the identity arrived. Local users log in with a password; SSO users are
// asserted by oauth2-proxy (GitHub org membership is its gate) and authorized
// here, by a row an admin approved.
const (
	AuthLocal = "local"
	AuthSSO   = "sso"
)

// User is a UI login. PasswordHash is a bcrypt hash; PasswordSet is true once a
// password has been chosen (either from env bootstrap or the first-load setup).
//
// Email + AuthSource identify an SSO user: the identity comes from the proxy's
// headers, the role and status come from this row. Username is what the console
// displays; for SSO it is the GitHub login.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	PasswordSet  bool
	Role         string
	Email        string
	Status       string
	AuthSource   string
	CreatedAt    int64
	// LastLoginAt is when this identity last signed in, 0 if never. An invited
	// address that has never appeared is a person who has not accepted yet.
	LastLoginAt int64
}

// Active reports whether the user may use the console at all.
func (u *User) Active() bool { return u != nil && u.Status == StatusActive }

// Open creates (if needed) and opens the SQLite DB under dataDir.
//
// A dataDir of ":memory:" opens a purely in-memory database instead: no
// directory is created and nothing is persisted. This is how the edge console
// runs in cluster mode (EDGE_LOGIN_DISABLED) — it is stateless there, driven
// entirely by the PMCLUSTER_API_URL / PMCLUSTER_API_TOKEN env vars, so it needs
// no volume at all. Standalone/local runs pass a real dataDir and keep their
// users + settings in SQLite.
func Open(dataDir string) (*Store, error) {
	dsn := filepath.Join(dataDir, "pmcluster-ui.db")
	if dataDir == ":memory:" {
		dsn = ":memory:"
	} else if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		PRAGMA journal_mode=DELETE;
		CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL DEFAULT '',
			password_set  INTEGER NOT NULL DEFAULT 0,
			role          TEXT NOT NULL DEFAULT 'operator',
			created_at    INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := ensureRoleColumn(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate role column: %w", err)
	}
	if err := ensureIdentityColumns(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate identity columns: %w", err)
	}
	if err := ensureRolesSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate roles: %w", err)
	}
	return &Store{db: db}, nil
}

// ensureRoleColumn upgrades pre-RBAC databases: adds the role column if it is
// missing and promotes the existing first user (the original bootstrap admin)
// to the admin role so existing installs keep an administrator.
func ensureRoleColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(users)`)
	if err != nil {
		return err
	}
	hasRole := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "role" {
			hasRole = true
		}
	}
	_ = rows.Close()
	if hasRole {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'operator'`); err != nil {
		return err
	}
	// Promote the lowest-ID user (the pre-RBAC bootstrap admin) to admin.
	_, err = db.Exec(`UPDATE users SET role = 'admin' WHERE id = (SELECT MIN(id) FROM users)`)
	return err
}

// ensureIdentityColumns adds email / status / auth_source to a pre-SSO
// database. Every existing row is a local, active account: they logged in with a
// password before this existed, and nothing about that changes.
func ensureIdentityColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(users)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		have[name] = true
	}
	_ = rows.Close()
	for _, col := range []struct{ name, ddl string }{
		{"email", `ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT ''`},
		{"status", `ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`},
		{"auth_source", `ALTER TABLE users ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'local'`},
		{"last_login_at", `ALTER TABLE users ADD COLUMN last_login_at INTEGER NOT NULL DEFAULT 0`},
	} {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			return err
		}
	}
	// SSO lookup goes through email on every request.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS users_email ON users(email)`); err != nil {
		return err
	}
	return nil
}

// Close releases the underlying connection.
func (s *Store) Close() error { return s.db.Close() }

// ByUsername returns a user or ErrNotFound.
func (s *Store) ByUsername(ctx context.Context, username string) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_set, role, email, status, auth_source, created_at, last_login_at
		   FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.Role, &u.Email, &u.Status, &u.AuthSource, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ByID returns a user by primary key or ErrNotFound.
func (s *Store) ByID(ctx context.Context, id int64) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_set, role, email, status, auth_source, created_at, last_login_at
		   FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.Role, &u.Email, &u.Status, &u.AuthSource, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// FirstUser returns the lowest-ID user (the installer/admin), or ErrNotFound.
func (s *Store) FirstUser(ctx context.Context) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_set, role, email, status, auth_source, created_at, last_login_at
		   FROM users ORDER BY id LIMIT 1`).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.Role, &u.Email, &u.Status, &u.AuthSource, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ListUsers returns every user ordered by id.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, password_hash, password_set, role, email, status, auth_source, created_at, last_login_at
		   FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.Role, &u.Email, &u.Status, &u.AuthSource, &u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of stored users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins returns the number of users holding the admin role.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n)
	return n, err
}

// CreateUser inserts a user and returns the stored row.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, passwordSet bool, role string) (*User, error) {
	if role == "" {
		role = RoleOperator
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, password_set, role, email, status, auth_source, created_at)
		 VALUES (?, ?, ?, ?, '', ?, ?, ?)`,
		username, passwordHash, boolInt(passwordSet), role,
		StatusActive, AuthLocal, time.Now().Unix()); err != nil {
		return nil, err
	}
	return s.ByUsername(ctx, username)
}

// UpdateUser updates a user's role and optionally its password. A non-empty
// passwordHash replaces the stored hash and marks it set; an empty hash leaves
// the password untouched.
func (s *Store) UpdateUser(ctx context.Context, id int64, role, passwordHash string) error {
	if role == "" {
		role = RoleOperator
	}
	if passwordHash != "" {
		_, err := s.db.ExecContext(ctx,
			`UPDATE users SET role = ?, password_hash = ?, password_set = 1 WHERE id = ?`,
			role, passwordHash, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

// ByEmail returns the console user for an SSO identity, or ErrNotFound.
func (s *Store) ByEmail(ctx context.Context, email string) (*User, error) {
	u := &User{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_set, role, email, status, auth_source, created_at, last_login_at
		   FROM users WHERE email = ? AND email <> ''`, email).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.Role, &u.Email, &u.Status, &u.AuthSource, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// EnsureSSOUser resolves an SSO identity to a console row, creating it in the
// pending state when it is new.
//
// This is the whole approval model: GitHub (through oauth2-proxy, restricted to
// the configured org) says WHO the person is; this table says whether they may
// use the console and with which role. A stranger's first login therefore grants
// nothing but a row that an admin can see and approve.
func (s *Store) EnsureSSOUser(ctx context.Context, email, username string) (*User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	username = strings.TrimSpace(username)
	if email == "" {
		return nil, ErrNotFound
	}
	// Match on EITHER key. oauth2-proxy sends the profile email when GitHub
	// reports one and an EMPTY email when the account hides it — the console then
	// falls back to the login. Matching on email alone would send exactly those
	// people to the approval queue on every single sign-in.
	if u, err := s.FindIdentity(ctx, email, username); err == nil {
		// The address can arrive later than the account: GitHub starts reporting
		// an email the day the person makes theirs public. Fill it in once, so the
		// row keeps matching even if the login is renamed.
		if u.Email == "" && email != "" && strings.Contains(email, "@") {
			_, _ = s.db.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, strings.ToLower(email), u.ID)
			u.Email = strings.ToLower(email)
		}
		s.TouchLogin(ctx, u.ID)
		return u, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if username == "" {
		username = email
	}
	// Username is uniquely indexed for the password path; an SSO login that
	// collides with one gets its email as the display name instead.
	if _, err := s.ByUsername(ctx, username); err == nil {
		username = email
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, password_set, role, email, status, auth_source, created_at)
		 VALUES (?, '', 0, ?, ?, ?, ?, ?)`,
		username, RoleViewer, email, StatusPending, AuthSSO, time.Now().Unix()); err != nil {
		return nil, err
	}
	return s.ByEmail(ctx, email)
}

// FindIdentity resolves the identity the proxy just vouched for: by address when
// there is one, then by login, which is all a private-email GitHub account offers.
// Only SSO rows match by login — a password account's username is not an identity
// claim from the provider and must never be reachable that way.
func (s *Store) FindIdentity(ctx context.Context, email, login string) (*User, error) {
	if email != "" {
		if u, err := s.ByEmail(ctx, email); err == nil {
			return u, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if login != "" {
		if u, err := s.ByUsername(ctx, login); err == nil {
			if u.AuthSource == AuthSSO || u.Email == "" {
				return u, nil
			}
			return nil, ErrNotFound
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, ErrNotFound
}

// InviteSSOAccount pre-provisions an identity the provider will vouch for later.
//
// The point is to skip the approval queue: an admin who already knows the address
// says so up front, so the person's first sign-in lands in the console instead of
// a waiting page. Status is active from the start and there is no password — the
// provider is the only way in, which also means a typo in the address grants
// nothing: nobody will ever sign in as a mistyped email.
// The identifier may be an email address or the GitHub login. Both are needed:
// GitHub reports no email for accounts that hide it, and oauth2-proxy then passes
// an empty X-Auth-Request-Email, so for those people the login IS the identity.
func (s *Store) InviteSSOAccount(ctx context.Context, identifier, username, role string) (*User, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" || strings.ContainsAny(identifier, " 	") {
		return nil, ErrInvalidEmail
	}
	if !s.RoleExists(ctx, role) {
		return nil, ErrRoleUnknown
	}
	email := ""
	if strings.Contains(identifier, "@") {
		email = strings.ToLower(identifier)
	}
	if email == "" && username == "" {
		username = identifier
	}
	username = strings.TrimSpace(username)
	if u, err := s.FindIdentity(ctx, email, username); err == nil {
		// Already known: an invite is a way to say "active, this role", not a
		// second account. An existing disabled account stays disabled unless
		// somebody deliberately re-enables it.
		if u.Status == StatusDisabled {
			return nil, ErrAccountDisabled
		}
		if err := s.SetStatus(ctx, u.ID, StatusActive, role); err != nil {
			return nil, err
		}
		return s.FindIdentity(ctx, email, username)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if username == "" {
		username = email
	}
	// The username is uniquely indexed for the password path. An invited login
	// that collides gets a suffix rather than being refused: the identity is the
	// email/login the proxy sends, and the display name only has to be unique.
	base := username
	for i := 2; ; i++ {
		if _, err := s.ByUsername(ctx, username); errors.Is(err, ErrNotFound) {
			break
		}
		username = fmt.Sprintf("%s-%d", base, i)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, password_set, role, email, status, auth_source, created_at)
		 VALUES (?, '', 0, ?, ?, ?, ?, ?)`,
		username, role, email, StatusActive, AuthSSO, time.Now().Unix()); err != nil {
		return nil, err
	}
	// Read back by EITHER key: a login-only invite has no email, and ByEmail("")
	// can never match — which made "invite the account with the hidden email" the
	// one case the invite refused.
	return s.FindIdentity(ctx, email, username)
}

// UpdateIdentity changes the two fields that decide WHICH identity an account is:
// its display name and its address. Both are checked for collisions here rather
// than at the index, because "another account already uses that address" is a
// sentence an operator can act on and a UNIQUE constraint error is not.
func (s *Store) UpdateIdentity(ctx context.Context, id int64, username, email string) error {
	username = strings.TrimSpace(username)
	email = strings.TrimSpace(strings.ToLower(email))
	if username == "" {
		return ErrUsernameTaken // an empty name is not available to anybody
	}
	var other int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE username = ? AND id <> ?`, username, id).Scan(&other); err == nil {
		return ErrUsernameTaken
	}
	if email != "" {
		if err := s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE email = ? AND id <> ?`, email, id).Scan(&other); err == nil {
			return ErrEmailTaken
		}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET username = ?, email = ? WHERE id = ?`, username, email, id)
	return err
}

// TouchLogin records that this identity signed in. It is best effort: a console
// that cannot write the timestamp must still let the person in.
func (s *Store) TouchLogin(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

// ListPending returns the identities waiting for approval, oldest first.
func (s *Store) ListPending(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, password_hash, password_set, role, email, status, auth_source, created_at, last_login_at
		   FROM users WHERE status = ? ORDER BY id`, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSet, &u.Role, &u.Email, &u.Status, &u.AuthSource, &u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetStatus sets a user's status and, when approving, its role in the same
// statement — an identity that becomes active must never be active with a role
// nobody chose.
func (s *Store) SetStatus(ctx context.Context, id int64, status, role string) error {
	if role == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET status = ? WHERE id = ?`, status, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET status = ?, role = ? WHERE id = ?`, status, role, id)
	return err
}

// CountActiveAdmins returns how many admins can actually log in. It is the guard
// the console keeps for "do not lock everybody out".
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ? AND status = ?`, RoleAdmin, StatusActive).Scan(&n)
	return n, err
}

// DeleteUser removes a user by id.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPassword sets (or replaces) a password hash and marks it set.
func (s *Store) SetPassword(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_set = 1 WHERE username = ?`,
		passwordHash, username)
	return err
}

// CreateEnvUser seeds the env-configured user if it doesn't already exist.
// Returns true if a new user was created. PasswordSetup is always true here.
// The env user is the operator console's bootstrap identity, so it is created
// with the admin role.
func (s *Store) CreateEnvUser(ctx context.Context, username, passwordHash string) (bool, error) {
	if _, err := s.ByUsername(ctx, username); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if _, err := s.CreateUser(ctx, username, passwordHash, true, RoleAdmin); err != nil {
		return false, err
	}
	return true, nil
}

// Setting returns a setting value or ErrNotFound.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// Settings returns all settings as a map.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SeedSettingOnce writes value under key only if the key has never been set.
// This is how the API token provisioned by pmcluster is "stored once": it lands
// in the console DB on first boot and is never re-seeded, so an operator's
// explicit clear from Settings (SetSetting(key, "")) or subsequent edit is
// respected and not overwritten on the next start.
func (s *Store) SeedSettingOnce(ctx context.Context, key, value string) error {
	if _, err := s.Setting(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.SetSetting(ctx, key, value)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
