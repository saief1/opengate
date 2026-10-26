package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/mailer"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

const (
	formCSRFCookie   = "og_csrf"
	verifyTokenTTL   = 24 * time.Hour
	resetTokenTTL    = time.Hour
	signupPerIP      = 5
	signupPerIPEvery = 10 * time.Minute
)

type signupInput struct {
	Email       string `json:"email"`
	Name        string `json:"name"`
	Password    string `json:"password"`
	OrgName     string `json:"org_name"`
	AcceptTerms bool   `json:"accept_terms"`
}

type loginFailure struct {
	status       int
	typ, code    string
	msg          string
	retryAfter   time.Duration
	wroteAlready bool
}

// loginUser checks credentials, starts a session on success, and leaves the
// response unwritten unless the argon2 gate already answered.
func (s *Server) loginUser(w http.ResponseWriter, r *http.Request, email, password string) (*store.User, *store.Org, []*store.Org, *loginFailure) {
	ip := s.clientIP(r)
	email = strings.ToLower(strings.TrimSpace(email))
	acct, spray := "login|"+email+"|"+ip, "spray|"+ip
	for _, k := range []string{acct, spray} {
		if locked, d := s.lockout.locked(k); locked {
			return nil, nil, nil, &loginFailure{status: http.StatusTooManyRequests, typ: "rate_limit_error", code: "login_locked_out",
				msg: "Too many failed sign-in attempts; try again later.", retryAfter: d}
		}
	}
	if !s.pwSlot(w, r) {
		return nil, nil, nil, &loginFailure{wroteAlready: true}
	}
	defer s.pwFree()
	u, err := s.store.UserByEmail(r.Context(), email)
	ok := false
	if err == nil && !u.Disabled {
		ok = secure.VerifyPassword(password, u.PasswordHash)
	} else {
		secure.VerifyDummy(password)
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
		return nil, nil, nil, &loginFailure{status: http.StatusUnauthorized, typ: "invalid_request_error", code: "invalid_credentials", msg: "Invalid email or password."}
	}
	if !u.EmailVerified {
		return nil, nil, nil, &loginFailure{status: http.StatusForbidden, typ: "permission_error", code: "email_unverified",
			msg: "Confirm your email before signing in. Check your inbox for the verification link."}
	}
	s.lockout.success(acct)
	orgs, err := s.store.ListOrgs(r.Context(), u.ID)
	if err != nil || len(orgs) == 0 {
		return nil, nil, nil, &loginFailure{status: http.StatusForbidden, typ: "permission_error", code: "no_org", msg: "This account is not a member of any organization."}
	}
	if err := s.startSession(w, r, u, orgs[0].ID); err != nil {
		return nil, nil, nil, &loginFailure{status: http.StatusInternalServerError, typ: "server_error", code: "internal_error", msg: "internal error (see server log)"}
	}
	s.auditSystem(r, orgs[0].ID, u, "auth.login", 200, nil)
	return u, orgs[0], orgs, nil
}

func (s *Server) writeLoginFailure(w http.ResponseWriter, f *loginFailure) {
	if f == nil || f.wroteAlready {
		return
	}
	if f.retryAfter > 0 {
		w.Header().Set("Retry-After", itoa(int(f.retryAfter.Seconds())+1))
	}
	writeError(w, f.status, f.typ, f.code, f.msg)
}

// signupCustomer creates a user and an organization they own.
// verificationRequired is true when a real mailer is configured: no session
// is started until the address is confirmed.
func (s *Server) signupCustomer(w http.ResponseWriter, r *http.Request, in signupInput) (*store.User, *store.Org, bool, *loginFailure) {
	if !s.cfg.Auth.AllowSignup {
		return nil, nil, false, &loginFailure{status: http.StatusForbidden, typ: "permission_error", code: "signup_disabled",
			msg: "Sign-up is disabled on this OpenGate; ask an admin for an invite."}
	}
	ip := s.clientIP(r)
	if locked, d := s.lockout.locked("signup|" + ip); locked {
		return nil, nil, false, &loginFailure{status: http.StatusTooManyRequests, typ: "rate_limit_error", code: "login_locked_out",
			msg: "Too many attempts; try again later.", retryAfter: d}
	}
	if ok, retry := s.signupLimit.allow(ip); !ok {
		return nil, nil, false, &loginFailure{status: http.StatusTooManyRequests, typ: "rate_limit_error", code: "rate_limited",
			msg: "Too many accounts created from this address; try again later.", retryAfter: retry}
	}
	if ok, retry := s.signupAll.allow("all"); !ok {
		return nil, nil, false, &loginFailure{status: http.StatusTooManyRequests, typ: "rate_limit_error", code: "rate_limited",
			msg: "Sign-up is temporarily busy; try again later.", retryAfter: retry}
	}
	if !s.pwSlot(w, r) {
		return nil, nil, false, &loginFailure{wroteAlready: true}
	}
	defer s.pwFree()
	hash, err := secure.HashPassword(in.Password)
	if err != nil {
		return nil, nil, false, &loginFailure{status: http.StatusBadRequest, typ: "invalid_request_error", code: "invalid_request", msg: err.Error()}
	}
	if strings.TrimSpace(in.OrgName) == "" {
		in.OrgName = strings.TrimSpace(in.Name + "'s organization")
	}
	u, err := s.store.CreateUser(r.Context(), in.Email, in.Name, hash)
	if errors.Is(err, store.ErrConflict) {
		s.lockout.fail("signup|" + ip)
		return nil, nil, false, &loginFailure{status: http.StatusConflict, typ: "invalid_request_error", code: "email_taken",
			msg: "An account with this email already exists; sign in instead."}
	}
	if err != nil {
		return nil, nil, false, &loginFailure{status: http.StatusBadRequest, typ: "invalid_request_error", code: "invalid_request", msg: err.Error()}
	}
	o, err := s.store.CreateOrg(r.Context(), in.OrgName)
	if err != nil {
		return nil, nil, false, &loginFailure{status: http.StatusBadRequest, typ: "invalid_request_error", code: "invalid_request", msg: err.Error()}
	}
	if err := s.store.AddMember(r.Context(), o.ID, u.ID, store.RoleOwner); err != nil {
		return nil, nil, false, &loginFailure{status: http.StatusInternalServerError, typ: "server_error", code: "internal_error", msg: "internal error (see server log)"}
	}
	verify := s.mail != nil && s.mail.Configured()
	if verify {
		if err := s.store.SetEmailVerified(r.Context(), u.ID, time.Time{}); err != nil {
			return nil, nil, false, &loginFailure{status: http.StatusInternalServerError, typ: "server_error", code: "internal_error", msg: "internal error (see server log)"}
		}
		u.EmailVerified = false
		u.EmailVerifiedAt = time.Time{}
		if err := s.sendVerify(r, u); err != nil {
			s.log.Error("verification email failed", "err", err, "user", u.ID)
		}
	}
	if s.cloud != nil {
		s.lockout.fail("signup|" + ip)
	}
	s.auditSystem(r, o.ID, u, "auth.signup", http.StatusCreated, map[string]any{"verification_required": verify})
	return u, o, verify, nil
}

func (s *Server) publicBase(r *http.Request) string {
	if u := strings.TrimRight(strings.TrimSpace(s.cfg.Cloud.PublicURL), "/"); u != "" {
		return u
	}
	scheme := "http"
	if s.requestIsHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) sendVerify(r *http.Request, u *store.User) error {
	plain, err := s.store.IssueEmailToken(r.Context(), u.ID, store.EmailTokenVerify, verifyTokenTTL)
	if err != nil {
		return err
	}
	link := s.publicBase(r) + "/verify-email?token=" + url.QueryEscape(plain)
	return s.mail.Send(r.Context(), mailer.Message{
		To: u.Email, Subject: "Verify your OpenGate email",
		Text: "Confirm your email to finish creating your OpenGate account:\n\n" + link +
			"\n\nThis link expires in 24 hours and works once. If you did not sign up, you can ignore this message.\n",
	})
}

func (s *Server) sendReset(r *http.Request, u *store.User) error {
	plain, err := s.store.IssueEmailToken(r.Context(), u.ID, store.EmailTokenReset, resetTokenTTL)
	if err != nil {
		return err
	}
	link := s.publicBase(r) + "/reset-password?token=" + url.QueryEscape(plain)
	return s.mail.Send(r.Context(), mailer.Message{
		To: u.Email, Subject: "Reset your OpenGate password",
		Text: "A password reset was requested for your OpenGate account:\n\n" + link +
			"\n\nThis link expires in 1 hour and works once. If you did not ask for a reset, you can ignore this message.\n",
	})
}

func (s *Server) ensureFormCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(formCSRFCookie); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	tok := secure.RandomToken(32)
	http.SetCookie(w, &http.Cookie{
		Name: formCSRFCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: s.cookieSecure(r), SameSite: http.SameSiteStrictMode, MaxAge: 3600,
	})
	return tok
}

func (s *Server) formCSRFMatch(r *http.Request) bool {
	c, err := r.Cookie(formCSRFCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return secure.ConstantTimeEqual(c.Value, r.FormValue("csrf"))
}

func wantsJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.Contains(ct, "application/json") || strings.Contains(r.Header.Get("Accept"), "application/json")
}

// acceptPublicPost checks the CSRF header for JSON and the double-submit
// cookie for HTML forms.
func (s *Server) acceptPublicPost(w http.ResponseWriter, r *http.Request) bool {
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if r.Header.Get(csrfHeader) != csrfValue {
			writeError(w, http.StatusForbidden, "permission_error", "csrf", "missing "+csrfHeader+" header")
			return false
		}
		return true
	}
	if err := r.ParseForm(); err != nil {
		bad(w, err)
		return false
	}
	if !s.formCSRFMatch(r) {
		writeError(w, http.StatusForbidden, "permission_error", "csrf", "missing or invalid form token")
		return false
	}
	return true
}

func (s *Server) resetAllowed(w http.ResponseWriter, r *http.Request, email string) bool {
	ip := s.clientIP(r)
	if ok, retry := s.resetLimit.allow(ip); !ok {
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limited", "Too many reset requests; try again later.")
		return false
	}
	if email != "" {
		if ok, retry := s.resetAcct.allow(strings.ToLower(email)); !ok {
			w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limited", "Too many reset requests for this email; try again later.")
			return false
		}
	}
	return true
}
