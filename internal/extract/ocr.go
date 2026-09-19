package extract

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MistralOCR reads scanned documents through the Mistral OCR API
// (https://docs.mistral.ai/api/endpoint/ocr). The endpoint is configurable, so
// any service speaking the same shape works too.
type MistralOCR struct {
	BaseURL, APIKey, Model string
	HTTP                   *http.Client
}

// NewMistralOCR builds an OCR backend.
func NewMistralOCR(baseURL, apiKey, model string, timeout time.Duration) *MistralOCR {
	return &MistralOCR{BaseURL: strings.TrimSuffix(baseURL, "/"), APIKey: apiKey, Model: model,
		HTTP: &http.Client{Timeout: timeout}}
}

// maxOCRBytes guards against posting a huge scan as base64.
const maxOCRBytes = 40 << 20

// Text runs OCR over the document and returns its text as markdown.
func (m *MistralOCR) Text(ctx context.Context, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxOCRBytes {
		return "", fmt.Errorf("document is %d bytes, too large for OCR", info.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mediaType := mime.TypeByExtension(filepath.Ext(path))
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	dataURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(raw)

	// Images and PDFs use different document fields in the OCR API.
	document := map[string]string{"type": "document_url", "document_url": dataURL}
	if strings.HasPrefix(mediaType, "image/") {
		document = map[string]string{"type": "image_url", "image_url": dataURL}
	}
	body, err := json.Marshal(map[string]any{"model": m.Model, "document": document})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/ocr", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)

	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ocr endpoint returned %s: %s", resp.Status, truncate(answer, 500))
	}
	var parsed struct {
		Pages []struct {
			Markdown string `json:"markdown"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(answer, &parsed); err != nil {
		return "", fmt.Errorf("unexpected response from ocr endpoint: %s", truncate(answer, 500))
	}
	var out strings.Builder
	for _, p := range parsed.Pages {
		out.WriteString(p.Markdown)
		out.WriteString("\n")
	}
	return out.String(), nil
}
