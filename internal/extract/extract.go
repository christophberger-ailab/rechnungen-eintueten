// Package extract turns an invoice document into structured data. It tries the
// cheapest and most reliable technique first and only falls back to a language
// model when the document does not give the data away by itself:
//
//  1. embedded ZUGFeRD/Factur-X XML, or a plain XRechnung XML file
//  2. keyword scanning of the PDF text layer (German and English)
//  3. an LLM reading the extracted text
//  4. an OCR model, for PDFs that are nothing but a scan
package extract

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Extraction methods, recorded per invoice so the dashboard can show how the
// data was obtained.
const (
	MethodXML  = "xml"
	MethodText = "text"
	MethodLLM  = "llm"
	MethodOCR  = "ocr"
)

// Data is the invoice data we care about. Amounts are minor units (cents).
type Data struct {
	SenderName    string  `json:"sender_name"`
	SenderAddress string  `json:"sender_address"`
	SenderVATID   string  `json:"sender_vat_id"`
	Number        string  `json:"invoice_number"`
	Date          string  `json:"invoice_date"` // YYYY-MM-DD
	TotalCents    int64   `json:"-"`
	Total         string  `json:"total"` // decimal string, used by the LLM schema
	Currency      string  `json:"currency"`
	VATCents      int64   `json:"-"`
	VAT           string  `json:"vat"`
	VATRate       float64 `json:"vat_rate"`
}

// Complete reports whether every mandatory field is filled. VAT is optional:
// reverse-charge and small-business invoices do not show any.
func (d Data) Complete() bool {
	return d.SenderName != "" && d.Number != "" && d.Date != "" && d.TotalCents != 0 && d.Currency != ""
}

// missing names the mandatory fields that are still empty.
func (d Data) missing() []string {
	var m []string
	for _, f := range []struct {
		name  string
		empty bool
	}{
		{"sender", d.SenderName == ""},
		{"number", d.Number == ""},
		{"date", d.Date == ""},
		{"total", d.TotalCents == 0},
		{"currency", d.Currency == ""},
	} {
		if f.empty {
			m = append(m, f.name)
		}
	}
	return m
}

// fill copies fields from other into d wherever d is still empty.
func (d *Data) fill(other Data) {
	if d.SenderName == "" {
		d.SenderName = other.SenderName
	}
	if d.SenderAddress == "" {
		d.SenderAddress = other.SenderAddress
	}
	if d.SenderVATID == "" {
		d.SenderVATID = other.SenderVATID
	}
	if d.Number == "" {
		d.Number = other.Number
	}
	if d.Date == "" {
		d.Date = other.Date
	}
	if d.TotalCents == 0 {
		d.TotalCents = other.TotalCents
	}
	if d.Currency == "" {
		d.Currency = other.Currency
	}
	if d.VATCents == 0 {
		d.VATCents = other.VATCents
	}
	if d.VATRate == 0 {
		d.VATRate = other.VATRate
	}
}

// LLM extracts invoice data from plain text.
type LLM interface {
	ExtractInvoice(ctx context.Context, text string) (Data, error)
}

// OCR turns a scanned document into text.
type OCR interface {
	Text(ctx context.Context, path string) (string, error)
}

// Extractor runs the extraction chain. LLM and OCR may be nil, in which case
// the corresponding steps are skipped.
type Extractor struct {
	LLM LLM
	OCR OCR
	Log func(format string, args ...any)
}

// minTextLen is the number of characters below which we consider a PDF to have
// no usable text layer, i.e. to be a pure image.
const minTextLen = 120

// Extract reads path and returns the invoice data plus the method that
// produced it. It returns data even when incomplete; callers decide whether
// that is good enough.
func (e *Extractor) Extract(ctx context.Context, path string) (Data, string, error) {
	// 1. Structured invoice: a standalone XRechnung, or XML embedded in the PDF.
	if strings.EqualFold(filepath.Ext(path), ".xml") {
		d, err := ParseInvoiceXMLFile(path)
		if err != nil {
			return Data{}, "", fmt.Errorf("parse xml invoice: %w", err)
		}
		return d, MethodXML, nil
	}
	if xml, name, err := EmbeddedInvoiceXML(path); err == nil && xml != nil {
		e.logf("found embedded invoice XML %q", name)
		if d, err := ParseInvoiceXML(xml); err == nil && d.Complete() {
			return d, MethodXML, nil
		} else if err != nil {
			e.logf("embedded XML unusable: %v", err)
		}
	}

	// 2. Text layer plus keyword scanning.
	text, err := PDFText(path)
	if err != nil {
		e.logf("no text layer: %v", err)
	}
	method := MethodText
	data := FromText(text)
	if data.Complete() {
		return data, method, nil
	}

	// 3. Scan-only document: let the OCR model produce the text first.
	if len(strings.TrimSpace(text)) < minTextLen {
		if e.OCR == nil {
			return data, method, fmt.Errorf("document has no text layer and no OCR backend is configured")
		}
		e.logf("document looks like a scan, running OCR")
		ocrText, err := e.OCR.Text(ctx, path)
		if err != nil {
			return data, method, fmt.Errorf("ocr: %w", err)
		}
		text, method = ocrText, MethodOCR
		data.fill(FromText(text))
		if data.Complete() {
			return data, method, nil
		}
	}

	// 4. Last resort: have the LLM read the text.
	if e.LLM == nil {
		return data, method, fmt.Errorf("incomplete extraction (missing %s) and no LLM backend is configured",
			strings.Join(data.missing(), ", "))
	}
	e.logf("falling back to the LLM, still missing %s", strings.Join(data.missing(), ", "))
	llmData, err := e.LLM.ExtractInvoice(ctx, text)
	if err != nil {
		return data, method, fmt.Errorf("llm: %w", err)
	}
	llmData.fill(data)
	if method != MethodOCR {
		method = MethodLLM
	}
	if !llmData.Complete() {
		return llmData, method, fmt.Errorf("incomplete extraction, missing %s",
			strings.Join(llmData.missing(), ", "))
	}
	return llmData, method, nil
}

func (e *Extractor) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}
