package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Processing states an invoice moves through. The pipeline advances an invoice
// stage by stage; every stage is idempotent so a re-run picks up where it
// stopped.
const (
	StatusNew         = "new"          // attachment stored, nothing extracted yet
	StatusNeedsReview = "needs_review" // extraction incomplete, needs a human
	StatusExtracted   = "extracted"    // invoice data complete, modules pending
	StatusDone        = "done"         // archived, handed to DATEV and paid in sevDesk
	StatusError       = "error"
)

// Sub-states of the modules that act on an extracted invoice. They advance
// independently: a DATEV hiccup must not hold up the sevDesk booking.
const (
	DatevSent   = "sent"
	DatevOK     = "ok"
	DatevFailed = "failed"

	SevDeskUploaded = "uploaded"
	SevDeskPaid     = "paid"
	SevDeskFailed   = "failed"
)

// Invoice is one incoming invoice document and everything we know about it.
type Invoice struct {
	ID          int64
	FileHash    string
	FileName    string
	FilePath    string
	MailFrom    string
	MailSubject string
	ReceivedAt  time.Time

	SenderName    string
	SenderAddress string
	SenderVATID   string
	Number        string
	Date          string // ISO 8601, kept as text: invoices carry a date, not a timestamp
	TotalCents    int64
	Currency      string
	VATCents      int64
	VATRate       float64
	SKR04         string

	Method       string // xml, text, llm, ocr
	Status       string
	DatevState   string
	SevDeskID    string
	SevDeskState string
	ArchivePath  string
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Complete reports whether extraction produced every mandatory field. VAT is
// optional: not every invoice shows it (reverse charge, small businesses).
func (i *Invoice) Complete() bool {
	return i.SenderName != "" && i.Number != "" && i.Date != "" &&
		i.TotalCents != 0 && i.Currency != ""
}

// Missing lists the mandatory fields that extraction did not fill in.
func (i *Invoice) Missing() []string {
	var m []string
	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"sender", i.SenderName != ""},
		{"number", i.Number != ""},
		{"date", i.Date != ""},
		{"total", i.TotalCents != 0},
		{"currency", i.Currency != ""},
	} {
		if !f.ok {
			m = append(m, f.name)
		}
	}
	return m
}

// Total renders the amount the way a human reads it, e.g. "1234.50 EUR".
func (i *Invoice) Total() string {
	return fmt.Sprintf("%s %s", FormatCents(i.TotalCents), i.Currency)
}

// FormatCents renders minor units as a decimal amount.
func FormatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

const invoiceColumns = `id, file_hash, file_name, file_path, mail_from, mail_subject, received_at,
	sender_name, sender_address, sender_vat_id, number, date, total_cents, currency, vat_cents, vat_rate, skr04,
	method, status, datev_state, sevdesk_id, sevdesk_state, archive_path, last_error, created_at, updated_at`

func scanInvoice(s interface{ Scan(...any) error }) (*Invoice, error) {
	var i Invoice
	var received, created, updated string
	err := s.Scan(&i.ID, &i.FileHash, &i.FileName, &i.FilePath, &i.MailFrom, &i.MailSubject, &received,
		&i.SenderName, &i.SenderAddress, &i.SenderVATID, &i.Number, &i.Date, &i.TotalCents, &i.Currency,
		&i.VATCents, &i.VATRate, &i.SKR04, &i.Method, &i.Status, &i.DatevState, &i.SevDeskID,
		&i.SevDeskState, &i.ArchivePath, &i.LastError, &created, &updated)
	if err != nil {
		return nil, err
	}
	i.ReceivedAt = parseTime(received)
	i.CreatedAt = parseTime(created)
	i.UpdatedAt = parseTime(updated)
	return &i, nil
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ErrDuplicate is returned by InsertInvoice when the document is already known.
var ErrDuplicate = errors.New("invoice already stored")

// InsertInvoice stores a freshly downloaded attachment. Documents are
// identified by their content hash, so the same mail fetched twice is stored
// once.
func (db *DB) InsertInvoice(i *Invoice) error {
	res, err := db.Exec(`INSERT OR IGNORE INTO invoices
		(file_hash, file_name, file_path, mail_from, mail_subject, received_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		i.FileHash, i.FileName, i.FilePath, i.MailFrom, i.MailSubject,
		i.ReceivedAt.UTC().Format(time.RFC3339), StatusNew)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDuplicate
	}
	i.ID, err = res.LastInsertId()
	i.Status = StatusNew
	return err
}

// SaveInvoice writes back every mutable field of an invoice.
func (db *DB) SaveInvoice(i *Invoice) error {
	_, err := db.Exec(`UPDATE invoices SET
		sender_name=?, sender_address=?, sender_vat_id=?, number=?, date=?, total_cents=?, currency=?,
		vat_cents=?, vat_rate=?, skr04=?, method=?, status=?, datev_state=?, sevdesk_id=?, sevdesk_state=?,
		archive_path=?, last_error=?, updated_at=datetime('now') WHERE id=?`,
		i.SenderName, i.SenderAddress, i.SenderVATID, i.Number, i.Date, i.TotalCents, i.Currency,
		i.VATCents, i.VATRate, i.SKR04, i.Method, i.Status, i.DatevState, i.SevDeskID, i.SevDeskState,
		i.ArchivePath, i.LastError, i.ID)
	return err
}

// Invoice loads a single invoice by ID.
func (db *DB) Invoice(id int64) (*Invoice, error) {
	return scanInvoice(db.QueryRow(`SELECT `+invoiceColumns+` FROM invoices WHERE id=?`, id))
}

// InvoicesByStatus returns all invoices currently in any of the given states.
func (db *DB) InvoicesByStatus(status ...string) ([]*Invoice, error) {
	if len(status) == 0 {
		return nil, nil
	}
	args := make([]any, len(status))
	for i, s := range status {
		args[i] = s
	}
	q := `SELECT ` + invoiceColumns + ` FROM invoices WHERE status IN (?` +
		strings.Repeat(",?", len(status)-1) + `) ORDER BY id`
	return db.queryInvoices(q, args...)
}

// RecentInvoices returns the newest invoices for the dashboard history table.
func (db *DB) RecentInvoices(limit int) ([]*Invoice, error) {
	return db.queryInvoices(`SELECT `+invoiceColumns+` FROM invoices ORDER BY id DESC LIMIT ?`, limit)
}

func (db *DB) queryInvoices(q string, args ...any) ([]*Invoice, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invoice
	for rows.Next() {
		i, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// StatusCounts returns the number of invoices per status, for the dashboard.
func (db *DB) StatusCounts() (map[string]int, error) {
	rows, err := db.Query(`SELECT status, count(*) FROM invoices GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		counts[s] = n
	}
	return counts, rows.Err()
}

// HasInvoice reports whether a document with that content hash is known.
func (db *DB) HasInvoice(hash string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM invoices WHERE file_hash=?`, hash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
