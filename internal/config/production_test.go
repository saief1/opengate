package config

import (
	"strings"
	"testing"
)

func TestApplyEnvProduction(t *testing.T) {
	c := Default()
	t.Setenv("OPENGATE_ENV", "production")
	t.Setenv("OPENGATE_PRODUCTION", "")
	c.applyEnv()
	if !c.Production {
		t.Fatal("OPENGATE_ENV=production must set Production")
	}
	c = Default()
	t.Setenv("OPENGATE_ENV", "")
	t.Setenv("OPENGATE_PRODUCTION", "true")
	c.applyEnv()
	if !c.Production {
		t.Fatal("OPENGATE_PRODUCTION=true must set Production")
	}
}

func TestRefuseDemoFlags(t *testing.T) {
	c := Default()
	c.Production = true
	t.Setenv("OPENGATE_DEMO_MOCK_LISTEN", "")
	t.Setenv("OPENGATE_DEMO_TRAFFIC", "")
	t.Setenv("OPENGATE_PUBLIC_PLAYGROUND", "")
	if err := c.RefuseDemoFlags(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENGATE_DEMO_MOCK_LISTEN", "127.0.0.1:9090")
	if err := c.RefuseDemoFlags(); err == nil || !strings.Contains(err.Error(), "OPENGATE_DEMO_MOCK_LISTEN") {
		t.Fatalf("mock listen: %v", err)
	}
	t.Setenv("OPENGATE_DEMO_MOCK_LISTEN", "")
	t.Setenv("OPENGATE_DEMO_TRAFFIC", "1")
	if err := c.RefuseDemoFlags(); err == nil || !strings.Contains(err.Error(), "OPENGATE_DEMO_TRAFFIC") {
		t.Fatalf("traffic: %v", err)
	}
	t.Setenv("OPENGATE_DEMO_TRAFFIC", "")
	t.Setenv("OPENGATE_PUBLIC_PLAYGROUND", "yes")
	if err := c.RefuseDemoFlags(); err == nil || !strings.Contains(err.Error(), "OPENGATE_PUBLIC_PLAYGROUND") {
		t.Fatalf("playground: %v", err)
	}
	c.Production = false
	if err := c.RefuseDemoFlags(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPlaygroundFlag(t *testing.T) {
	t.Setenv("OPENGATE_PUBLIC_PLAYGROUND", "")
	t.Setenv("OPENGATE_DEMO_MOCK_LISTEN", "")
	if err := CheckPlaygroundFlag(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENGATE_PUBLIC_PLAYGROUND", "1")
	if err := CheckPlaygroundFlag(); err == nil {
		t.Fatal("playground without the mock must fail")
	}
	t.Setenv("OPENGATE_DEMO_MOCK_LISTEN", "127.0.0.1:9090")
	if err := CheckPlaygroundFlag(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProductionStorage(t *testing.T) {
	c := Default()
	c.Production = true
	c.Database = "/data/demo.db"
	t.Setenv("LITESTREAM_BUCKET", "")
	t.Setenv("OPENGATE_SQLITE_NO_REPLICA", "")
	if err := c.ValidateProductionStorage(); err == nil || !strings.Contains(err.Error(), "demo.db") {
		t.Fatalf("demo.db: %v", err)
	}
	c.Database = "/data/opengate.db"
	if err := c.ValidateProductionStorage(); err == nil {
		t.Fatal("sqlite without a replica must fail")
	}
	t.Setenv("LITESTREAM_BUCKET", "opengate-backups")
	if err := c.ValidateProductionStorage(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LITESTREAM_BUCKET", "")
	t.Setenv("OPENGATE_SQLITE_NO_REPLICA", "1")
	if err := c.ValidateProductionStorage(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENGATE_SQLITE_NO_REPLICA", "")
	c.Database = "postgres://opengate:secret@db.internal:5432/opengate?sslmode=require"
	if err := c.ValidateProductionStorage(); err != nil {
		t.Fatal(err)
	}
	c.Production = false
	c.Database = "/data/demo.db"
	if err := c.ValidateProductionStorage(); err != nil {
		t.Fatal(err)
	}
}
