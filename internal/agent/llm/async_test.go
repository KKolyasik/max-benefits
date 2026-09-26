package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// yandex fakes the asynchronous API: the operation is done after `wait`
// checks, and the first `busy` checks fail with 503.
type yandex struct {
	wait, busy int32
	submit     int
	done       string
	checks     atomic.Int32
	body       map[string]any
	auth       string
	folder     string
}

func (y *yandex) start(t *testing.T) *Async {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			y.auth, y.folder = r.Header.Get("Authorization"), r.Header.Get("x-folder-id")
			_ = json.NewDecoder(r.Body).Decode(&y.body)
			if y.submit != 0 {
				http.Error(w, `{"message":"no"}`, y.submit)
				return
			}
			_, _ = w.Write([]byte(`{"id":"op1","done":false}`))
			return
		}
		n := y.checks.Add(1)
		switch {
		case r.URL.Path != "/operations/op1":
			http.NotFound(w, r)
		case n <= y.busy:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		case n <= y.wait:
			_, _ = w.Write([]byte(`{"id":"op1","done":false}`))
		default:
			_, _ = w.Write([]byte(y.done))
		}
	}))
	t.Cleanup(srv.Close)
	return &Async{
		APIKey: "key", Folder: "folder", Model: "gpt://folder/yandexgpt-5.1", HTTP: srv.Client(),
		Poll: time.Millisecond, SubmitURL: srv.URL + "/completionAsync", OperationsURL: srv.URL + "/operations/",
	}
}

const doneOK = `{"id":"op1","done":true,"response":{
	"alternatives":[{"message":{"role":"assistant","text":"{\"cards\":[]}"},"status":"ALTERNATIVE_STATUS_FINAL"}],
	"usage":{"inputTextTokens":"120","completionTokens":"30","totalTokens":"150"}}}`

// The operations server fails longer than one check repeats a request, and
// the answer still comes: the request goes on at Yandex meanwhile.
func TestAsyncComplete(t *testing.T) {
	y := &yandex{wait: 6, busy: 4, done: doneOK}
	a := y.start(t)
	msgs := []Message{{Role: "system", Content: "правила"}, {Role: "user", Content: "запрос"}}

	answer, err := a.Complete(context.Background(), msgs, "cards", map[string]any{"type": "object"})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != `{"cards":[]}` || answer.Truncated || answer.InputTokens != 120 || answer.OutputTokens != 30 {
		t.Errorf("answer %+v", answer)
	}
	if y.auth != "Api-Key key" || y.folder != "folder" {
		t.Errorf("headers %q %q", y.auth, y.folder)
	}
	body, _ := json.Marshal(y.body)
	for _, want := range []string{`"modelUri":"gpt://folder/yandexgpt-5.1"`, `"maxTokens":"4000"`,
		`"messages":[{"role":"system","text":"правила"},{"role":"user","text":"запрос"}]`, `"jsonSchema":{"schema":{"type":"object"}}`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("request %s has no %s", body, want)
		}
	}
}

func TestAsyncErrors(t *testing.T) {
	cases := map[string]struct {
		y    *yandex
		want string
	}{
		"rejected key":          {&yandex{submit: http.StatusUnauthorized}, ErrUnauthorized.Error()},
		"model without async":   {&yandex{submit: http.StatusNotFound}, "YANDEX_ASYNC=false"},
		"failed operation":      {&yandex{done: `{"id":"op1","done":true,"error":{"code":3,"message":"bad schema"}}`}, "bad schema"},
		"blocked answer":        {&yandex{done: `{"id":"op1","done":true,"response":{"alternatives":[{"status":"ALTERNATIVE_STATUS_CONTENT_FILTER"}]}}`}, "content filter"},
		"no answer in time":     {&yandex{wait: 1 << 30}, "no answer"},
		"server busy on submit": {&yandex{submit: http.StatusServiceUnavailable}, errBusy.Error()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := tc.y.start(t)
			a.MaxWait = 50 * time.Millisecond
			_, err := a.Complete(context.Background(), nil, "x", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("truncated", func(t *testing.T) {
		a := (&yandex{done: strings.Replace(doneOK, "STATUS_FINAL", "STATUS_TRUNCATED_FINAL", 1)}).start(t)
		if answer, err := a.Complete(context.Background(), nil, "x", nil); err != nil || !answer.Truncated {
			t.Errorf("answer %+v, err %v", answer, err)
		}
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		a := (&yandex{wait: 1 << 30}).start(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := a.Complete(ctx, nil, "x", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v", err)
		}
	})
}
