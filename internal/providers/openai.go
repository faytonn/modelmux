package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// OpenAI calls the Chat Completions API (POST /v1/chat/completions).
type OpenAI struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewOpenAI creates an adapter. client should be shared and long-lived so
// connections are reused; timeouts come from the request context, not the client.
func NewOpenAI(name, baseURL, apiKey string, client *http.Client) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	return &OpenAI{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client}
}

func (o *OpenAI) Name() string { return o.name }

type openAIRequest struct {
	Model               string    `json:"model"`
	Messages            []Message `json:"messages"` // OpenAI accepts system messages inline
	MaxCompletionTokens int       `json:"max_completion_tokens"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (o *OpenAI) Chat(ctx context.Context, req Request) (*Response, error) {
	body := openAIRequest{Model: req.Model, Messages: req.Messages, MaxCompletionTokens: req.MaxTokens}
	headers := map[string]string{"Authorization": "Bearer " + o.apiKey}

	var out openAIResponse
	if err := postJSON(ctx, o.client, o.name, o.baseURL+"/v1/chat/completions", headers, body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New(o.name + ": response has no choices")
	}
	return &Response{
		Content:      out.Choices[0].Message.Content,
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
	}, nil
}
