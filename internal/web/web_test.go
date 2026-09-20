package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/pipeline"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/store"
)

func newServer(t *testing.T) (http.Handler, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, pipeline.NewRunner(db)).Handler(), db
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func post(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPagesRender(t *testing.T) {
	h, _ := newServer(t)
	for _, path := range []string{"/", "/settings", "/partials/status", "/partials/invoices",
		"/static/htmx.min.js"} {
		if rec := get(t, h, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d\n%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	h, db := newServer(t)
	rec := post(t, h, "/settings", url.Values{
		"imap.host": {"imap.example.com"},
		"imap.pass": {"geheim"},
		"imap.port": {"993"},
		"imap.tls":  {"on"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d", rec.Code)
	}
	values, err := db.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if values["imap.host"] != "imap.example.com" || values["imap.pass"] != "geheim" {
		t.Fatalf("stored %v", values)
	}
	if values["imap.tls"] != "true" {
		t.Errorf("checkbox stored as %q, want \"true\"", values["imap.tls"])
	}

	// A blank secret must keep the stored one, and the page must never echo it.
	rec = post(t, h, "/settings", url.Values{"imap.host": {"imap2.example.com"}, "imap.pass": {""}})
	if body := rec.Body.String(); strings.Contains(body, "geheim") {
		t.Error("the settings page leaked a stored secret")
	}
	values, _ = db.Settings()
	if values["imap.pass"] != "geheim" {
		t.Errorf("blank secret overwrote the stored one: %q", values["imap.pass"])
	}
	if values["imap.host"] != "imap2.example.com" {
		t.Errorf("host not updated: %q", values["imap.host"])
	}
	// An unchecked checkbox has to turn the flag off.
	if values["imap.tls"] != "false" {
		t.Errorf("unchecked checkbox stored as %q, want \"false\"", values["imap.tls"])
	}
}

func TestSenderCRUD(t *testing.T) {
	h, db := newServer(t)
	rec := post(t, h, "/senders", url.Values{
		"email": {"Rechnung@Lieferant.de"}, "name": {"Lieferant"},
		"skr04": {"6815"}, "active": {"on"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /senders = %d", rec.Code)
	}
	senders, err := db.Senders()
	if err != nil || len(senders) != 1 {
		t.Fatalf("senders = %v, %v", senders, err)
	}
	if senders[0].Email != "rechnung@lieferant.de" || !senders[0].Active {
		t.Errorf("sender = %+v", senders[0])
	}

	if rec := post(t, h, "/senders", url.Values{"email": {""}}); !strings.Contains(rec.Body.String(), "E-Mail") {
		t.Error("an empty address should be refused with a message")
	}

	path := "/senders/" + strconv.FormatInt(senders[0].ID, 10) + "/delete"
	if rec := post(t, h, path, nil); rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d", path, rec.Code)
	}
	if senders, _ := db.Senders(); len(senders) != 0 {
		t.Errorf("sender survived deletion: %+v", senders)
	}
}

func TestInvoiceEditing(t *testing.T) {
	h, db := newServer(t)
	inv := &store.Invoice{FileHash: "h", FileName: "r.pdf", FilePath: "/tmp/r.pdf",
		Status: store.StatusNeedsReview}
	if err := db.InsertInvoice(inv); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInvoice(inv); err != nil {
		t.Fatal(err)
	}

	path := "/invoices/1"
	if rec := get(t, h, path); rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	rec := post(t, h, path, url.Values{
		"sender_name": {"Musterlieferant GmbH"}, "number": {"RE-2026-0042"},
		"date": {"14.03.2026"}, "total": {"1.190,00"}, "currency": {"eur"},
		"vat": {"190,00"}, "vat_rate": {"19"}, "skr04": {"6815"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST %s = %d\n%s", path, rec.Code, rec.Body.String())
	}
	got, err := db.Invoice(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Date != "2026-03-14" || got.TotalCents != 119000 || got.Currency != "EUR" {
		t.Errorf("corrections not applied: %+v", got)
	}
	if got.Status != store.StatusExtracted {
		t.Errorf("a completed invoice must go back into the pipeline, status = %q", got.Status)
	}

	if rec := get(t, h, "/invoices/999"); rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown invoice = %d, want 404", rec.Code)
	}
}

func TestStartRun(t *testing.T) {
	h, _ := newServer(t)
	rec := post(t, h, "/run", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /run = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "id=\"status\"") {
		t.Errorf("run did not return the status panel:\n%s", rec.Body.String())
	}
}
