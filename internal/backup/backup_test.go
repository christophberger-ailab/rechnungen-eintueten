package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benbjohnson/litestream/file"
	_ "modernc.org/sqlite"
)

// validConfig returns a Config that satisfies Validate, so each test below
// can zero out exactly the one field it means to check.
func validConfig() Config {
	return Config{
		Bucket:          "invoices",
		AccessKeyID:     "AKIA...",
		SecretAccessKey: "secret",
	}
}

func TestConfigValidate(t *testing.T) {
	t.Run("complete config is valid", func(t *testing.T) {
		if err := validConfig().Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantSub string // substring the error must contain
	}{
		{"missing bucket", func(c *Config) { c.Bucket = "" }, "bucket"},
		{"missing access key id", func(c *Config) { c.AccessKeyID = "" }, "access key id"},
		{"missing secret access key", func(c *Config) { c.SecretAccessKey = "" }, "secret access key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Validate() error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestRestoreRefusesExistingFile checks the disaster-recovery safety net:
// Restore must never overwrite a file already at dbPath. This is checked
// before any object-storage access, so it needs no network or credentials.
func TestRestoreRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(dbPath, []byte("not actually a sqlite file"), 0o600); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	err := Restore(context.Background(), dbPath, validConfig(), nil)
	if err == nil {
		t.Fatal("Restore() = nil, want error for existing dbPath")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Restore() error = %q, want it to mention the file already exists", err.Error())
	}

	// The refusal must happen before touching the file at all.
	got, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read dbPath after refused restore: %v", err)
	}
	if string(got) != "not actually a sqlite file" {
		t.Error("Restore() modified dbPath despite refusing to run")
	}
}

// TestRoundTripViaFileBackend exercises the real Litestream wiring (DB,
// Replica, Store, RestoreOptions) that Start/Stop/Restore use, but through
// the internal start/restore helpers with a local file.ReplicaClient
// instead of S3 — so the round trip needs no network or credentials. The
// public S3-facing surface (Config, Validate, Restore's existing-file
// check) is covered separately above.
func TestRoundTripViaFileBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	replicaPath := filepath.Join(dir, "replica")

	r, err := start(ctx, dbPath, file.NewReplicaClient(replicaPath), 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	dsn := "file:" + dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open app db: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `CREATE TABLE events (id INTEGER PRIMARY KEY, message TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO events (message) VALUES ('hello')`); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	if err := waitForLTXFiles(t, replicaPath, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close app db: %v", err)
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	restoredPath := filepath.Join(dir, "restored.db")
	if err := restore(ctx, restoredPath, file.NewReplicaClient(replicaPath)); err != nil {
		t.Fatalf("restore: %v", err)
	}

	restoredDB, err := sql.Open("sqlite", "file:"+restoredPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer restoredDB.Close()

	var message string
	if err := restoredDB.QueryRowContext(ctx, `SELECT message FROM events LIMIT 1`).Scan(&message); err != nil {
		t.Fatalf("query restored db: %v", err)
	}
	if message != "hello" {
		t.Errorf("restored message = %q, want %q", message, "hello")
	}
}

func waitForLTXFiles(t *testing.T, replicaPath string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(replicaPath, "ltx", "0", "*.ltx"))
		if err != nil {
			return err
		}
		if len(matches) > 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return context.DeadlineExceeded
}
