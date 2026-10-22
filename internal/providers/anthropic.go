package providers

import (
	"context"
	"net/http"
	"strings"
)

const anthropicVersion = "2023-06-01"

// Anthropic calls the Claude Messages API (POST /v1/messages).
type Anthropic struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewAnthropic creates an adapter. client should be shared and long-lived so
// connections are reused; timeouts come from the request context, not the client.
func NewAnthropic(name, baseURL, apiKey string, client *http.Client) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &Anthropic{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client}
}

func (a *Anthropic) Name() string { return a.name }

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) Chat(ctx context.Context, req Request) (*Response, error) {
	// Anthropic takes the system prompt as a top-level field, not as a message.
	body := anthropicRequest{Model: req.Model, MaxTokens: req.MaxTokens}
	var system []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = append(system, m.Content)
			continue
		}
		body.Messages = append(body.Messages, anthropicMessage{Role: m.Role, Content: m.Content})
	}
	body.System = strings.Join(system, "\n\n")

	headers := map[string]string{"x-api-key": a.apiKey, "anthropic-version": anthropicVersion}
	var out anthropicResponse
	if err := postJSON(ctx, a.client, a.name, a.baseURL+"/v1/messages", headers, body, &out); err != nil {
		return nil, err
	}

	var text strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return &Response{Content: text.String(), InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}, nil
}
