package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// Hop is one upstream attempt of a request (retries and failovers).
type Hop struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Status    int       `json:"status"` // HTTP status from the provider; 0 = no response (network error)
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// OK reports whether the attempt succeeded.
func (h Hop) OK() bool { return h.Status > 0 && h.Status < 400 && h.Error == "" }

const maxHops = 32

func encodeHops(h []Hop) string {
	if len(h) == 0 {
		return ""
	}
	if len(h) > maxHops {
		h = h[len(h)-maxHops:]
	}
	for i := range h {
		if len(h[i].Error) > 500 {
			h[i].Error = h[i].Error[:500] + "…"
		}
	}
	b, _ := json.Marshal(h)
	return string(b)
}

func decodeHops(s string) []Hop {
	if s == "" {
		return nil
	}
	var h []Hop
	if json.Unmarshal([]byte(s), &h) != nil {
		return nil
	}
	return h
}

// ---------- kill switches beyond keys and projects ----------

// Kill switch scopes stored in kill_switches.
const (
	KillGlobal   = "global"   // scope_id = "" : every request in the org
	KillAgent    = "agent"    // scope_id = project_id + "/" + agent
	KillCustomer = "customer" // scope_id = customer_id (org-wide)
)

// KillSwitch is an active org, agent or customer kill.
type KillSwitch struct {
	Scope    string    `json:"scope"`
	ScopeID  string    `json:"scope_id"`
	KilledAt time.Time `json:"killed_at"`
	Actor    string    `json:"actor,omitempty"`
}

// ValidKillScope reports whether scope can be stored in kill_switches.
func ValidKillScope(scope string) bool {
	return scope == KillGlobal || scope == KillAgent || scope == KillCustomer
}

// SetKillSwitch turns a kill switch on or off (idempotent).
func (s *Store) SetKillSwitch(ctx context.Context, orgID, scope, scopeID, actor string, on bool) error {
	if !ValidKillScope(scope) {
		return errors.New("scope must be global, agent or customer")
	}
	if scope == KillGlobal {
		scopeID = ""
	} else if strings.TrimSpace(scopeID) == "" {
		return errors.New("scope_id is required")
	}
	if !on {
		_, err := s.exec(ctx, `DELETE FROM kill_switches WHERE org_id=? AND scope=? AND scope_id=?`, orgID, scope, scopeID)
		return err
	}
	_, err := s.exec(ctx, `INSERT INTO kill_switches(org_id,scope,scope_id,killed_at,actor) VALUES(?,?,?,?,?) ON CONFLICT(org_id,scope,scope_id) DO NOTHING`,
		orgID, scope, scopeID, ms(time.Now()), actor)
	return err
}

// ListKillSwitches returns an org's active kill switches.
func (s *Store) ListKillSwitches(ctx context.Context, orgID string) ([]KillSwitch, error) {
	rows, err := s.query(ctx, `SELECT scope,scope_id,killed_at,actor FROM kill_switches WHERE org_id=? ORDER BY killed_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KillSwitch{}
	for rows.Next() {
		var k KillSwitch
		var at int64
		if err := rows.Scan(&k.Scope, &k.ScopeID, &at, &k.Actor); err != nil {
			return nil, err
		}
		k.KilledAt = fromMS(at)
		out = append(out, k)
	}
	return out, rows.Err()
}

// KillHit returns the first kill switch matching a request ("" = none).
// agentID is project_id/agent ("" if the key has no agent label).
func (s *Store) KillHit(ctx context.Context, orgID, agentID, customerID string) (string, error) {
	rows, err := s.query(ctx, `SELECT scope FROM kill_switches WHERE org_id=? AND ((scope='global' AND scope_id='') OR (scope='agent' AND scope_id=?) OR (scope='customer' AND scope_id=?))`,
		orgID, agentID, customerID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hit := ""
	for rows.Next() {
		var sc string
		if err := rows.Scan(&sc); err != nil {
			return "", err
		}
		if sc == KillGlobal || hit == "" {
			hit = sc
		}
	}
	return hit, rows.Err()
}

// ---------- measured performance ----------

// ModelLatency is the measured latency of one provider/model.
type ModelLatency struct {
	Model    string  `json:"model"` // provider/provider_model that served the request
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
	Requests int     `json:"requests"` // successful, non-cached requests measured
}

// ProviderSuccess is the measured attempt success rate of one provider.
type ProviderSuccess struct {
	Provider   string  `json:"provider"`
	Attempts   int     `json:"attempts"`
	OK         int     `json:"ok"`
	SuccessPct float64 `json:"success_pct"`
}

// Performance computes p50/p95 latency per model and success % per provider
// from request logs since t (most recent 100k rows). Per-provider success is
// counted per upstream attempt when hops are logged, so a failed first hop
// counts against its provider even if failover rescued the request. Requests
// refused before any provider and cache hits are not counted.
func (s *Store) Performance(ctx context.Context, f StatsFilter, since time.Time) ([]ModelLatency, []ProviderSuccess, error) {
	w, wa := f.where()
	rows, err := s.query(ctx, `SELECT provider, provider_model, status, latency_ms, hops FROM request_logs WHERE ts>=? AND provider<>'' AND cache_hit=0`+w+` ORDER BY ts DESC LIMIT 100000`,
		append([]any{ms(since)}, wa...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	lat := map[string][]float64{}
	type cnt struct{ n, ok int }
	prov := map[string]*cnt{}
	add := func(p string, ok bool) {
		c := prov[p]
		if c == nil {
			c = &cnt{}
			prov[p] = c
		}
		c.n++
		if ok {
			c.ok++
		}
	}
	for rows.Next() {
		var p, m string
		var status int
		var l float64
		var hops sql.NullString
		if err := rows.Scan(&p, &m, &status, &l, &hops); err != nil {
			return nil, nil, err
		}
		if status < 400 {
			lat[p+"/"+m] = append(lat[p+"/"+m], l)
		}
		if hs := decodeHops(hops.String); len(hs) > 0 {
			for _, h := range hs {
				add(h.Provider, h.OK())
			}
		} else if status != 499 {
			add(p, status < 400)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	models := []ModelLatency{}
	for m, v := range lat {
		sort.Float64s(v)
		at := func(q float64) float64 { return v[int(q*float64(len(v)-1))] }
		models = append(models, ModelLatency{Model: m, P50MS: at(.5), P95MS: at(.95), Requests: len(v)})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Requests != models[j].Requests {
			return models[i].Requests > models[j].Requests
		}
		return models[i].Model < models[j].Model
	})
	provs := []ProviderSuccess{}
	for p, c := range prov {
		provs = append(provs, ProviderSuccess{Provider: p, Attempts: c.n, OK: c.ok, SuccessPct: float64(int(float64(c.ok)/float64(c.n)*1000+0.5)) / 10})
	}
	sort.Slice(provs, func(i, j int) bool {
		if provs[i].Attempts != provs[j].Attempts {
			return provs[i].Attempts > provs[j].Attempts
		}
		return provs[i].Provider < provs[j].Provider
	})
	return models, provs, nil
}
