// Package datev hands invoice originals to DATEV by mail and reads the
// confirmations DATEV sends back.
package datev

import (
	"fmt"
	"strings"
	"time"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/mailbox"
)

// Uploader mails invoice originals to DATEV.
type Uploader struct {
	SMTP    mailbox.SMTPConfig
	To      string
	Subject string // template, "{{number}}" is replaced by the invoice number
}

// Send mails one invoice document to DATEV.
func (u Uploader) Send(number, sender, filePath string) error {
	if u.To == "" {
		return fmt.Errorf("no DATEV address configured")
	}
	subject := strings.ReplaceAll(u.Subject, "{{number}}", number)
	if subject == "" {
		subject = "Rechnungseingang " + number
	}
	body := fmt.Sprintf("Rechnungseingang\n\nLieferant: %s\nRechnungsnummer: %s\n\n"+
		"Automatisch übermittelt.\n", sender, number)
	return u.SMTP.Send([]string{u.To}, subject, body, []string{filePath})
}

// Reply is a confirmation mail from DATEV.
type Reply struct {
	Date    time.Time
	Subject string
	Text    string
	Failed  bool
}

// failureWords and successWords classify a DATEV confirmation. Failure wins:
// a mail that says both is a mail we want a human to look at.
var (
	failureWords = []string{"fehler", "fehlgeschlagen", "abgelehnt", "konnte nicht", "nicht verarbeitet",
		"ungültig", "ungueltig", "error", "failed", "failure", "rejected", "invalid", "could not"}
	successWords = []string{"erfolgreich", "verarbeitet", "übernommen", "uebernommen", "success",
		"processed", "accepted", "received"}
)

// Replies turns DATEV's answer mails into classified replies.
func Replies(messages []mailbox.Message) []Reply {
	out := make([]Reply, 0, len(messages))
	for _, m := range messages {
		out = append(out, Reply{
			Date:    m.Date,
			Subject: m.Subject,
			Text:    m.Text,
			Failed:  failed(m.Subject + "\n" + m.Text),
		})
	}
	return out
}

// failed reports whether a DATEV mail complains about something. A mail that
// names neither outcome counts as a failure, so that nothing silently gets
// treated as booked.
func failed(text string) bool {
	text = strings.ToLower(text)
	for _, w := range failureWords {
		if strings.Contains(text, w) {
			return true
		}
	}
	for _, w := range successWords {
		if strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// Mentions reports whether the reply refers to the given invoice number.
func (r Reply) Mentions(number string) bool {
	if number == "" {
		return false
	}
	return strings.Contains(normalize(r.Subject+" "+r.Text), normalize(number))
}

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
