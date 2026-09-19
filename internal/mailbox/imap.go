// Package mailbox reads incoming mail over IMAP and sends mail over SMTP. It
// is the only place that knows how a mail server is spoken to.
package mailbox

import (
	"fmt"
	"io"
	"mime"
	"net/mail"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

// IMAPConfig describes the mailbox to read.
type IMAPConfig struct {
	Host    string
	Port    int
	User    string
	Pass    string
	Mailbox string
	TLS     bool
}

// Attachment is a file that came in with a mail.
type Attachment struct {
	FileName string
	Data     []byte
}

// Message is an incoming mail reduced to what the pipeline needs.
type Message struct {
	UID         imap.UID
	From        string
	Subject     string
	Date        time.Time
	Text        string
	Attachments []Attachment
}

// Fetch returns the messages received since the given date whose sender is one
// of `from`. An empty `from` accepts every sender.
//
// It runs in two passes: envelopes first, bodies only for the senders we care
// about, so a busy mailbox costs one cheap round trip plus the mails that
// matter.
func (c IMAPConfig) Fetch(since time.Time, from []string) ([]Message, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	defer client.Close()

	mbox := c.Mailbox
	if mbox == "" {
		mbox = "INBOX"
	}
	if _, err := client.Select(mbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return nil, fmt.Errorf("select %s: %w", mbox, err)
	}

	uids, err := client.UIDSearch(&imap.SearchCriteria{Since: since}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	all := uids.AllUIDs()
	if len(all) == 0 {
		return nil, nil
	}

	envelopes, err := client.Fetch(imap.UIDSetNum(all...),
		&imap.FetchOptions{Envelope: true, UID: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch envelopes: %w", err)
	}

	wanted := make([]imap.UID, 0, len(envelopes))
	for _, env := range envelopes {
		if env.Envelope == nil {
			continue
		}
		if matches(senderAddress(env.Envelope), from) {
			wanted = append(wanted, env.UID)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	bodies, err := client.Fetch(imap.UIDSetNum(wanted...), &imap.FetchOptions{
		UID:         true,
		Envelope:    true,
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch bodies: %w", err)
	}

	var out []Message
	for _, b := range bodies {
		msg := Message{UID: b.UID}
		if b.Envelope != nil {
			msg.From = senderAddress(b.Envelope)
			msg.Subject = b.Envelope.Subject
			msg.Date = b.Envelope.Date
		}
		for _, section := range b.BodySection {
			if err := parseBody(&msg, section.Bytes); err != nil {
				return nil, fmt.Errorf("parse message %v: %w", b.UID, err)
			}
			break
		}
		out = append(out, msg)
	}
	return out, nil
}

func (c IMAPConfig) connect() (*imapclient.Client, error) {
	addr := fmt.Sprintf("%s:%d", c.Host, c.Port)
	var (
		client *imapclient.Client
		err    error
	)
	if c.TLS {
		client, err = imapclient.DialTLS(addr, nil)
	} else {
		client, err = imapclient.DialStartTLS(addr, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	if err := client.Login(c.User, c.Pass).Wait(); err != nil {
		client.Close()
		return nil, fmt.Errorf("login as %s: %w", c.User, err)
	}
	return client, nil
}

func senderAddress(env *imap.Envelope) string {
	addrs := env.From
	if len(addrs) == 0 {
		addrs = env.Sender
	}
	if len(addrs) == 0 {
		return ""
	}
	return strings.ToLower(addrs[0].Addr())
}

// matches reports whether addr is one of the wanted addresses. An empty list
// means "every sender".
func matches(addr string, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, w := range wanted {
		if strings.EqualFold(strings.TrimSpace(w), addr) {
			return true
		}
	}
	return false
}

// documentExtensions are the attachment types that can hold an invoice.
var documentExtensions = map[string]bool{".pdf": true, ".xml": true}

// parseBody walks the MIME tree and picks up the plain text body plus every
// attachment that could be an invoice.
func parseBody(msg *Message, raw []byte) error {
	reader, err := gomail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	defer reader.Close()

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// A single broken part must not cost us the rest of the mail.
			if gomessage.IsUnknownCharset(err) || gomessage.IsUnknownEncoding(err) {
				continue
			}
			return err
		}
		switch header := part.Header.(type) {
		case *gomail.InlineHeader:
			if contentType, _, _ := header.ContentType(); contentType == "text/plain" && msg.Text == "" {
				body, err := io.ReadAll(io.LimitReader(part.Body, 1<<20))
				if err != nil {
					return err
				}
				msg.Text = string(body)
			}
		case *gomail.AttachmentHeader:
			name, _ := header.Filename()
			if !documentExtensions[strings.ToLower(filepath.Ext(name))] {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(part.Body, 64<<20))
			if err != nil {
				return err
			}
			msg.Attachments = append(msg.Attachments, Attachment{FileName: sanitize(name), Data: data})
		}
	}
}

// sanitize reduces a mail attachment name to a safe base file name.
func sanitize(name string) string {
	if decoded, err := new(mime.WordDecoder).DecodeHeader(name); err == nil && decoded != "" {
		name = decoded
	}
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/<>:"|?*`, r) || r < 0x20 {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "attachment.pdf"
	}
	return name
}

// Address returns the bare mail address of a header value such as
// `"Firma GmbH" <rechnung@firma.de>`.
func Address(header string) string {
	if addr, err := mail.ParseAddress(header); err == nil {
		return strings.ToLower(addr.Address)
	}
	return strings.ToLower(strings.TrimSpace(header))
}
