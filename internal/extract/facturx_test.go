package extract

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// TestExtractEmbeddedXML builds a ZUGFeRD-style PDF - a normal invoice PDF
// with factur-x.xml attached - and checks that the chain reads the XML rather
// than the printed text.
func TestExtractEmbeddedXML(t *testing.T) {
	dir := t.TempDir()
	xmlPath := filepath.Join(dir, "factur-x.xml")
	if err := os.WriteFile(xmlPath, []byte(ciiInvoiceXML), 0o644); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(dir, "zugferd.pdf")

	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	if err := api.AddAttachmentsFile("testdata/rechnung.pdf", pdfPath, []string{xmlPath}, false, conf); err != nil {
		t.Skipf("could not build a test PDF with an attachment: %v", err)
	}

	raw, name, err := EmbeddedInvoiceXML(pdfPath)
	if err != nil {
		t.Fatalf("EmbeddedInvoiceXML: %v", err)
	}
	if name != "factur-x.xml" || len(raw) == 0 {
		t.Fatalf("attachment = %q, %d bytes", name, len(raw))
	}

	d, method, err := (&Extractor{}).Extract(context.Background(), pdfPath)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if method != MethodXML {
		t.Errorf("method = %q, want %q", method, MethodXML)
	}
	// The XML says ZF-2026-7; the printed page says RE-2026-0042.
	if d.Number != "ZF-2026-7" || d.TotalCents != 23800 {
		t.Errorf("data came from the wrong source: %+v", d)
	}
}
