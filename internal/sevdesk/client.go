// Package sevdesk implements a minimal client for the sevDesk REST API v1
// (https://api.sevdesk.de/), covering the parts needed to file supplier
// invoices (Vouchers) as PDF attachments and reconcile them against bank
// transactions.
//
// It follows sevDesk's OpenAPI description. Two of its habits shape the code
// below: responses wrap their payload in {"objects": ...} and return numbers
// and status codes as JSON strings, while requests take object references as
// {"id": "...", "objectName": "..."} and amounts as JSON numbers.
//
// The client targets sevdesk-Update 2.0, which books voucher positions to an
// accountDatev and states the VAT regulation as a taxRule.
package sevdesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client talks to the sevDesk REST API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a Client for the given base URL and API token. The token is
// sent as-is in the Authorization header (no "Bearer " prefix, as sevDesk
// expects). An empty baseURL defaults to "https://my.sevdesk.de/api/v1".
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = "https://my.sevdesk.de/api/v1"
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
	}
}

// Voucher status codes used by sevDesk.
const (
	StatusDraft = 50   // "Entwurf"
	StatusOpen  = 100  // "Offen"
	StatusPaid  = 1000 // "Bezahlt"
)

// Status codes of a bank transaction (CheckAccountTransaction).
const (
	StatusTransactionCreated = 100 // imported, not linked to anything yet
	StatusTransactionLinked  = 200
	StatusTransactionPrivate = 300
	StatusTransactionAuto    = 350
	StatusTransactionBooked  = 400
)

// ref is an object reference as sevDesk expects it in request bodies and
// returns it in responses: {"id": "...", "objectName": "..."}.
type ref struct {
	ID         string `json:"id"`
	ObjectName string `json:"objectName"`
}

// Voucher is a supplier invoice (Beleg) in sevDesk.
type Voucher struct {
	ID           string
	Status       int
	Number       string // supplier invoice number (voucher "description")
	SupplierName string
	Date         string // YYYY-MM-DD
	TotalCents   int64
	Currency     string
}

// VoucherInput describes a new supplier invoice to create.
type VoucherInput struct {
	FilePath     string // original PDF; uploaded and attached to the voucher
	SupplierName string
	Number       string
	Date         string // YYYY-MM-DD
	Currency     string
	SKR04        string // booking account, e.g. "6815"
	TotalCents   int64  // gross
	VATCents     int64
	VATRate      float64 // percent, e.g. 19
	// TaxRule is the sevDesk tax rule id; empty means TaxRuleStandard.
	// "5" is reverse charge, "11" a small business under §19 UStG.
	TaxRule string
}

// Transaction is a bank transaction (CheckAccountTransaction).
type Transaction struct {
	ID          string
	Date        string
	AmountCents int64
	Currency    string
	Payee       string
	Purpose     string
	Status      int
}

// apiVoucher mirrors the JSON shape of a sevDesk Voucher object as returned
// by GET /Voucher, GET /Voucher/{id} and Voucher/Factory/saveVoucher.
type apiVoucher struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Description  string `json:"description"`
	VoucherDate  string `json:"voucherDate"`
	SumGross     string `json:"sumGross"`
	Currency     string `json:"currency"`
	SupplierName string `json:"supplierName"`
}

func (a apiVoucher) toVoucher() Voucher {
	status, _ := strconv.Atoi(a.Status)
	sum, _ := strconv.ParseFloat(a.SumGross, 64)
	return Voucher{
		ID:           a.ID,
		Status:       status,
		Number:       a.Description,
		SupplierName: a.SupplierName,
		Date:         dateOnly(a.VoucherDate),
		TotalCents:   amountToCents(sum),
		Currency:     a.Currency,
	}
}

// apiTransaction mirrors the JSON shape of a sevDesk CheckAccountTransaction
// object as returned by GET /CheckAccountTransaction and
// GET /CheckAccountTransaction/{id}.
type apiTransaction struct {
	ID             string `json:"id"`
	ValueDate      string `json:"valueDate"`
	Amount         string `json:"amount"`
	PayeePayerName string `json:"payeePayerName"`
	PaymtPurpose   string `json:"paymtPurpose"`
	Status         string `json:"status"`
	CheckAccount   ref    `json:"checkAccount"`
	// A transaction carries no currency of its own - a check account is
	// single-currency - so this stays empty unless the API echoes one back.
	Currency string `json:"currency,omitempty"`
}

func (a apiTransaction) toTransaction() Transaction {
	status, _ := strconv.Atoi(a.Status)
	amt, _ := strconv.ParseFloat(a.Amount, 64)
	return Transaction{
		ID:          a.ID,
		Date:        dateOnly(a.ValueDate),
		AmountCents: amountToCents(amt),
		Currency:    a.Currency,
		Payee:       a.PayeePayerName,
		Purpose:     a.PaymtPurpose,
		Status:      status,
	}
}

// dateOnly normalises the dates sevDesk returns - an ISO timestamp such as
// "2024-05-10T00:00:00+02:00", or the German "10.05.2024" - down to
// YYYY-MM-DD.
func dateOnly(s string) string {
	if len(s) >= 10 && s[2] == '.' && s[5] == '.' {
		return s[6:10] + "-" + s[3:5] + "-" + s[:2]
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// sevDate renders an ISO date the way sevDesk documents its date fields,
// dd.mm.yyyy. Anything it does not recognise is passed through untouched.
func sevDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return t.Format("02.01.2006")
}

// amount converts minor units to the decimal number sevDesk expects in
// amount fields: 123456 becomes 1234.56.
func amount(cents int64) float64 { return float64(cents) / 100 }

// centsToAmount renders minor units as a decimal string, for the few fields
// sevDesk documents as strings.
func centsToAmount(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// amountToCents converts a sevDesk decimal amount to minor units, rounding to
// the nearest cent.
func amountToCents(a float64) int64 { return int64(math.Round(a * 100)) }

// baseName returns the final path element of a file path, without pulling
// in path/filepath.
func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	if i := strings.LastIndexByte(path, '\\'); i >= 0 {
		path = path[i+1:]
	}
	return path
}

// httpClient returns c.HTTP, initializing it with a 60s timeout on first use.
func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return c.HTTP
}

// do performs a JSON request against the sevDesk API: it sets Authorization
// and Content-Type, marshals body (if any) as the request payload, checks
// the response status code and, if out is non-nil, unmarshals the response
// body into it. On error it returns a message containing a truncated copy
// of the response body, which is invaluable for sevDesk's often terse
// validation errors.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("sevdesk: encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("sevdesk: build request: %w", err)
	}
	req.Header.Set("Authorization", c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("sevdesk: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("sevdesk: %s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sevdesk: %s %s: status %d: %s", method, path, resp.StatusCode, truncate(data, 500))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("sevdesk: %s %s: decode response: %w: %s", method, path, err, truncate(data, 500))
	}
	return nil
}

// maxResponseBytes caps how much of a response we read into memory.
const maxResponseBytes = 8 << 20

// truncate returns s (as a string) cut to at most n bytes, for embedding
// response bodies in error messages without blowing them up.
func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// uploadTempFile uploads a local file to sevDesk's temporary file store and
// returns the filename sevDesk assigned it, which is later referenced when
// saving a voucher.
//
// Endpoint: POST /Voucher/Factory/uploadTempFile (multipart/form-data,
// field name "file").
func (c *Client) uploadTempFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("sevdesk: open %s: %w", path, err)
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", baseName(path))
	if err != nil {
		return "", fmt.Errorf("sevdesk: build upload for %s: %w", path, err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("sevdesk: read %s: %w", path, err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("sevdesk: build upload for %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/Voucher/Factory/uploadTempFile", &buf)
	if err != nil {
		return "", fmt.Errorf("sevdesk: build upload request: %w", err)
	}
	req.Header.Set("Authorization", c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("sevdesk: upload %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("sevdesk: upload %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("sevdesk: upload %s: status %d: %s", path, resp.StatusCode, truncate(data, 500))
	}

	// The upload answers with an "objects" object - not the array the list
	// and get endpoints use - carrying the name sevDesk filed the PDF under.
	var out struct {
		Objects struct {
			Filename string `json:"filename"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("sevdesk: upload %s: decode response: %w: %s", path, err, truncate(data, 500))
	}
	if out.Objects.Filename == "" {
		return "", fmt.Errorf("sevdesk: upload %s: no filename in response: %s", path, truncate(data, 500))
	}
	return out.Objects.Filename, nil
}

// account is a booking account as sevDesk knows it, together with the tax
// rules it permits.
type account struct {
	ref        ref
	taxRuleIDs []string
}

// resolveAccount looks up the internal AccountDatev reference for an SKR-04
// account number. Voucher positions reference the account by sevDesk's own id,
// not by the DATEV number, so every upload starts with this lookup.
//
// GET /ReceiptGuidance/forAccountNumber also states which tax rules the
// account allows, which is how the voucher's taxRule is chosen.
func (c *Client) resolveAccount(ctx context.Context, skr04 string) (account, error) {
	q := url.Values{"accountNumber": {skr04}}
	var out struct {
		Objects []struct {
			AccountDatevID  json.Number `json:"accountDatevId"`
			AccountName     string      `json:"accountName"`
			AllowedTaxRules []struct {
				ID json.Number `json:"id"`
			} `json:"allowedTaxRules"`
		} `json:"objects"`
	}
	if err := c.do(ctx, http.MethodGet, "/ReceiptGuidance/forAccountNumber", q, nil, &out); err != nil {
		return account{}, fmt.Errorf("sevdesk: resolve SKR04 account %s: %w", skr04, err)
	}
	if len(out.Objects) == 0 || out.Objects[0].AccountDatevID.String() == "" {
		return account{}, fmt.Errorf("sevdesk: no accounting guidance for SKR04 account %s", skr04)
	}
	first := out.Objects[0]
	acc := account{ref: ref{ID: first.AccountDatevID.String(), ObjectName: "AccountDatev"}}
	for _, rule := range first.AllowedTaxRules {
		acc.taxRuleIDs = append(acc.taxRuleIDs, rule.ID.String())
	}
	return acc, nil
}

// TaxRuleStandard is "Umsatzsteuerpflichtige Umsätze", the rule that applies
// to an ordinary German supplier invoice with VAT.
const TaxRuleStandard = "1"

// taxRule picks the tax rule for a position on this account: the preferred one
// when the account allows it, otherwise the first one it does allow. sevDesk
// rejects a voucher whose tax rule does not fit its booking account, and the
// account itself is the only place that knows which ones fit.
func (a account) taxRule(preferred string) ref {
	if len(a.taxRuleIDs) == 0 {
		return ref{ID: preferred, ObjectName: "TaxRule"}
	}
	for _, id := range a.taxRuleIDs {
		if id == preferred {
			return ref{ID: id, ObjectName: "TaxRule"}
		}
	}
	return ref{ID: a.taxRuleIDs[0], ObjectName: "TaxRule"}
}

// voucherSave is the "voucher" half of a Voucher/Factory/saveVoucher request.
// taxType belongs to sevdesk-Update 1.0 and taxRule to 2.0; both are sent so
// the same request works on either generation of account.
type voucherSave struct {
	ObjectName   string `json:"objectName"`
	MapAll       bool   `json:"mapAll"`
	Status       int    `json:"status"`      // 50 draft, 100 open
	CreditDebit  string `json:"creditDebit"` // "C" credit: we bought; "D" debit: we sold
	VoucherType  string `json:"voucherType"` // "VOU" for a regular voucher
	TaxType      string `json:"taxType"`
	TaxRule      ref    `json:"taxRule"`
	VoucherDate  string `json:"voucherDate"` // dd.mm.yyyy
	SupplierName string `json:"supplierName"`
	Description  string `json:"description"` // the supplier's invoice number
	Currency     string `json:"currency,omitempty"`
}

// voucherPosSave is one entry of the "voucherPosSave" array of a
// Voucher/Factory/saveVoucher request.
//
// "net" is a flag, not an amount: it selects whether sevDesk regards sumNet or
// sumGross. We extract the gross total from the invoice, so it stays false.
//
// The position references its booking account through accountDatev, which is
// the sevdesk-Update 2.0 form. Accounts still on 1.0 expect an accountingType
// instead, whose ids live in a different namespace - sending a wrong one is
// worse than sending none, so it is left out.
type voucherPosSave struct {
	ObjectName   string  `json:"objectName"`
	MapAll       bool    `json:"mapAll"`
	AccountDatev ref     `json:"accountDatev"`
	TaxRate      float64 `json:"taxRate"`
	Net          bool    `json:"net"`
	SumNet       float64 `json:"sumNet"`
	SumGross     float64 `json:"sumGross"`
}

// saveVoucherRequest is the body of POST /Voucher/Factory/saveVoucher. The
// order of filename and the position array matters to sevDesk, so do not
// reshuffle these fields.
type saveVoucherRequest struct {
	Voucher        voucherSave      `json:"voucher"`
	VoucherPosSave []voucherPosSave `json:"voucherPosSave"`
	FileName       string           `json:"filename,omitempty"`
}

// saveVoucherResponse carries the created voucher. The OpenAPI description
// puts it at the top level, while sevDesk's other endpoints wrap their payload
// in "objects"; both shapes are accepted so either behaviour works.
type saveVoucherResponse struct {
	Voucher apiVoucher `json:"voucher"`
	Objects struct {
		Voucher apiVoucher `json:"voucher"`
	} `json:"objects"`
}

// voucherID returns the id from whichever shape the server used.
func (r saveVoucherResponse) voucherID() string {
	if r.Voucher.ID != "" {
		return r.Voucher.ID
	}
	return r.Objects.Voucher.ID
}

func (c *Client) createVoucher(ctx context.Context, v voucherSave, pos voucherPosSave, filename string) (string, error) {
	body := saveVoucherRequest{
		Voucher:        v,
		VoucherPosSave: []voucherPosSave{pos},
		FileName:       filename,
	}
	var out saveVoucherResponse
	if err := c.do(ctx, http.MethodPost, "/Voucher/Factory/saveVoucher", nil, body, &out); err != nil {
		return "", err
	}
	id := out.voucherID()
	if id == "" {
		return "", errors.New("sevdesk: create voucher: no id in response")
	}
	return id, nil
}

// CreateVoucher uploads the PDF, then creates a voucher in status "Offen"
// with one position booked to the given SKR-04 account. Returns the voucher id.
func (c *Client) CreateVoucher(ctx context.Context, in VoucherInput) (string, error) {
	filename, err := c.uploadTempFile(ctx, in.FilePath)
	if err != nil {
		return "", err
	}
	acc, err := c.resolveAccount(ctx, in.SKR04)
	if err != nil {
		return "", err
	}
	taxRule := in.TaxRule
	if taxRule == "" {
		taxRule = TaxRuleStandard
	}

	v := voucherSave{
		ObjectName:   "Voucher",
		MapAll:       true,
		Status:       StatusOpen,
		CreditDebit:  "C", // an incoming invoice: we bought something
		VoucherType:  "VOU",
		TaxType:      "default",
		TaxRule:      acc.taxRule(taxRule),
		VoucherDate:  sevDate(in.Date),
		SupplierName: in.SupplierName,
		Description:  in.Number,
		Currency:     in.Currency,
	}
	pos := voucherPosSave{
		ObjectName:   "VoucherPos",
		MapAll:       true,
		AccountDatev: acc.ref,
		TaxRate:      in.VATRate,
		Net:          false, // we know the gross total, so sumGross is what counts
		SumNet:       amount(in.TotalCents - in.VATCents),
		SumGross:     amount(in.TotalCents),
	}

	id, err := c.createVoucher(ctx, v, pos, filename)
	if err != nil {
		return "", fmt.Errorf("sevdesk: create voucher %q: %w", in.Number, err)
	}
	return id, nil
}

// Vouchers returns vouchers in the given status (use StatusOpen for "Offen").
func (c *Client) Vouchers(ctx context.Context, status int) ([]Voucher, error) {
	q := url.Values{"status": {strconv.Itoa(status)}}
	var out struct {
		Objects []apiVoucher `json:"objects"`
	}
	if err := c.do(ctx, http.MethodGet, "/Voucher", q, nil, &out); err != nil {
		return nil, fmt.Errorf("sevdesk: list vouchers: %w", err)
	}
	vouchers := make([]Voucher, 0, len(out.Objects))
	for _, a := range out.Objects {
		vouchers = append(vouchers, a.toVoucher())
	}
	return vouchers, nil
}

// Voucher loads one voucher by id.
func (c *Client) Voucher(ctx context.Context, id string) (*Voucher, error) {
	// Like the other single-resource GETs, this one answers with a
	// one-element "objects" array.
	var out struct {
		Objects []apiVoucher `json:"objects"`
	}
	if err := c.do(ctx, http.MethodGet, "/Voucher/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, fmt.Errorf("sevdesk: get voucher %s: %w", id, err)
	}
	if len(out.Objects) == 0 {
		return nil, fmt.Errorf("sevdesk: voucher %s not found", id)
	}
	v := out.Objects[0].toVoucher()
	return &v, nil
}

// Transactions returns the bank transactions that are still waiting to be
// booked.
//
// GET /CheckAccountTransaction has no status filter, so the whole list is
// fetched and narrowed here. Status 100 means the transaction was created and
// is not linked to anything yet; 200 is linked, 300 and 350 are private or
// auto-booked, 400 is booked.
func (c *Client) Transactions(ctx context.Context) ([]Transaction, error) {
	var out struct {
		Objects []apiTransaction `json:"objects"`
	}
	if err := c.do(ctx, http.MethodGet, "/CheckAccountTransaction", nil, nil, &out); err != nil {
		return nil, fmt.Errorf("sevdesk: list transactions: %w", err)
	}
	txs := make([]Transaction, 0, len(out.Objects))
	for _, a := range out.Objects {
		if tx := a.toTransaction(); tx.Status == StatusTransactionCreated {
			txs = append(txs, tx)
		}
	}
	return txs, nil
}

// transaction loads one CheckAccountTransaction by id, needed to recover the
// checkAccount reference it belongs to.
func (c *Client) transaction(ctx context.Context, id string) (apiTransaction, error) {
	var out struct {
		Objects []apiTransaction `json:"objects"`
	}
	if err := c.do(ctx, http.MethodGet, "/CheckAccountTransaction/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return apiTransaction{}, err
	}
	if len(out.Objects) == 0 {
		return apiTransaction{}, fmt.Errorf("sevdesk: transaction %s not found", id)
	}
	return out.Objects[0], nil
}

// bookAmountRequest is the body of PUT /Voucher/{id}/bookAmount.
type bookAmountRequest struct {
	Amount                  float64 `json:"amount"`
	Date                    string  `json:"date"`
	Type                    string  `json:"type"`
	CheckAccount            ref     `json:"checkAccount"`
	CheckAccountTransaction ref     `json:"checkAccountTransaction"`
	CreateFeed              bool    `json:"createFeed"`
}

// Booking types accepted by bookAmount. FULL_PAYMENT settles the voucher;
// BookingOther settles it although the amount differs, which is what a
// currency conversion leaves behind. (sevDesk's own CF code for currency
// fluctuations is deprecated.)
const (
	BookingFull  = "FULL_PAYMENT"
	BookingOther = "O"
)

// BookVoucher links a transaction to a voucher and books amountCents of it.
//
// Pass exact=false when the payment does not settle the voucher to the cent -
// a foreign currency payment that came in a little over or under. The booking
// is then recorded as a settlement with a difference instead of a partial
// payment, which is what lets the voucher reach "Bezahlt".
func (c *Client) BookVoucher(ctx context.Context, voucherID, transactionID string, amountCents int64, exact bool) error {
	// bookAmount requires the check account the transaction belongs to, which
	// only the transaction itself knows.
	tx, err := c.transaction(ctx, transactionID)
	if err != nil {
		return fmt.Errorf("sevdesk: book voucher %s: look up transaction %s: %w", voucherID, transactionID, err)
	}

	bookingType := BookingOther
	if exact {
		bookingType = BookingFull
	}
	body := bookAmountRequest{
		Amount:                  amount(amountCents),
		Date:                    sevDate(time.Now().Format("2006-01-02")),
		Type:                    bookingType,
		CheckAccount:            tx.CheckAccount,
		CheckAccountTransaction: ref{ID: transactionID, ObjectName: "CheckAccountTransaction"},
		CreateFeed:              true,
	}

	path := "/Voucher/" + url.PathEscape(voucherID) + "/bookAmount"
	if err := c.do(ctx, http.MethodPut, path, nil, body, nil); err != nil {
		return fmt.Errorf("sevdesk: book voucher %s against transaction %s: %w", voucherID, transactionID, err)
	}
	return nil
}

// CreateFXVoucher books a currency conversion gain or loss as its own voucher.
// account is the SKR-04 account to book against, amountCents is always
// positive, and gain selects between "Erlös aus Währungsumrechnung" (we paid
// less than invoiced, so it is revenue) and "Verlust aus Währungsumrechnung"
// (we paid more, so it is an expense).
func (c *Client) CreateFXVoucher(ctx context.Context, account, currency string, amountCents int64, gain bool, date string) (string, error) {
	if amountCents < 0 {
		return "", errors.New("sevdesk: CreateFXVoucher: amountCents must be positive")
	}

	// "D" is a debit: we sold something. A conversion gain is revenue, a
	// conversion loss is an expense and therefore a credit.
	description, creditDebit := "Verlust aus Währungsumrechnung", "C"
	if gain {
		description, creditDebit = "Erlös aus Währungsumrechnung", "D"
	}

	acc, err := c.resolveAccount(ctx, account)
	if err != nil {
		return "", err
	}
	v := voucherSave{
		ObjectName:   "Voucher",
		MapAll:       true,
		Status:       StatusOpen,
		CreditDebit:  creditDebit,
		VoucherType:  "VOU",
		TaxType:      "default",
		TaxRule:      acc.taxRule(TaxRuleStandard),
		VoucherDate:  sevDate(date),
		SupplierName: description,
		Description:  description,
		Currency:     currency,
	}
	// A conversion difference carries no VAT.
	pos := voucherPosSave{
		ObjectName:   "VoucherPos",
		MapAll:       true,
		AccountDatev: acc.ref,
		TaxRate:      0,
		Net:          false,
		SumNet:       amount(amountCents),
		SumGross:     amount(amountCents),
	}

	id, err := c.createVoucher(ctx, v, pos, "")
	if err != nil {
		return "", fmt.Errorf("sevdesk: create FX voucher: %w", err)
	}
	return id, nil
}
