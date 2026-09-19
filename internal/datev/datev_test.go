package datev

import (
	"testing"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/mailbox"
)

func TestReplies(t *testing.T) {
	msgs := []mailbox.Message{
		{Subject: "Beleg RE-2026-0042 erfolgreich verarbeitet", Text: "Alles gut."},
		{Subject: "Fehler beim Import", Text: "Beleg RE-2026-0043 konnte nicht verarbeitet werden."},
		{Subject: "Hinweis", Text: "Kein klares Ergebnis."},
	}
	got := Replies(msgs)
	if len(got) != 3 {
		t.Fatalf("got %d replies", len(got))
	}
	if got[0].Failed {
		t.Error("success mail classified as failure")
	}
	if !got[1].Failed {
		t.Error("failure mail classified as success")
	}
	if !got[2].Failed {
		t.Error("an unclear mail must count as a failure so a human looks at it")
	}
	if !got[1].Mentions("RE-2026-0043") {
		t.Error("reply does not link to its invoice number")
	}
	if got[1].Mentions("RE-2026-0042") {
		t.Error("reply linked to the wrong invoice")
	}
}

func TestMentionsIgnoresFormatting(t *testing.T) {
	r := Reply{Text: "Beleg RE 2026 0042 gebucht"}
	if !r.Mentions("RE-2026-0042") {
		t.Error("invoice number must match across punctuation differences")
	}
	if r.Mentions("") {
		t.Error("an empty number must not match anything")
	}
}
