package narrative

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

// OpenAICompatible generates narratives via any OpenAI-compatible
// chat-completions endpoint: OpenAI itself, Ollama, vLLM, LM Studio, Groq,
// Mistral, OpenRouter, and most other hosted or local LLM servers.
type OpenAICompatible struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewOpenAICompatible creates a generator that POSTs to {baseURL}/chat/completions.
// apiKey may be empty for local servers that don't require authentication.
func NewOpenAICompatible(baseURL, apiKey, model string) *OpenAICompatible {
	return &OpenAICompatible{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

type chatRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Messages  []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate produces a 2–3 sentence blame narrative for the given edge.
func (g *OpenAICompatible) Generate(ctx context.Context, edge models.BlameEdge) (string, error) {
	if edge.CostSnapshot == nil || edge.DeployEvent == nil {
		return "", fmt.Errorf("narrative: edge %s has nil denormalized fields — hydrate before generating", edge.ID)
	}

	systemPrompt, userPrompt := BuildPrompt(BuildContext(edge))

	body, err := json.Marshal(chatRequest{
		Model:     g.model,
		MaxTokens: 350,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("narrative: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("narrative: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("narrative: POST %s: %w", g.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("narrative: read response: %w", err)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("narrative: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(raw))
		if cr.Error != nil {
			msg = cr.Error.Message
		}
		return "", fmt.Errorf("narrative: LLM API returned %d: %s", resp.StatusCode, msg)
	}
	if len(cr.Choices) == 0 || cr.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("narrative: no completion in LLM response")
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}
