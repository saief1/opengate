package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/opengate-dev/opengate/internal/secure"
	"github.com/opengate-dev/opengate/internal/store"
)

const (
	sessionCookie = "og_session"
	// csrfHeader must accompany every state-changing cookie-authenticated
	// request. Browsers cannot add custom headers cross-origin without a
	// CORS preflight (which OpenGate never approves), and the cookie is
	// SameSite=Strict on top.
	csrfHeader = "X-Requested-With"
	csrfValue  = "opengate"
)

// principal is the authenticated admin caller.
type principal struct {
	Kind        string // "token" (OPENGATE_ADMIN_TOKEN, operator) or "user"
	User        *store.User
	OrgID       string
	Role        string
	sessionHash string
	// audit details set by handlers
	auditAction, auditTarget string
	auditDetail              any
}

type principalKey struct{}

func principalOf(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey{}).(*principal)
	return p
}

// orgID is the organization an admin request acts on.
func orgID(r *http.Request) string {
	if p := principalOf(r); p != nil && p.OrgID != "" {
		return p.OrgID
	}
	return store.DefaultOrgID
}

// audit attaches a descriptive action/target/detail to the request's audit
// entry (written by the admin middleware for every state change).
func (s *Server) audit(r *http.Request, action, target string, detail any) {
	if p := principalOf(r); p != nil {
		p.auditAction, p.auditTarget, p.auditDetail = action, target, detail
	}
}

// minRole returns the role an admin route requires.
func minRole(method, pattern string) string {
	switch pattern {
	case "POST /admin/api/routing/preview", "POST /admin/api/guardrails/test":
		return store.RoleViewer // dry runs, no state change
	case "POST /admin/api/keys", "POST /admin/api/keys/{id}/kill", "POST /admin/api/keys/{id}/unkill",
		"POST /admin/api/projects/{id}/kill", "POST /admin/api/projects/{id}/unkill":
		return store.RoleMember // members create keys and may always pull the kill switch
	case "PATCH /admin/api/org", "DELETE /admin/api/members/{id}":
		return store.RoleAdmin // further owner checks happen in the handlers
	}
	if method == http.MethodGet || method == http.MethodHead {
		return store.RoleViewer
	}
	return store.RoleAdmin
}

func (s *Server) cookieSecure(r *http.Request) bool {
	if s.cfg.Auth.SecureCookies || r.TLS != nil {
		return true
	}
	if !s.cfg.TrustProxyHeaders {
		return false
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return s.fromTrustedProxy(host) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.cookieSecure(r),
		SameSite: http.SameSiteStrictMode, MaxAge: int(maxAge.Seconds())})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

// sessionPrincipal resolves the session cookie (nil if absent/invalid).
func (s *Server) sessionPrincipal(r *http.Request) (*principal, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	h := secure.HashKey(c.Value)
	now := time.Now()
	sess, err := s.store.GetSession(r.Context(), h, now)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if now.Sub(sess.CreatedAt) > s.cfg.Auth.SessionMaxAge {
		_ = s.store.DeleteSession(r.Context(), h)
		return nil, nil
	}
	u, err := s.store.GetUser(r.Context(), sess.UserID)
	if err != nil || u.Disabled {
		return nil, nil
	}
	role, err := s.store.MemberRole(r.Context(), sess.OrgID, u.ID)
	if err != nil {
		return nil, err
	}
	if now.Sub(sess.LastSeenAt) > time.Minute { // sliding expiry, throttled
		_ = s.store.TouchSession(r.Context(), h, now, now.Add(s.cfg.Auth.SessionTTL))
	}
	return &principal{Kind: "user", User: u, OrgID: sess.OrgID, Role: role, sessionHash: h}, nil
}

type auditRecorder struct {
	http.ResponseWriter
	status int
}

func (a *auditRecorder) WriteHeader(c int) {
	if a.status == 0 {
		a.status = c
	}
	a.ResponseWriter.WriteHeader(c)
}

func (a *auditRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(b)
}

// adminAuth authenticates (admin token or session), enforces the route's
// minimum role and CSRF header, and audits every state-changing request.
func (s *Server) adminAuth(pattern string, next http.HandlerFunc) http.Handler {
	method, _, _ := strings.Cut(pattern, " ")
	need := minRole(method, pattern)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ip := s.clientIP(r)
		lk := "token|" + ip
		var p *principal
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			// The correct token always works: lockout only stops guessing,
			// so nobody can lock the real operator out.
			if s.cfg.AdminToken == "" || !secure.ConstantTimeEqual(strings.TrimSpace(tok), s.cfg.AdminToken) {
				if locked, d := s.lockout.locked(lk); locked {
					w.Header().Set("Retry-After", itoa(int(d.Seconds())+1))
					writeError(w, http.StatusTooManyRequests, "rate_limit_error", "admin_locked_out",
						"Too many failed admin authentications from this address; try again later.")
					return
				}
				s.metrics.adminFails.Inc()
				if s.lockout.fail(lk) {
					s.log.Warn("admin lockout", "remote_ip", ip, "duration", s.cfg.AdminLockout.Lockout.String())
				}
				writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_admin_token", "admin token required")
				return
			}
			s.lockout.success(lk)
			org := store.DefaultOrgID
			if h := strings.TrimSpace(r.Header.Get("X-OpenGate-Org")); h != "" {
				if _, err := s.store.GetOrg(r.Context(), h); err != nil {
					writeError(w, http.StatusNotFound, "invalid_request_error", "org_not_found", "unknown organization in X-OpenGate-Org")
					return
				}
				org = h
			}
			p = &principal{Kind: "token", OrgID: org, Role: store.RoleOwner}
		} else {
			var err error
			if p, err = s.sessionPrincipal(r); err != nil {
				storeErr(w, err)
				return
			}
			if p == nil {
				writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "sign in or send the admin token")
				return
			}
			if method != http.MethodGet && method != http.MethodHead && r.Header.Get(csrfHeader) != csrfValue {
				writeError(w, http.StatusForbidden, "permission_error", "csrf", "missing "+csrfHeader+" header")
				return
			}
			if p.Role == "" {
				writeError(w, http.StatusForbidden, "permission_error", "not_a_member", "you are not a member of this organization")
				return
			}
		}
		if store.RoleRank(p.Role) < store.RoleRank(need) {
			writeError(w, http.StatusForbidden, "permission_error", "insufficient_role", "this action requires the "+need+" role")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
		if method == http.MethodGet || method == http.MethodHead {
			next(w, r)
			return
		}
		rec := &auditRecorder{ResponseWriter: w}
		next(rec, r)
		s.writeAudit(r, p, pattern, rec.status, ip)
	})
}

func (s *Server) writeAudit(r *http.Request, p *principal, pattern string, status int, ip string) {
	if status == 0 {
		status = http.StatusOK
	}
	a := &store.AuditEntry{OrgID: p.OrgID, ActorType: p.Kind, Status: status, IP: ip, Action: p.auditAction, Target: p.auditTarget}
	if p.User != nil {
		a.ActorID, a.ActorEmail = p.User.ID, p.User.Email
	}
	if a.Action == "" {
		a.Action = pattern
		a.Target = r.URL.Path
	}
	if p.auditDetail != nil {
		a.Detail, _ = json.Marshal(p.auditDetail)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.InsertAudit(ctx, a); err != nil {
		s.log.Error("audit log write failed", "err", err, "action", a.Action)
	}
}

// auditSystem records an event outside an authenticated admin request
// (logins, invite acceptance).
func (s *Server) auditSystem(r *http.Request, orgID string, u *store.User, action string, status int, detail any) {
	a := &store.AuditEntry{OrgID: orgID, ActorType: "user", Action: action, Status: status, IP: s.clientIP(r)}
	if u != nil {
		a.ActorID, a.ActorEmail, a.Target = u.ID, u.Email, u.ID
	}
	if detail != nil {
		a.Detail, _ = json.Marshal(detail)
	}
	if err := s.store.InsertAudit(context.Background(), a); err != nil {
		s.log.Error("audit log write failed", "err", err, "action", action)
	}
}
