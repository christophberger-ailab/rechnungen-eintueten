// Package pipeline runs the modules in order: read mail, extract invoice data,
// archive the original, hand it to DATEV and book it in sevDesk.
//
// Every stage is idempotent and driven by the state stored with the invoice,
// so a run that dies halfway simply continues on the next one.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/config"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/extract"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/mailbox"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/sevdesk"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/store"
)

// Pipeline holds everything one run needs. It is built fresh per run so that
// configuration changes made in the web UI take effect immediately.
type Pipeline struct {
	db      *store.DB
	cfg     *config.Config
	senders []store.Sender
	run     *store.Run
	extr    *extract.Extractor
	sev     *sevdesk.Client
}

// New builds a pipeline from the configuration currently stored in the
// database.
func New(db *store.DB) (*Pipeline, error) {
	values, err := db.Settings()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(values)
	if err != nil {
		return nil, err
	}
	senders, err := db.Senders()
	if err != nil {
		return nil, err
	}
	p := &Pipeline{db: db, cfg: cfg, senders: senders}
	p.extr = &extract.Extractor{
		LLM: p.llm(),
		OCR: p.ocr(),
		Log: func(format string, args ...any) { p.logf("extract", "info", format, args...) },
	}
	if cfg.SevDeskEnabled && cfg.SevDeskToken != "" {
		p.sev = sevdesk.New(cfg.SevDeskBaseURL, cfg.SevDeskToken)
	}
	return p, nil
}

func (p *Pipeline) llm() extract.LLM {
	if p.cfg.LLMKey == "" {
		return nil
	}
	timeout := time.Duration(p.cfg.LLMTimeout) * time.Second
	if strings.EqualFold(p.cfg.LLMKind, "openai") {
		return extract.NewOpenAILLM(p.cfg.LLMBaseURL, p.cfg.LLMKey, p.cfg.LLMModel, timeout)
	}
	return extract.NewAnthropicLLM(p.cfg.LLMBaseURL, p.cfg.LLMKey, p.cfg.LLMModel, timeout)
}

func (p *Pipeline) ocr() extract.OCR {
	if p.cfg.OCRKey == "" {
		return nil
	}
	return extract.NewMistralOCR(p.cfg.OCRBaseURL, p.cfg.OCRKey, p.cfg.OCRModel,
		time.Duration(p.cfg.LLMTimeout)*time.Second)
}

// Run executes every stage once and records the result. It returns an error
// only when the run could not be recorded at all; failures inside a stage are
// logged against the run and the invoice.
func (p *Pipeline) Run(ctx context.Context, trigger string) (*store.Run, error) {
	run, err := p.db.StartRun(trigger)
	if err != nil {
		return nil, err
	}
	p.run = run

	stages := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"mail", p.collect},
		{"extract", p.extractAll},
		{"archive", p.archiveAll},
		{"datev", p.datevAll},
		{"sevdesk", p.sevdeskAll},
		{"finish", p.finishAll},
	}
	var problems []string
	for _, stage := range stages {
		if err := ctx.Err(); err != nil {
			problems = append(problems, "abgebrochen")
			break
		}
		if err := stage.fn(ctx); err != nil {
			p.logf(stage.name, "error", "%v", err)
			problems = append(problems, fmt.Sprintf("%s: %v", stage.name, err))
		}
	}

	run.Status = "ok"
	run.Summary = fmt.Sprintf("%d neue Dokumente, %d verarbeitet, %d mit Fehlern",
		run.Found, run.Processed, run.Failed)
	if len(problems) > 0 {
		run.Status = "error"
		run.Summary = strings.Join(problems, "; ")
	}
	return run, p.db.FinishRun(run)
}

// collect is module 1: read the mailbox and store the attachments of mails
// from known senders.
func (p *Pipeline) collect(ctx context.Context) error {
	if p.cfg.IMAPHost == "" {
		p.logf("mail", "info", "kein IMAP-Server konfiguriert, überspringe Postfach")
		return nil
	}
	known := make([]string, 0, len(p.senders))
	for _, s := range p.senders {
		if s.Active {
			known = append(known, s.Email)
		}
	}
	if len(known) == 0 {
		p.logf("mail", "warn", "keine bekannten Absender konfiguriert")
		return nil
	}

	since := time.Now().AddDate(0, 0, -p.cfg.IMAPDays)
	messages, err := p.imap().Fetch(since, known)
	if err != nil {
		return err
	}
	spool := p.cfg.SpoolDir
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return err
	}

	for _, msg := range messages {
		for _, attachment := range msg.Attachments {
			inv, err := p.storeAttachment(spool, msg, attachment)
			switch {
			case err == store.ErrDuplicate:
				continue
			case err != nil:
				p.logf("mail", "error", "Anhang %q aus %q: %v", attachment.FileName, msg.From, err)
				p.run.Failed++
				continue
			}
			p.run.Found++
			p.db.Log(p.run.ID, inv.ID, "mail", "info",
				"Anhang %q von %s gespeichert", attachment.FileName, msg.From)
		}
	}
	return nil
}

// storeAttachment writes one attachment to the spool directory and records it.
// Documents are identified by their content hash, so a mail seen twice does
// not create a second invoice.
func (p *Pipeline) storeAttachment(spool string, msg mailbox.Message, a mailbox.Attachment) (*store.Invoice, error) {
	sum := sha256.Sum256(a.Data)
	hash := hex.EncodeToString(sum[:])
	if known, err := p.db.HasInvoice(hash); err != nil {
		return nil, err
	} else if known {
		return nil, store.ErrDuplicate
	}

	path := filepath.Join(spool, hash[:16]+strings.ToLower(filepath.Ext(a.FileName)))
	if err := os.WriteFile(path, a.Data, 0o600); err != nil {
		return nil, err
	}
	inv := &store.Invoice{
		FileHash:    hash,
		FileName:    a.FileName,
		FilePath:    path,
		MailFrom:    msg.From,
		MailSubject: msg.Subject,
		ReceivedAt:  msg.Date,
		SKR04:       p.skr04(msg.From),
	}
	if err := p.db.InsertInvoice(inv); err != nil {
		os.Remove(path)
		return nil, err
	}
	return inv, nil
}

// skr04 returns the booking account configured for a sender.
func (p *Pipeline) skr04(from string) string {
	from = mailbox.Address(from)
	for _, s := range p.senders {
		if strings.EqualFold(s.Email, from) {
			return s.SKR04
		}
	}
	return ""
}

func (p *Pipeline) imap() mailbox.IMAPConfig {
	return mailbox.IMAPConfig{
		Host: p.cfg.IMAPHost, Port: p.cfg.IMAPPort, User: p.cfg.IMAPUser,
		Pass: p.cfg.IMAPPass, Mailbox: p.cfg.IMAPMailbox, TLS: p.cfg.IMAPTLS,
	}
}

func (p *Pipeline) smtp() mailbox.SMTPConfig {
	return mailbox.SMTPConfig{
		Host: p.cfg.SMTPHost, Port: p.cfg.SMTPPort, User: p.cfg.SMTPUser,
		Pass: p.cfg.SMTPPass, From: firstNonEmpty(p.cfg.DatevFrom, p.cfg.SMTPFrom),
	}
}

// extractAll is module 2: fill in the invoice data of everything that is not
// extracted yet.
func (p *Pipeline) extractAll(ctx context.Context) error {
	invoices, err := p.db.InvoicesByStatus(store.StatusNew, store.StatusNeedsReview)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, method, err := p.extr.Extract(ctx, inv.FilePath)
		inv.Method = method
		apply(inv, data)
		switch {
		case err != nil && !data.Complete():
			inv.Status, inv.LastError = store.StatusNeedsReview, err.Error()
			p.db.Log(p.run.ID, inv.ID, "extract", "warn", "unvollständig: %v", err)
			p.run.Failed++
		case err != nil:
			// Complete data despite a complaint from a later step: usable.
			inv.Status, inv.LastError = store.StatusExtracted, ""
			p.db.Log(p.run.ID, inv.ID, "extract", "info", "erkannt (%s), Hinweis: %v", method, err)
			p.run.Processed++
		default:
			inv.Status, inv.LastError = store.StatusExtracted, ""
			p.db.Log(p.run.ID, inv.ID, "extract", "info", "erkannt (%s): %s über %s",
				method, inv.Number, inv.Total())
			p.run.Processed++
		}
		if err := p.db.SaveInvoice(inv); err != nil {
			return err
		}
	}
	return nil
}

// apply copies extracted data onto the invoice without dropping values that a
// human corrected in the web UI.
func apply(inv *store.Invoice, d extract.Data) {
	set(&inv.SenderName, d.SenderName)
	set(&inv.SenderAddress, d.SenderAddress)
	set(&inv.SenderVATID, d.SenderVATID)
	set(&inv.Number, d.Number)
	set(&inv.Date, d.Date)
	set(&inv.Currency, d.Currency)
	if inv.TotalCents == 0 {
		inv.TotalCents = d.TotalCents
	}
	if inv.VATCents == 0 {
		inv.VATCents = d.VATCents
	}
	if inv.VATRate == 0 {
		inv.VATRate = d.VATRate
	}
}

func set(dst *string, value string) {
	if *dst == "" {
		*dst = value
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (p *Pipeline) logf(module, level, format string, args ...any) {
	var runID int64
	if p.run != nil {
		runID = p.run.ID
	}
	p.db.Log(runID, 0, module, level, format, args...)
}

// invoiceDate is the date an invoice is filed under: the date printed on it,
// or the day it arrived when the document does not say.
func invoiceDate(inv *store.Invoice) time.Time {
	if t, err := time.Parse("2006-01-02", inv.Date); err == nil {
		return t
	}
	if !inv.ReceivedAt.IsZero() {
		return inv.ReceivedAt
	}
	return time.Now()
}
