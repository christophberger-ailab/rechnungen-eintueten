package mailbox

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gomail "github.com/emersion/go-message/mail"
)

func TestComposeProducesReadableMail(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "rechnung.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := SMTPConfig{From: "buchhaltung@example.com"}
	raw, err := cfg.compose([]string{"belege@datev.de"}, "Rechnungseingang RE-1",
		"Hallo,\n\nanbei die Rechnung für März – mit Umlauten.\n", []string{pdf})
	if err != nil {
		t.Fatal(err)
	}

	reader, err := gomail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("composed mail does not parse: %v", err)
	}
	defer reader.Close()
	if got := reader.Header.Get("To"); got != "belege@datev.de" {
		t.Errorf("To = %q", got)
	}
	subject, err := reader.Header.Subject()
	if err != nil || subject != "Rechnungseingang RE-1" {
		t.Errorf("Subject = %q, %v", subject, err)
	}

	var text string
	var attachments []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part.Body)
		if err != nil {
			t.Fatal(err)
		}
		switch header := part.Header.(type) {
		case *gomail.InlineHeader:
			text = string(body)
		case *gomail.AttachmentHeader:
			name, _ := header.Filename()
			attachments = append(attachments, name)
			if string(body) != "%PDF-1.4 fake" {
				t.Errorf("attachment body = %q", body)
			}
		}
	}
	if !strings.Contains(text, "März") {
		t.Errorf("body lost its umlauts: %q", text)
	}
	if len(attachments) != 1 || attachments[0] != "rechnung.pdf" {
		t.Errorf("attachments = %v", attachments)
	}
}

func TestParseBodyPicksInvoiceAttachments(t *testing.T) {
	raw := "From: Lieferant <rechnung@lieferant.de>\r\n" +
		"To: me@example.com\r\n" +
		"Subject: Ihre Rechnung\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=xyz\r\n\r\n" +
		"--xyz\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nGuten Tag\r\n" +
		"--xyz\r\nContent-Type: application/pdf; name=\"rechnung.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"rechnung.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nUkVDSE5VTkc=\r\n" +
		"--xyz\r\nContent-Type: image/png; name=\"logo.png\"\r\n" +
		"Content-Disposition: attachment; filename=\"logo.png\"\r\n\r\nnope\r\n" +
		"--xyz--\r\n"

	var msg Message
	if err := parseBody(&msg, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "Guten Tag") {
		t.Errorf("text = %q", msg.Text)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d, want only the PDF", len(msg.Attachments))
	}
	if msg.Attachments[0].FileName != "rechnung.pdf" || string(msg.Attachments[0].Data) != "RECHNUNG" {
		t.Errorf("attachment = %+v", msg.Attachments[0])
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"rechnung.pdf":               "rechnung.pdf",
		`..\..\etc\passwd`:           "passwd",
		"/tmp/evil.pdf":              "evil.pdf",
		"=?utf-8?q?Rechnung=2Epdf?=": "Rechnung.pdf",
		"":                           "attachment.pdf",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatches(t *testing.T) {
	if !matches("a@b.de", nil) {
		t.Error("an empty list must accept every sender")
	}
	if !matches("a@b.de", []string{"x@y.de", " A@B.de "}) {
		t.Error("sender matching must ignore case and spacing")
	}
	if matches("a@b.de", []string{"x@y.de"}) {
		t.Error("unknown sender was accepted")
	}
}

func TestAddress(t *testing.T) {
	if got := Address(`"Firma GmbH" <Rechnung@Firma.de>`); got != "rechnung@firma.de" {
		t.Errorf("Address = %q", got)
	}
	if got := Address("  Plain@Example.com "); got != "plain@example.com" {
		t.Errorf("Address = %q", got)
	}
}
