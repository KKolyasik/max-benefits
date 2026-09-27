package maxapi

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/KKolyasik/max-benefits/internal/bot"
)

const token = "test-token"

// Updates in the format of the MAX Bot API (taken from the official SDK
// fixtures): a dialog start, a text message, a message in a group chat that
// must be ignored, and a button press.
const updatesJSON = `{
  "updates": [
    {"timestamp": 1, "chat_id": 182, "user": {"user_id": 123, "name": "John"}, "user_locale": "ru", "update_type": "bot_started"},
    {"timestamp": 2, "update_type": "message_created", "message": {
      "recipient": {"chat_id": 182, "chat_type": "dialog", "user_id": 999},
      "body": {"mid": "mid.1", "seq": 1, "text": "привет"},
      "sender": {"user_id": 123, "name": "John", "is_bot": false}}},
    {"timestamp": 3, "update_type": "message_created", "message": {
      "recipient": {"chat_id": -700, "chat_type": "chat"},
      "body": {"mid": "mid.2", "seq": 2, "text": "в группе"},
      "sender": {"user_id": 123, "name": "John", "is_bot": false}}},
    {"timestamp": 4, "update_type": "message_callback",
      "callback": {"timestamp": 4, "callback_id": "cb-1", "payload": "cat:benefits", "user": {"user_id": 123, "name": "John"}},
      "message": {
        "recipient": {"chat_id": 182, "chat_type": "dialog", "user_id": 123},
        "body": {"mid": "mid.3", "seq": 3, "text": "Выбери раздел"},
        "sender": {"user_id": 999, "name": "Bot", "is_bot": true}}}
  ],
  "marker": 5
}`

// wantEvents are the bot events made from updatesJSON.
var wantEvents = []bot.Event{
	{Type: bot.EventStart, UserID: 123},
	{Type: bot.EventText, UserID: 123, Text: "привет"},
	{Type: bot.EventCallback, UserID: 123, CallbackID: "cb-1", Payload: "cat:benefits", SourceText: "Выбери раздел"},
}

type request struct {
	method string
	path   string
	query  map[string]string
	body   map[string]any
}

type fakeMAX struct {
	t        *testing.T
	mu       sync.Mutex
	requests []request
	polls    int
	limited  int      // respond 429 to this many message sends
	webhooks []string // urls of the bot's webhook subscriptions
}

func (f *fakeMAX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"verify.token","message":"Invalid access_token"}`)
		return
	}
	req := request{method: r.Method, path: r.URL.Path, query: map[string]string{}}
	for k := range r.URL.Query() {
		req.query[k] = r.URL.Query().Get(k)
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req.body)
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/me":
		_, _ = io.WriteString(w, `{"user_id": 999, "first_name": "Навигатор", "username": "navigator_bot", "is_bot": true}`)
	case r.URL.Path == "/updates":
		f.mu.Lock()
		f.polls++
		first := f.polls == 1
		f.mu.Unlock()
		if first {
			_, _ = io.WriteString(w, updatesJSON)
			return
		}
		if req.query["marker"] != "5" {
			f.t.Errorf("poll without the marker: %v", req.query)
		}
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Millisecond):
		}
		_, _ = io.WriteString(w, `{"updates": [], "marker": 5}`)
	case r.URL.Path == "/messages" && r.Method == http.MethodPost:
		f.mu.Lock()
		limited := f.limited > 0
		f.limited--
		f.mu.Unlock()
		if limited {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":"too.many.requests","message":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w, `{"message": {"body": {"mid": "mid.new", "seq": 10, "text": "ok"}}}`)
	case r.URL.Path == "/answers":
		_, _ = io.WriteString(w, `{"success": true}`)
	case r.URL.Path == "/subscriptions":
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			subs := make([]map[string]any, 0, len(f.webhooks))
			for _, u := range f.webhooks {
				subs = append(subs, map[string]any{"url": u, "time": 1})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"subscriptions": subs})
			return
		case http.MethodPost:
			u, _ := req.body["url"].(string)
			if !slices.Contains(f.webhooks, u) {
				f.webhooks = append(f.webhooks, u)
			}
		case http.MethodDelete:
			f.webhooks = slices.DeleteFunc(f.webhooks, func(u string) bool { return u == req.query["url"] })
		}
		_, _ = io.WriteString(w, `{"success": true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeMAX) sent(path string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.requests {
		if r.path == path && r.method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

func newTestClient(t *testing.T, fake *fakeMAX) *Client {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := New(Config{Token: token, BaseURL: srv.URL}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPollConvertsUpdates(t *testing.T) {
	fake := &fakeMAX{t: t}
	c := newTestClient(t, fake)
	if name, err := c.Me(context.Background()); err != nil || name != "Навигатор (@navigator_bot)" {
		t.Fatalf("Me() = %q, %v", name, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var events []bot.Event
	done := make(chan struct{})
	go func() {
		c.Poll(ctx, func(ev bot.Event) {
			events = append(events, ev)
		})
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for {
		fake.mu.Lock()
		polls := fake.polls
		fake.mu.Unlock()
		if polls >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("poller did not keep polling")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if !slices.Equal(events, wantEvents) {
		t.Fatalf("got events %+v, want %+v", events, wantEvents)
	}
}

func TestSendMessageWithKeyboard(t *testing.T) {
	fake := &fakeMAX{t: t}
	c := newTestClient(t, fake)
	err := c.Send(context.Background(), 123, bot.Message{
		Text:     "**Привет**",
		Markdown: true,
		Keyboard: [][]bot.Button{{{Text: "Льготы", Payload: "cat:benefits"}, {Text: "Сайт", URL: "https://max.ru"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	reqs := fake.sent("/messages")
	if len(reqs) != 1 {
		t.Fatalf("expected one message, got %d", len(reqs))
	}
	r := reqs[0]
	if r.query["user_id"] != "123" || r.query["disable_link_preview"] != "true" {
		t.Errorf("unexpected query: %v", r.query)
	}
	if _, set := r.body["notify"]; r.body["text"] != "**Привет**" || r.body["format"] != "markdown" || set {
		t.Errorf("unexpected body: %v", r.body)
	}
	kb := r.body["attachments"].([]any)[0].(map[string]any)
	if kb["type"] != "inline_keyboard" {
		t.Fatalf("expected an inline keyboard: %v", kb)
	}
	row := kb["payload"].(map[string]any)["buttons"].([]any)[0].([]any)
	cb, link := row[0].(map[string]any), row[1].(map[string]any)
	if cb["type"] != "callback" || cb["payload"] != "cat:benefits" || cb["text"] != "Льготы" {
		t.Errorf("bad callback button: %v", cb)
	}
	if link["type"] != "link" || link["url"] != "https://max.ru" {
		t.Errorf("bad link button: %v", link)
	}
}

func TestSendSilently(t *testing.T) {
	fake := &fakeMAX{t: t}
	c := newTestClient(t, fake)
	if err := c.Send(context.Background(), 123, bot.Message{Text: "ночной отчёт", Silent: true}); err != nil {
		t.Fatal(err)
	}
	if r := fake.sent("/messages")[0]; r.body["notify"] != false {
		t.Errorf("a silent message must not notify: %v", r.body)
	}
}

func TestAnswerCallbackRemovesKeyboard(t *testing.T) {
	fake := &fakeMAX{t: t}
	c := newTestClient(t, fake)
	err := c.AnswerCallback(context.Background(), 123, "cb-1", bot.CallbackAnswer{
		Edit:         &bot.Message{Text: "Выбери раздел\n\n👉 Льготы"},
		Notification: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := fake.sent("/answers")[0]
	if r.query["callback_id"] != "cb-1" || r.body["notification"] != "ok" {
		t.Errorf("unexpected answer: %v %v", r.query, r.body)
	}
	msg := r.body["message"].(map[string]any)
	// An empty array removes the keyboard; null or a missing field keeps it.
	if atts, ok := msg["attachments"].([]any); !ok || len(atts) != 0 {
		t.Errorf("attachments must be an empty array, got %#v", msg["attachments"])
	}
}

func TestRateLimitedRequestIsRetried(t *testing.T) {
	retryAfter429 = 10 * time.Millisecond
	t.Cleanup(func() { retryAfter429 = time.Second })

	fake := &fakeMAX{t: t, limited: 2}
	c := newTestClient(t, fake)
	if err := c.Send(context.Background(), 123, bot.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.sent("/messages")); n != 3 {
		t.Fatalf("expected 2 rejected attempts and 1 successful, got %d", n)
	}
	for _, r := range fake.sent("/messages") {
		if r.body["text"] != "hi" {
			t.Fatalf("the body must be resent on retry, got %v", r.body)
		}
	}
}

func TestDialogPacing(t *testing.T) {
	c := newTestClient(t, &fakeMAX{t: t})
	start := time.Now()
	for range 3 {
		if err := c.Send(context.Background(), 1, bot.Message{Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	// Three messages to one dialog need at least two pauses.
	if elapsed := time.Since(start); elapsed < 2*dialogPause-50*time.Millisecond {
		t.Fatalf("messages to one dialog were not paced: %v", elapsed)
	}
}

// TestCustomCA checks that MAX_CA_FILE lets the client trust a server whose
// certificate is not in the system bundle, like the MAX API with the Russian
// Trusted Root CA.
func TestCustomCA(t *testing.T) {
	srv := httptest.NewTLSServer(&fakeMAX{t: t})
	t.Cleanup(srv.Close)
	log := slog.New(slog.DiscardHandler)

	untrusted, err := New(Config{Token: token, BaseURL: srv.URL}, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Me(context.Background()); err == nil {
		t.Fatal("a self-signed server must not be trusted without the CA file")
	}

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	trusted, err := New(Config{Token: token, BaseURL: srv.URL, CAFile: caFile}, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trusted.Me(context.Background()); err != nil {
		t.Fatalf("the CA file must be trusted: %v", err)
	}

	derFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(derFile, srv.Certificate().Raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := certPool(derFile); err != nil {
		t.Fatalf("DER certificates must be accepted too: %v", err)
	}
}
