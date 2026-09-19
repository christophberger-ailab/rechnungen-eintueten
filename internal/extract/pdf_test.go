package extract

import (
	"context"
	"testing"
)

// TestExtractPDF runs the whole chain over a real PDF with a text layer: no
// embedded XML, no LLM, no OCR - the keyword scanner has to carry it.
func TestExtractPDF(t *testing.T) {
	e := &Extractor{}
	d, method, err := e.Extract(context.Background(), "testdata/rechnung.pdf")
	if err != nil {
		t.Fatalf("Extract: %v (data %+v)", err, d)
	}
	if method != MethodText {
		t.Errorf("method = %q, want %q", method, MethodText)
	}
	if d.Number != "RE-2026-0042" {
		t.Errorf("number = %q", d.Number)
	}
	if d.Date != "2026-03-14" {
		t.Errorf("date = %q", d.Date)
	}
	if d.TotalCents != 119000 || d.Currency != "EUR" {
		t.Errorf("total = %d %s, want 119000 EUR", d.TotalCents, d.Currency)
	}
	if d.VATCents != 19000 || d.VATRate != 19 {
		t.Errorf("vat = %d at %v%%, want 19000 at 19%%", d.VATCents, d.VATRate)
	}
	if d.SenderVATID != "DE123456789" {
		t.Errorf("vat id = %q", d.SenderVATID)
	}
}
