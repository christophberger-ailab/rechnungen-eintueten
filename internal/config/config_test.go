package config

import "testing"

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(map[string]string{"imap.host": "mail.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAPHost != "mail.example.com" {
		t.Errorf("host = %q", cfg.IMAPHost)
	}
	if cfg.IMAPPort != 993 || cfg.IMAPMailbox != "INBOX" || !cfg.IMAPTLS {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.SevDeskTol != 5 {
		t.Errorf("FX tolerance = %v, want 5", cfg.SevDeskTol)
	}
}

func TestLoadParsesTypes(t *testing.T) {
	cfg, err := Load(map[string]string{
		"imap.port": "143", "imap.tls": "false", "sevdesk.fx_tolerance": "2.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAPPort != 143 || cfg.IMAPTLS || cfg.SevDeskTol != 2.5 {
		t.Errorf("parsed as %+v", cfg)
	}
	if _, err := Load(map[string]string{"imap.port": "keine Zahl"}); err == nil {
		t.Error("want an error for an unparseable number")
	}
}

func TestValuesRoundTrip(t *testing.T) {
	first, err := Load(map[string]string{"imap.port": "143", "imap.tls": "false", "llm.model": "x"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(first.Values())
	if err != nil {
		t.Fatal(err)
	}
	if *first != *second {
		t.Errorf("round trip changed the configuration:\n%+v\n%+v", first, second)
	}
}

func TestGroupsCoverEveryField(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	var inGroups int
	for _, g := range cfg.Groups() {
		if g.Name == "" {
			t.Error("a field is missing its group")
		}
		inGroups += len(g.Fields)
	}
	if fields := cfg.Fields(); inGroups != len(fields) {
		t.Errorf("groups hold %d fields, Fields() returns %d", inGroups, len(fields))
	}
	for _, f := range cfg.Fields() {
		if f.Key == "" || f.Label == "" {
			t.Errorf("field without key or label: %+v", f)
		}
	}
}

func TestSecretsAreMarked(t *testing.T) {
	cfg, _ := Load(nil)
	secrets := map[string]bool{}
	for _, f := range cfg.Fields() {
		if f.Secret {
			secrets[f.Key] = true
		}
	}
	for _, key := range []string{"imap.pass", "smtp.pass", "llm.api_key", "ocr.api_key", "sevdesk.token"} {
		if !secrets[key] {
			t.Errorf("%s is not marked as a secret", key)
		}
	}
}
