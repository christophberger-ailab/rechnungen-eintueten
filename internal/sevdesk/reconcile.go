package sevdesk

import "strings"

// Match is a bank transaction that pays a voucher.
type Match struct {
	Transaction Transaction
	// DeltaCents is the invoice total minus the amount actually paid. A
	// positive delta means less was paid than invoiced, which books as a gain
	// from currency conversion; a negative delta books as a loss.
	DeltaCents int64
	// Score records why we believe this transaction belongs to the voucher.
	Score int
}

// Exact reports whether the transaction pays the invoice to the cent.
func (m Match) Exact() bool { return m.DeltaCents == 0 }

// Gain reports whether the difference is a currency conversion gain
// ("Erlös aus Währungsumrechnung"); otherwise it is a loss
// ("Verlust aus Währungsumrechnung").
func (m Match) Gain() bool { return m.DeltaCents > 0 }

// Scores for the evidence that ties a transaction to a voucher.
const (
	scoreAmountOnly = iota
	scoreSupplier
	scoreNumber
)

// FindMatch picks the bank transaction that pays v.
//
// An exact amount is always accepted. A near miss is accepted only for
// invoices in a foreign currency (the home currency being what the bank
// account is kept in), where the conversion rate moves the amount by a little:
// that is the case the user books as a gain or loss from currency conversion.
// tolerancePct caps how far off the payment may be, in percent.
//
// When nothing but the amount ties a transaction to the voucher, the match has
// to be unique - two payments over the same amount are not worth guessing at.
func FindMatch(v Voucher, transactions []Transaction, tolerancePct float64, homeCurrency string) (Match, bool) {
	var candidates []Match
	for _, tx := range transactions {
		delta := v.TotalCents - abs(tx.AmountCents)
		if delta != 0 && (!foreign(v.Currency, homeCurrency) || !withinTolerance(delta, v.TotalCents, tolerancePct)) {
			continue
		}
		candidates = append(candidates, Match{Transaction: tx, DeltaCents: delta, Score: score(tx, v)})
	}
	if len(candidates) == 0 {
		return Match{}, false
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if better(c, best) {
			best = c
		}
	}
	if best.Score == scoreAmountOnly && len(candidates) > 1 {
		return Match{}, false // nothing but the amount, and it is not unique
	}
	return best, true
}

// better prefers the stronger evidence and, at equal evidence, the payment
// that matches the invoice more closely.
func better(a, b Match) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return abs(a.DeltaCents) < abs(b.DeltaCents)
}

func score(tx Transaction, v Voucher) int {
	text := normalize(tx.Purpose + " " + tx.Payee)
	if n := normalize(v.Number); n != "" && strings.Contains(text, n) {
		return scoreNumber
	}
	if s := normalize(v.SupplierName); len(s) >= 4 && strings.Contains(text, s) {
		return scoreSupplier
	}
	return scoreAmountOnly
}

// foreign reports whether the invoice is billed in another currency than the
// bank account is kept in - the only case where a payment may legitimately
// differ from the invoice total.
func foreign(invoiceCurrency, homeCurrency string) bool {
	if invoiceCurrency == "" {
		return false
	}
	if homeCurrency == "" {
		homeCurrency = "EUR"
	}
	return !strings.EqualFold(invoiceCurrency, homeCurrency)
}

func withinTolerance(delta, total int64, tolerancePct float64) bool {
	if total == 0 || tolerancePct <= 0 {
		return false
	}
	return float64(abs(delta)) <= float64(abs(total))*tolerancePct/100
}

// normalize strips everything that differs between how a number is printed on
// an invoice and how it shows up in a payment reference.
func normalize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, s)
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
