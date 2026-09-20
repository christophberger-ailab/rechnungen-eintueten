// Package archive files invoice originals in the bookkeeping directory tree.
package archive

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dir returns the directory an invoice of that date belongs into, for example
// "FIBU 2026/Rechnungseingang/E26Q3" below base.
func Dir(base string, date time.Time) string {
	year := date.Year()
	quarter := (int(date.Month())-1)/3 + 1
	return filepath.Join(base,
		fmt.Sprintf("FIBU %04d", year),
		"Rechnungseingang",
		fmt.Sprintf("E%02dQ%d", year%100, quarter))
}

// Store copies src into the directory for that date under the given file name
// and returns the path it was written to. An existing file with the same name
// is never overwritten: a counter is appended instead.
func Store(base string, date time.Time, name, src string) (string, error) {
	dir := Dir(base, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := freeName(dir, name)
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// FileName builds a sortable file name from the invoice data:
// "2026-03-14_Musterlieferant_RE-2026-0042.pdf".
func FileName(date, sender, number, original string) string {
	ext := filepath.Ext(original)
	if ext == "" {
		ext = ".pdf"
	}
	parts := []string{date, clean(sender), clean(number)}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return filepath.Base(original)
	}
	return strings.Join(kept, "_") + ext
}

// clean reduces a field to characters that are safe in a file name on every
// platform, and keeps it short enough to stay readable.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		case r == ' ', r == '.', r == '/', r == '\\':
			return '-'
		default:
			if r > 127 {
				return r // keep umlauts and the like
			}
			return -1
		}
	}, strings.TrimSpace(s))
	s = strings.Trim(s, "-_")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func freeName(dir, name string) string {
	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		dst = filepath.Join(dir, fmt.Sprintf("%s(%d)%s", stem, i, ext))
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			return dst
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
