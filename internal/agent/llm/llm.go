// Package llm calls a language model through the OpenAI-compatible Chat
// Completions API, which Yandex AI Studio, Ollama and most other providers
// implement, or through the asynchronous mode of Yandex AI Studio, which is
// twice as cheap. Only what the agent needs is here: one request with a JSON
// schema for the answer.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrUnreachable means the model server could not be reached at all.
	ErrUnreachable = errors.New("model is unreachable")
	// ErrUnauthorized means the server rejected the API key.
	ErrUnauthorized = errors.New("model rejected the API key")
)

// MaxAnswerTokens caps the model's answer.
const MaxAnswerTokens = 4000

const temperature = 0.2

// retryPauses are the pauses before repeating a request that failed on the
// way. Tests shorten them.
var retryPauses = []time.Duration{2 * time.Second, 10 * time.Second}

// Client talks to one model.
type Client struct {
	// BaseURL ends with /v1, e.g. https://ai.api.cloud.yandex.net/v1.
	BaseURL string
	APIKey  string
	Model   string
	// Project is sent as OpenAI-Project; Yandex AI Studio takes the folder
	// ID there.
	Project string
	HTTP    *http.Client
}

// Message is a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Answer is the model's reply.
type Answer struct {
	Content string
	// Truncated is set when the reply hit the token limit.
	Truncated    bool
	InputTokens  int
	OutputTokens int
}

type request struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	Temperature    float64        `json:"temperature"`
	MaxTokens      int            `json:"max_tokens"`
	ResponseFormat responseFormat `json:"response_format"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string `json:"name"`
	Schema any    `json:"schema"`
}

type response struct {
	Choices []struct {
		FinishReason string  `json:"finish_reason"`
		Message      Message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete asks the model to answer the conversation with JSON matching the
// schema.
func (c *Client) Complete(ctx context.Context, messages []Message, schemaName string, schema any) (Answer, error) {
	body, err := json.Marshal(request{
		Model:       c.Model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   MaxAnswerTokens,
		ResponseFormat: responseFormat{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: schemaName, Schema: schema},
		},
	})
	if err != nil {
		return Answer{}, err
	}
	status, data, err := send(ctx, c.HTTP, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		if c.Project != "" {
			req.Header.Set("OpenAI-Project", c.Project)
		}
		return req, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return Answer{}, ctx.Err()
		}
		return Answer{}, fmt.Errorf("%w at %s: %w", ErrUnreachable, c.BaseURL, err)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Answer{}, fmt.Errorf("%w (%d): %s", ErrUnauthorized, status, snippet(data))
	case status != http.StatusOK:
		return Answer{}, fmt.Errorf("model: %d %s", status, snippet(data))
	}

	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return Answer{}, fmt.Errorf("decode model answer: %w", err)
	}
	if len(r.Choices) == 0 {
		return Answer{}, errors.New("model returned no choices")
	}
	return Answer{
		Content:      r.Choices[0].Message.Content,
		Truncated:    r.Choices[0].FinishReason == "length",
		InputTokens:  r.Usage.PromptTokens,
		OutputTokens: r.Usage.CompletionTokens,
	}, nil
}

// send makes the request newReq builds and returns the status and the body.
// A dropped connection, 429 and 5xx are repeated after a pause: networks and
// APIs fail now and then, and one blip shouldn't cost a query. A timeout is
// not repeated, since a slow model would only be slow again.
func send(ctx context.Context, client *http.Client, newReq func() (*http.Request, error)) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return 0, nil, err
		}
		status, data, err := sendOnce(client, req)
		again := status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
		if err != nil {
			var nerr net.Error
			again = !errors.As(err, &nerr) || !nerr.Timeout()
		}
		if !again || attempt == len(retryPauses) || ctx.Err() != nil {
			return status, data, err
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(retryPauses[attempt]):
		}
	}
}

func sendOnce(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("read the answer: %w", err)
	}
	return resp.StatusCode, data, nil
}

func snippet(data []byte) string {
	s := strings.TrimSpace(string(data))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
