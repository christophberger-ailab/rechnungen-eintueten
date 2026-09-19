package mailbox

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SMTPConfig describes the outgoing mail server.
type SMTPConfig struct {
	Host string
	Port int
	User string
	Pass string
	From string
}

// Send delivers a mail with the given attachments. Port 465 is served over
// implicit TLS, every other port over STARTTLS.
func (c SMTPConfig) Send(to []string, subject, body string, attachments []string) error {
	msg, err := c.compose(to, subject, body, attachments)
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", c.Host, c.Port)
	var auth smtp.Auth
	if c.User != "" {
		auth = smtp.PlainAuth("", c.User, c.Pass, c.Host)
	}
	if c.Port != 465 {
		return smtp.SendMail(addr, auth, c.From, to, msg)
	}
	return c.sendTLS(addr, auth, to, msg)
}

func (c SMTPConfig) sendTLS(addr string, auth smtp.Auth, to []string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return err
	}
	defer client.Quit()
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(c.From); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

// compose builds a multipart/mixed message: one text part plus one part per
// attachment.
func (c SMTPConfig) compose(to []string, subject, body string, attachments []string) ([]byte, error) {
	var buf bytes.Buffer
	mixed := multipart.NewWriter(&buf)

	fmt.Fprintf(&buf, "From: %s\r\n", c.From)
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", mixed.Boundary())

	text, err := mixed.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {`text/plain; charset="utf-8"`},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	qp := quotedPrintable(body)
	if _, err := text.Write([]byte(qp)); err != nil {
		return nil, err
	}

	for _, path := range attachments {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(path)
		part, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mime.FormatMediaType(mediaType(name), map[string]string{"name": name})},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": name})},
		})
		if err != nil {
			return nil, err
		}
		if err := writeBase64(part, data); err != nil {
			return nil, err
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mediaType(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	return "application/octet-stream"
}

// writeBase64 writes data in 76 character lines, as MIME requires.
func writeBase64(w interface{ Write([]byte) (int, error) }, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	for len(encoded) > 0 {
		n := min(76, len(encoded))
		if _, err := fmt.Fprintf(w, "%s\r\n", encoded[:n]); err != nil {
			return err
		}
		encoded = encoded[n:]
	}
	return nil
}

// quotedPrintable encodes the few characters that must not travel raw. Mail
// bodies here are short status texts, so the simple form is enough.
func quotedPrintable(s string) string {
	var b strings.Builder
	var lineLen int
	for _, r := range []byte(strings.ReplaceAll(s, "\r\n", "\n")) {
		switch {
		case r == '\n':
			b.WriteString("\r\n")
			lineLen = 0
			continue
		case r == '=' || r < 32 || r > 126:
			fmt.Fprintf(&b, "=%02X", r)
			lineLen += 3
		default:
			b.WriteByte(r)
			lineLen++
		}
		if lineLen >= 73 {
			b.WriteString("=\r\n")
			lineLen = 0
		}
	}
	return b.String()
}
