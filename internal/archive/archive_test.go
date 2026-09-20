package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDir(t *testing.T) {
	cases := map[string]string{
		"2026-08-01": "base/FIBU 2026/Rechnungseingang/E26Q3",
		"2026-01-31": "base/FIBU 2026/Rechnungseingang/E26Q1",
		"2025-12-31": "base/FIBU 2025/Rechnungseingang/E25Q4",
		"2026-04-01": "base/FIBU 2026/Rechnungseingang/E26Q2",
	}
	for in, want := range cases {
		d, err := time.Parse("2006-01-02", in)
		if err != nil {
			t.Fatal(err)
		}
		if got := Dir("base", d); got != filepath.FromSlash(want) {
			t.Errorf("Dir(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestFileName(t *testing.T) {
	got := FileName("2026-03-14", "Musterlieferant GmbH", "RE 2026/0042", "anhang.pdf")
	if want := "2026-03-14_Musterlieferant-GmbH_RE-2026-0042.pdf"; got != want {
		t.Errorf("FileName = %q, want %q", got, want)
	}
	if got := FileName("", "", "", "original.pdf"); got != "original.pdf" {
		t.Errorf("fallback FileName = %q", got)
	}
}

func TestStoreDoesNotOverwrite(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src.pdf")
	if err := os.WriteFile(src, []byte("invoice"), 0o644); err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	first, err := Store(base, date, "rechnung.pdf", src)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Store(base, date, "rechnung.pdf", src)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("second Store overwrote the first file at %s", first)
	}
	if filepath.Base(second) != "rechnung(2).pdf" {
		t.Errorf("second file = %q", filepath.Base(second))
	}
	if b, err := os.ReadFile(first); err != nil || string(b) != "invoice" {
		t.Errorf("content = %q, err %v", b, err)
	}
}
