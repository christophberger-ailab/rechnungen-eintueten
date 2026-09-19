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
	if got := dateOnly("2026-03-14T00:00:00+01:00"); got != "2026-03-14" {
		t.Errorf("dateOnly = %q", got)
	}
	if got := dateOnly("kurz"); got != "kurz" {
		t.Errorf("dateOnly on a short value = %q", got)
	}
}

// TestCreateVoucher checks the request plumbing against a stub server: the
// token header, the upload, the account lookup and the amounts on the saved
// voucher.
func TestCreateVoucher(t *testing.T) {
	var seen []string
	var saved map[string]any

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "token123" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Voucher/Factory/uploadTempFile":
			w.Write([]byte(`{"objects":{"filename":"tmp-1.pdf"}}`))
		case "/ReceiptGuidance/forAccountNumber":
			w.Write([]byte(`{"objects":[{"accountDatevId":"4711"}]}`))
		case "/Voucher/Factory/saveVoucher":
			json.NewDecoder(r.Body).Decode(&saved)
			w.Write([]byte(`{"objects":{"voucher":{"id":"9001"}}}`))
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
	voucher, _ := saved["voucher"].(map[string]any)
	if voucher["description"] != "RE-2026-0042" || voucher["supplierName"] != "Musterlieferant GmbH" {
		t.Errorf("voucher = %v", voucher)
	}
	if status, _ := voucher["status"].(float64); int(status) != StatusOpen {
		t.Errorf("status = %v, want %d (Offen)", voucher["status"], StatusOpen)
	}
	positions, _ := saved["voucherPosSave"].([]any)
	if len(positions) != 1 {
		t.Fatalf("positions = %v", saved["voucherPosSave"])
	}
	pos, _ := positions[0].(map[string]any)
	if pos["sum"] != "1190.00" || pos["net"] != "1000.00" {
		t.Errorf("position amounts = %v / %v, want 1190.00 gross over 1000.00 net", pos["sum"], pos["net"])
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
