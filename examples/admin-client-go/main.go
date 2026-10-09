// Command admin-client-go is a minimal, dependency-free Go client for the
// OpenGate admin API: it creates a key with a budget, lists today's spend and
// pulls the kill switch. Usage:
//
//	OPENGATE_URL=http://localhost:8080 OPENGATE_ADMIN_TOKEN=... go run ./examples/admin-client-go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// Client talks to /admin/api with the admin token (or use a session cookie).
type Client struct {
	BaseURL, Token, Org string
	HTTP                *http.Client
}

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+"/admin/api"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	if c.Org != "" {
		req.Header.Set("X-OpenGate-Org", c.Org) // act on a non-default org
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Message, Code string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: %d %s (%s)", method, path, resp.StatusCode, e.Error.Message, e.Error.Code)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// VirtualKey mirrors components.schemas.VirtualKey in docs/openapi.yaml.
type VirtualKey struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	Killed    bool   `json:"killed"`
}

func main() {
	c := &Client{BaseURL: env("OPENGATE_URL", "http://localhost:8080"), Token: os.Getenv("OPENGATE_ADMIN_TOKEN"), HTTP: http.DefaultClient}

	// 1. Create a key for an agent with a $5/day budget and 60 rpm.
	var created struct {
		Key        string     `json:"key"` // plaintext, shown once
		VirtualKey VirtualKey `json:"virtual_key"`
	}
	err := c.do("POST", "/keys", map[string]any{"name": "nightly-summarizer", "agent": "summarizer",
		"limits": map[string]any{"daily_usd": 5, "rpm": 60}}, &created)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("new key:", created.Key, "id:", created.VirtualKey.ID)

	// 2. Today's spend.
	var ov struct {
		Today struct {
			Requests int     `json:"requests"`
			CostUSD  float64 `json:"cost_usd"`
		} `json:"today"`
	}
	if err := c.do("GET", "/overview", nil, &ov); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("today: %d requests, $%.4f\n", ov.Today.Requests, ov.Today.CostUSD)

	// 3. Kill switch: blocks the key and aborts its in-flight streams.
	var killed struct {
		Aborted int `json:"aborted_inflight"`
	}
	if err := c.do("POST", "/keys/"+created.VirtualKey.ID+"/kill", nil, &killed); err != nil {
		log.Fatal(err)
	}
	fmt.Println("killed; aborted in-flight:", killed.Aborted)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
