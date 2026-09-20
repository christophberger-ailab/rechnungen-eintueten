package extract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
)

// PDFText returns the text layer of a PDF. A PDF that is nothing but a scan
// yields (almost) nothing here, which is how the extractor detects it.
func PDFText(path string) (text string, err error) {
	// The PDF parser panics on malformed input instead of returning an error.
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("pdf parser failed: %v", r)
		}
	}()

	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	plain, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, plain); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Keyword patterns. Invoices in this pipeline arrive in German or English, so
// every pattern carries both vocabularies.
var (
	reNumber = regexp.MustCompile(`(?i)(?:rechnungs-?\s?nummer|rechnungs-?\s?nr\.?|rg-?\s?nr\.?|beleg-?\s?nr\.?|invoice\s*(?:number|no\.?|#)|bill\s*number)\s*[:#.]?\s*([A-Za-z0-9][A-Za-z0-9\-/_.]{2,})`)
	reDate   = regexp.MustCompile(`(?i)(?:rechnungs-?datum|beleg-?datum|invoice\s*date|date\s*of\s*issue|datum|date)\s*[:.]?\s*(\d{4}-\d{2}-\d{2}|\d{1,2}\.\s?\d{1,2}\.\s?\d{2,4}|\d{1,2}/\d{1,2}/\d{2,4}|\d{1,2}\.?\s+\p{L}+\s+\d{4}|\p{L}+\s+\d{1,2},?\s+\d{4})`)
	reTotal  = regexp.MustCompile(`(?i)(?:gesamt-?betrag|gesamt-?summe|rechnungs-?betrag|end-?betrag|zahlbetrag|brutto(?:betrag)?|zu\s+zahlen|grand\s+total|total\s+(?:amount|due)|amount\s+due|balance\s+due|invoice\s+total|total)\s*[:.]?\s*` + amountPattern)
	reVAT    = regexp.MustCompile(`(?i)(?:mwst|mehrwertsteuer|ust|umsatzsteuer|vat|sales\s*tax)\.?\s*(?:\(?\s*(\d{1,2}(?:[.,]\d+)?)\s*%\s*\)?)?[^\n]{0,20}?` + amountPattern)
	reRate   = regexp.MustCompile(`(?i)(?:mwst|mehrwertsteuer|ust|umsatzsteuer|vat|sales\s*tax)\.?[^\n%]{0,20}?(\d{1,2}(?:[.,]\d+)?)\s*%`)
	reVATID  = regexp.MustCompile(`(?i)(?:ust[-\s]?id\.?[-\s]?nr\.?|umsatzsteuer-?identifikations-?nummer|vat\s*(?:reg\.?|registration)?\s*(?:no\.?|number|id)?)\s*[:.]?\s*([A-Z]{2}\s?[0-9A-Z]{8,12})`)
	reAnyCur = regexp.MustCompile(`(?i)(EUR|USD|CHF|GBP|€|\$|£)`)
)

// amountPattern matches a currency amount with an optional currency marker on
// either side, e.g. "EUR 1.234,56", "1,234.56 USD", "€ 99,00".
const amountPattern = `(?:(EUR|USD|CHF|GBP|€|\$|£)\s*)?((?:\d{1,3}(?:[.,\s]\d{3})*|\d+)[.,]\d{2})\s*(EUR|USD|CHF|GBP|€|\$|£)?`

var currencyCodes = map[string]string{
	"€": "EUR", "$": "USD", "£": "GBP", "EUR": "EUR", "USD": "USD", "CHF": "CHF", "GBP": "GBP",
}

// FromText scans plain invoice text for the fields we need. It is deliberately
// forgiving: whatever it cannot find is left empty and filled in by a later
// step of the chain.
func FromText(text string) Data {
	if strings.TrimSpace(text) == "" {
		return Data{}
	}
	var d Data
	if m := reNumber.FindStringSubmatch(text); m != nil {
		d.Number = strings.Trim(strings.TrimSpace(m[1]), ".,;:")
	}
	if m := reDate.FindStringSubmatch(text); m != nil {
		d.Date = NormalizeDate(m[1])
	}
	if m := lastMatch(reTotal, text); m != nil {
		d.TotalCents, _ = ParseAmount(m[2])
		d.Currency = currencyCodes[strings.ToUpper(firstNonEmpty(m[1], m[3]))]
	}
	if m := lastMatch(reVAT, text); m != nil {
		d.VATCents, _ = ParseAmount(m[3])
		if m[1] != "" {
			d.VATRate, _ = strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64)
		}
	}
	if d.VATRate == 0 {
		if m := reRate.FindStringSubmatch(text); m != nil {
			d.VATRate, _ = strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64)
		}
	}
	if m := reVATID.FindStringSubmatch(text); m != nil {
		d.SenderVATID = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", ""))
	}
	if d.Currency == "" {
		if m := reAnyCur.FindStringSubmatch(text); m != nil {
			d.Currency = currencyCodes[strings.ToUpper(m[1])]
		}
	}
	d.SenderName, d.SenderAddress = senderFromHeader(text)
	return withDerivedVAT(d)
}

// senderFromHeader takes the issuer from the letterhead: the first lines of an
// invoice carry the sender, and the first line that is neither a date nor an
// amount is a good guess for the company name.
func senderFromHeader(text string) (name, address string) {
	var head []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		head = append(head, line)
		if len(head) == 6 {
			break
		}
	}
	for i, line := range head {
		if len(line) < 3 || len(line) > 70 {
			continue
		}
		if reDate.MatchString(line) || reAnyCur.MatchString(line) {
			continue
		}
		if strings.ContainsAny(line, "@") || strings.Count(line, " ") > 8 {
			continue
		}
		rest := head[i+1:]
		if len(rest) > 3 {
			rest = rest[:3]
		}
		return line, strings.Join(rest, ", ")
	}
	return "", ""
}

// lastMatch returns the last match of re in text. Totals appear at the bottom
// of an invoice, often after a "subtotal" line that matches just as well, so
// the last hit is the better guess.
func lastMatch(re *regexp.Regexp, text string) []string {
	all := re.FindAllStringSubmatch(text, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var errNoAmount = errors.New("not an amount")

// ParseAmount converts an amount written in either German or English notation
// into minor units. "1.234,56", "1,234.56" and "1234.56" all yield 123456.
func ParseAmount(s string) (int64, error) {
	s = strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == ',' || r == '.' || r == '-' {
			return r
		}
		return -1
	}, s)
	if s == "" {
		return 0, errNoAmount
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "-")

	// Whichever separator comes last is the decimal separator - unless it is
	// followed by three digits, which makes it a thousands separator.
	dec := max(strings.LastIndexByte(s, ','), strings.LastIndexByte(s, '.'))
	if dec >= 0 {
		if n := len(s) - dec - 1; n != 1 && n != 2 {
			dec = -1
		}
	}
	whole, frac := s, ""
	if dec >= 0 {
		whole, frac = s[:dec], s[dec+1:]
	}
	whole = strings.NewReplacer(",", "", ".", "").Replace(whole)
	if whole == "" {
		whole = "0"
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, errNoAmount
	}
	cents, err := strconv.ParseInt((frac + "00")[:2], 10, 64)
	if err != nil {
		return 0, errNoAmount
	}
	total := units*100 + cents
	if neg {
		total = -total
	}
	return total, nil
}

// dateLayouts covers the notations seen on German and English invoices.
var dateLayouts = []string{
	"2006-01-02", "02.01.2006", "2.1.2006", "02.01.06", "2.1.06",
	"01/02/2006", "02/01/2006", "2 January 2006", "02 January 2006",
	"January 2, 2006", "January 2 2006", "2. January 2006",
}

// germanMonths lets the English-only time package read German month names.
var germanMonths = strings.NewReplacer(
	"Januar", "January", "Februar", "February", "März", "March", "Maerz", "March",
	"Mai", "May", "Juni", "June", "Juli", "July", "Oktober", "October",
	"Dezember", "December", "Okt", "Oct", "Dez", "Dec", "Mrz", "Mar", "Mai.", "May",
)

// NormalizeDate converts a date found in an invoice into ISO 8601. It returns
// an empty string when the input is not a date it knows.
func NormalizeDate(s string) string {
	s = germanMonths.Replace(strings.Join(strings.Fields(s), " "))
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}
