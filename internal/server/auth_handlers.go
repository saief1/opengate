package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

func (s *Server) registerAuth(mux *http.ServeMux, h func(string, http.HandlerFunc)) {
	pub := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != csrfValue {
				writeError(w, http.StatusForbidden, "permission_error", "csrf", "missing "+csrfHeader+" header")
				return
			}
			fn(w, r)
		}))
	}
	pub("GET /admin/api/auth/config", s.authConfig)
	pub("POST /admin/api/auth/login", s.authLogin)
	pub("POST /admin/api/auth/logout", s.authLogout)
	pub("GET /admin/api/auth/me", s.authMe)
	pub("POST /admin/api/auth/switch-org", s.authSwitchOrg)
	// POST with the token in the body: tokens must never appear in URLs
	// (access logs, proxies, browser history).
	pub("POST /admin/api/auth/invites/preview", s.authInvitePreview)
	pub("POST /admin/api/auth/invites/accept", s.authAcceptInvite)
	pub("POST /admin/api/auth/signup", s.authSignup)
	pub("POST /admin/api/auth/password", s.authChangePassword)

	h("GET /admin/api/orgs", s.adminOrgs)
	h("POST /admin/api/orgs", s.adminCreateOrg)
	h("GET /admin/api/org", s.adminOrg)
	h("PATCH /admin/api/org", s.adminRenameOrg)
	h("GET /admin/api/members", s.adminMembers)
	h("PATCH /admin/api/members/{id}", s.adminPatchMember)
	h("DELETE /admin/api/members/{id}", s.adminRemoveMember)
	h("GET /admin/api/invites", s.adminInvites)
	h("POST /admin/api/invites", s.adminCreateInvite)
	h("DELETE /admin/api/invites/{id}", s.adminRevokeInvite)
	h("GET /admin/api/audit", s.adminAudit)
	h("GET /admin/api/settings", s.adminSettings)
}

func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	n, _ := s.store.CountUsers(r.Context())
	writeJSON(w, 200, map[string]any{"allow_signup": s.cfg.Auth.AllowSignup, "has_users": n > 0, "version": Version, "cloud": s.cloud != nil})
}

// startSession creates a session in org and sets the cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User, org string) error {
	tok := secure.RandomToken(32)
	now := time.Now().UTC()
	sess := &store.Session{TokenHash: secure.HashKey(tok), UserID: u.ID, OrgID: org, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(s.cfg.Auth.SessionTTL), IP: s.clientIP(r), UserAgent: r.UserAgent()}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		return err
	}
	s.setSessionCookie(w, r, tok, s.cfg.Auth.SessionMaxAge)
	_ = s.store.TouchLogin(r.Context(), u.ID)
	return nil
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	ip := s.clientIP(r)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	// Lock per account+IP (so a third party cannot lock a user out from
	// elsewhere) and, with a 5x higher threshold, per IP against spraying.
	acct, spray := "login|"+email+"|"+ip, "spray|"+ip
	for _, k := range []string{acct, spray} {
		if locked, d := s.lockout.locked(k); locked {
			w.Header().Set("Retry-After", itoa(int(d.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "login_locked_out", "Too many failed sign-in attempts; try again later.")
			return
		}
	}
	if !s.pwSlot(w, r) {
		return
	}
	defer s.pwFree()
	u, err := s.store.UserByEmail(r.Context(), email)
	ok := false
	if err == nil && !u.Disabled {
		ok = secure.VerifyPassword(in.Password, u.PasswordHash)
	} else {
		secure.VerifyDummy(in.Password) // equal timing for unknown users
	}
	if !ok {
		s.metrics.adminFails.Inc()
		s.lockout.fail(acct)
		s.sprayFail(spray)
		if u != nil {
			orgs, _ := s.store.ListOrgs(r.Context(), u.ID)
			for _, o := range orgs {
				s.auditSystem(r, o.ID, u, "auth.login_failed", http.StatusUnauthorized, nil)
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_credentials", "Invalid email or password.")
		return
	}
	s.lockout.success(acct)
	orgs, err := s.store.ListOrgs(r.Context(), u.ID)
	if err != nil || len(orgs) == 0 {
		writeError(w, http.StatusForbidden, "permission_error", "no_org", "This account is not a member of any organization.")
		return
	}
	if err := s.startSession(w, r, u, orgs[0].ID); err != nil {
		storeErr(w, err)
		return
	}
	s.auditSystem(r, orgs[0].ID, u, "auth.login", 200, nil)
	writeJSON(w, 200, map[string]any{"user": u, "org": orgs[0], "orgs": orgs})
}

// sprayFail counts per-IP failures toward a 5x higher threshold.
func (s *Server) sprayFail(key string) {
	s.sprayMu.Lock()
	defer s.sprayMu.Unlock()
	s.sprayCount[key]++
	if s.sprayCount[key] >= 5 {
		s.sprayCount[key] = 0
		s.lockout.fail(key)
	}
	if len(s.sprayCount) > 10000 {
		s.sprayCount = map[string]int{}
	}
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.store.DeleteSession(r.Context(), secure.HashKey(c.Value))
	}
	s.clearSessionCookie(w, r)
	writeJSON(w, 200, map[string]string{"status": "signed_out"})
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if s.cfg.AdminToken != "" && secure.ConstantTimeEqual(strings.TrimSpace(tok), s.cfg.AdminToken) {
			orgs, _ := s.store.ListOrgs(r.Context(), "")
			org := store.DefaultOrgID
			if h := r.Header.Get("X-OpenGate-Org"); h != "" {
				org = h
			}
			writeJSON(w, 200, map[string]any{"kind": "token", "role": store.RoleOwner, "org_id": org, "orgs": orgs, "version": Version})
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_admin_token", "admin token required")
		return
	}
	p, err := s.sessionPrincipal(r)
	if err != nil {
		storeErr(w, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "not signed in")
		return
	}
	orgs, _ := s.store.ListOrgs(r.Context(), p.User.ID)
	writeJSON(w, 200, map[string]any{"kind": "user", "user": p.User, "role": p.Role, "org_id": p.OrgID, "orgs": orgs, "version": Version})
}

func (s *Server) authSwitchOrg(w http.ResponseWriter, r *http.Request) {
	p, err := s.sessionPrincipal(r)
	if err != nil || p == nil {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "not signed in")
		return
	}
	var in struct {
		OrgID string `json:"org_id"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	role, err := s.store.MemberRole(r.Context(), in.OrgID, p.User.ID)
	if err != nil || role == "" {
		writeError(w, http.StatusForbidden, "permission_error", "not_a_member", "you are not a member of this organization")
		return
	}
	if err := s.store.SwitchSessionOrg(r.Context(), p.sessionHash, in.OrgID); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"org_id": in.OrgID, "role": role})
}

func (s *Server) authInvitePreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	inv, err := s.store.InviteByToken(r.Context(), secure.HashKey(in.Token), time.Now())
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", "invite_invalid", "This invite link is invalid, used or expired.")
		return
	}
	o, _ := s.store.GetOrg(r.Context(), inv.OrgID)
	_, uerr := s.store.UserByEmail(r.Context(), inv.Email)
	writeJSON(w, 200, map[string]any{"email": inv.Email, "role": inv.Role, "org": o.Name, "expires_at": inv.ExpiresAt, "existing_user": uerr == nil})
}

// authAcceptInvite joins the invited org: new users choose a password,
// existing users confirm theirs.
func (s *Server) authAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	ip := "invite|" + s.clientIP(r)
	if locked, _ := s.lockout.locked(ip); locked {
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "login_locked_out", "Too many failed attempts; try again later.")
		return
	}
	inv, err := s.store.InviteByToken(r.Context(), secure.HashKey(in.Token), time.Now())
	if err != nil {
		s.lockout.fail(ip)
		writeError(w, http.StatusNotFound, "invalid_request_error", "invite_invalid", "This invite link is invalid, used or expired.")
		return
	}
	if !s.pwSlot(w, r) {
		return
	}
	defer s.pwFree()
	u, err := s.store.UserByEmail(r.Context(), inv.Email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		hash, herr := secure.HashPassword(in.Password)
		if herr != nil {
			bad(w, herr)
			return
		}
		if u, err = s.store.CreateUser(r.Context(), inv.Email, in.Name, hash); err != nil {
			bad(w, err)
			return
		}
	case err != nil:
		storeErr(w, err)
		return
	default:
		if !secure.VerifyPassword(in.Password, u.PasswordHash) {
			s.lockout.fail(ip)
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_credentials", "Enter the password of your existing OpenGate account.")
			return
		}
	}
	cur, curErr := s.store.MemberRole(r.Context(), inv.OrgID, u.ID)
	if curErr != nil && !s.seatAvailable(w, r, inv.OrgID) {
		return
	}
	if err := s.store.AcceptInvite(r.Context(), inv.ID); err != nil {
		writeError(w, http.StatusConflict, "invalid_request_error", "invite_invalid", "This invite was already used.")
		return
	}
	// Never change an existing membership through an invite: keep the
	// higher of the current and invited roles (so the last owner can't be
	// demoted by accepting a lower-role invite).
	role := inv.Role
	if curErr == nil && store.RoleRank(cur) >= store.RoleRank(inv.Role) {
		role = cur
	} else if err := s.store.AddMember(r.Context(), inv.OrgID, u.ID, inv.Role); err != nil {
		storeErr(w, err)
		return
	}
	if err := s.startSession(w, r, u, inv.OrgID); err != nil {
		storeErr(w, err)
		return
	}
	s.auditSystem(r, inv.OrgID, u, "invite.accept", 200, map[string]string{"role": role, "invited_role": inv.Role, "invite_id": inv.ID})
	writeJSON(w, 200, map[string]any{"user": u, "org_id": inv.OrgID, "role": role})
}

// authSignup creates an account and a new organization (only when
// auth.allow_signup is on).
func (s *Server) authSignup(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Auth.AllowSignup {
		writeError(w, http.StatusForbidden, "permission_error", "signup_disabled", "Sign-up is disabled on this OpenGate; ask an admin for an invite.")
		return
	}
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		OrgName  string `json:"org_name"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if locked, _ := s.lockout.locked("signup|" + s.clientIP(r)); locked {
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "login_locked_out", "Too many attempts; try again later.")
		return
	}
	if !s.pwSlot(w, r) {
		return
	}
	defer s.pwFree()
	hash, err := secure.HashPassword(in.Password)
	if err != nil {
		bad(w, err)
		return
	}
	if strings.TrimSpace(in.OrgName) == "" {
		in.OrgName = strings.TrimSpace(in.Name + "'s organization")
	}
	u, err := s.store.CreateUser(r.Context(), in.Email, in.Name, hash)
	if errors.Is(err, store.ErrConflict) {
		s.lockout.fail("signup|" + s.clientIP(r))
		writeError(w, http.StatusConflict, "invalid_request_error", "email_taken", "An account with this email already exists; sign in instead.")
		return
	}
	if err != nil {
		bad(w, err)
		return
	}
	o, err := s.store.CreateOrg(r.Context(), in.OrgName)
	if err != nil {
		bad(w, err)
		return
	}
	if err := s.store.AddMember(r.Context(), o.ID, u.ID, store.RoleOwner); err != nil {
		storeErr(w, err)
		return
	}
	if err := s.startSession(w, r, u, o.ID); err != nil {
		storeErr(w, err)
		return
	}
	if s.cloud != nil {
		// Throttle account creation per IP (admin_lockout thresholds): a
		// burst of sign-ups from one address locks further sign-ups for a while.
		s.lockout.fail("signup|" + s.clientIP(r))
	}
	s.auditSystem(r, o.ID, u, "auth.signup", 201, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"user": u, "org": o})
}

func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	p, err := s.sessionPrincipal(r)
	if err != nil || p == nil {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "not signed in")
		return
	}
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if !s.pwSlot(w, r) {
		return
	}
	defer s.pwFree()
	if !secure.VerifyPassword(in.Current, p.User.PasswordHash) {
		s.lockout.fail("login|" + p.User.Email + "|" + s.clientIP(r))
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_credentials", "Current password is wrong.")
		return
	}
	hash, err := secure.HashPassword(in.New)
	if err != nil {
		bad(w, err)
		return
	}
	if err := s.store.SetUserPassword(r.Context(), p.User.ID, hash); err != nil { // ends all sessions
		storeErr(w, err)
		return
	}
	s.auditSystem(r, p.OrgID, p.User, "auth.password_changed", 200, nil)
	if err := s.startSession(w, r, p.User, p.OrgID); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "password_changed"})
}

// ---------- org, members, invites, audit ----------

func (s *Server) adminOrgs(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	uid := ""
	if p.Kind == "user" {
		uid = p.User.ID
	}
	orgs, err := s.store.ListOrgs(r.Context(), uid)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": orgs, "current": p.OrgID})
}

func (s *Server) adminCreateOrg(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if p.Kind != "token" && !s.cfg.Auth.AllowSignup {
		writeError(w, http.StatusForbidden, "permission_error", "org_creation_disabled", "Only the operator (admin token) can create organizations on this install.")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if p.Kind == "user" && !s.orgCreationAllowed(w, r, p.User.ID) {
		return
	}
	o, err := s.store.CreateOrg(r.Context(), in.Name)
	if err != nil {
		bad(w, err)
		return
	}
	if p.Kind == "user" {
		_ = s.store.AddMember(r.Context(), o.ID, p.User.ID, store.RoleOwner)
	}
	s.audit(r, "org.create", o.ID, map[string]string{"name": o.Name})
	writeJSON(w, http.StatusCreated, o)
}

func (s *Server) adminOrg(w http.ResponseWriter, r *http.Request) {
	o, err := s.store.GetOrg(r.Context(), orgID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	o.Role = principalOf(r).Role
	writeJSON(w, 200, o)
}

func (s *Server) adminRenameOrg(w http.ResponseWriter, r *http.Request) {
	if principalOf(r).Role != store.RoleOwner {
		writeError(w, http.StatusForbidden, "permission_error", "insufficient_role", "only owners can rename the organization")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if err := s.store.RenameOrg(r.Context(), orgID(r), in.Name); err != nil {
		bad(w, err)
		return
	}
	s.audit(r, "org.rename", orgID(r), in)
	s.adminOrg(w, r)
}

func (s *Server) adminMembers(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.ListMembers(r.Context(), orgID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": ms})
}

// canManage reports whether the caller may grant/revoke role target.
func canManage(p *principal, target string) bool {
	if target == store.RoleOwner {
		return p.Role == store.RoleOwner
	}
	return store.RoleRank(p.Role) >= store.RoleRank(store.RoleAdmin)
}

func (s *Server) adminPatchMember(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	var in struct {
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	uid := r.PathValue("id")
	cur, err := s.store.MemberRole(r.Context(), orgID(r), uid)
	if err != nil || cur == "" {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "member not found")
		return
	}
	if store.RoleRank(in.Role) == 0 {
		bad(w, errors.New("role must be owner, admin, member or viewer"))
		return
	}
	if !canManage(p, cur) || !canManage(p, in.Role) {
		writeError(w, http.StatusForbidden, "permission_error", "insufficient_role", "only owners can grant or revoke the owner role")
		return
	}
	if cur == store.RoleOwner && in.Role != store.RoleOwner {
		if n, _ := s.store.CountOwners(r.Context(), orgID(r)); n <= 1 {
			bad(w, errors.New("an organization needs at least one owner"))
			return
		}
	}
	if err := s.store.AddMember(r.Context(), orgID(r), uid, in.Role); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "member.role", uid, map[string]string{"from": cur, "to": in.Role})
	writeJSON(w, 200, map[string]string{"user_id": uid, "role": in.Role})
}

func (s *Server) adminRemoveMember(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	uid := r.PathValue("id")
	cur, err := s.store.MemberRole(r.Context(), orgID(r), uid)
	if err != nil || cur == "" {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "member not found")
		return
	}
	if !canManage(p, cur) {
		writeError(w, http.StatusForbidden, "permission_error", "insufficient_role", "only owners can remove owners")
		return
	}
	if cur == store.RoleOwner {
		if n, _ := s.store.CountOwners(r.Context(), orgID(r)); n <= 1 {
			bad(w, errors.New("an organization needs at least one owner"))
			return
		}
	}
	if err := s.store.RemoveMember(r.Context(), orgID(r), uid); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "member.remove", uid, map[string]string{"role": cur})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminInvites(w http.ResponseWriter, r *http.Request) {
	inv, err := s.store.ListInvites(r.Context(), orgID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": inv})
}

func (s *Server) adminCreateInvite(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if in.Role == "" {
		in.Role = store.RoleMember
	}
	if !s.planLimit(w, r, orgID(r), "members") {
		return
	}
	if store.RoleRank(in.Role) == 0 {
		bad(w, errors.New("role must be owner, admin, member or viewer"))
		return
	}
	if !canManage(p, in.Role) {
		writeError(w, http.StatusForbidden, "permission_error", "insufficient_role", "only owners can invite owners")
		return
	}
	tok := secure.RandomToken(32)
	inv := &store.Invite{OrgID: orgID(r), Email: in.Email, Role: in.Role, TokenHash: secure.HashKey(tok), ExpiresAt: time.Now().Add(s.cfg.Auth.InviteTTL).UTC()}
	if p.User != nil {
		inv.CreatedBy = p.User.Email
	} else {
		inv.CreatedBy = "admin token"
	}
	if err := s.store.CreateInvite(r.Context(), inv); err != nil {
		bad(w, err)
		return
	}
	s.audit(r, "invite.create", inv.ID, map[string]string{"email": inv.Email, "role": inv.Role})
	writeJSON(w, http.StatusCreated, map[string]any{"invite": inv, "token": tok, "path": "/ui/#invite=" + tok,
		"note": "Send this link to the invitee (OpenGate does not send email). It is shown only once."})
}

func (s *Server) adminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeInvite(r.Context(), orgID(r), r.PathValue("id")); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "invite.revoke", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := atoiDefault(q.Get("limit"), 100), atoiDefault(q.Get("offset"), 0)
	rows, total, err := s.store.ListAudit(r.Context(), orgID(r), limit, offset)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": rows, "total": total})
}

func atoiDefault(s string, d int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return d
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return d
		}
	}
	if s == "" {
		return d
	}
	return n
}

// adminSettings shows effective (non-secret) server settings.
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	c := s.cfg
	writeJSON(w, 200, map[string]any{
		"version": Version, "database": string(s.store.Dialect()), "rate_limit_backend": c.RateLimit.Backend,
		"budgets": map[string]any{"strict": c.Budgets.Strict, "sync_interval": c.Budgets.SyncInterval.String()},
		"metrics": map[string]any{"enabled": !c.Metrics.Disabled, "public": c.Metrics.Public},
		"auth": map[string]any{"allow_signup": c.Auth.AllowSignup, "session_ttl": c.Auth.SessionTTL.String(), "session_max_age": c.Auth.SessionMaxAge.String(),
			"invite_ttl": c.Auth.InviteTTL.String()},
		"admin_lockout": map[string]any{"max_failures": c.AdminLockout.MaxFailures, "window": c.AdminLockout.Window.String(), "lockout": c.AdminLockout.Lockout.String()},
		"cache":         map[string]any{"enabled": !c.Cache.Disabled, "max_ttl": c.Cache.MaxTTL.String(), "share_across_customers": c.Cache.ShareAcrossCustomers},
		"billing": map[string]any{"customer_header": c.Billing.CustomerHeader, "stripe_enabled": s.stripe != nil, "stripe_event_name": c.Billing.Stripe.EventName,
			"stripe_unit": c.Billing.Stripe.Unit},
		"guardrail_defaults": c.Guardrails, "body_log_max_bytes": c.BodyLogMaxBytes, "max_request_bytes": c.MaxRequestBytes,
		"share_env_provider_keys": c.ShareEnvProviderKeys, "pricing_source": s.pricing.Source(),
	})
}

// pwSlot bounds concurrent argon2id work (each verify/hash uses ~19 MiB and
// tens of ms of CPU), so unauthenticated login/signup/invite floods can't
// exhaust memory or outpace the lockout. It waits briefly, then answers 429.
func (s *Server) pwSlot(w http.ResponseWriter, r *http.Request) bool {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case s.pwSem <- struct{}{}:
		return true
	case <-t.C:
	case <-r.Context().Done():
	}
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusTooManyRequests, "rate_limit_error", "auth_busy", "Too many sign-in attempts in progress; retry shortly.")
	return false
}

func (s *Server) pwFree() { <-s.pwSem }
