// Package config is the typed view of the settings table. The bindings table
// is the single source of truth: it drives loading, saving and the settings
// form rendered by the web UI.
package config

import (
	"fmt"
	"slices"
	"strconv"
)

// Config holds every configurable value of the application. Every field needs
// a line in bindings.
type Config struct {
	IMAPHost    string
	IMAPPort    int
	IMAPUser    string
	IMAPPass    string
	IMAPMailbox string
	IMAPTLS     bool
	IMAPDays    int

	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string

	LLMBaseURL string
	LLMKey     string
	LLMModel   string
	LLMKind    string
	LLMTimeout int

	OCRBaseURL string
	OCRKey     string
	OCRModel   string

	DatevTo      string
	DatevFrom    string
	DatevSubject string
	DatevEnabled bool

	SevDeskBaseURL string
	SevDeskToken   string
	SevDeskEnabled bool
	SevDeskTol     float64
	SevDeskTaxRule string
	SevDeskGain    string
	SevDeskLoss    string

	ArchiveDir string
	SpoolDir   string

	BackupEnabled   bool
	BackupEndpoint  string
	BackupRegion    string
	BackupBucket    string
	BackupPath      string
	BackupKeyID     string
	BackupSecret    string
	BackupPathStyle bool
	BackupInterval  int

	DailyAt  string
	HTTPAddr string
}

// bindings ties every setting to its field in c, in the order of the settings
// form. The constructors fix the type of the field and of its default, so a
// mismatch is a compile error.
func (c *Config) bindings() []binding {
	return slices.Concat(
		in("Mailbox (IMAP)",
			text(&c.IMAPHost, "imap.host", "Server", ""),
			number(&c.IMAPPort, "imap.port", "Port", 993),
			text(&c.IMAPUser, "imap.user", "Benutzer", ""),
			secret(&c.IMAPPass, "imap.pass", "Passwort"),
			text(&c.IMAPMailbox, "imap.mailbox", "Postfach", "INBOX"),
			toggle(&c.IMAPTLS, "imap.tls", "TLS", true),
			number(&c.IMAPDays, "imap.days", "Zeitraum (Tage)", 30),
		),
		in("Mailversand (SMTP)",
			text(&c.SMTPHost, "smtp.host", "Server", ""),
			number(&c.SMTPPort, "smtp.port", "Port", 587),
			text(&c.SMTPUser, "smtp.user", "Benutzer", ""),
			secret(&c.SMTPPass, "smtp.pass", "Passwort"),
			text(&c.SMTPFrom, "smtp.from", "Absenderadresse", ""),
		),
		in("LLM",
			text(&c.LLMBaseURL, "llm.base_url", "API Base URL", "https://openrouter.ai/api/v1"),
			secret(&c.LLMKey, "llm.api_key", "API Key"),
			text(&c.LLMModel, "llm.model", "Modell", "~z-ai/glm-flash-latest"),
			text(&c.LLMKind, "llm.kind", "API-Dialekt (anthropic|openai)", "openai"),
			number(&c.LLMTimeout, "llm.timeout", "Timeout (s)", 120),
		),
		in("OCR",
			text(&c.OCRBaseURL, "ocr.base_url", "API Base URL", "https://api.mistral.ai/v1"),
			secret(&c.OCRKey, "ocr.api_key", "API Key"),
			text(&c.OCRModel, "ocr.model", "Modell", "mistral-ocr-latest"),
		),
		in("DATEV",
			text(&c.DatevTo, "datev.to", "DATEV-Empfangsadresse", ""),
			text(&c.DatevFrom, "datev.from", "Absenderadresse", ""),
			text(&c.DatevSubject, "datev.subject", "Betreff-Vorlage", "Rechnungseingang {{number}}"),
			toggle(&c.DatevEnabled, "datev.enabled", "Aktiv", true),
		),
		in("sevDesk",
			text(&c.SevDeskBaseURL, "sevdesk.base_url", "API Base URL", "https://my.sevdesk.de/api/v1"),
			secret(&c.SevDeskToken, "sevdesk.token", "API Token"),
			toggle(&c.SevDeskEnabled, "sevdesk.enabled", "Aktiv", true),
			decimal(&c.SevDeskTol, "sevdesk.fx_tolerance", "FX-Toleranz (%)", 5),
			text(&c.SevDeskTaxRule, "sevdesk.tax_rule", "Steuerregel (1=USt-pflichtig, 5=Reverse Charge, 11=§19 UStG)", "1"),
			text(&c.SevDeskGain, "sevdesk.gain_account", "Konto Erlös Währungsumrechnung", "4840"),
			text(&c.SevDeskLoss, "sevdesk.loss_account", "Konto Verlust Währungsumrechnung", "6880"),
		),
		in("Ablage",
			text(&c.ArchiveDir, "archive.base_dir", "Basisverzeichnis", "./FIBU"),
			text(&c.SpoolDir, "spool.dir", "Spool-Verzeichnis (Originale)", "./spool"),
		),
		in("Backup (Litestream)",
			toggle(&c.BackupEnabled, "backup.enabled", "Aktiv", false),
			text(&c.BackupEndpoint, "backup.endpoint", "S3-Endpoint", "https://fsn1.your-objectstorage.com"),
			text(&c.BackupRegion, "backup.region", "Region", "fsn1"),
			text(&c.BackupBucket, "backup.bucket", "Bucket", ""),
			text(&c.BackupPath, "backup.path", "Pfad im Bucket", "rechnungen"),
			secret(&c.BackupKeyID, "backup.access_key_id", "Access Key ID"),
			secret(&c.BackupSecret, "backup.secret_access_key", "Secret Access Key"),
			toggle(&c.BackupPathStyle, "backup.force_path_style", "Path-Style-URLs", true),
			number(&c.BackupInterval, "backup.sync_interval", "Sync-Intervall (s)", 10),
		),
		in("Ablauf",
			text(&c.DailyAt, "schedule.daily_at", "Tägliche Ausführung (HH:MM)", "03:00"),
			text(&c.HTTPAddr, "http.addr", "Web-UI Adresse", "localhost:8080"),
		),
	)
}

// Field describes one configuration value for the settings form.
type Field struct {
	Key, Label, Group, Kind, Value string
	Secret                         bool
}

// binding is a Field plus the means to read and write the Config field behind
// it. Value stays empty until Fields fills it in.
type binding struct {
	Field
	def string // default in stored form, used when nothing is stored
	get func() string
	set func(raw string) error
}

// in puts the bindings into one group of the settings form.
func in(group string, bs ...binding) []binding {
	for i := range bs {
		bs[i].Group = group
	}
	return bs
}

func text(p *string, key, label, def string) binding {
	return binding{
		Field: Field{Key: key, Label: label, Kind: "text"},
		def:   def,
		get:   func() string { return *p },
		set:   func(raw string) error { *p = raw; return nil },
	}
}

// secret is a text setting that is never sent back to the browser.
func secret(p *string, key, label string) binding {
	b := text(p, key, label, "")
	b.Secret = true
	return b
}

func number(p *int, key, label string, def int) binding {
	return binding{
		Field: Field{Key: key, Label: label, Kind: "number"},
		def:   strconv.Itoa(def),
		get:   func() string { return strconv.Itoa(*p) },
		set: func(raw string) error {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return err
			}
			*p = n
			return nil
		},
	}
}

func decimal(p *float64, key, label string, def float64) binding {
	format := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	return binding{
		Field: Field{Key: key, Label: label, Kind: "number"},
		def:   format(def),
		get:   func() string { return format(*p) },
		set: func(raw string) error {
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return err
			}
			*p = f
			return nil
		},
	}
}

func toggle(p *bool, key, label string, def bool) binding {
	return binding{
		Field: Field{Key: key, Label: label, Kind: "checkbox"},
		def:   strconv.FormatBool(def),
		get:   func() string { return strconv.FormatBool(*p) },
		set:   func(raw string) error { *p = raw == "true" || raw == "on" || raw == "1"; return nil },
	}
}

// Fields returns the settings form description, in declaration order.
func (c *Config) Fields() []Field {
	var out []Field
	for _, b := range c.bindings() {
		f := b.Field
		f.Value = b.get()
		out = append(out, f)
	}
	return out
}

// Groups returns the fields bundled per group, keeping declaration order.
func (c *Config) Groups() []struct {
	Name   string
	Fields []Field
} {
	type group = struct {
		Name   string
		Fields []Field
	}
	var out []group
	index := map[string]int{}
	for _, f := range c.Fields() {
		i, ok := index[f.Group]
		if !ok {
			i = len(out)
			index[f.Group] = i
			out = append(out, group{Name: f.Group})
		}
		out[i].Fields = append(out[i].Fields, f)
	}
	return out
}

// Load builds a Config from stored settings, falling back to the defaults
// declared in bindings.
func Load(values map[string]string) (*Config, error) {
	c := &Config{}
	var err error
	for _, b := range c.bindings() {
		raw, ok := values[b.Key]
		if !ok || raw == "" {
			raw = b.def
		}
		if e := b.set(raw); e != nil && err == nil {
			err = fmt.Errorf("setting %s: %w", b.Key, e)
		}
	}
	return c, err
}

// Values turns a Config back into the flat map stored in the database.
func (c *Config) Values() map[string]string {
	m := map[string]string{}
	for _, b := range c.bindings() {
		m[b.Key] = b.get()
	}
	return m
}
