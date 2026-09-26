package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func TestYandex(t *testing.T) {
	raw, err := os.ReadFile("testdata/yandex.xml")
	if err != nil {
		t.Fatal(err)
	}
	var auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]string{"rawData": base64.StdEncoding.EncodeToString(raw)})
	}))
	defer srv.Close()

	y := &Yandex{APIKey: "secret", FolderID: "folder1", Region: 2, URL: srv.URL, HTTP: srv.Client()}
	results, err := y.Search(context.Background(), "проездной", 5)
	if err != nil {
		t.Fatal(err)
	}

	want := []Result{
		{URL: "https://metro.spb.ru/ticket.html", Title: "Льготный проездной для студентов",
			Snippets: []string{"Стоимость проездного 1599 рублей.", "Оформить можно в кассе."}},
		{URL: "https://ekp.spb.ru/", Title: "ЕКП"},
	}
	if !reflect.DeepEqual(results, want) {
		t.Errorf("got %+v", results)
	}
	query := body["query"].(map[string]any)
	if auth != "Api-Key secret" || query["queryText"] != "проездной" || body["folderId"] != "folder1" || body["region"] != "2" {
		t.Errorf("request: %q %v", auth, body)
	}
}

func TestYandexErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	y := &Yandex{URL: srv.URL, HTTP: srv.Client()}
	if _, err := y.Search(context.Background(), "x", 5); !errors.Is(err, ErrUnavailable) {
		t.Errorf("rejected key must stop the run: %v", err)
	}

	noResults := `<yandexsearch><response><error code="15">Sorry</error></response></yandexsearch>`
	if results, err := parseYandexXML([]byte(noResults)); err != nil || len(results) != 0 {
		t.Errorf("no results is not an error: %v %v", results, err)
	}
	limit := `<yandexsearch><response><error code="55">Limit</error></response></yandexsearch>`
	if _, err := parseYandexXML([]byte(limit)); err == nil {
		t.Error("expected an error")
	}
}

func TestSearXNG(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("q") + "|" + r.URL.Query().Get("format")
		_, _ = w.Write([]byte(`{"results":[
			{"url":"https://students.spbu.ru/a","title":"A","content":"про стипендию"},
			{"url":"https://students.spbu.ru/b","title":"B","content":"ещё"},
			{"url":"https://www.itmo.ru/c","title":"C","content":""},
			{"url":"https://itmo.ru/d","title":"D","content":"тот же сайт"},
			{"url":"https://spbgik.ru/e","title":"E","content":"третий сайт"}]}`))
	}))
	defer srv.Close()

	s := &SearXNG{URL: srv.URL, HTTP: srv.Client()}
	results, err := s.Search(context.Background(), "стипендия", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{
		{URL: "https://students.spbu.ru/a", Title: "A", Snippets: []string{"про стипендию"}},
		{URL: "https://www.itmo.ru/c", Title: "C"},
	}
	if !reflect.DeepEqual(results, want) {
		t.Errorf("got %+v", results)
	}
	if query != "стипендия|json" {
		t.Errorf("query %q", query)
	}
}

func TestSearXNGUnavailable(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"engines failed": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"results":[],"unresponsive_engines":[["google","CAPTCHA"],["bing","timeout"]]}`))
		},
		"json disabled": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			s := &SearXNG{URL: srv.URL, HTTP: srv.Client()}
			if _, err := s.Search(context.Background(), "x", 5); !errors.Is(err, ErrUnavailable) {
				t.Errorf("got %v", err)
			}
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"unresponsive_engines":[]}`))
	}))
	defer srv.Close()
	s := &SearXNG{URL: srv.URL, HTTP: srv.Client()}
	if results, err := s.Search(context.Background(), "x", 5); err != nil || len(results) != 0 {
		t.Errorf("nothing found is not an error: %v %v", results, err)
	}
}
