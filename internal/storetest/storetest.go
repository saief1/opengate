// Package storetest picks the database backend for tests. By default tests
// use SQLite; set OPENGATE_TEST_POSTGRES_DSN (an admin-capable postgres://
// URL) to run the same tests against PostgreSQL, each in a fresh database.
package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvDSN is the environment variable that enables PostgreSQL tests.
const EnvDSN = "OPENGATE_TEST_POSTGRES_DSN"

// Backend returns "postgres" or "sqlite".
func Backend() string {
	if os.Getenv(EnvDSN) != "" {
		return "postgres"
	}
	return "sqlite"
}

// DSN returns a fresh, empty database for the test: a SQLite file in a temp
// dir, or a newly created PostgreSQL database that is dropped on cleanup.
func DSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvDSN)
	if base == "" {
		return filepath.Join(t.TempDir(), "og.db")
	}
	return PostgresDSN(t, base)
}

// PostgresDSN creates a throwaway database on the server at base.
func PostgresDSN(t testing.TB, base string) string {
	t.Helper()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("og_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		a, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer a.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = a.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return u.String()
}
