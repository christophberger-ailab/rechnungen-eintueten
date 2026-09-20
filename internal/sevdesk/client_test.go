package sevdesk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCentsToAmount(t *testing.T) {
	cases := map[int64]string{0: "0.00", 5: "0.05", 119000: "1190.00", -250: "-2.50"}
	for in, want := range cases {
		if got := centsToAmount(in); got != want {
			t.Errorf("centsToAmount(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAmountToCents(t *testing.T) {
	cases := map[float64]int64{0: 0, 1190.00: 119000, 0.05: 5, -2.5: -250, 12.345: 1235}
	for in, want := range cases {
		if got := amountToCents(in); got != want {
			t.Errorf("amountToCents(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestDateOnly(t *testing.T) {
	cases := map[string]string{
		"2026-03-14T00:00:00+01:00": "2026-03-14",
		"14.03.2026":                "2026-03-14", // the form the docs show
		"kurz":                      "kurz",
	}
	for in, want := range cases {
		if got := dateOnly(in); got != want {
			t.Errorf("dateOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSevDate(t *testing.T) {
	if got := sevDate("2026-03-14"); got != "14.03.2026" {
		t.Errorf("sevDate = %q", got)
	}
	if got := sevDate(""); got != "" {
		t.Errorf("sevDate on an empty date = %q", got)
	}
}

func TestAmount(t *testing.T) {
	if got := amount(119000); got != 1190 {
		t.Errorf("amount(119000) = %v", got)
	}
	if got := amount(-5); got != -0.05 {
		t.Errorf("amount(-5) = %v", got)
	}
}

// TestCreateVoucher checks the request against the shapes the sevDesk OpenAPI
// description defines: the token header, the upload, the account lookup, the
// required taxRule, the German date format and the position amounts.
func TestCreateVoucher(t *testing.T) {
	var seen []string
	var accountQuery string
	var saved map[string]any

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "token123" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Voucher/Factory/uploadTempFile":
			w.Write([]byte(`{"objects":{"filename":"tmp-1.pdf","mimeType":"application/pdf"}}`))
		case "/ReceiptGuidance/forAccountNumber":
			accountQuery = r.URL.Query().Get("accountNumber")
			// accountDatevId is an integer and the account states its rules.
			w.Write([]byte(`{"objects":[{"accountDatevId":4711,"accountNumber":"6815",
				"allowedTaxRules":[{"id":1},{"id":5}]}]}`))
		case "/Voucher/Factory/saveVoucher":
			json.NewDecoder(r.Body).Decode(&saved)
			w.Write([]byte(`{"voucher":{"id":"9001","status":"100"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer stub.Close()

	pdf := filepath.Join(t.TempDir(), "rechnung.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := New(stub.URL, "token123")
	id, err := client.CreateVoucher(context.Background(), VoucherInput{
		FilePath: pdf, SupplierName: "Musterlieferant GmbH", Number: "RE-2026-0042",
		Date: "2026-03-14", Currency: "EUR", SKR04: "6815",
		TotalCents: 119000, VATCents: 19000, VATRate: 19,
	})
	if err != nil {
		t.Fatalf("CreateVoucher: %v", err)
	}
	if id != "9001" {
		t.Errorf("voucher id = %q", id)
	}
	if accountQuery != "6815" {
		t.Errorf("account lookup used %q", accountQuery)
	}

	voucher, _ := saved["voucher"].(map[string]any)
	if voucher["description"] != "RE-2026-0042" || voucher["supplierName"] != "Musterlieferant GmbH" {
		t.Errorf("voucher = %v", voucher)
	}
	if voucher["creditDebit"] != "C" {
		t.Errorf("creditDebit = %v, want C (we bought something)", voucher["creditDebit"])
	}
	if status, _ := voucher["status"].(float64); int(status) != StatusOpen {
		t.Errorf("status = %v, want %d (Offen)", voucher["status"], StatusOpen)
	}
	if voucher["voucherDate"] != "14.03.2026" {
		t.Errorf("voucherDate = %v, want the documented dd.mm.yyyy form", voucher["voucherDate"])
	}
	taxRule, _ := voucher["taxRule"].(map[string]any)
	if taxRule["id"] != "1" || taxRule["objectName"] != "TaxRule" {
		t.Errorf("taxRule = %v, want the standard rule the account allows", voucher["taxRule"])
	}

	positions, _ := saved["voucherPosSave"].([]any)
	if len(positions) != 1 {
		t.Fatalf("positions = %v", saved["voucherPosSave"])
	}
	pos, _ := positions[0].(map[string]any)
	if pos["net"] != false {
		t.Errorf("net = %v, want false: it is a flag, and we know the gross total", pos["net"])
	}
	if pos["sumGross"] != 1190.0 || pos["sumNet"] != 1000.0 {
		t.Errorf("amounts = %v gross / %v net, want 1190 over 1000", pos["sumGross"], pos["sumNet"])
	}
	if pos["taxRate"] != 19.0 {
		t.Errorf("taxRate = %v", pos["taxRate"])
	}
	account, _ := pos["accountDatev"].(map[string]any)
	if account["id"] != "4711" || account["objectName"] != "AccountDatev" {
		t.Errorf("accountDatev = %v", pos["accountDatev"])
	}
	if pos["sum"] != nil {
		t.Errorf("the position carries a \"sum\" field, which the API does not define")
	}
}

// TestCreateVoucherFallsBackToAnAllowedTaxRule covers an account that does not
// permit the standard rule - a reverse-charge account, for instance.
func TestCreateVoucherFallsBackToAnAllowedTaxRule(t *testing.T) {
	var saved map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Voucher/Factory/uploadTempFile":
			w.Write([]byte(`{"objects":{"filename":"tmp-1.pdf"}}`))
		case "/ReceiptGuidance/forAccountNumber":
			w.Write([]byte(`{"objects":[{"accountDatevId":815,"allowedTaxRules":[{"id":5}]}]}`))
		case "/Voucher/Factory/saveVoucher":
			json.NewDecoder(r.Body).Decode(&saved)
			w.Write([]byte(`{"objects":{"voucher":{"id":"7"}}}`))
		}
	}))
	defer stub.Close()

	pdf := filepath.Join(t.TempDir(), "r.pdf")
	os.WriteFile(pdf, []byte("%PDF"), 0o644)

	id, err := New(stub.URL, "t").CreateVoucher(context.Background(), VoucherInput{
		FilePath: pdf, Number: "RE-2", Date: "2026-03-14", SKR04: "3735", TotalCents: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "7" {
		t.Errorf("id = %q: the objects-wrapped response shape was not understood", id)
	}
	voucher, _ := saved["voucher"].(map[string]any)
	taxRule, _ := voucher["taxRule"].(map[string]any)
	if taxRule["id"] != "5" {
		t.Errorf("taxRule = %v, want the only rule the account allows", voucher["taxRule"])
	}
}

func TestVouchersAndVoucher(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Voucher":
			if got := r.URL.Query().Get("status"); got != "100" {
				t.Errorf("status filter = %q, want 100", got)
			}
			w.Write([]byte(`{"objects":[{"id":"1","status":"100","description":"RE-1",
				"supplierName":"A","voucherDate":"2026-03-14T00:00:00+01:00","sumGross":"119.00","currency":"EUR"}]}`))
		case "/Voucher/1":
			w.Write([]byte(`{"objects":[{"id":"1","status":"1000","sumGross":"119.00"}]}`))
		}
	}))
	defer stub.Close()

	client := New(stub.URL, "t")
	list, err := client.Vouchers(context.Background(), StatusOpen)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("vouchers = %v", list)
	}
	if list[0].TotalCents != 11900 || list[0].Date != "2026-03-14" || list[0].Number != "RE-1" {
		t.Errorf("voucher = %+v", list[0])
	}

	one, err := client.Voucher(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if one.Status != StatusPaid {
		t.Errorf("status = %d, want %d", one.Status, StatusPaid)
	}
}

func TestDoReportsServerErrors(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"unknown account"}}`))
	}))
	defer stub.Close()

	_, err := New(stub.URL, "t").Vouchers(context.Background(), StatusOpen)
	if err == nil {
		t.Fatal("want an error for a 400 response")
	}
	if got := err.Error(); !strings.Contains(got, "unknown account") || !strings.Contains(got, "400") {
		t.Errorf("error message lost the detail: %q", got)
	}
}

// TestTransactionsKeepsOnlyUnbookedOnes covers the filtering the endpoint
// cannot do for us: GET /CheckAccountTransaction takes no status parameter.
func TestTransactionsKeepsOnlyUnbookedOnes(t *testing.T) {
	var query string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"objects":[
			{"id":"1","status":"100","amount":"-1085.00","valueDate":"2026-03-20T00:00:00+01:00",
			 "paymtPurpose":"INV-99123","payeePayerName":"Acme","checkAccount":{"id":"5","objectName":"CheckAccount"}},
			{"id":"2","status":"400","amount":"-20.00","paymtPurpose":"schon gebucht"},
			{"id":"3","status":"200","amount":"-30.00","paymtPurpose":"verknuepft"}
		]}`))
	}))
	defer stub.Close()

	txs, err := New(stub.URL, "t").Transactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if query != "" {
		t.Errorf("request carried the query %q, but the endpoint has no status filter", query)
	}
	if len(txs) != 1 || txs[0].ID != "1" {
		t.Fatalf("transactions = %+v, want only the unbooked one", txs)
	}
	if txs[0].AmountCents != -108500 {
		t.Errorf("amount = %d, want -108500 (the API sends it as a string)", txs[0].AmountCents)
	}
	if txs[0].Date != "2026-03-20" || txs[0].Purpose != "INV-99123" {
		t.Errorf("transaction = %+v", txs[0])
	}
}

func TestBookVoucher(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exact    bool
		wantType string
	}{
		{"exact payment settles in full", true, BookingFull},
		{"inexact payment still settles", false, BookingOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			var method, path string
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/CheckAccountTransaction/77" {
					w.Write([]byte(`{"objects":[{"id":"77","status":"100","amount":"-1085.00",
						"checkAccount":{"id":"5","objectName":"CheckAccount"}}]}`))
					return
				}
				method, path = r.Method, r.URL.Path
				json.NewDecoder(r.Body).Decode(&body)
				w.Write([]byte(`{"id":"2","objectName":"VoucherLog","toStatus":"1000"}`))
			}))
			defer stub.Close()

			if err := New(stub.URL, "t").BookVoucher(context.Background(), "9001", "77", 108500, tc.exact); err != nil {
				t.Fatal(err)
			}
			if method != http.MethodPut || path != "/Voucher/9001/bookAmount" {
				t.Errorf("%s %s, want PUT /Voucher/9001/bookAmount", method, path)
			}
			if body["type"] != tc.wantType {
				t.Errorf("type = %v, want %s", body["type"], tc.wantType)
			}
			if body["amount"] != 1085.0 {
				t.Errorf("amount = %v, want the number 1085", body["amount"])
			}
			// The check account is required and only the transaction knows it.
			checkAccount, _ := body["checkAccount"].(map[string]any)
			if checkAccount["id"] != "5" {
				t.Errorf("checkAccount = %v", body["checkAccount"])
			}
			transaction, _ := body["checkAccountTransaction"].(map[string]any)
			if transaction["id"] != "77" || transaction["objectName"] != "CheckAccountTransaction" {
				t.Errorf("checkAccountTransaction = %v", body["checkAccountTransaction"])
			}
		})
	}
}

func TestCreateFXVoucherSides(t *testing.T) {
	for _, tc := range []struct {
		name            string
		gain            bool
		wantCreditDebit string
		wantDescription string
	}{
		{"paid less than invoiced is revenue", true, "D", "Erlös aus Währungsumrechnung"},
		{"paid more than invoiced is an expense", false, "C", "Verlust aus Währungsumrechnung"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var saved map[string]any
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/ReceiptGuidance/forAccountNumber":
					w.Write([]byte(`{"objects":[{"accountDatevId":4840,"allowedTaxRules":[{"id":1}]}]}`))
				case "/Voucher/Factory/saveVoucher":
					json.NewDecoder(r.Body).Decode(&saved)
					w.Write([]byte(`{"voucher":{"id":"42"}}`))
				default:
					t.Errorf("unexpected path %s: an FX voucher has no document to upload", r.URL.Path)
				}
			}))
			defer stub.Close()

			id, err := New(stub.URL, "t").CreateFXVoucher(context.Background(), "4840", "USD", 500, tc.gain, "2026-03-14")
			if err != nil {
				t.Fatal(err)
			}
			if id != "42" {
				t.Errorf("id = %q", id)
			}
			voucher, _ := saved["voucher"].(map[string]any)
			if voucher["creditDebit"] != tc.wantCreditDebit {
				t.Errorf("creditDebit = %v, want %s", voucher["creditDebit"], tc.wantCreditDebit)
			}
			if voucher["description"] != tc.wantDescription {
				t.Errorf("description = %v, want %q", voucher["description"], tc.wantDescription)
			}
			positions, _ := saved["voucherPosSave"].([]any)
			pos, _ := positions[0].(map[string]any)
			if pos["sumGross"] != 5.0 || pos["taxRate"] != 0.0 {
				t.Errorf("position = %v, want 5.00 without VAT", pos)
			}
		})
	}
}

func TestCreateFXVoucherRejectsNegativeAmounts(t *testing.T) {
	_, err := New("http://example.invalid", "t").
		CreateFXVoucher(context.Background(), "4840", "USD", -1, true, "2026-03-14")
	if err == nil {
		t.Error("want an error for a negative delta")
	}
}
