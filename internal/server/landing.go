package server

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

var landingRelRef = regexp.MustCompile(`(href|src)="([^"]*)"`)

// landingHTML renders web/static/landing.html for serving at the site root:
// relative asset refs are pointed at /ui/, and with billing disabled the paid
// plan CTA becomes a plain free sign-up labelled "Coming soon".
func (s *Server) landingHTML() ([]byte, error) {
	raw, err := fs.ReadFile(s.webFS, "landing.html")
	if err != nil {
		return nil, err
	}
	html := landingRelRef.ReplaceAllStringFunc(string(raw), func(m string) string {
		sub := landingRelRef.FindStringSubmatch(m)
		v := sub[2]
		if v == "" || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "#") || strings.Contains(v, ":") {
			return m
		}
		return sub[1] + `="/ui/` + v + `"`
	})
	if s.cfg.Cloud.BillingDisabled {
		html = billingOffLanding(html)
	}
	return []byte(html), nil
}

var paidCTA = regexp.MustCompile(`href="/signup\?plan=[^"]*">[^<]*</a>`)

func billingOffLanding(html string) string {
	html = paidCTA.ReplaceAllString(html, `href="/signup">Coming soon · start free</a>`)
	html = strings.Replace(html, `<span class="pill x43">Recommended</span>`, `<span class="pill x43">Coming soon</span>`, 1)
	return html
}

func (s *Server) serveLanding(w http.ResponseWriter, r *http.Request) {
	b, err := s.landingHTML()
	if err != nil {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}
