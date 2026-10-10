// Command opengate is a self-hostable, secure-by-default AI gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/saief1/opengate/internal/config"
	"github.com/saief1/opengate/internal/mockprovider"
	"github.com/saief1/opengate/internal/pricing"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/server"
	"github.com/saief1/opengate/internal/store"
	"github.com/saief1/opengate/web"
)

const usage = `OpenGate — secure-by-default AI gateway

Usage:
  opengate [serve] [-config opengate.yaml]   run the gateway (default)
  opengate keygen                            print a fresh master key and admin token
  opengate create-key -name NAME [-project P] [-agent A] [-user U] [-config ...]
                                             create a virtual key directly in the database
  opengate create-user -email E [-name N] [-role owner] [-org ORG_ID] [-config ...]
                                             create a dashboard account (password from
                                             OPENGATE_NEW_PASSWORD or stdin)
  opengate pricing                           print the built-in price table (YAML)
  opengate mock-provider [-listen :9090]     run a fake OpenAI/Anthropic/Gemini upstream (dev/demo)
  opengate demo-seed [-days 30] [-config ...] insert synthetic historical traffic (demo only)
  opengate healthcheck                       exit 0 if the local gateway answers /healthz (for containers)
  opengate version

Environment: OPENGATE_CONFIG, OPENGATE_ADMIN_TOKEN, OPENGATE_MASTER_KEY, OPENGATE_LISTEN,
OPENGATE_DATABASE, OPENGATE_PRICING_FILE, OPENGATE_ALERT_WEBHOOK_URL, <PROVIDER>_API_KEY.
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "keygen":
		err = keygen()
	case "create-key":
		err = createKey(args)
	case "create-user":
		err = createUser(args)
	case "pricing":
		_, err = os.Stdout.Write(pricing.DefaultYAML)
	case "mock-provider":
		err = mock(args)
	case "demo-seed":
		err = demoSeed(args)
	case "healthcheck":
		err = healthcheck()
	case "version":
		fmt.Println(server.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, error) {
	path := fs.String("config", os.Getenv("OPENGATE_CONFIG"), "path to YAML config")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Load(*path)
}

// newLogger builds the structured logger from config (JSON by default).
func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(strings.ToUpper(cfg.Log.Level)))
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if cfg.Log.Format == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h).With("service", "opengate")
}

// redactDSN hides the password of a postgres:// URL for logging.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

func serve(args []string) error {
	cfg, err := loadConfig(flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	slog.SetDefault(log)
	if err := cfg.ValidateSecrets(); err != nil {
		return fmt.Errorf("%w\n\nRun `opengate keygen` to generate secure values", err)
	}
	mk, err := secure.ParseMasterKey(cfg.MasterKey)
	if err != nil {
		return err
	}
	cipher, err := secure.NewCipher(mk)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return fmt.Errorf("open database %s: %w", redactDSN(cfg.Database), err)
	}
	defer st.Close()
	prices, err := pricing.NewRegistry(cfg.PricingFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv, err := server.New(ctx, server.Options{Config: cfg, Store: st, Cipher: cipher, Pricing: prices, Logger: log, WebFS: web.FS()})
	if err != nil {
		return err
	}
	go srv.Run(ctx)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := prices.Reload(); err != nil {
				log.Error("pricing reload failed", "err", err)
			} else {
				log.Info("pricing reloaded", "source", prices.Source())
			}
		}
	}()
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("OpenGate listening", "addr", cfg.Listen, "version", server.Version, "dashboard", "http://"+cfg.Listen+"/ui/", "pricing", prices.Source(), "database", redactDSN(cfg.Database), "db_backend", string(st.Dialect()), "rate_limit_backend", cfg.RateLimit.Backend)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	}
	return nil
}

func keygen() error {
	mk, err := secure.GenerateMasterKey()
	if err != nil {
		return err
	}
	tok, err := secure.GenerateMasterKey()
	if err != nil {
		return err
	}
	fmt.Printf("OPENGATE_MASTER_KEY=%s\nOPENGATE_ADMIN_TOKEN=%s\n", mk, "oga_"+tok[:43])
	fmt.Fprintln(os.Stderr, "# Store these in your secret manager. Losing the master key makes stored provider keys unrecoverable.")
	return nil
}

func createKey(args []string) error {
	fs := flag.NewFlagSet("create-key", flag.ExitOnError)
	name := fs.String("name", "", "key name (required)")
	project := fs.String("project", "default", "project name (created if missing)")
	agent := fs.String("agent", "", "agent label")
	user := fs.String("user", "", "user label")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-name is required")
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	p, err := st.EnsureProject(ctx, *project)
	if err != nil {
		return err
	}
	plain, k, err := st.CreateKey(ctx, p.ID, *name, *agent, *user)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created key %s (%s) in project %q — store it now, it is not shown again\n", k.ID, k.Name, p.Name)
	fmt.Println(plain)
	return nil
}

// createUser bootstraps a dashboard account (e.g. the first owner).
func createUser(args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ExitOnError)
	email := fs.String("email", "", "email address (login)")
	name := fs.String("name", "", "display name")
	role := fs.String("role", store.RoleOwner, "owner|admin|member|viewer")
	org := fs.String("org", store.DefaultOrgID, "organization id")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if store.RoleRank(*role) == 0 {
		return fmt.Errorf("invalid role %q", *role)
	}
	pw := os.Getenv("OPENGATE_NEW_PASSWORD")
	if pw == "" {
		fmt.Fprint(os.Stderr, "Password (min 10 chars, read from stdin): ")
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return err
		}
		pw = strings.TrimRight(string(b), "\r\n")
		fmt.Fprintln(os.Stderr)
	}
	hash, err := secure.HashPassword(pw)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.GetOrg(ctx, *org); err != nil {
		return fmt.Errorf("organization %q: %w", *org, err)
	}
	u, err := st.CreateUser(ctx, *email, *name, hash)
	if err != nil {
		return err
	}
	if err := st.AddMember(ctx, *org, u.ID, *role); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created %s (%s) as %s of %s\n", u.Email, u.ID, *role, *org)
	return nil
}

func mock(args []string) error {
	fs := flag.NewFlagSet("mock-provider", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9090", "listen address")
	jitter := fs.Duration("jitter", 150*time.Millisecond, "random extra latency per request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "mock provider on http://%s  (base URLs: /openai/v1, /openrouter/v1, /anthropic, /gemini)\n", *listen)
	hs := &http.Server{Addr: *listen, Handler: mockprovider.New(mockprovider.Options{Jitter: *jitter, ChunkDelay: 15 * time.Millisecond}), ReadHeaderTimeout: 10 * time.Second}
	return hs.ListenAndServe()
}

// demoSeed writes synthetic, backdated request logs for existing keys so the
// dashboard has history to show. It never contacts any provider.
func demoSeed(args []string) error {
	fs := flag.NewFlagSet("demo-seed", flag.ExitOnError)
	days := fs.Int("days", 30, "days of history")
	perDay := fs.Int("per-day", 120, "approx requests per day")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()
	prices, err := pricing.NewRegistry(cfg.PricingFile)
	if err != nil {
		return err
	}
	ctx := context.Background()
	keys, err := st.ListKeys(ctx, "")
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("create some keys first")
	}
	models := []struct{ alias, prov, model string }{
		{"openai/gpt-4o-mini", "openai", "gpt-4o-mini"}, {"smart", "anthropic", "claude-sonnet-4-5"},
		{"anthropic/claude-haiku-4-5", "anthropic", "claude-haiku-4-5"}, {"gemini/gemini-2.5-flash", "gemini", "gemini-2.5-flash"},
		{"openai/gpt-4o", "openai", "gpt-4o"}, {"openrouter/meta-llama/llama-3.3-70b-instruct", "openrouter", "meta-llama/llama-3.3-70b-instruct"},
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	n := 0
	for d := *days; d >= 1; d-- {
		day := today.AddDate(0, 0, -d)
		growth := 0.6 + 0.8*float64(*days-d)/float64(*days)
		count := int(float64(*perDay) * growth * (0.7 + 0.6*rand.Float64()))
		for i := 0; i < count; i++ {
			k := keys[rand.IntN(len(keys))]
			if k.Revoked {
				continue
			}
			m := models[rand.IntN(len(models))]
			pt, ct := 300+rand.IntN(4000), 50+rand.IntN(900)
			cost, _ := prices.Table().Cost(m.prov, m.model, pt, ct)
			l := &store.RequestLog{ID: secure.RandomID("req_"), TS: day.Add(time.Duration(rand.Int64N(int64(24 * time.Hour)))),
				ProjectID: k.ProjectID, KeyID: k.ID, Agent: k.Agent, UserLabel: k.UserLabel, Model: m.alias, Provider: m.prov, ProviderModel: m.model,
				PromptTokens: pt, CompletionTokens: ct, CostUSD: cost, LatencyMS: int64(300 + rand.IntN(2500)), Status: 200, Stream: rand.IntN(2) == 0, Attempts: 1}
			if rand.IntN(40) == 0 {
				l.Status, l.ErrorCode, l.ErrorMessage, l.CostUSD, l.PromptTokens, l.CompletionTokens = 502, "upstream_error", "synthetic upstream failure", 0, 0, 0
			}
			if err := st.InsertLog(ctx, l); err != nil {
				return err
			}
			n++
		}
	}
	fmt.Fprintf(os.Stderr, "inserted %d synthetic historical requests over %d days\n", n, *days)
	return nil
}

func healthcheck() error {
	addr := os.Getenv("OPENGATE_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	addr = strings.Replace(addr, "0.0.0.0", "127.0.0.1", 1)
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
