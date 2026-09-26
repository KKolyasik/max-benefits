package llm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	yandexCompletionAsync = "https://ai.api.cloud.yandex.net/foundationModels/v1/completionAsync"
	yandexOperations      = "https://operation.api.cloud.yandex.net/operations/"
)

// errBusy means the server answered 429 or 5xx even after a few attempts.
var errBusy = errors.New("model server is busy")

// Async talks to a Yandex model in the asynchronous mode of Yandex AI
// Studio: the request waits in a queue, from seconds to hours, and costs
// half as much. Only Yandex's own models (YandexGPT, Alice AI LLM) have it.
type Async struct {
	APIKey string
	Folder string
	// Model is the model URI, e.g. gpt://<folder>/yandexgpt-5.1.
	Model string
	HTTP  *http.Client
	// Poll is the longest pause between checks of a request, 30 seconds by
	// default; MaxWait is how long to wait for an answer, 3 hours.
	Poll    time.Duration
	MaxWait time.Duration
	// SubmitURL and OperationsURL are overridable in tests.
	SubmitURL     string
	OperationsURL string
}

type asyncRequest struct {
	ModelURI          string            `json:"modelUri"`
	CompletionOptions completionOptions `json:"completionOptions"`
	Messages          []asyncMessage    `json:"messages"`
	JSONSchema        asyncSchema       `json:"jsonSchema"`
}

type completionOptions struct {
	Temperature float64 `json:"temperature"`
	// Yandex APIs write 64-bit integers as strings.
	MaxTokens string `json:"maxTokens"`
}

type asyncMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type asyncSchema struct {
	Schema any `json:"schema"`
}

type operation struct {
	ID    string `json:"id"`
	Done  bool   `json:"done"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response *struct {
		Alternatives []struct {
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
			Status string `json:"status"`
		} `json:"alternatives"`
		Usage struct {
			InputTextTokens  count `json:"inputTextTokens"`
			CompletionTokens count `json:"completionTokens"`
		} `json:"usage"`
	} `json:"response"`
}

// count is a number that Yandex APIs may write as a string.
type count int

func (c *count) UnmarshalJSON(b []byte) error {
	n, err := strconv.Atoi(strings.Trim(string(b), `"`))
	if err != nil {
		return fmt.Errorf("token count %s: %w", b, err)
	}
	*c = count(n)
	return nil
}

// Complete sends the conversation and waits for the answer, which must match
// the schema.
func (a *Async) Complete(ctx context.Context, messages []Message, _ string, schema any) (Answer, error) {
	req := asyncRequest{
		ModelURI:          a.Model,
		CompletionOptions: completionOptions{Temperature: temperature, MaxTokens: strconv.Itoa(MaxAnswerTokens)},
		JSONSchema:        asyncSchema{Schema: schema},
	}
	for _, m := range messages {
		req.Messages = append(req.Messages, asyncMessage{Role: m.Role, Text: m.Content})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Answer{}, err
	}
	var op operation
	if err := a.call(ctx, http.MethodPost, cmp.Or(a.SubmitURL, yandexCompletionAsync), body, &op); err != nil {
		return Answer{}, err
	}

	// Short requests are done in seconds, so the checks start often.
	longest := cmp.Or(a.Poll, 30*time.Second)
	pause := min(time.Second, longest)
	deadline := time.Now().Add(cmp.Or(a.MaxWait, 3*time.Hour))
	for !op.Done {
		if time.Now().After(deadline) {
			return Answer{}, fmt.Errorf("model gave no answer in %s (operation %s)", cmp.Or(a.MaxWait, 3*time.Hour), op.ID)
		}
		select {
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		case <-time.After(pause):
		}
		pause = min(pause*2, longest)
		var next operation
		err := a.call(ctx, http.MethodGet, cmp.Or(a.OperationsURL, yandexOperations)+op.ID, nil, &next)
		switch {
		case errors.Is(err, ErrUnreachable) || errors.Is(err, errBusy):
			continue // the request goes on without us, check again later
		case err != nil:
			return Answer{}, err
		}
		op = next
	}

	if op.Error != nil {
		return Answer{}, fmt.Errorf("model: %s (code %d)", op.Error.Message, op.Error.Code)
	}
	if op.Response == nil || len(op.Response.Alternatives) == 0 {
		return Answer{}, errors.New("model returned no alternatives")
	}
	alt := op.Response.Alternatives[0]
	if alt.Status == "ALTERNATIVE_STATUS_CONTENT_FILTER" {
		return Answer{}, errors.New("model's answer was blocked by the content filter")
	}
	return Answer{
		Content:      alt.Message.Text,
		Truncated:    alt.Status == "ALTERNATIVE_STATUS_TRUNCATED_FINAL",
		InputTokens:  int(op.Response.Usage.InputTextTokens),
		OutputTokens: int(op.Response.Usage.CompletionTokens),
	}, nil
}

// call sends a request to the Yandex API and decodes the answer into out.
func (a *Async) call(ctx context.Context, method, url string, body []byte, out *operation) error {
	status, data, err := send(ctx, a.HTTP, func() (*http.Request, error) {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Api-Key "+a.APIKey)
		if a.Folder != "" {
			req.Header.Set("x-folder-id", a.Folder)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w (%d): %s", ErrUnauthorized, status, snippet(data))
	case status == http.StatusNotFound && body != nil:
		return fmt.Errorf("model: %s has no asynchronous mode, set YANDEX_ASYNC=false: %s", a.Model, snippet(data))
	case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %d %s", errBusy, status, snippet(data))
	case status != http.StatusOK:
		return fmt.Errorf("model: %d %s", status, snippet(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode model answer: %w", err)
	}
	return nil
}
