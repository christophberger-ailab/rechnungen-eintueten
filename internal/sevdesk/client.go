// Package sevdesk implements a minimal client for the sevDesk REST API v1
// (https://api.sevdesk.de/), covering the parts needed to file supplier
// invoices (Vouchers) as PDF attachments and reconcile them against bank
// transactions.
//
// sevDesk wraps every response body in {"objects": ...} and expects object
// references in request bodies as {"id": "...", "objectName": "..."}. Most
// scalar fields (including numbers) come back as JSON strings, a
// long-standing quirk of sevDesk's backend; this client parses them
// accordingly.
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
	// NOTE: CheckAccountTransaction has no documented per-row currency
	// field in the sources available while writing this client (a sevDesk
	// check account is normally single-currency); Currency is populated
	// only if the API happens to echo one back.
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

// dateOnly trims a sevDesk timestamp such as "2024-05-10T00:00:00+02:00"
// down to its leading YYYY-MM-DD.
func dateOnly(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// centsToAmount converts minor units (cents) to the decimal string sevDesk
// expects for amount fields, e.g. 12345 -> "123.45".
func centsToAmount(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// amountToCents converts a sevDesk decimal amount (JSON numbers decode to
// float64) to minor units (cents), rounding to the nearest cent.
func amountToCents(amount float64) int64 {
	return int64(math.Round(amount * 100))
}

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

	// NOTE: the exact response shape of uploadTempFile is not published;
	// third-party clients report an "objects" object carrying the assigned
	// "filename" directly (not array-wrapped, unlike list/get endpoints).
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

// resolveAccountDatev looks up the internal AccountDatev reference for a
// SKR04 booking account number, as required by voucher positions.
//
// NOTE: this uses GET /ReceiptGuidance/forAccountNumber with an
// "accountNumber" query parameter, inferred from third-party client source
// code rather than sevDesk's own published docs (which were not reachable
// while writing this client). The response is assumed to carry an
// "accountDatevId" field per object, matching those sources.
func (c *Client) resolveAccountDatev(ctx context.Context, skr04 string) (ref, error) {
	q := url.Values{"accountNumber": {skr04}}
	var out struct {
		Objects []struct {
			AccountDatevID string `json:"accountDatevId"`
		} `json:"objects"`
	}
	if err := c.do(ctx, http.MethodGet, "/ReceiptGuidance/forAccountNumber", q, nil, &out); err != nil {
		return ref{}, fmt.Errorf("sevdesk: resolve SKR04 account %s: %w", skr04, err)
	}
	if len(out.Objects) == 0 || out.Objects[0].AccountDatevID == "" {
		return ref{}, fmt.Errorf("sevdesk: no accounting guidance for SKR04 account %s", skr04)
	}
	return ref{ID: out.Objects[0].AccountDatevID, ObjectName: "AccountDatev"}, nil
}

// voucherSave is the "voucher" half of a Voucher/Factory/saveVoucher request.
type voucherSave struct {
	ObjectName   string `json:"objectName"`
	MapAll       bool   `json:"mapAll"`
	Status       int    `json:"status"`
	CreditDebit  string `json:"creditDebit"` // "C" (credit/expense) or "D" (debit)
	VoucherType  string `json:"voucherType"` // "VOU" for a regular voucher
	TaxType      string `json:"taxType"`     // "default" = gross amounts on positions
	VoucherDate  string `json:"voucherDate"`
	SupplierName string `json:"supplierName"`
	Description  string `json:"description"`
	Currency     string `json:"currency"`
}

// voucherPosSave is one entry of the "voucherPosSave" array of a
// Voucher/Factory/saveVoucher request.
type voucherPosSave struct {
	ObjectName   string  `json:"objectName"`
	MapAll       bool    `json:"mapAll"`
	AccountDatev ref     `json:"accountDatev"`
	TaxRate      float64 `json:"taxRate"`
	Net          string  `json:"net,omitempty"`
	Sum          string  `json:"sum"`
}

// saveVoucherRequest is the body of POST /Voucher/Factory/saveVoucher.
type saveVoucherRequest struct {
	Voucher        voucherSave      `json:"voucher"`
	VoucherPosSave []voucherPosSave `json:"voucherPosSave"`
	FileName       string           `json:"filename,omitempty"`
}

// saveVoucherResponse is the (best-effort) shape of a saveVoucher response.
//
// NOTE: sevDesk's own docs for this response were not reachable while
// writing this client; this mirrors the "objects.voucher" shape reported by
// third-party client libraries.
type saveVoucherResponse struct {
	Objects struct {
		Voucher apiVoucher `json:"voucher"`
	} `json:"objects"`
}

// createVoucher shares the saveVoucher plumbing between CreateVoucher and
// CreateFXVoucher.
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
	if out.Objects.Voucher.ID == "" {
		return "", errors.New("sevdesk: create voucher: no id in response")
	}
	return out.Objects.Voucher.ID, nil
}

// CreateVoucher uploads the PDF, then creates a voucher in status "Offen"
// with one position booked to the given SKR-04 account. Returns the voucher id.
func (c *Client) CreateVoucher(ctx context.Context, in VoucherInput) (string, error) {
	filename, err := c.uploadTempFile(ctx, in.FilePath)
	if err != nil {
		return "", err
	}
	account, err := c.resolveAccountDatev(ctx, in.SKR04)
	if err != nil {
		return "", err
	}

	v := voucherSave{
		ObjectName:   "Voucher",
		MapAll:       true,
		Status:       StatusOpen,
		CreditDebit:  "C",
		VoucherType:  "VOU",
		TaxType:      "default",
		VoucherDate:  in.Date,
		SupplierName: in.SupplierName,
		Description:  in.Number,
		Currency:     in.Currency,
	}
	pos := voucherPosSave{
		ObjectName:   "VoucherPos",
		MapAll:       true,
		AccountDatev: account,
		TaxRate:      in.VATRate,
		Net:          centsToAmount(in.TotalCents - in.VATCents),
		Sum:          centsToAmount(in.TotalCents),
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
	// NOTE: like sevDesk's other single-resource GETs, GET /Voucher/{id}
	// is assumed to still wrap its result in an "objects" array.
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

// Transactions returns bank transactions that are not fully booked yet.
//
// NOTE: sevDesk's documented CheckAccountTransaction status codes are 100
// (created/open), 200 (linked), 300/350 (private/automatic) and 400
// (booked); "not fully booked" is approximated here as status 100, since
// the precise set of codes that count as unbooked is not published.
func (c *Client) Transactions(ctx context.Context) ([]Transaction, error) {
	q := url.Values{"status": {"100"}}
	var out struct {
		Objects []apiTransaction `json:"objects"`
	}
	if err := c.do(ctx, http.MethodGet, "/CheckAccountTransaction", q, nil, &out); err != nil {
		return nil, fmt.Errorf("sevdesk: list transactions: %w", err)
	}
	txs := make([]Transaction, 0, len(out.Objects))
	for _, a := range out.Objects {
		txs = append(txs, a.toTransaction())
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
	Amount                  string `json:"amount"`
	Date                    string `json:"date"`
	Type                    string `json:"type"` // "N" = normal payment
	CheckAccount            ref    `json:"checkAccount"`
	CheckAccountTransaction ref    `json:"checkAccountTransaction"`
	CreateFeed              bool   `json:"createFeed"`
}

// BookVoucher links a transaction to a voucher, booking amountCents of it.
//
// Endpoint: PUT /Voucher/{voucherId}/bookAmount.
//
// NOTE: third-party client sources disagree on this endpoint's spelling
// ("bookAmount" vs. a documented typo "bookAmmount" in at least one
// generated client); "bookAmount" is used here as it matches sevDesk's own
// tech-blog posts and most community clients. The request body shape
// (amount, date, type, checkAccount, checkAccountTransaction, createFeed)
// is inferred from a community-maintained Go client's generated types.
func (c *Client) BookVoucher(ctx context.Context, voucherID, transactionID string, amountCents int64) error {
	tx, err := c.transaction(ctx, transactionID)
	if err != nil {
		return fmt.Errorf("sevdesk: book voucher %s: look up transaction %s: %w", voucherID, transactionID, err)
	}

	body := bookAmountRequest{
		Amount:                  centsToAmount(amountCents),
		Date:                    time.Now().Format("2006-01-02"),
		Type:                    "N",
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

// CreateFXVoucher books a currency-conversion gain or loss as its own
// voucher. account is the SKR-04 account to book against and
// amountCents is always positive; gain selects between
// "Erlös aus Währungsumrechnung" and "Verlust aus Währungsumrechnung".
func (c *Client) CreateFXVoucher(ctx context.Context, account, currency string, amountCents int64, gain bool, date string) (string, error) {
	if amountCents < 0 {
		return "", errors.New("sevdesk: CreateFXVoucher: amountCents must be positive")
	}

	desc := "Verlust aus Währungsumrechnung"
	// NOTE: creditDebit "C"/"D" for a gain vs. a loss position is inferred
	// by analogy with CreateVoucher's expense booking ("C"); sevDesk's own
	// docs for which side a currency-conversion gain belongs on were not
	// reachable while writing this client.
	creditDebit := "D"
	if gain {
		desc = "Erlös aus Währungsumrechnung"
		creditDebit = "C"
	}

	acc, err := c.resolveAccountDatev(ctx, account)
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
		VoucherDate:  date,
		SupplierName: desc,
		Description:  desc,
		Currency:     currency,
	}
	pos := voucherPosSave{
		ObjectName:   "VoucherPos",
		MapAll:       true,
		AccountDatev: acc,
		TaxRate:      0,
		Net:          centsToAmount(amountCents),
		Sum:          centsToAmount(amountCents),
	}

	id, err := c.createVoucher(ctx, v, pos, "")
	if err != nil {
		return "", fmt.Errorf("sevdesk: create FX voucher: %w", err)
	}
	return id, nil
}
