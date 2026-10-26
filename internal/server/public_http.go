package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

func (s *Server) registerPublic(mux *http.ServeMux) {
	mux.HandleFunc("GET /signup", s.pageSignup)
	mux.HandleFunc("POST /signup", s.postSignup)
	mux.HandleFunc("GET /login", s.pageLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("GET /logout", s.pageLogout)
	mux.HandleFunc("POST /logout", s.postLogout)
	mux.HandleFunc("GET /forgot-password", s.pageForgot)
	mux.HandleFunc("POST /forgot-password", s.postForgot)
	mux.HandleFunc("GET /reset-password", s.pageReset)
	mux.HandleFunc("POST /reset-password", s.postReset)
	mux.HandleFunc("GET /verify-email", s.getVerify)
	mux.HandleFunc("POST /verify-email", s.postVerify)
	mux.HandleFunc("POST /verify-email/resend", s.postResend)
	mux.HandleFunc("GET /terms", s.pageTerms)
	mux.HandleFunc("GET /privacy", s.pagePrivacy)
	mux.HandleFunc("GET /pricing", s.pagePricing)
	if s.webFS != nil {
		mux.HandleFunc("GET /public.css", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			http.ServeFileFS(w, r, s.webFS, "public.css")
		})
	}
}

func (s *Server) pageSignup(w http.ResponseWriter, r *http.Request) {
	csrf := s.ensureFormCSRF(w, r)
	errMsg := ""
	if !s.cfg.Auth.AllowSignup {
		errMsg = "Sign-up is disabled on this gateway. Ask an admin for an invite."
	}
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Create account", Heading: "Create your account",
		Lead:  "Creates an organization you own. Email verification is required when an email provider is configured.",
		Error: errMsg, CSRF: csrf, Plan: r.URL.Query().Get("plan"), Body: "signup",
	})
}

func (s *Server) postSignup(w http.ResponseWriter, r *http.Request) {
	if !s.acceptPublicPost(w, r) {
		return
	}
	in := signupInput{}
	jsonOut := wantsJSON(r) && strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if jsonOut {
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
	} else {
		in.Email = r.FormValue("email")
		in.Name = r.FormValue("name")
		in.Password = r.FormValue("password")
		in.OrgName = r.FormValue("org_name")
		in.AcceptTerms = r.FormValue("accept_terms") == "yes"
	}
	if !in.AcceptTerms {
		s.failSignup(w, r, in, &loginFailure{status: http.StatusBadRequest, typ: "invalid_request_error", code: "terms_required",
			msg: "Agree to the Terms and Privacy Policy to create an account."}, jsonOut)
		return
	}
	u, o, verify, fail := s.signupCustomer(w, r, in)
	if fail != nil {
		s.failSignup(w, r, in, fail, jsonOut)
		return
	}
	if !verify {
		if err := s.startSession(w, r, u, o.ID); err != nil {
			storeErr(w, err)
			return
		}
	}
	if jsonOut {
		writeJSON(w, http.StatusCreated, map[string]any{"user": u, "org": o, "verification_required": verify})
		return
	}
	if verify {
		http.Redirect(w, r, "/login?notice=check-email", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) failSignup(w http.ResponseWriter, r *http.Request, in signupInput, fail *loginFailure, jsonOut bool) {
	if fail.wroteAlready {
		return
	}
	if jsonOut {
		s.writeLoginFailure(w, fail)
		return
	}
	s.renderPage(w, fail.status, pageView{
		Title: "Create account", Heading: "Create your account", Error: fail.msg,
		Email: in.Email, Name: in.Name, Org: in.OrgName, Plan: r.FormValue("plan"),
		CSRF: s.ensureFormCSRF(w, r), Body: "signup",
	})
}

func (s *Server) pageLogin(w http.ResponseWriter, r *http.Request) {
	notice := ""
	if r.URL.Query().Get("notice") == "check-email" {
		notice = "Check your email for a verification link, then sign in."
	}
	if r.URL.Query().Get("notice") == "reset" {
		notice = "Password updated. Sign in with the new password."
	}
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Sign in", Heading: "Sign in", Notice: notice,
		CSRF: s.ensureFormCSRF(w, r), Body: "login",
	})
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if !s.acceptPublicPost(w, r) {
		return
	}
	email, password := r.FormValue("email"), r.FormValue("password")
	jsonOut := strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if jsonOut {
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		email, password = in.Email, in.Password
	}
	u, org, orgs, fail := s.loginUser(w, r, email, password)
	if fail != nil {
		if fail.wroteAlready {
			return
		}
		if jsonOut {
			s.writeLoginFailure(w, fail)
			return
		}
		s.renderPage(w, fail.status, pageView{
			Title: "Sign in", Heading: "Sign in", Error: fail.msg, Email: email,
			CSRF: s.ensureFormCSRF(w, r), Body: "login",
		})
		return
	}
	if jsonOut {
		writeJSON(w, 200, map[string]any{"user": u, "org": org, "orgs": orgs})
		return
	}
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) pageLogout(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Sign out", Heading: "Sign out", Lead: "This ends your customer session.",
		CSRF: s.ensureFormCSRF(w, r), Body: "logout",
	})
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if !s.acceptPublicPost(w, r) {
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.store.DeleteSession(r.Context(), secure.HashKey(c.Value))
	}
	s.clearSessionCookie(w, r)
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, 200, map[string]string{"status": "signed_out"})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) pageForgot(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Reset password", Heading: "Reset your password",
		Lead: "If an account exists for that email, we will send a one-time link that expires in one hour.",
		CSRF: s.ensureFormCSRF(w, r), Body: "forgot",
	})
}

func (s *Server) postForgot(w http.ResponseWriter, r *http.Request) {
	if !s.acceptPublicPost(w, r) {
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	jsonOut := strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if jsonOut {
		var in struct {
			Email string `json:"email"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		email = in.Email
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !s.resetAllowed(w, r, email) {
		return
	}
	if u, err := s.store.UserByEmail(r.Context(), email); err == nil && !u.Disabled && s.mail != nil {
		if err := s.sendReset(r, u); err != nil {
			s.log.Error("password reset email failed", "err", err)
		}
	}
	const msg = "If an account exists for that email, a reset link is on its way."
	if jsonOut {
		writeJSON(w, 200, map[string]string{"status": "ok", "message": msg})
		return
	}
	s.renderPage(w, http.StatusOK, pageView{Title: "Reset password", Heading: "Check your email", Notice: msg, Body: "message"})
}

func (s *Server) pageReset(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Choose a new password", Heading: "Choose a new password",
		Token: r.URL.Query().Get("token"), CSRF: s.ensureFormCSRF(w, r), Body: "reset",
	})
}

func (s *Server) postReset(w http.ResponseWriter, r *http.Request) {
	if !s.acceptPublicPost(w, r) {
		return
	}
	token, password := r.FormValue("token"), r.FormValue("password")
	jsonOut := strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if jsonOut {
		var in struct {
			Token    string `json:"token"`
			Password string `json:"password"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		token, password = in.Token, in.Password
	}
	if !s.resetAllowed(w, r, "") {
		return
	}
	uid, err := s.store.ConsumeEmailToken(r.Context(), store.EmailTokenReset, token, time.Now())
	if err != nil {
		s.failReset(w, r, token, "This reset link is invalid, used, or expired.", jsonOut)
		return
	}
	if !s.pwSlot(w, r) {
		return
	}
	defer s.pwFree()
	hash, err := secure.HashPassword(password)
	if err != nil {
		s.failReset(w, r, token, err.Error(), jsonOut)
		return
	}
	if err := s.store.SetUserPassword(r.Context(), uid, hash); err != nil {
		storeErr(w, err)
		return
	}
	_ = s.store.SetEmailVerified(r.Context(), uid, time.Now().UTC())
	if u, err := s.store.GetUser(r.Context(), uid); err == nil {
		s.auditSystem(r, "", u, "auth.password_reset", 200, nil)
	}
	if jsonOut {
		writeJSON(w, 200, map[string]string{"status": "password_reset"})
		return
	}
	http.Redirect(w, r, "/login?notice=reset", http.StatusSeeOther)
}

func (s *Server) failReset(w http.ResponseWriter, r *http.Request, token, msg string, jsonOut bool) {
	if jsonOut {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "token_invalid", msg)
		return
	}
	s.renderPage(w, http.StatusBadRequest, pageView{
		Title: "Choose a new password", Heading: "Choose a new password", Error: msg,
		Token: token, CSRF: s.ensureFormCSRF(w, r), Body: "reset",
	})
}

func (s *Server) getVerify(w http.ResponseWriter, r *http.Request) {
	s.finishVerify(w, r, r.URL.Query().Get("token"), false)
}

func (s *Server) postVerify(w http.ResponseWriter, r *http.Request) {
	if !s.acceptPublicPost(w, r) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	token := r.FormValue("token")
	jsonOut := strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if jsonOut {
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		token = in.Token
	}
	s.finishVerify(w, r, token, jsonOut)
}

func (s *Server) finishVerify(w http.ResponseWriter, r *http.Request, token string, jsonOut bool) {
	uid, err := s.store.ConsumeEmailToken(r.Context(), store.EmailTokenVerify, token, time.Now())
	if err != nil {
		msg := "This verification link is invalid, used, or expired."
		if jsonOut {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "token_invalid", msg)
			return
		}
		s.renderPage(w, http.StatusBadRequest, pageView{Title: "Verify email", Heading: "Link not valid", Error: msg, Body: "message"})
		return
	}
	if err := s.store.SetEmailVerified(r.Context(), uid, time.Now().UTC()); err != nil {
		storeErr(w, err)
		return
	}
	u, err := s.store.GetUser(r.Context(), uid)
	if err != nil {
		storeErr(w, err)
		return
	}
	orgs, _ := s.store.ListOrgs(r.Context(), u.ID)
	orgID := ""
	if len(orgs) > 0 {
		orgID = orgs[0].ID
		if err := s.startSession(w, r, u, orgID); err != nil {
			storeErr(w, err)
			return
		}
	}
	s.auditSystem(r, orgID, u, "auth.email_verified", 200, nil)
	if jsonOut {
		writeJSON(w, 200, map[string]any{"status": "verified", "user": u})
		return
	}
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) postResend(w http.ResponseWriter, r *http.Request) {
	if !s.acceptPublicPost(w, r) {
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	email := r.FormValue("email")
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		email = in.Email
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !s.resetAllowed(w, r, email) {
		return
	}
	if u, err := s.store.UserByEmail(r.Context(), email); err == nil && !u.EmailVerified && s.mail != nil {
		if err := s.sendVerify(r, u); err != nil {
			s.log.Error("resend verification failed", "err", err)
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) pageTerms(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Terms of use", Heading: "Terms of use", Draft: true, Body: "legal",
		HTML: legalArticle(
			"These terms are a placeholder for OpenGate, based in Quebec, Canada. Replace this page with counsel-approved terms before accepting customers.",
			"OpenGate provides an AI gateway: request routing, budgets, logs, and related hosted services. A customer account is an organization. The person who signs up is the owner of that organization.",
			"The service is provided as available. Draft plans and prices on the pricing page are not a binding offer until a paid subscription is confirmed in Stripe.",
			"You must not use the service to break the law, to probe or disrupt systems you do not operate, or to submit credentials or secrets you are not allowed to share with a processor.",
			"Governing law for this placeholder is the law of Quebec, Canada, and the federal laws of Canada that apply there. This sentence is not a jurisdiction clause until counsel confirms it.",
			"Contact before launch: legal@opengatehq.com (placeholder address — not monitored until you configure it).",
		),
	})
}

func (s *Server) pagePrivacy(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Privacy", Heading: "Privacy", Draft: true, Body: "legal",
		HTML: legalArticle(
			"This privacy notice is a placeholder for OpenGate, Quebec, Canada. It must be reviewed under Quebec's Act respecting the protection of personal information in the private sector (Law 25) and PIPEDA before launch.",
			"Account data we expect to store: name, email address, password hash (argon2id), organization membership, session records, and billing identifiers from Stripe. Provider API keys are encrypted with the deployment master key.",
			"Request logs may contain prompts and model output when a project turns body logging on. Treat that as customer content. The public demo playground, when enabled, is mock-only and is not this production service.",
			"Email verification and password reset use single-use tokens that expire. We send those messages only through the mail provider you configure (Resend or SMTP).",
			"We do not sell personal information. Subprocessors you must list before launch include the host (for example Fly.io), the database operator, the email provider, and Stripe for paid plans.",
			"To ask for access or deletion once the service is live, write to privacy@opengatehq.com (placeholder — not monitored until you configure it).",
		),
	})
}

func (s *Server) pagePricing(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, pageView{
		Title: "Pricing", Heading: "Pricing", Draft: true, Body: "legal",
		Lead: "Draft catalog for OpenGate Cloud. Stripe is the source of truth for anything you charge.",
		HTML: legalArticle(
			"Free — $0. 10,000 requests per month, 3 members, 2 projects, 7-day request logs. Self-serve sign-up.",
			"Team — $49 per month (draft). 1,000,000 requests per month, 25 members, unlimited projects, 30-day logs, email support. 14-day trial with no card, only for an organization's first subscription, when Stripe test or live prices are configured.",
			"Enterprise — custom. Unlimited requests, contract terms, 1-year logs, priority support. Not self-serve.",
			"These numbers match the draft plan catalog in the cloud configuration. Do not advertise them as final until finance and counsel sign off. Taxes for Quebec, Canada are not included until Stripe Tax is turned on.",
			"Start on the free plan at /signup. Paid checkout and the customer portal stay in test mode until you follow the launch checklist.",
		),
	})
}
