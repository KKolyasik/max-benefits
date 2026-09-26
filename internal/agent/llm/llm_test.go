package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	retryPauses = []time.Duration{time.Millisecond, time.Millisecond}
	os.Exit(m.Run())
}

func TestComplete(t *testing.T) {
	var got struct {
		path, auth, project string
		body                map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.project = r.Header.Get("OpenAI-Project")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"cards\":[]}"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":20}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/v1/", APIKey: "key", Model: "gpt://folder/yandexgpt/latest", Project: "folder", HTTP: srv.Client()}
	answer, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, "cards", map[string]any{"type": "object"})
	if err != nil {
		t.Fatal(err)
	}

	if answer.Content != `{"cards":[]}` || answer.Truncated || answer.InputTokens != 100 || answer.OutputTokens != 20 {
		t.Errorf("answer: %+v", answer)
	}
	if got.path != "/v1/chat/completions" || got.auth != "Bearer key" || got.project != "folder" {
		t.Errorf("request: %s %q %q", got.path, got.auth, got.project)
	}
	format := got.body["response_format"].(map[string]any)
	if got.body["model"] != "gpt://folder/yandexgpt/latest" || format["type"] != "json_schema" {
		t.Errorf("body: %v", got.body)
	}
}

// One failed attempt must not cost the query: the network and the API fail
// now and then.
func TestCompleteRepeatsAFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if answer, err := c.Complete(context.Background(), nil, "x", nil); err != nil || answer.Content != "{}" || calls.Load() != 2 {
		t.Errorf("answer %+v, err %v after %d calls", answer, err, calls.Load())
	}
}

func TestCompleteErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   error
	}{
		"rejected key": {http.StatusUnauthorized, `{"error":"bad key"}`, ErrUnauthorized},
		"forbidden":    {http.StatusForbidden, `{}`, ErrUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
			if _, err := c.Complete(context.Background(), nil, "x", nil); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("server down", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		c := &Client{BaseURL: url, HTTP: http.DefaultClient}
		if _, err := c.Complete(context.Background(), nil, "x", nil); !errors.Is(err, ErrUnreachable) {
			t.Errorf("got %v", err)
		}
	})

	t.Run("bad request is not repeated", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			http.Error(w, "bad schema", http.StatusBadRequest)
		}))
		defer srv.Close()
		c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
		if _, err := c.Complete(context.Background(), nil, "x", nil); err == nil || calls.Load() != 1 {
			t.Errorf("err %v after %d calls", err, calls.Load())
		}
	})

	t.Run("truncated", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"content":"{\"ca"}}]}`))
		}))
		defer srv.Close()
		c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
		answer, err := c.Complete(context.Background(), nil, "x", nil)
		if err != nil || !answer.Truncated {
			t.Errorf("answer %+v, err %v", answer, err)
		}
	})
}
