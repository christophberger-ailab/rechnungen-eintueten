// Package config is the typed view of the settings table. The struct tags are
// the single source of truth: they drive loading, saving and the settings form
// rendered by the web UI.
package config

import (
	"fmt"
	"reflect"
	"strconv"
)

// Config holds every configurable value of the application.
type Config struct {
	IMAPHost    string `cfg:"imap.host" group:"Mailbox (IMAP)" label:"Server"`
	IMAPPort    int    `cfg:"imap.port" group:"Mailbox (IMAP)" label:"Port" default:"993"`
	IMAPUser    string `cfg:"imap.user" group:"Mailbox (IMAP)" label:"Benutzer"`
	IMAPPass    string `cfg:"imap.pass" group:"Mailbox (IMAP)" label:"Passwort" secret:"true"`
	IMAPMailbox string `cfg:"imap.mailbox" group:"Mailbox (IMAP)" label:"Postfach" default:"INBOX"`
	IMAPTLS     bool   `cfg:"imap.tls" group:"Mailbox (IMAP)" label:"TLS" default:"true"`
	IMAPDays    int    `cfg:"imap.days" group:"Mailbox (IMAP)" label:"Zeitraum (Tage)" default:"30"`

	SMTPHost string `cfg:"smtp.host" group:"Mailversand (SMTP)" label:"Server"`
	SMTPPort int    `cfg:"smtp.port" group:"Mailversand (SMTP)" label:"Port" default:"587"`
	SMTPUser string `cfg:"smtp.user" group:"Mailversand (SMTP)" label:"Benutzer"`
	SMTPPass string `cfg:"smtp.pass" group:"Mailversand (SMTP)" label:"Passwort" secret:"true"`
	SMTPFrom string `cfg:"smtp.from" group:"Mailversand (SMTP)" label:"Absenderadresse"`

	LLMBaseURL string `cfg:"llm.base_url" group:"LLM" label:"API Base URL" default:"https://api.anthropic.com/v1"`
	LLMKey     string `cfg:"llm.api_key" group:"LLM" label:"API Key" secret:"true"`
	LLMModel   string `cfg:"llm.model" group:"LLM" label:"Modell" default:"claude-opus-5"`
	LLMKind    string `cfg:"llm.kind" group:"LLM" label:"API-Dialekt (anthropic|openai)" default:"anthropic"`
	LLMTimeout int    `cfg:"llm.timeout" group:"LLM" label:"Timeout (s)" default:"120"`

	OCRBaseURL string `cfg:"ocr.base_url" group:"OCR" label:"API Base URL" default:"https://api.mistral.ai/v1"`
	OCRKey     string `cfg:"ocr.api_key" group:"OCR" label:"API Key" secret:"true"`
	OCRModel   string `cfg:"ocr.model" group:"OCR" label:"Modell" default:"mistral-ocr-latest"`

	DatevTo      string `cfg:"datev.to" group:"DATEV" label:"DATEV-Empfangsadresse"`
	DatevFrom    string `cfg:"datev.from" group:"DATEV" label:"Absenderadresse"`
	DatevSubject string `cfg:"datev.subject" group:"DATEV" label:"Betreff-Vorlage" default:"Rechnungseingang {{number}}"`
	DatevEnabled bool   `cfg:"datev.enabled" group:"DATEV" label:"Aktiv" default:"true"`

	SevDeskBaseURL string  `cfg:"sevdesk.base_url" group:"sevDesk" label:"API Base URL" default:"https://my.sevdesk.de/api/v1"`
	SevDeskToken   string  `cfg:"sevdesk.token" group:"sevDesk" label:"API Token" secret:"true"`
	SevDeskEnabled bool    `cfg:"sevdesk.enabled" group:"sevDesk" label:"Aktiv" default:"true"`
	SevDeskTol     float64 `cfg:"sevdesk.fx_tolerance" group:"sevDesk" label:"FX-Toleranz (%)" default:"5"`
	SevDeskGain    string  `cfg:"sevdesk.gain_account" group:"sevDesk" label:"Konto Erlös Währungsumrechnung" default:"4840"`
	SevDeskLoss    string  `cfg:"sevdesk.loss_account" group:"sevDesk" label:"Konto Verlust Währungsumrechnung" default:"6880"`

	ArchiveDir string `cfg:"archive.base_dir" group:"Ablage" label:"Basisverzeichnis" default:"./FIBU"`
	SpoolDir   string `cfg:"spool.dir" group:"Ablage" label:"Spool-Verzeichnis (Originale)" default:"./spool"`

	DailyAt  string `cfg:"schedule.daily_at" group:"Ablauf" label:"Tägliche Ausführung (HH:MM)" default:"03:00"`
	HTTPAddr string `cfg:"http.addr" group:"Ablauf" label:"Web-UI Adresse" default:":8080"`
}

// Field describes one configuration value for the settings form.
type Field struct {
	Key, Label, Group, Kind, Value string
	Secret                         bool
}

// Fields returns the settings form description, in declaration order.
func (c *Config) Fields() []Field {
	var out []Field
	c.each(func(f reflect.StructField, v reflect.Value) {
		out = append(out, Field{
			Key:    f.Tag.Get("cfg"),
			Label:  f.Tag.Get("label"),
			Group:  f.Tag.Get("group"),
			Kind:   kindOf(v),
			Value:  format(v),
			Secret: f.Tag.Get("secret") == "true",
		})
	})
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
// declared in the struct tags.
func Load(values map[string]string) (*Config, error) {
	c := &Config{}
	var err error
	c.each(func(f reflect.StructField, v reflect.Value) {
		key := f.Tag.Get("cfg")
		raw, ok := values[key]
		if !ok || raw == "" {
			raw = f.Tag.Get("default")
		}
		if e := set(v, raw); e != nil && err == nil {
			err = fmt.Errorf("setting %s: %w", key, e)
		}
	})
	return c, err
}

// Values turns a Config back into the flat map stored in the database.
func (c *Config) Values() map[string]string {
	m := map[string]string{}
	c.each(func(f reflect.StructField, v reflect.Value) { m[f.Tag.Get("cfg")] = format(v) })
	return m
}

func (c *Config) each(fn func(reflect.StructField, reflect.Value)) {
	rv := reflect.ValueOf(c).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Tag.Get("cfg") == "" {
			continue
		}
		fn(rt.Field(i), rv.Field(i))
	}
}

func kindOf(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Bool:
		return "checkbox"
	case reflect.Int, reflect.Float64:
		return "number"
	default:
		return "text"
	}
}

func format(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	default:
		return v.String()
	}
}

func set(v reflect.Value, raw string) error {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(raw == "true" || raw == "on" || raw == "1")
	case reflect.Int:
		if raw == "" {
			return nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Float64:
		if raw == "" {
			return nil
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		v.SetFloat(f)
	default:
		v.SetString(raw)
	}
	return nil
}
