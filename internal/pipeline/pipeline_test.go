package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/store"
)

// TestRunExtractsArchivesAndFinishes drives a spooled invoice through the
// stages that need no external service: extraction, archiving and the final
// state.
func TestRunExtractsArchivesAndFinishes(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// DATEV and sevDesk off: this test is about the local stages.
	if err := db.SetSettings(map[string]string{
		"archive.base_dir": filepath.Join(dir, "fibu"),
		"spool.dir":        filepath.Join(dir, "spool"),
		"datev.enabled":    "false",
		"sevdesk.enabled":  "false",
	}); err != nil {
		t.Fatal(err)
	}

	spooled := filepath.Join(dir, "rechnung.pdf")
	source, err := os.ReadFile("../extract/testdata/rechnung.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spooled, source, 0o600); err != nil {
		t.Fatal(err)
	}
	inv := &store.Invoice{
		FileHash: "deadbeef", FileName: "rechnung.pdf", FilePath: spooled,
		MailFrom: "rechnung@lieferant.de", ReceivedAt: time.Now(),
	}
	if err := db.InsertInvoice(inv); err != nil {
		t.Fatal(err)
	}

	result, err := NewRunner(db).RunOnce(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("run status = %q: %s", result.Status, result.Summary)
	}
	if result.Processed != 1 {
		t.Errorf("processed = %d, want 1", result.Processed)
	}

	got, err := db.Invoice(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusDone {
		t.Errorf("status = %q, want %q (error: %s)", got.Status, store.StatusDone, got.LastError)
	}
	if got.Number != "RE-2026-0042" || got.TotalCents != 119000 {
		t.Errorf("extraction = %q / %d", got.Number, got.TotalCents)
	}
	want := filepath.Join(dir, "fibu", "FIBU 2026", "Rechnungseingang", "E26Q1",
		"2026-03-14_Musterlieferant-GmbH_RE-2026-0042.pdf")
	if got.ArchivePath != want {
		t.Errorf("archive path = %q, want %q", got.ArchivePath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("archived file missing: %v", err)
	}
}

// TestRunIsIdempotent checks that a second run over the same data changes
// nothing and files no second copy.
func TestRunIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSettings(map[string]string{
		"archive.base_dir": filepath.Join(dir, "fibu"),
		"datev.enabled":    "false",
		"sevdesk.enabled":  "false",
	}); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../extract/testdata/rechnung.pdf")
	if err != nil {
		t.Fatal(err)
	}
	spooled := filepath.Join(dir, "rechnung.pdf")
	if err := os.WriteFile(spooled, source, 0o600); err != nil {
		t.Fatal(err)
	}
	inv := &store.Invoice{FileHash: "cafe", FileName: "rechnung.pdf", FilePath: spooled}
	if err := db.InsertInvoice(inv); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(db)
	if _, err := runner.RunOnce(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	first, err := db.Invoice(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunOnce(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	second, err := db.Invoice(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ArchivePath != second.ArchivePath {
		t.Errorf("second run filed another copy: %q then %q", first.ArchivePath, second.ArchivePath)
	}
	entries, err := os.ReadDir(filepath.Dir(first.ArchivePath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("archive holds %d files, want 1", len(entries))
	}
}
