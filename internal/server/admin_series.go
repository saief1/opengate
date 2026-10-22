package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/store"
)

// adminSeries powers the dashboard charts: a rolling window (default 24h)
// summary, p50/p95 latency and a spend series grouped by one dimension.
//
//	GET /admin/api/series?by=provider|agent|key|customer|model|blocked&hours=24&bucket=hour|day&project_id=
func (s *Server) adminSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, _ := strconv.Atoi(q.Get("hours"))
	if hours <= 0 || hours > 24*365 {
		hours = 24
	}
	bucket := time.Hour
	if q.Get("bucket") == "day" || (q.Get("bucket") == "" && hours > 72) {
		bucket = 24 * time.Hour
	}
	by := q.Get("by")
	if by == "" {
		by = "provider"
	}
	now := time.Now().UTC()
	since := now.Add(-time.Duration(hours) * time.Hour).Truncate(bucket)
	f := store.StatsFilter{OrgID: orgID(r), ProjectID: q.Get("project_id")}
	series, err := s.store.GroupSeries(r.Context(), f, by, since, bucket)
	if err != nil {
		bad(w, err)
		return
	}
	sum, _ := s.store.SummarySince(r.Context(), f, since)
	prev, _ := s.store.SummarySince(r.Context(), f, since.Add(-time.Duration(hours)*time.Hour))
	// prev covers two windows; subtract the current one to get the previous window.
	prev.Requests -= sum.Requests
	prev.CostUSD -= sum.CostUSD
	prev.Errors -= sum.Errors
	prev.Blocked -= sum.Blocked
	prev.Tokens -= sum.Tokens
	p50, p95, n, _ := s.store.LatencyPercentiles(r.Context(), f, since)
	writeJSON(w, 200, map[string]any{
		"by": by, "since": since, "now": now, "hours": hours, "bucket_seconds": int(bucket.Seconds()),
		"series": series, "summary": sum,
		"previous": map[string]any{"requests": prev.Requests, "cost_usd": prev.CostUSD, "errors": prev.Errors, "blocked": prev.Blocked, "tokens": prev.Tokens},
		"latency":  map[string]any{"p50_ms": p50, "p95_ms": p95, "samples": n},
	})
}

// adminAlerts reports whether budget alert webhooks are configured (host only,
// never the full URL) and the org's most recent threshold alerts.
func (s *Server) adminAlerts(w http.ResponseWriter, r *http.Request) {
	names, ids := s.orgProjects(r)
	owned := map[string]string{}
	for _, id := range ids {
		owned["project\x00"+id] = names[id]
	}
	if ks, err := s.orgKeys(r, ""); err == nil {
		for _, k := range ks {
			owned["key\x00"+k.ID] = k.Name
		}
	}
	all, err := s.store.RecentAlerts(r.Context(), 500)
	if err != nil {
		storeErr(w, err)
		return
	}
	type row struct {
		store.SentAlert
		Label string `json:"label"`
	}
	recent := []row{}
	for _, a := range all {
		label, ok := owned[a.Scope+"\x00"+a.ScopeID]
		if !ok && a.Scope == "agent" {
			if pid, agent, found := strings.Cut(a.ScopeID, "/"); found {
				if pn, mine := names[pid]; mine {
					label, ok = agent+" @ "+pn, true
				}
			}
		}
		if ok {
			recent = append(recent, row{a, label})
			if len(recent) == 20 {
				break
			}
		}
	}
	host := ""
	if u, err := url.Parse(s.cfg.Alerts.WebhookURL); err == nil && s.cfg.Alerts.WebhookURL != "" {
		host = u.Host
	}
	writeJSON(w, 200, map[string]any{
		"configured": s.cfg.Alerts.WebhookURL != "", "webhook_host": host,
		"thresholds": s.cfg.Alerts.Thresholds, "recent": recent,
	})
}
