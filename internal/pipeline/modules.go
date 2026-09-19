package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/archive"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/datev"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/sevdesk"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/store"
)

// archiveAll is module 5: file the original below the configured FIBU
// directory, under "FIBU <YYYY>/Rechnungseingang/E<YY>Q<Q>".
func (p *Pipeline) archiveAll(ctx context.Context) error {
	invoices, err := p.db.InvoicesByStatus(store.StatusExtracted)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		if inv.ArchivePath != "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		date := invoiceDate(inv)
		name := archive.FileName(date.Format("2006-01-02"), inv.SenderName, inv.Number, inv.FileName)
		path, err := archive.Store(p.cfg.ArchiveDir, date, name, inv.FilePath)
		if err != nil {
			p.db.Log(p.run.ID, inv.ID, "archive", "error", "%v", err)
			p.run.Failed++
			continue
		}
		inv.ArchivePath = path
		if err := p.db.SaveInvoice(inv); err != nil {
			return err
		}
		p.db.Log(p.run.ID, inv.ID, "archive", "info", "abgelegt unter %s", path)
	}
	return nil
}

// datevAll is module 3: mail new invoices to DATEV, then read the answers.
func (p *Pipeline) datevAll(ctx context.Context) error {
	if !p.cfg.DatevEnabled || p.cfg.DatevTo == "" {
		return nil
	}
	uploader := datev.Uploader{SMTP: p.smtp(), To: p.cfg.DatevTo, Subject: p.cfg.DatevSubject}

	invoices, err := p.db.InvoicesByStatus(store.StatusExtracted)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		if inv.DatevState != "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := uploader.Send(inv.Number, inv.SenderName, inv.FilePath); err != nil {
			inv.DatevState = store.DatevFailed
			inv.LastError = err.Error()
			p.db.Log(p.run.ID, inv.ID, "datev", "error", "Versand fehlgeschlagen: %v", err)
			p.run.Failed++
		} else {
			inv.DatevState = store.DatevSent
			p.db.Log(p.run.ID, inv.ID, "datev", "info", "an %s gesendet", p.cfg.DatevTo)
		}
		if err := p.db.SaveInvoice(inv); err != nil {
			return err
		}
	}
	return p.datevReplies()
}

// datevReplies looks for DATEV's confirmations and raises the failures to the
// user by marking the invoice.
func (p *Pipeline) datevReplies() error {
	if p.cfg.IMAPHost == "" {
		return nil
	}
	waiting, err := p.db.InvoicesByStatus(store.StatusExtracted)
	if err != nil {
		return err
	}
	var pending []*store.Invoice
	for _, inv := range waiting {
		if inv.DatevState == store.DatevSent {
			pending = append(pending, inv)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	since := time.Now().AddDate(0, 0, -p.cfg.IMAPDays)
	messages, err := p.imap().Fetch(since, []string{p.cfg.DatevTo})
	if err != nil {
		return fmt.Errorf("DATEV-Antworten lesen: %w", err)
	}
	replies := datev.Replies(messages)
	for _, inv := range pending {
		for _, reply := range replies {
			if !reply.Mentions(inv.Number) {
				continue
			}
			if reply.Failed {
				inv.DatevState = store.DatevFailed
				inv.LastError = "DATEV meldet einen Fehler: " + reply.Subject
				p.db.Log(p.run.ID, inv.ID, "datev", "error", "Antwort: %s", reply.Subject)
				p.run.Failed++
			} else {
				inv.DatevState = store.DatevOK
				p.db.Log(p.run.ID, inv.ID, "datev", "info", "bestätigt: %s", reply.Subject)
			}
			if err := p.db.SaveInvoice(inv); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// sevdeskAll is module 4: upload invoices as vouchers, then match the open
// ones against the bank transactions.
func (p *Pipeline) sevdeskAll(ctx context.Context) error {
	if p.sev == nil {
		return nil
	}
	invoices, err := p.db.InvoicesByStatus(store.StatusExtracted)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		if inv.SevDeskState != "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if inv.SKR04 == "" {
			p.db.Log(p.run.ID, inv.ID, "sevdesk", "warn",
				"kein SKR-04-Konto für %s hinterlegt, übersprungen", inv.MailFrom)
			continue
		}
		id, err := p.sev.CreateVoucher(ctx, sevdesk.VoucherInput{
			FilePath:     inv.FilePath,
			SupplierName: inv.SenderName,
			Number:       inv.Number,
			Date:         inv.Date,
			Currency:     inv.Currency,
			SKR04:        inv.SKR04,
			TotalCents:   inv.TotalCents,
			VATCents:     inv.VATCents,
			VATRate:      inv.VATRate,
		})
		if err != nil {
			inv.SevDeskState = store.SevDeskFailed
			inv.LastError = err.Error()
			p.db.Log(p.run.ID, inv.ID, "sevdesk", "error", "Upload fehlgeschlagen: %v", err)
			p.run.Failed++
		} else {
			inv.SevDeskID, inv.SevDeskState = id, store.SevDeskUploaded
			p.db.Log(p.run.ID, inv.ID, "sevdesk", "info", "als Beleg %s angelegt (Offen)", id)
		}
		if err := p.db.SaveInvoice(inv); err != nil {
			return err
		}
	}
	return p.reconcile(ctx)
}

// reconcile matches payments to the vouchers that are still open and books
// them, including the gain or loss when a foreign currency payment differs
// slightly from the invoice total.
func (p *Pipeline) reconcile(ctx context.Context) error {
	invoices, err := p.db.InvoicesByStatus(store.StatusExtracted)
	if err != nil {
		return err
	}
	var open []*store.Invoice
	for _, inv := range invoices {
		if inv.SevDeskState == store.SevDeskUploaded && inv.SevDeskID != "" {
			open = append(open, inv)
		}
	}
	if len(open) == 0 {
		return nil
	}
	transactions, err := p.sev.Transactions(ctx)
	if err != nil {
		return fmt.Errorf("Bankumsätze lesen: %w", err)
	}

	for _, inv := range open {
		if err := ctx.Err(); err != nil {
			return err
		}
		voucher := sevdesk.Voucher{
			ID: inv.SevDeskID, Status: sevdesk.StatusOpen, Number: inv.Number,
			SupplierName: inv.SenderName, Date: inv.Date,
			TotalCents: inv.TotalCents, Currency: inv.Currency,
		}
		match, ok := sevdesk.FindMatch(voucher, transactions, p.cfg.SevDeskTol, "EUR")
		if !ok {
			continue
		}
		if err := p.book(ctx, inv, match); err != nil {
			inv.LastError = err.Error()
			p.db.Log(p.run.ID, inv.ID, "sevdesk", "error", "Zahlung zuordnen: %v", err)
			p.run.Failed++
		}
		if err := p.db.SaveInvoice(inv); err != nil {
			return err
		}
	}
	return nil
}

// book connects a payment to its voucher. A difference from a foreign
// currency conversion is booked as its own receipt first, so that the invoice
// ends up settled in full.
func (p *Pipeline) book(ctx context.Context, inv *store.Invoice, match sevdesk.Match) error {
	if !match.Exact() {
		account, kind := p.cfg.SevDeskLoss, "Verlust aus Währungsumrechnung"
		if match.Gain() {
			account, kind = p.cfg.SevDeskGain, "Erlös aus Währungsumrechnung"
		}
		delta := match.DeltaCents
		if delta < 0 {
			delta = -delta
		}
		id, err := p.sev.CreateFXVoucher(ctx, account, inv.Currency, delta, match.Gain(), inv.Date)
		if err != nil {
			return fmt.Errorf("%s über %s anlegen: %w", kind, store.FormatCents(delta), err)
		}
		p.db.Log(p.run.ID, inv.ID, "sevdesk", "info", "%s über %s gebucht (Beleg %s)",
			kind, store.FormatCents(delta), id)
	}

	paid := match.Transaction.AmountCents
	if paid < 0 {
		paid = -paid
	}
	if err := p.sev.BookVoucher(ctx, inv.SevDeskID, match.Transaction.ID, paid); err != nil {
		return err
	}

	// Trust sevDesk, not our own bookkeeping: read the status back.
	voucher, err := p.sev.Voucher(ctx, inv.SevDeskID)
	if err != nil {
		return fmt.Errorf("Status prüfen: %w", err)
	}
	if voucher.Status != sevdesk.StatusPaid {
		return fmt.Errorf("Beleg steht nach der Buchung auf Status %d statt %d",
			voucher.Status, sevdesk.StatusPaid)
	}
	inv.SevDeskState = store.SevDeskPaid
	inv.LastError = ""
	p.db.Log(p.run.ID, inv.ID, "sevdesk", "info", "Zahlung %s zugeordnet, Beleg ist bezahlt",
		match.Transaction.ID)
	return nil
}

// finishAll marks invoices done once every enabled module is through with
// them.
func (p *Pipeline) finishAll(context.Context) error {
	invoices, err := p.db.InvoicesByStatus(store.StatusExtracted)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		datevDone := !p.cfg.DatevEnabled || p.cfg.DatevTo == "" || inv.DatevState == store.DatevOK
		sevDone := p.sev == nil || inv.SevDeskState == store.SevDeskPaid
		if inv.ArchivePath == "" || !datevDone || !sevDone {
			continue
		}
		inv.Status = store.StatusDone
		if err := p.db.SaveInvoice(inv); err != nil {
			return err
		}
		p.db.Log(p.run.ID, inv.ID, "finish", "info", "vollständig verarbeitet")
	}
	return nil
}
