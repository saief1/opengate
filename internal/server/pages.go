package server

import (
	"html/template"
	"net/http"
	"strings"
)

// pageTmpl is the public HTML (signup, login, legal). Styles live in
// /public.css so the page CSP can stay script-src 'self' and style-src 'self'.
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · OpenGate</title>
<link rel="stylesheet" href="/public.css">
</head>
<body>
<header class="bar">
  <a class="brand" href="/">OpenGate</a>
  <nav>
    <a href="/pricing">Pricing</a>
    <a href="/login">Sign in</a>
    <a class="btn" href="/signup">Start free</a>
  </nav>
</header>
<main>
  {{if .Draft}}<p class="draft">DRAFT — FOR LEGAL REVIEW. This page is placeholder text for OpenGate (Quebec, Canada). It is not legal advice and has not been reviewed by counsel. Do not rely on it until it is replaced.</p>{{end}}
  <h1>{{.Heading}}</h1>
  {{if .Lead}}<p class="lead">{{.Lead}}</p>{{end}}
  {{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
  {{if .Notice}}<p class="ok" role="status">{{.Notice}}</p>{{end}}
  {{template "body" .}}
</main>
<footer>
  <span>OpenGate · Quebec, Canada</span>
  <a href="/pricing">Pricing</a>
  <a href="/terms">Terms</a>
  <a href="/privacy">Privacy</a>
  <a href="/signup">Sign up</a>
</footer>
</body>
</html>
{{define "signup"}}
<form method="post" action="/signup">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  {{if .Plan}}<input type="hidden" name="plan" value="{{.Plan}}">{{end}}
  <label>Work email<input type="email" name="email" autocomplete="username" required value="{{.Email}}"></label>
  <label>Your name<input name="name" autocomplete="name" value="{{.Name}}"></label>
  <label>Password<input type="password" name="password" autocomplete="new-password" minlength="10" required></label>
  <label>Organization<input name="org_name" value="{{.Org}}"></label>
  <label class="check"><input type="checkbox" name="accept_terms" value="yes" required> I agree to the <a href="/terms">Terms</a> and <a href="/privacy">Privacy Policy</a>.</label>
  <button type="submit">Create account</button>
</form>
<p class="fine">Already have an account? <a href="/login">Sign in</a>. Operator access stays on the <a href="/ui/">dashboard admin token</a> and is not part of this form.</p>
{{end}}
{{define "login"}}
<form method="post" action="/login">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <label>Email<input type="email" name="email" autocomplete="username" required value="{{.Email}}"></label>
  <label>Password<input type="password" name="password" autocomplete="current-password" required></label>
  <button type="submit">Sign in</button>
</form>
<p class="fine"><a href="/forgot-password">Forgot password?</a> · <a href="/signup">Create an account</a></p>
<p class="fine">This page is for customer accounts. The operator admin token is a separate sign-in on the <a href="/ui/">dashboard</a>.</p>
{{end}}
{{define "logout"}}
<form method="post" action="/logout">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <button type="submit">Sign out</button>
</form>
{{end}}
{{define "forgot"}}
<form method="post" action="/forgot-password">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <label>Email<input type="email" name="email" autocomplete="username" required value="{{.Email}}"></label>
  <button type="submit">Send reset link</button>
</form>
{{end}}
{{define "reset"}}
<form method="post" action="/reset-password">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <input type="hidden" name="token" value="{{.Token}}">
  <label>New password<input type="password" name="password" autocomplete="new-password" minlength="10" required></label>
  <button type="submit">Update password</button>
</form>
{{end}}
{{define "message"}}{{end}}
{{define "legal"}}<article class="legal">{{.HTML}}</article>{{end}}
`))

type pageView struct {
	Title, Heading, Lead, Error, Notice string
	Email, Name, Org, Plan, CSRF, Token string
	Draft                               bool
	HTML                                template.HTML
	Body                                string
}

func (s *Server) renderPage(w http.ResponseWriter, status int, v pageView) {
	if v.Body == "" {
		v.Body = "message"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	name := v.Body
	// Execute the layout, which calls the body template named by the page.
	t, err := pageTmpl.Clone()
	if err != nil {
		s.log.Error("clone page template", "err", err)
		return
	}
	if _, err := t.New("body").Parse(`{{template "` + name + `" .}}`); err != nil {
		s.log.Error("page body", "err", err, "page", name)
		return
	}
	if err := t.Execute(w, v); err != nil {
		s.log.Error("render page", "err", err, "page", name)
	}
}

func legalArticle(paragraphs ...string) template.HTML {
	var b strings.Builder
	for _, p := range paragraphs {
		b.WriteString("<p>")
		b.WriteString(template.HTMLEscapeString(p))
		b.WriteString("</p>")
	}
	return template.HTML(b.String())
}
