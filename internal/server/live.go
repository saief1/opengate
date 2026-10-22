package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/saief1/opengate/internal/budget"
	"github.com/saief1/opengate/internal/store"
)

// ---------- live event hub (dashboard SSE) ----------

type liveEvent struct {
	Type string
	Data any
}

type liveHub struct {
	mu   sync.Mutex
	subs map[chan liveEvent]string // channel -> org
	done chan struct{}
	once sync.Once
}

const maxLiveSubscribers = 200

func newLiveHub() *liveHub {
	return &liveHub{subs: map[chan liveEvent]string{}, done: make(chan struct{})}
}

func (h *liveHub) subscribe(org string) (chan liveEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= maxLiveSubscribers {
		return nil, false
	}
	ch := make(chan liveEvent, 64)
	h.subs[ch] = org
	return ch, true
}

func (h *liveHub) unsubscribe(ch chan liveEvent) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// publish fans an event out to the org's subscribers; slow ones drop events
// (the dashboard re-syncs on its periodic tick).
func (h *liveHub) publish(org, typ string, data any) {
	if org == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, o := range h.subs {
		if o != org {
			continue
		}
		select {
		case ch <- liveEvent{Type: typ, Data: data}:
		default:
		}
	}
}

func (h *liveHub) close() { h.once.Do(func() { close(h.done) }) }

func (h *liveHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// orgOfProject resolves (and caches) a project's org.
func (s *Server) orgOfProject(ctx context.Context, id string) string {
	if v, ok := s.projOrg.Load(id); ok {
		return v.(string)
	}
	p, err := s.store.GetProject(ctx, id)
	if err != nil {
		return ""
	}
	s.projOrg.Store(id, p.OrgID)
	return p.OrgID
}

// publishLog pushes a finished request to live dashboards (bodies stripped).
func (s *Server) publishLog(ctx context.Context, l *store.RequestLog) {
	if s.hub.count() == 0 {
		return
	}
	c := *l
	c.RequestBody, c.ResponseBody = "", ""
	org := s.orgOfProject(ctx, c.ProjectID)
	s.hub.publish(org, "request", &c)
}

func (s *Server) publishMCP(c *store.MCPCall) {
	if s.hub.count() == 0 {
		return
	}
	cp := *c
	s.hub.publish(cp.OrgID, "mcp", &cp)
}

// onAlert forwards budget alerts and kill-switch changes to live dashboards.
func (s *Server) onAlert(e budget.Event) {
	if s.hub.count() == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		org := ""
		switch e.Scope {
		case "org":
			org = e.ScopeID
		case store.ScopeProject:
			org = s.orgOfProject(ctx, e.ScopeID)
		case store.ScopeKey:
			if k, err := s.store.GetKey(ctx, e.ScopeID); err == nil {
				org = s.orgOfProject(ctx, k.ProjectID)
			}
		case store.ScopeAgent:
			p, _, _ := strings.Cut(e.ScopeID, "/")
			org = s.orgOfProject(ctx, p)
		}
		typ := "alert"
		if e.Kind == "kill_switch" {
			typ = "kill"
		}
		s.hub.publish(org, typ, e)
	}()
}

// adminStream is a Server-Sent Events feed of the caller's org: new request
// log rows, MCP calls, alerts, kill-switch changes and a spend tick.
func (s *Server) adminStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	org := orgID(r)
	ch, ok := s.hub.subscribe(org)
	if !ok {
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, "server_error", "too_many_streams", "Too many live streams; the dashboard falls back to polling.")
		return
	}
	defer s.hub.unsubscribe(ch)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(typ string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	tick := func() bool {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		now := time.Now().UTC()
		sm, err := s.store.SummarySince(ctx, store.StatsFilter{OrgID: org}, budget.PeriodStart(budget.Daily, now))
		if err != nil {
			return true
		}
		return send("tick", map[string]any{"time": now, "today": sm})
	}
	if !send("hello", map[string]any{"org_id": org, "demo": s.demoReady(org), "time": time.Now().UTC()}) || !tick() {
		return
	}
	tt := time.NewTicker(10 * time.Second)
	defer tt.Stop()
	pt := time.NewTicker(15 * time.Second)
	defer pt.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.done:
			return
		case ev := <-ch:
			if !send(ev.Type, ev.Data) {
				return
			}
		case <-tt.C:
			if !tick() {
				return
			}
		case <-pt.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// ---------- small per-IP rate limiter for the public demo endpoints ----------

type ipLimiter struct {
	mu     sync.Mutex
	per    int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

func newIPLimiter(per int, window time.Duration) *ipLimiter {
	return &ipLimiter{per: per, window: window, hits: map[string][]time.Time{}, now: time.Now}
}

// allow records a hit for key and reports whether it is within the limit.
func (l *ipLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	if len(l.hits) > 10000 { // bound memory under a spray
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	h := l.hits[key]
	i := 0
	for i < len(h) && h[i].Before(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= l.per {
		l.hits[key] = h
		return false, h[0].Sub(cut) + time.Second
	}
	l.hits[key] = append(h, now)
	return true, 0
}
