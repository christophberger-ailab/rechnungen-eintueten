package sevdesk

import "testing"

var usdInvoice = Voucher{
	ID: "1", Status: StatusOpen, Number: "INV-99123",
	SupplierName: "Acme Services", Date: "2026-03-14",
	TotalCents: 108500, Currency: "USD",
}

func TestFindMatchExact(t *testing.T) {
	txs := []Transaction{
		{ID: "a", AmountCents: -20000, Purpose: "Miete"},
		{ID: "b", AmountCents: -108500, Purpose: "Zahlung INV-99123"},
	}
	m, ok := FindMatch(usdInvoice, txs, 5, "EUR")
	if !ok {
		t.Fatal("no match found")
	}
	if m.Transaction.ID != "b" || !m.Exact() {
		t.Errorf("matched %q, delta %d", m.Transaction.ID, m.DeltaCents)
	}
}

func TestFindMatchCurrencyGain(t *testing.T) {
	// Paid 1080.00 for a 1085.00 invoice: 5.00 less, a conversion gain.
	txs := []Transaction{{ID: "b", AmountCents: -108000, Purpose: "ACME SERVICES LTD"}}
	m, ok := FindMatch(usdInvoice, txs, 5, "EUR")
	if !ok {
		t.Fatal("no match found")
	}
	if m.Exact() {
		t.Error("expected an inexact match")
	}
	if m.DeltaCents != 500 {
		t.Errorf("delta = %d, want 500", m.DeltaCents)
	}
	if !m.Gain() {
		t.Error("paying less than invoiced must book as a gain")
	}
}

func TestFindMatchCurrencyLoss(t *testing.T) {
	txs := []Transaction{{ID: "b", AmountCents: -109000, Purpose: "INV-99123"}}
	m, ok := FindMatch(usdInvoice, txs, 5, "EUR")
	if !ok {
		t.Fatal("no match found")
	}
	if m.DeltaCents != -500 || m.Gain() {
		t.Errorf("delta = %d, gain = %v; want -500 and a loss", m.DeltaCents, m.Gain())
	}
}

func TestFindMatchBeyondTolerance(t *testing.T) {
	// 10% off is more than the 5% the conversion rule allows.
	txs := []Transaction{{ID: "b", AmountCents: -97650, Purpose: "INV-99123"}}
	if _, ok := FindMatch(usdInvoice, txs, 5, "EUR"); ok {
		t.Error("a payment 10% off must not match")
	}
}

func TestFindMatchHomeCurrencyNeedsExactAmount(t *testing.T) {
	eurInvoice := usdInvoice
	eurInvoice.Currency = "EUR"
	txs := []Transaction{{ID: "b", AmountCents: -108400, Purpose: "INV-99123"}}
	if _, ok := FindMatch(eurInvoice, txs, 5, "EUR"); ok {
		t.Error("a EUR invoice must be paid to the cent")
	}
}

func TestFindMatchAmbiguousAmountOnly(t *testing.T) {
	txs := []Transaction{
		{ID: "a", AmountCents: -108500, Purpose: "Sammelzahlung"},
		{ID: "b", AmountCents: -108500, Purpose: "Ueberweisung"},
	}
	if _, ok := FindMatch(usdInvoice, txs, 5, "EUR"); ok {
		t.Error("two equal amounts without a reference must not match")
	}
}

func TestFindMatchPrefersInvoiceNumber(t *testing.T) {
	txs := []Transaction{
		{ID: "a", AmountCents: -108500, Payee: "Acme Services Ltd"},
		{ID: "b", AmountCents: -108500, Purpose: "RG INV 99123 vom 14.03."},
	}
	m, ok := FindMatch(usdInvoice, txs, 5, "EUR")
	if !ok {
		t.Fatal("no match found")
	}
	if m.Transaction.ID != "b" || m.Score != scoreNumber {
		t.Errorf("matched %q with score %d", m.Transaction.ID, m.Score)
	}
}
