package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestInsertInvoiceRejectsDuplicates(t *testing.T) {
	db := open(t)
	inv := &Invoice{FileHash: "abc", FileName: "r.pdf", FilePath: "/tmp/r.pdf", ReceivedAt: time.Now()}
	if err := db.InsertInvoice(inv); err != nil {
		t.Fatal(err)
	}
	if inv.ID == 0 {
		t.Fatal("insert did not assign an id")
	}
	again := &Invoice{FileHash: "abc", FileName: "kopie.pdf", FilePath: "/tmp/kopie.pdf"}
	if err := db.InsertInvoice(again); !errors.Is(err, ErrDuplicate) {
		t.Errorf("second insert: %v, want ErrDuplicate", err)
	}
	known, err := db.HasInvoice("abc")
	if err != nil || !known {
		t.Errorf("HasInvoice = %v, %v", known, err)
	}
	if known, _ := db.HasInvoice("unbekannt"); known {
		t.Error("HasInvoice reported an unknown hash as known")
	}
}

func TestSaveAndQueryInvoice(t *testing.T) {
	db := open(t)
	inv := &Invoice{FileHash: "x", FileName: "r.pdf", FilePath: "/tmp/r.pdf"}
	if err := db.InsertInvoice(inv); err != nil {
		t.Fatal(err)
	}
	inv.Number, inv.TotalCents, inv.Currency = "RE-1", 11900, "EUR"
	inv.Status = StatusExtracted
	if err := db.SaveInvoice(inv); err != nil {
		t.Fatal(err)
	}
	got, err := db.Invoice(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != "RE-1" || got.TotalCents != 11900 {
		t.Errorf("read back %+v", got)
	}
	if got.Total() != "119.00 EUR" {
		t.Errorf("Total() = %q", got.Total())
	}
	open, err := db.InvoicesByStatus(StatusExtracted)
	if err != nil || len(open) != 1 {
		t.Fatalf("InvoicesByStatus = %d invoices, %v", len(open), err)
	}
	counts, err := db.StatusCounts()
	if err != nil || counts[StatusExtracted] != 1 {
		t.Errorf("StatusCounts = %v, %v", counts, err)
	}
	if none, err := db.InvoicesByStatus(); err != nil || none != nil {
		t.Errorf("InvoicesByStatus() with no status = %v, %v", none, err)
	}
}

func TestInvoiceCompleteness(t *testing.T) {
	inv := &Invoice{}
	if inv.Complete() {
		t.Error("an empty invoice must not count as complete")
	}
	if len(inv.Missing()) != 5 {
		t.Errorf("Missing = %v, want all five fields", inv.Missing())
	}
	inv = &Invoice{SenderName: "A", Number: "1", Date: "2026-01-01", TotalCents: 1, Currency: "EUR"}
	if !inv.Complete() || len(inv.Missing()) != 0 {
		t.Errorf("Complete = %v, Missing = %v", inv.Complete(), inv.Missing())
	}
}

func TestSettingsAndSenders(t *testing.T) {
	db := open(t)
	if err := db.SetSettings(map[string]string{"imap.host": "mail.example.com", "imap.port": "993"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting("imap.host", "imap.example.com"); err != nil {
		t.Fatal(err)
	}
	values, err := db.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if values["imap.host"] != "imap.example.com" || values["imap.port"] != "993" {
		t.Errorf("settings = %v", values)
	}

	if err := db.SaveSender(Sender{Email: "b@example.com", SKR04: "6815", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSender(Sender{Email: "a@example.com", SKR04: "6800", Active: true}); err != nil {
		t.Fatal(err)
	}
	// Saving the same address again updates instead of duplicating.
	if err := db.SaveSender(Sender{Email: "a@example.com", SKR04: "6805", Name: "A", Active: false}); err != nil {
		t.Fatal(err)
	}
	senders, err := db.Senders()
	if err != nil {
		t.Fatal(err)
	}
	if len(senders) != 2 || senders[0].Email != "a@example.com" {
		t.Fatalf("senders = %+v", senders)
	}
	if senders[0].SKR04 != "6805" || senders[0].Active {
		t.Errorf("update did not take: %+v", senders[0])
	}
	if err := db.DeleteSender(senders[0].ID); err != nil {
		t.Fatal(err)
	}
	if senders, _ := db.Senders(); len(senders) != 1 {
		t.Errorf("after delete: %d senders", len(senders))
	}
}

func TestRunsAndEvents(t *testing.T) {
	db := open(t)
	run, err := db.StartRun("test")
	if err != nil {
		t.Fatal(err)
	}
	db.Log(run.ID, 0, "mail", "info", "%d Anhänge", 2)
	run.Status, run.Found, run.Summary = "ok", 2, "fertig"
	if err := db.FinishRun(run); err != nil {
		t.Fatal(err)
	}
	last, err := db.LastRun()
	if err != nil || last == nil {
		t.Fatalf("LastRun = %v, %v", last, err)
	}
	if last.Status != "ok" || last.Found != 2 || last.FinishedAt.IsZero() {
		t.Errorf("run = %+v", last)
	}
	events, err := db.Events(run.ID, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %d, %v", len(events), err)
	}
	if events[0].Message != "2 Anhänge" || events[0].Module != "mail" {
		t.Errorf("event = %+v", events[0])
	}
}

func TestFormatCents(t *testing.T) {
	cases := map[int64]string{0: "0.00", 5: "0.05", 119000: "1190.00", -250: "-2.50"}
	for in, want := range cases {
		if got := FormatCents(in); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestLastRunWithoutRuns(t *testing.T) {
	db := open(t)
	last, err := db.LastRun()
	if err != nil || last != nil {
		t.Errorf("LastRun on an empty database = %v, %v", last, err)
	}
}
