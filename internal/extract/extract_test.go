package extract

import "testing"

func TestParseAmount(t *testing.T) {
	cases := map[string]int64{
		"1.234,56":    123456, // German
		"1,234.56":    123456, // English
		"1234.56":     123456,
		"1234,56":     123456,
		"99,00 €":     9900,
		"EUR 1 234,5": 123450,
		"-42,00":      -4200,
		"7":           700,
		"1.234":       123400, // thousands separator, no decimals
	}
	for in, want := range cases {
		got, err := ParseAmount(in)
		if err != nil {
			t.Errorf("ParseAmount(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseAmount(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := ParseAmount("keine Zahl"); err == nil {
		t.Error("ParseAmount on non-numeric input: want error, got nil")
	}
}

func TestNormalizeDate(t *testing.T) {
	cases := map[string]string{
		"2026-03-14":     "2026-03-14",
		"14.03.2026":     "2026-03-14",
		"14. März 2026":  "2026-03-14",
		"March 14, 2026": "2026-03-14",
		"3.4.26":         "2026-04-03",
		"nonsense":       "",
	}
	for in, want := range cases {
		if got := NormalizeDate(in); got != want {
			t.Errorf("NormalizeDate(%q) = %q, want %q", in, got, want)
		}
	}
}

const germanInvoice = `Musterlieferant GmbH
Hauptstraße 5
10115 Berlin
USt-IdNr.: DE123456789

Rechnung

Rechnungs-Nr.: RE-2026-0042
Rechnungsdatum: 14.03.2026

Position          Menge      Preis
Beratung              2    500,00 €

Zwischensumme            1.000,00 €
MwSt. 19%                  190,00 €
Gesamtbetrag             1.190,00 €
`

func TestFromTextGerman(t *testing.T) {
	d := FromText(germanInvoice)
	if d.Number != "RE-2026-0042" {
		t.Errorf("number = %q", d.Number)
	}
	if d.Date != "2026-03-14" {
		t.Errorf("date = %q", d.Date)
	}
	if d.TotalCents != 119000 {
		t.Errorf("total = %d, want 119000", d.TotalCents)
	}
	if d.Currency != "EUR" {
		t.Errorf("currency = %q", d.Currency)
	}
	if d.VATCents != 19000 {
		t.Errorf("vat = %d, want 19000", d.VATCents)
	}
	if d.VATRate != 19 {
		t.Errorf("vat rate = %v, want 19", d.VATRate)
	}
	if d.SenderVATID != "DE123456789" {
		t.Errorf("vat id = %q", d.SenderVATID)
	}
	if d.SenderName != "Musterlieferant GmbH" {
		t.Errorf("sender = %q", d.SenderName)
	}
	if !d.Complete() {
		t.Errorf("extraction incomplete, missing %v", d.missing())
	}
}

const englishInvoice = `Acme Services Ltd
221B Baker Street
London NW1 6XE

INVOICE

Invoice No. INV-99123
Invoice date: March 14, 2026

Subtotal                  1,000.00 USD
Sales tax 8.5%               85.00 USD
Total due                 1,085.00 USD
`

func TestFromTextEnglish(t *testing.T) {
	d := FromText(englishInvoice)
	if d.Number != "INV-99123" {
		t.Errorf("number = %q", d.Number)
	}
	if d.Date != "2026-03-14" {
		t.Errorf("date = %q", d.Date)
	}
	if d.TotalCents != 108500 {
		t.Errorf("total = %d, want 108500", d.TotalCents)
	}
	if d.Currency != "USD" {
		t.Errorf("currency = %q", d.Currency)
	}
	if d.VATCents != 8500 {
		t.Errorf("vat = %d, want 8500", d.VATCents)
	}
	if d.VATRate != 8.5 {
		t.Errorf("vat rate = %v, want 8.5", d.VATRate)
	}
}

func TestFromTextEmpty(t *testing.T) {
	if d := FromText("   \n "); d.Complete() {
		t.Error("empty text yielded complete data")
	}
}
