package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// systemPrompt keeps the model on the narrow job of reading fields off an
// invoice. Invoices arrive in German and English, so the prompt says so.
const systemPrompt = `You extract structured data from invoices written in German or English.
Return only what the document states. Leave a field empty when the invoice does not contain it;
never guess or compute a value that is not printed on the document.
Amounts are decimal numbers with a dot as the decimal separator and no thousands separator.
The total is the gross amount payable, including VAT.
Dates use the format YYYY-MM-DD.`

// invoiceSchema is the structured-output schema the model must fill in.
var invoiceSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"sender_name":    map[string]any{"type": "string", "description": "company issuing the invoice"},
		"sender_address": map[string]any{"type": "string", "description": "postal address of the issuer, one line"},
		"sender_vat_id":  map[string]any{"type": "string", "description": "VAT identification number, e.g. DE123456789"},
		"invoice_number": map[string]any{"type": "string"},
		"invoice_date":   map[string]any{"type": "string", "description": "YYYY-MM-DD"},
		"total":          map[string]any{"type": "string", "description": "gross total, e.g. 1234.56"},
		"currency":       map[string]any{"type": "string", "description": "ISO 4217 code, e.g. EUR"},
		"vat":            map[string]any{"type": "string", "description": "VAT amount, empty if the invoice shows none"},
		"vat_rate":       map[string]any{"type": "number", "description": "VAT percentage, 0 if the invoice shows none"},
	},
	"required": []string{"sender_name", "sender_address", "sender_vat_id", "invoice_number",
		"invoice_date", "total", "currency", "vat", "vat_rate"},
	"additionalProperties": false,
}

// maxPromptChars caps how much document text we hand to the model. Invoice
// data lives on the first page; anything beyond that is terms and conditions.
const maxPromptChars = 24000

func prompt(text string) string {
	if len(text) > maxPromptChars {
		text = text[:maxPromptChars]
	}
	return "Extract the invoice data from this document:\n\n" + text
}

// decode turns the model's JSON answer into Data.
func decode(raw string) (Data, error) {
	raw = strings.TrimSpace(raw)
	// Models occasionally wrap JSON in a markdown fence despite the schema.
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")

	var d Data
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return Data{}, fmt.Errorf("model did not return JSON: %w", err)
	}
	d.TotalCents, _ = ParseAmount(d.Total)
	d.VATCents, _ = ParseAmount(d.VAT)
	d.Date = NormalizeDate(d.Date)
	d.Currency = strings.ToUpper(strings.TrimSpace(d.Currency))
	return withDerivedVAT(d), nil
}

// AnthropicLLM extracts invoice data with Claude through the official SDK.
type AnthropicLLM struct {
	client anthropic.Client
	model  string
}

// NewAnthropicLLM builds a Claude-backed extractor. baseURL may be empty to
// use the default API endpoint.
func NewAnthropicLLM(baseURL, apiKey, model string, timeout time.Duration) *AnthropicLLM {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithRequestTimeout(timeout)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &AnthropicLLM{client: anthropic.NewClient(opts...), model: model}
}

// ExtractInvoice reads the invoice fields off the document text.
func (a *AnthropicLLM) ExtractInvoice(ctx context.Context, text string) (Data, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 8000,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		// Reading fields off an invoice is a simple job; low effort keeps it cheap.
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: invoiceSchema},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt(text))),
		},
	})
	if err != nil {
		return Data{}, err
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return Data{}, fmt.Errorf("model declined the request: %s", resp.StopDetails.Explanation)
	}
	var out strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	return decode(out.String())
}

// OpenAILLM talks to any OpenAI-compatible chat-completions endpoint, for
// setups that run a different or self-hosted model.
type OpenAILLM struct {
	BaseURL, APIKey, Model string
	HTTP                   *http.Client
}

// NewOpenAILLM builds an extractor for an OpenAI-compatible endpoint.
func NewOpenAILLM(baseURL, apiKey, model string, timeout time.Duration) *OpenAILLM {
	return &OpenAILLM{BaseURL: strings.TrimSuffix(baseURL, "/"), APIKey: apiKey, Model: model,
		HTTP: &http.Client{Timeout: timeout}}
}

// ExtractInvoice reads the invoice fields off the document text.
func (o *OpenAILLM) ExtractInvoice(ctx context.Context, text string) (Data, error) {
	body, err := json.Marshal(map[string]any{
		"model": o.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt + "\nAnswer with a single JSON object."},
			{"role": "user", "content": prompt(text)},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": "invoice", "strict": true, "schema": invoiceSchema,
			},
		},
	})
	if err != nil {
		return Data{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Data{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return Data{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Data{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Data{}, fmt.Errorf("llm endpoint returned %s: %s", resp.Status, truncate(raw, 500))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return Data{}, fmt.Errorf("unexpected response from llm endpoint: %s", truncate(raw, 500))
	}
	return decode(parsed.Choices[0].Message.Content)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
