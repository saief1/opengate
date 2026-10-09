package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/opengate-dev/opengate/internal/secure"
)

// Roles, most privileged first.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleViewer = "viewer"
)

// RoleRank orders roles (higher = more privileged; 0 = invalid).
func RoleRank(r string) int {
	switch r {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleMember:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// ErrConflict is returned for duplicates.
var ErrConflict = errors.New("already exists")

// Org is a tenant.
type Org struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Role      string    `json:"role,omitempty"` // caller's role when listed for a user
}

// CreateOrg inserts an organization (and its "default" project).
func (s *Store) CreateOrg(ctx context.Context, name string) (*Org, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, errors.New("organization name is required (max 100 chars)")
	}
	o := &Org{ID: secure.RandomID("org_"), Name: name, CreatedAt: time.Now().UTC()}
	if _, err := s.exec(ctx, `INSERT INTO orgs(id,name,created_at) VALUES(?,?,?)`, o.ID, o.Name, ms(o.CreatedAt)); err != nil {
		return nil, err
	}
	if _, err := s.CreateProjectInOrg(ctx, o.ID, "default", false); err != nil {
		return nil, err
	}
	return o, nil
}

// GetOrg fetches an organization.
func (s *Store) GetOrg(ctx context.Context, id string) (*Org, error) {
	var o Org
	var c int64
	err := s.queryRow(ctx, `SELECT id,name,created_at FROM orgs WHERE id=?`, id).Scan(&o.ID, &o.Name, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	o.CreatedAt = fromMS(c)
	return &o, err
}

// RenameOrg changes an organization's display name.
func (s *Store) RenameOrg(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return errors.New("organization name is required (max 100 chars)")
	}
	return s.execOne(ctx, `UPDATE orgs SET name=? WHERE id=?`, name, id)
}

// ListOrgs lists all organizations, or those a user belongs to (with role).
func (s *Store) ListOrgs(ctx context.Context, userID string) ([]*Org, error) {
	q, args := `SELECT o.id,o.name,o.created_at,'' FROM orgs o ORDER BY o.created_at`, []any{}
	if userID != "" {
		q, args = `SELECT o.id,o.name,o.created_at,m.role FROM orgs o JOIN memberships m ON m.org_id=o.id WHERE m.user_id=? ORDER BY o.created_at`, []any{userID}
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Org{}
	for rows.Next() {
		var o Org
		var c int64
		if err := rows.Scan(&o.ID, &o.Name, &c, &o.Role); err != nil {
			return nil, err
		}
		o.CreatedAt = fromMS(c)
		out = append(out, &o)
	}
	return out, rows.Err()
}

// User is a dashboard account.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
	LastLoginAt  time.Time `json:"last_login_at"`
}

// NormalizeEmail validates and lower-cases an address.
func NormalizeEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return "", fmt.Errorf("invalid email address")
	}
	return e, nil
}

// CreateUser inserts a user with an already-hashed password.
func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash string) (*User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	u := &User{ID: secure.RandomID("usr_"), Email: email, Name: strings.TrimSpace(name), PasswordHash: passwordHash, CreatedAt: time.Now().UTC()}
	_, err = s.exec(ctx, `INSERT INTO users(id,email,name,password_hash,created_at) VALUES(?,?,?,?,?)`, u.ID, u.Email, u.Name, u.PasswordHash, ms(u.CreatedAt))
	if isUnique(err) {
		return nil, ErrConflict
	}
	return u, err
}

const userCols = `id,email,name,password_hash,disabled,created_at,last_login_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var d int
	var c, l int64
	if err := sc.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &d, &c, &l); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Disabled, u.CreatedAt = d == 1, fromMS(c)
	if l > 0 {
		u.LastLoginAt = fromMS(l)
	}
	return &u, nil
}

// UserByEmail fetches a user by (normalized) email.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	return scanUser(s.queryRow(ctx, `SELECT `+userCols+` FROM users WHERE email=?`, email))
}

// GetUser fetches a user by id.
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	return scanUser(s.queryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

// CountUsers returns the number of accounts.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// SetUserPassword replaces a password hash and ends all the user's sessions.
func (s *Store) SetUserPassword(ctx context.Context, id, hash string) error {
	if err := s.execOne(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, id); err != nil {
		return err
	}
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE user_id=?`, id)
	return err
}

// TouchLogin records a successful login.
func (s *Store) TouchLogin(ctx context.Context, id string) error {
	return s.execOne(ctx, `UPDATE users SET last_login_at=? WHERE id=?`, ms(time.Now()), id)
}

// Member is a user's membership in an org.
type Member struct {
	User
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// AddMember upserts a membership.
func (s *Store) AddMember(ctx context.Context, orgID, userID, role string) error {
	if RoleRank(role) == 0 {
		return fmt.Errorf("invalid role %q", role)
	}
	_, err := s.exec(ctx, `INSERT INTO memberships(org_id,user_id,role,created_at) VALUES(?,?,?,?) ON CONFLICT(org_id,user_id) DO UPDATE SET role=excluded.role`,
		orgID, userID, role, ms(time.Now()))
	return err
}

// MemberRole returns a user's role in an org ("" if none).
func (s *Store) MemberRole(ctx context.Context, orgID, userID string) (string, error) {
	var r string
	err := s.queryRow(ctx, `SELECT role FROM memberships WHERE org_id=? AND user_id=?`, orgID, userID).Scan(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return r, err
}

// ListMembers lists an org's members.
func (s *Store) ListMembers(ctx context.Context, orgID string) ([]*Member, error) {
	rows, err := s.query(ctx, `SELECT u.id,u.email,u.name,u.password_hash,u.disabled,u.created_at,u.last_login_at,m.role,m.created_at
FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.org_id=? ORDER BY m.created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Member{}
	for rows.Next() {
		var m Member
		var d int
		var c, l, j int64
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.PasswordHash, &d, &c, &l, &m.Role, &j); err != nil {
			return nil, err
		}
		m.Disabled, m.CreatedAt, m.JoinedAt = d == 1, fromMS(c), fromMS(j)
		if l > 0 {
			m.LastLoginAt = fromMS(l)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// CountOwners counts an org's owners.
func (s *Store) CountOwners(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM memberships WHERE org_id=? AND role='owner'`, orgID).Scan(&n)
	return n, err
}

// RemoveMember deletes a membership and the user's sessions in that org.
func (s *Store) RemoveMember(ctx context.Context, orgID, userID string) error {
	if err := s.execOne(ctx, `DELETE FROM memberships WHERE org_id=? AND user_id=?`, orgID, userID); err != nil {
		return err
	}
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE user_id=? AND org_id=?`, userID, orgID)
	return err
}

// ---------- sessions ----------

// Session is a logged-in browser.
type Session struct {
	TokenHash  string    `json:"-"`
	UserID     string    `json:"user_id"`
	OrgID      string    `json:"org_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
}

// CreateSession stores a session; the caller keeps the plaintext token.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	if len(sess.UserAgent) > 256 {
		sess.UserAgent = sess.UserAgent[:256]
	}
	_, err := s.exec(ctx, `INSERT INTO sessions(token_hash,user_id,org_id,created_at,expires_at,last_seen_at,ip,user_agent) VALUES(?,?,?,?,?,?,?,?)`,
		sess.TokenHash, sess.UserID, sess.OrgID, ms(sess.CreatedAt), ms(sess.ExpiresAt), ms(sess.LastSeenAt), sess.IP, sess.UserAgent)
	return err
}

// GetSession returns a live session.
func (s *Store) GetSession(ctx context.Context, tokenHash string, now time.Time) (*Session, error) {
	var x Session
	var c, e, l int64
	err := s.queryRow(ctx, `SELECT token_hash,user_id,org_id,created_at,expires_at,last_seen_at,ip,user_agent FROM sessions WHERE token_hash=? AND expires_at>?`, tokenHash, ms(now)).
		Scan(&x.TokenHash, &x.UserID, &x.OrgID, &c, &e, &l, &x.IP, &x.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	x.CreatedAt, x.ExpiresAt, x.LastSeenAt = fromMS(c), fromMS(e), fromMS(l)
	return &x, err
}

// TouchSession slides a session's expiry.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, now, expires time.Time) error {
	_, err := s.exec(ctx, `UPDATE sessions SET last_seen_at=?, expires_at=? WHERE token_hash=?`, ms(now), ms(expires), tokenHash)
	return err
}

// SwitchSessionOrg changes the session's active org.
func (s *Store) SwitchSessionOrg(ctx context.Context, tokenHash, orgID string) error {
	return s.execOne(ctx, `UPDATE sessions SET org_id=? WHERE token_hash=?`, orgID, tokenHash)
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// PruneSessions removes expired sessions.
func (s *Store) PruneSessions(ctx context.Context, now time.Time) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE expires_at<=?`, ms(now))
	return err
}

// ---------- invites ----------

// Invite lets someone join an org with a role.
type Invite struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id"`
	Email      string    `json:"email"`
	Role       string    `json:"role"`
	TokenHash  string    `json:"-"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	AcceptedAt time.Time `json:"accepted_at,omitempty"`
}

// CreateInvite stores an invite.
func (s *Store) CreateInvite(ctx context.Context, in *Invite) error {
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return err
	}
	if RoleRank(in.Role) == 0 {
		return fmt.Errorf("invalid role %q", in.Role)
	}
	in.Email, in.ID, in.CreatedAt = email, secure.RandomID("inv_"), time.Now().UTC()
	_, err = s.exec(ctx, `INSERT INTO invites(id,org_id,email,role,token_hash,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		in.ID, in.OrgID, in.Email, in.Role, in.TokenHash, in.CreatedBy, ms(in.CreatedAt), ms(in.ExpiresAt))
	return err
}

const inviteCols = `id,org_id,email,role,token_hash,created_by,created_at,expires_at,accepted_at`

func scanInvite(sc interface{ Scan(...any) error }) (*Invite, error) {
	var in Invite
	var c, e, a int64
	if err := sc.Scan(&in.ID, &in.OrgID, &in.Email, &in.Role, &in.TokenHash, &in.CreatedBy, &c, &e, &a); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	in.CreatedAt, in.ExpiresAt = fromMS(c), fromMS(e)
	if a > 0 {
		in.AcceptedAt = fromMS(a)
	}
	return &in, nil
}

// InviteByToken returns a pending, unexpired invite.
func (s *Store) InviteByToken(ctx context.Context, tokenHash string, now time.Time) (*Invite, error) {
	return scanInvite(s.queryRow(ctx, `SELECT `+inviteCols+` FROM invites WHERE token_hash=? AND accepted_at=0 AND expires_at>?`, tokenHash, ms(now)))
}

// ListInvites lists an org's pending invites.
func (s *Store) ListInvites(ctx context.Context, orgID string) ([]*Invite, error) {
	rows, err := s.query(ctx, `SELECT `+inviteCols+` FROM invites WHERE org_id=? AND accepted_at=0 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Invite{}
	for rows.Next() {
		in, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// AcceptInvite marks an invite used (once).
func (s *Store) AcceptInvite(ctx context.Context, id string) error {
	return s.execOne(ctx, `UPDATE invites SET accepted_at=? WHERE id=? AND accepted_at=0`, ms(time.Now()), id)
}

// RevokeInvite deletes a pending invite.
func (s *Store) RevokeInvite(ctx context.Context, orgID, id string) error {
	return s.execOne(ctx, `DELETE FROM invites WHERE id=? AND org_id=? AND accepted_at=0`, id, orgID)
}

// ---------- audit log ----------

// AuditEntry records one admin action.
type AuditEntry struct {
	ID         string          `json:"id"`
	OrgID      string          `json:"org_id"`
	TS         time.Time       `json:"ts"`
	ActorType  string          `json:"actor_type"` // user | token | system
	ActorID    string          `json:"actor_id"`
	ActorEmail string          `json:"actor_email"`
	Action     string          `json:"action"`
	Target     string          `json:"target"`
	Status     int             `json:"status"`
	IP         string          `json:"ip"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

// InsertAudit appends an audit entry.
func (s *Store) InsertAudit(ctx context.Context, a *AuditEntry) error {
	if a.ID == "" {
		a.ID = secure.RandomID("aud_")
	}
	if a.TS.IsZero() {
		a.TS = time.Now().UTC()
	}
	d := string(a.Detail)
	if len(d) > 4000 {
		d = d[:4000]
	}
	_, err := s.exec(ctx, `INSERT INTO audit_log(id,org_id,ts,actor_type,actor_id,actor_email,action,target,status,ip,detail) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OrgID, ms(a.TS), a.ActorType, a.ActorID, a.ActorEmail, a.Action, a.Target, a.Status, a.IP, d)
	return err
}

// ListAudit returns an org's audit entries, newest first.
func (s *Store) ListAudit(ctx context.Context, orgID string, limit, offset int) ([]*AuditEntry, int, error) {
	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM audit_log WHERE org_id=?`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.query(ctx, `SELECT id,org_id,ts,actor_type,actor_id,actor_email,action,target,status,ip,detail FROM audit_log WHERE org_id=? ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`, orgID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		var a AuditEntry
		var ts int64
		var d string
		if err := rows.Scan(&a.ID, &a.OrgID, &ts, &a.ActorType, &a.ActorID, &a.ActorEmail, &a.Action, &a.Target, &a.Status, &a.IP, &d); err != nil {
			return nil, 0, err
		}
		a.TS = fromMS(ts)
		if d != "" && json.Valid([]byte(d)) {
			a.Detail = json.RawMessage(d)
		}
		out = append(out, &a)
	}
	return out, total, rows.Err()
}

// ---------- org scoping helpers ----------

// KeyOrg returns the org a virtual key belongs to.
func (s *Store) KeyOrg(ctx context.Context, keyID string) (string, error) {
	var o string
	err := s.queryRow(ctx, `SELECT p.org_id FROM virtual_keys k JOIN projects p ON p.id=k.project_id WHERE k.id=?`, keyID).Scan(&o)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return o, err
}
