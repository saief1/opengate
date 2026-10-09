package budget

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Event is a webhook payload. "text" makes it directly usable as a Slack
// incoming-webhook message; the other fields are for machine consumers.
type Event struct {
	Text      string    `json:"text"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope,omitempty"`
	ScopeID   string    `json:"scope_id,omitempty"`
	ScopeName string    `json:"scope_name,omitempty"`
	Period    string    `json:"period,omitempty"`
	Threshold float64   `json:"threshold,omitempty"`
	SpentUSD  float64   `json:"spent_usd,omitempty"`
	LimitUSD  float64   `json:"limit_usd,omitempty"`
	Time      time.Time `json:"time"`
}

// sendHook lets tests observe events.
var sendHook func(Event)

// Alerter delivers events to a webhook asynchronously.
type Alerter struct {
	URL        string
	Thresholds []float64
	client     *http.Client
	log        *slog.Logger
	queue      chan Event
}

// NewAlerter starts a delivery worker. With an empty URL events are only logged.
func NewAlerter(url string, thresholds []float64, log *slog.Logger) *Alerter {
	a := &Alerter{URL: url, Thresholds: thresholds, client: &http.Client{Timeout: 10 * time.Second}, log: log, queue: make(chan Event, 256)}
	go a.loop()
	return a
}

// Send enqueues an event without blocking the request path.
func (a *Alerter) Send(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	a.log.Warn("alert", "kind", e.Kind, "text", e.Text)
	if sendHook != nil {
		sendHook(e)
	}
	if a.URL == "" {
		return
	}
	select {
	case a.queue <- e:
	default:
		a.log.Error("alert queue full; dropping alert", "text", e.Text)
	}
}

func (a *Alerter) loop() {
	for e := range a.queue {
		body, _ := json.Marshal(e)
		for attempt := 0; attempt < 3; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := a.client.Do(req)
			cancel()
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode < 300 {
					break
				}
			}
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}
}
