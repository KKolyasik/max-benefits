package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KKolyasik/max-benefits/internal/agent/fetch"
	"github.com/KKolyasik/max-benefits/internal/agent/llm"
	"github.com/KKolyasik/max-benefits/internal/agent/search"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

const testSurvey = `
categories:
  - {id: money, title: "Деньги", questions: [form, basis]}
  - {id: fun, title: "Досуг", questions: [age]}
questions:
  - id: form
    label: "Форма обучения"
    text: "Как учишься?"
    options: [{id: full_time, title: "Очно"}, {id: part_time, title: "Заочно"}]
  - id: basis
    label: "Основа"
    text: "Бюджет?"
    options: [{id: budget, title: "Бюджет"}, {id: contract, title: "Платно"}]
  - id: age
    label: "Возраст"
    text: "Сколько лет?"
    options: [{id: young, title: "До 23"}, {id: old, title: "23+"}]
`

const pageURL = "https://metro.spb.ru/ticket.html"

var transport = knowledge.Card{
	ID: "transport", Categories: []string{"money"}, Priority: 100,
	Match: knowledge.Condition{"form": {"full_time"}},
	Title: "🚇 Проездной", Summary: "Льготный проездной.",
	Links: []knowledge.CardLink{
		{Title: "Метро", URL: pageURL},
		{Title: "Для бюджетников", URL: "https://uni.ru/budget", When: knowledge.Condition{"basis": {"budget"}}},
	},
}

func testSurveyParsed(t *testing.T) *survey.Survey {
	t.Helper()
	s, err := survey.Parse([]byte(testSurvey))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeSearch struct {
	mu      sync.Mutex
	results []search.Result
	err     error
	calls   int
}

func (f *fakeSearch) Search(context.Context, string, int) ([]search.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.results, f.err
}

// pagesFetch reads the pages from the map; a page not in it can't be read.
type pagesFetch map[string]string

func (f pagesFetch) Fetch(_ context.Context, url, _ string) (fetch.Page, error) {
	text, ok := f[url]
	if !ok {
		return fetch.Page{}, errors.New("timeout")
	}
	return fetch.Page{Text: text}, nil
}

type fakeFetch struct{ text string }

func (f fakeFetch) Fetch(context.Context, string, string) (fetch.Page, error) {
	if f.text == "" {
		return fetch.Page{}, errors.New("unreachable")
	}
	return fetch.Page{Text: f.text}, nil
}

// fakeModel answers with prepared cards in order and records the prompts.
type fakeModel struct {
	mu      sync.Mutex
	answers [][]map[string]any
	err     error
	prompts [][]llm.Message
}

func (m *fakeModel) Complete(_ context.Context, msgs []llm.Message, _ string, _ any) (llm.Answer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prompts = append(m.prompts, msgs)
	if m.err != nil {
		return llm.Answer{}, m.err
	}
	cards := m.answers[0]
	m.answers = m.answers[1:]
	data, _ := json.Marshal(map[string]any{"cards": cards})
	return llm.Answer{Content: string(data), InputTokens: 1_000_000, OutputTokens: 10}, nil
}

// card is a model answer for a valid new card.
func card(overrides map[string]any) map[string]any {
	c := map[string]any{
		"id": "spb_scholarship", "updates": "", "categories": []string{"money"}, "priority": 70,
		"match": []string{"basis=budget"},
		"title": "🎓 Стипендия", "summary": "Выплата бюджетникам.",
		"steps": []string{"Подай заявление."}, "documents": []string{"Паспорт"}, "where": "Деканат",
		"links": []map[string]string{{"title": "Метро", "url": pageURL}},
	}
	for k, v := range overrides {
		c[k] = v
	}
	return c
}

func newCollector(t *testing.T, model *fakeModel, queries ...string) *Collector {
	t.Helper()
	dir := t.TempDir()
	state, err := LoadState(filepath.Join(dir, "drafts", ".seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) == 0 {
		queries = []string{"стипендия"}
	}
	var qs []Query
	for _, q := range queries {
		qs = append(qs, Query{Text: q, Category: "money", Results: 5})
	}
	return &Collector{
		Survey:  testSurveyParsed(t),
		Base:    []knowledge.Card{transport},
		Queries: qs,
		Search:  &fakeSearch{results: []search.Result{{URL: pageURL, Title: "Метро", Snippets: []string{"сниппет"}}}},
		Fetch:   fakeFetch{text: "Стипендия 5000 рублей."},
		Model:   model,
		Drafts:  Drafts{Dir: filepath.Join(dir, "drafts")},
		State:   state,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) },
	}
}

func drafts(t *testing.T, c *Collector) []Draft {
	t.Helper()
	list, err := c.Drafts.(Drafts).List()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestNewCardBecomesDraft(t *testing.T) {
	c := newCollector(t, &fakeModel{answers: [][]map[string]any{{card(nil)}}})

	r, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 1 || r.InputTokens != 1_000_000 {
		t.Fatalf("report %+v", r)
	}
	d := drafts(t, c)[0]
	if d.Card.ID != "spb_scholarship" || d.Query != "стипендия" || d.Sources[0] != pageURL || len(d.Notes) > 0 {
		t.Errorf("draft %+v", d)
	}
	if got := d.Card.Match["basis"]; len(got) != 1 || got[0] != "budget" {
		t.Errorf("match %v", d.Card.Match)
	}
}

func TestUnchangedPagesSkipTheModel(t *testing.T) {
	model := &fakeModel{answers: [][]map[string]any{{card(nil)}}}
	c := newCollector(t, model)
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	r, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Unchanged != 1 || len(model.prompts) != 1 {
		t.Errorf("second run must not call the model: %+v, calls %d", r, len(model.prompts))
	}

	c.Force = true
	model.answers = [][]map[string]any{{}}
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(model.prompts) != 2 {
		t.Error("force must call the model")
	}
}

func TestPendingDraftIsNotOverwritten(t *testing.T) {
	model := &fakeModel{answers: [][]map[string]any{{card(nil)}, {card(map[string]any{"title": "Другое"})}}}
	c := newCollector(t, model)
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Force = true
	r, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 0 || drafts(t, c)[0].Card.Title != "🎓 Стипендия" {
		t.Error("a draft waiting for review must stay as it is")
	}
}

// A proposal admins rejected is not sent again, while another one is.
func TestRejectedDraftIsNotSent(t *testing.T) {
	// The second run asks twice: the first answer repeats the rejected card.
	c := newCollector(t, &fakeModel{answers: [][]map[string]any{{card(nil)}, {card(nil)}, {card(map[string]any{"summary": "Другое."})}}})
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := drafts(t, c)[0]
	if err := c.Drafts.(Drafts).Delete(first); err != nil {
		t.Fatal(err)
	}
	c.Rejected = map[string]bool{DraftID(first): true}
	c.Force = true
	c.Queries = append(c.Queries, c.Queries[0])
	c.Parallel = 1
	r, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 1 || drafts(t, c)[0].Card.Summary != "Другое." {
		t.Errorf("only the new proposal must be sent: %+v", r)
	}
}

// The ID stands for the proposal: when and by which query it was found
// doesn't matter, what it proposes does.
func TestDraftID(t *testing.T) {
	d := Draft{Query: "проездной", Updates: "transport", Card: transport}
	again := d
	again.Query, again.FoundAt, again.Sources = "другой запрос", time.Now(), []string{"https://other.example"}
	if DraftID(d) != DraftID(again) {
		t.Error("the same proposal must keep its ID")
	}
	changed := d
	changed.Card.Summary = "другой текст"
	asNew := d
	asNew.Updates = ""
	if DraftID(changed) == DraftID(d) || DraftID(asNew) == DraftID(d) {
		t.Error("another proposal must get another ID")
	}
}

func TestModelFixesErrorsOnSecondTry(t *testing.T) {
	bad := card(map[string]any{"match": []string{"age=young"}})
	model := &fakeModel{answers: [][]map[string]any{{bad}, {card(nil)}}}
	c := newCollector(t, model)

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(model.prompts) != 2 {
		t.Fatalf("expected a repair request, got %d calls", len(model.prompts))
	}
	repair := model.prompts[1]
	if last := repair[len(repair)-1].Content; !strings.Contains(last, `question "age" is not asked in category "money"`) {
		t.Errorf("repair prompt: %s", last)
	}
	if d := drafts(t, c)[0]; len(d.Notes) > 0 {
		t.Errorf("notes %v", d.Notes)
	}
}

func TestUnfixedErrorsStayInDraft(t *testing.T) {
	bad := card(map[string]any{"match": []string{"age=young"}})
	c := newCollector(t, &fakeModel{answers: [][]map[string]any{{bad}, {bad}}})

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := drafts(t, c)[0]; len(d.Notes) != 1 || !strings.Contains(d.Notes[0], `"age"`) {
		t.Errorf("notes %v", d.Notes)
	}
}

// A card under the ID of a card in the base is that card, even if the model
// forgot updates: asked to fix it, a model may make a duplicate instead.
func TestTakenIDIsAnUpdate(t *testing.T) {
	model := &fakeModel{answers: [][]map[string]any{{card(map[string]any{"id": "transport"})}}}
	c := newCollector(t, model)
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := drafts(t, c)[0]
	if len(model.prompts) != 1 || d.Updates != "transport" || d.Card.ID != "transport" || len(d.Notes) > 0 {
		t.Errorf("draft %+v after %d calls", d, len(model.prompts))
	}
}

// A link from elsewhere than the sources is dropped without a second pass,
// which would double the price of the query.
func TestUnknownLinksAreDropped(t *testing.T) {
	model := &fakeModel{answers: [][]map[string]any{{card(map[string]any{"links": []map[string]string{
		{"title": "Метро", "url": pageURL}, {"title": "Прошлогодний конкурс", "url": "https://knvsh.example/contests/458"},
	}})}}}
	c := newCollector(t, model)

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := drafts(t, c)[0]
	if len(model.prompts) != 1 || len(d.Notes) > 0 || len(d.Card.Links) != 1 || d.Card.Links[0].URL != pageURL {
		t.Errorf("links %+v, notes %v after %d calls", d.Card.Links, d.Notes, len(model.prompts))
	}
}

// Only news sends a query to the model: a changed page or a new one. A site
// that is down this time and search results in another order are no news.
func TestOnlyNewsGoesToTheModel(t *testing.T) {
	model := &fakeModel{answers: make([][]map[string]any, 10)}
	c := newCollector(t, model)
	const a, b, newer, down = "https://a.example/1", "https://b.example/2", "https://c.example/3", "https://d.example/4"
	result := func(url string) search.Result {
		return search.Result{URL: url, Snippets: []string{"фрагмент " + url}}
	}
	s := &fakeSearch{results: []search.Result{result(a), result(b)}}
	pages := pagesFetch{a: "Стипендия 5000 рублей.", b: "Проездной 500 рублей."}
	c.Search, c.Fetch = s, pages
	step := func(what string, toModel bool) {
		t.Helper()
		calls := len(model.prompts)
		if _, err := c.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := len(model.prompts) > calls; got != toModel {
			t.Errorf("%s: to the model %v, want %v", what, got, toModel)
		}
	}

	step("the first run", true)
	step("the same pages", false)
	s.results = []search.Result{result(b), result(a)}
	step("another order", false)
	delete(pages, a)
	step("a site that is down", false)
	pages[a] = "Стипендия 5000 рублей."
	step("the site is back with the same page", false)
	pages[b] = "Проездной 600 рублей."
	step("a changed page", true)
	s.results = append(s.results, result(newer))
	pages[newer] = "Новая стипендия."
	step("a new page", true)
	s.results = append(s.results, result(down))
	step("a new page that doesn't open", true)
	step("the same page that doesn't open", false)
	pages[down] = "Наконец открылась."
	step("the page opened for the first time", true)
}

// A counter of views after the date of a news page is no news; what the
// page says, the date and the time included, is.
func TestPageDigest(t *testing.T) {
	const page = "Стипендии выросли\n\n15 сентября 2026, 08:00 26 745\n\nРазмер: 5 000 рублей."
	for _, same := range []string{
		"Стипендии выросли\n\n15 сентября 2026, 08:00 26 747\n\nРазмер: 5 000 рублей.",
		"Стипендии выросли\n\n15 сентября 2026, 08:00 27 001\n\nРазмер: 5 000 рублей.",
	} {
		if pageDigest(same) != pageDigest(page) {
			t.Errorf("only the counter changed:\n%s", same)
		}
	}
	for _, other := range []string{
		"Стипендии выросли\n\n15 сентября 2026, 08:00 26 745\n\nРазмер: 7 000 рублей.",
		"Стипендии выросли\n\n15 сентября 2026, 09:30 26 745\n\nРазмер: 5 000 рублей.",
		"Стипендии выросли\n\n16 сентября 2026, 08:00 26 745\n\nРазмер: 5 000 рублей.",
	} {
		if pageDigest(other) == pageDigest(page) {
			t.Errorf("the page changed:\n%s", other)
		}
	}
	// A number on a line of its own after the date is not a counter to drop.
	if pageDigest("28.09.2026 08:00 12\n7 000") == pageDigest("28.09.2026 08:00 15\n5 000") {
		t.Error("the sum on the next line changed")
	}
}

// The state file of the former format still reads: a query whose pages are
// the same as then does without the model, and moves to the new format on
// its run.
func TestFormerStateFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen.json")
	same := []Source{{URL: pageURL, Text: "Стипендия 5000 рублей."}}
	data, err := json.Marshal(map[string]string{"стипендия": legacyDigest(same), "проездной": "0a1b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Changed("стипендия", same)) != 0 || len(s.Changed("проездной", same)) == 0 {
		t.Fatal("the former digests must count")
	}
	if err := s.Remember("стипендия", same); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if string(saved["проездной"]) != `"0a1b"` || !strings.HasPrefix(string(saved["стипендия"]), "{") {
		t.Errorf("saved:\n%s", data)
	}
	again, err := LoadState(path)
	if err != nil || len(again.Changed("стипендия", same)) != 0 || len(again.Changed("проездной", same)) == 0 {
		t.Errorf("after a reload: %v", err)
	}
}

// Queries the token limit leaves out wait for the next run: they are not
// marked as seen, and their search isn't paid for.
func TestTokenLimitDefersQueries(t *testing.T) {
	c := newCollector(t, &fakeModel{answers: [][]map[string]any{{card(nil)}}}, "первый", "второй")
	s := &fakeSearch{results: []search.Result{{URL: pageURL}}}
	c.Search = s
	c.TokenLimit = 500_000 // the fake model spends a million at once

	r, err := c.Run(context.Background())
	if err != nil || r.Deferred != 1 || len(r.Drafts) != 1 || s.calls != 1 {
		t.Fatalf("report %+v, err %v, searches %d", r, err, s.calls)
	}
	pages := []Source{{URL: pageURL, Text: "Стипендия 5000 рублей."}}
	if len(c.State.Changed("первый", pages)) != 0 || len(c.State.Changed("второй", pages)) == 0 {
		t.Error("only the finished query is seen")
	}
}

// The first answer is paid for, so without tokens for a fix its drafts go
// to people with the problems listed.
func TestNoTokensToFixKeepsTheProblems(t *testing.T) {
	model := &fakeModel{answers: [][]map[string]any{{card(map[string]any{"match": []string{"age=young"}})}}}
	c := newCollector(t, model)
	c.TokenLimit = 500_000

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := drafts(t, c)[0]; len(model.prompts) != 1 || len(d.Notes) == 0 {
		t.Errorf("notes %v after %d calls", d.Notes, len(model.prompts))
	}
}

// Queries running at once share the report, the state and the drafts.
func TestParallelQueries(t *testing.T) {
	var answers [][]map[string]any
	var queries []string
	for i := range 6 {
		answers = append(answers, []map[string]any{card(map[string]any{"id": fmt.Sprintf("card_%d", i)})})
		queries = append(queries, fmt.Sprintf("запрос %d", i))
	}
	c := newCollector(t, &fakeModel{answers: answers}, queries...)
	c.Parallel = 3

	r, err := c.Run(context.Background())
	if err != nil || r.Queries != 6 || len(r.Drafts) != 6 || len(drafts(t, c)) != 6 || r.InputTokens != 6_000_000 {
		t.Fatalf("report %+v, err %v", r, err)
	}
	pages := []Source{{URL: pageURL, Text: "Стипендия 5000 рублей."}}
	for _, q := range queries {
		if len(c.State.Changed(q, pages)) != 0 {
			t.Errorf("query %q is not seen", q)
		}
	}
}

func TestUpdateWithoutChangesIsDropped(t *testing.T) {
	same := card(map[string]any{
		"id": "whatever", "updates": "transport", "priority": 100,
		"match": []string{"form=full_time"},
		"title": transport.Title, "summary": transport.Summary, "steps": []string{}, "documents": []string{}, "where": "",
		"links": []map[string]string{{"title": "Метро", "url": pageURL}, {"title": "Для бюджетников", "url": "https://uni.ru/budget"}},
	})
	c := newCollector(t, &fakeModel{answers: [][]map[string]any{{same}}})

	r, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Drafts) != 0 {
		t.Errorf("nothing changed, nothing to review: %v", r.Drafts)
	}
}

func TestUpdateKeepsLinkConditions(t *testing.T) {
	changed := card(map[string]any{
		"updates": "transport",
		"links":   []map[string]string{{"title": "Инструкция", "url": "https://uni.ru/budget/"}},
	})
	c := newCollector(t, &fakeModel{answers: [][]map[string]any{{changed}}})
	c.Search = &fakeSearch{results: []search.Result{{URL: pageURL}, {URL: "https://uni.ru/budget"}}}

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := drafts(t, c)[0]
	if d.Updates != "transport" || d.Card.ID != "transport" || len(d.Notes) > 0 {
		t.Fatalf("draft %+v", d)
	}
	if w := d.Card.Links[0].When["basis"]; len(w) != 1 || w[0] != "budget" {
		t.Errorf("when %v", d.Card.Links[0].When)
	}
}

// An update may keep the links of the card although the search didn't find
// them this time. Who sees the card and its links stays as people set it,
// whatever the model says.
func TestUpdateKeepsOldLinksAndConditions(t *testing.T) {
	news := "https://news.example/pass"
	changed := card(map[string]any{"updates": "transport", "links": []map[string]string{
		{"title": "Новость", "url": news}, {"title": "Метро", "url": pageURL},
	}})
	c := newCollector(t, &fakeModel{answers: [][]map[string]any{{changed}}})
	// Links must be https, so the model may switch a found http page to it.
	c.Search = &fakeSearch{results: []search.Result{{URL: "http://news.example/pass/"}}}

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := drafts(t, c)[0]
	var urls []string
	for _, l := range d.Card.Links {
		urls = append(urls, l.URL)
	}
	if got := strings.Join(urls, " "); got != news+" "+pageURL+" https://uni.ru/budget" || len(d.Notes) > 0 {
		t.Fatalf("links %s, notes %v", got, d.Notes)
	}
	if w := d.Card.Links[2].When["basis"]; len(w) != 1 || w[0] != "budget" {
		t.Errorf("when %v", d.Card.Links[2].When)
	}
	if !reflect.DeepEqual(d.Card.Match, transport.Match) {
		t.Errorf("match %v, want %v", d.Card.Match, transport.Match)
	}
}

func TestPageFallsBackToSnippets(t *testing.T) {
	model := &fakeModel{answers: [][]map[string]any{{}}}
	c := newCollector(t, model)
	c.Fetch = fakeFetch{}

	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if user := model.prompts[0][1].Content; !strings.Contains(user, "сниппет") {
		t.Errorf("snippets must replace an unreadable page:\n%s", user)
	}
}

func TestOneFailedQueryDoesNotStopTheRun(t *testing.T) {
	c := newCollector(t, &fakeModel{}, "первый", "второй")
	s := &fakeSearch{err: errors.New("temporary")}
	c.Search = s

	r, err := c.Run(context.Background())
	if err != nil || r.Failed != 2 || s.calls != 2 {
		t.Errorf("report %+v, err %v, calls %d", r, err, s.calls)
	}
}

func TestFatalErrorsStopTheRun(t *testing.T) {
	cases := map[string]struct {
		search error
		model  error
	}{
		"search unavailable": {search: search.ErrUnavailable},
		"model unreachable":  {model: llm.ErrUnreachable},
		"key rejected":       {model: llm.ErrUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCollector(t, &fakeModel{err: tc.model}, "первый", "второй")
			s := &fakeSearch{results: []search.Result{{URL: pageURL}}, err: tc.search}
			c.Search = s
			if _, err := c.Run(context.Background()); !fatal(err) || s.calls != 1 {
				t.Errorf("err %v, calls %d", err, s.calls)
			}
		})
	}
}

func TestCutPromptStopsTheRun(t *testing.T) {
	model := &fakeModel{answers: [][]map[string]any{{card(nil)}}}
	c := newCollector(t, model)
	c.Model = cutModel{model}

	if _, err := c.Run(context.Background()); !errors.Is(err, ErrContextTooSmall) {
		t.Errorf("got %v", err)
	}
}

// cutModel reports that it read only a few tokens, as Ollama does when the
// prompt exceeds its context window.
type cutModel struct{ *fakeModel }

func (m cutModel) Complete(ctx context.Context, msgs []llm.Message, name string, schema any) (llm.Answer, error) {
	a, err := m.fakeModel.Complete(ctx, msgs, name, schema)
	a.InputTokens = 100
	return a, err
}

func TestPrompt(t *testing.T) {
	s := testSurveyParsed(t)
	museum := knowledge.Card{ID: "museum", Categories: []string{"fun"}, Priority: 50, Title: "🖼 Музей", Summary: "Бесплатно."}
	msgs := buildMessages(Query{Text: "проездной", Category: "money"},
		[]Source{{URL: pageURL, Title: "Метро", Text: "Текст", Published: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), PDF: true}},
		s, []knowledge.Card{transport, museum}, time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))

	system, user := msgs[0].Content, msgs[1].Content
	for _, want := range []string{"Сегодня 2026-09-26", "- form «Форма обучения»: full_time (Очно), part_time (Заочно)",
		"- money «Деньги»: form, basis", "- transport [money] 🚇 Проездной", "- museum [fun] 🖼 Музей",
		"id: transport", "when: {basis: [budget]}"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt has no %q", want)
		}
	}
	if strings.Contains(system, "id: museum") {
		t.Error("cards of other sections are listed only by title")
	}
	if !strings.Contains(user, "[1] Метро (дата: 2026-08-01, PDF)\n"+pageURL+"\n\nТекст") {
		t.Errorf("user prompt:\n%s", user)
	}

	props := schema(s)["properties"].(map[string]any)["cards"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	rules := props["match"].(map[string]any)["items"].(map[string]any)["enum"].([]string)
	if got := strings.Join(rules, ","); got != "form=full_time,form=part_time,basis=budget,basis=contract,age=young,age=old" {
		t.Errorf("match enum %s", got)
	}
}

// Yandex AI Studio refuses a schema with objects and arrays nested more than
// five levels deep.
func TestSchemaIsShallowEnoughForYandex(t *testing.T) {
	var depth func(node map[string]any) int
	depth = func(node map[string]any) int {
		d := 0
		if props, ok := node["properties"].(map[string]any); ok {
			for _, p := range props {
				d = max(d, depth(p.(map[string]any)))
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			d = max(d, depth(items))
		}
		if typ := node["type"]; typ == "object" || typ == "array" {
			d++
		}
		return d
	}
	if d := depth(schema(testSurveyParsed(t))); d > 5 {
		t.Errorf("the schema is %d levels deep", d)
	}
}

func TestParseAnswer(t *testing.T) {
	got, err := parseAnswer("```json\n" + `{"cards":[{"id":" x ","priority":500,"steps":["шаг"," "],
		"match":["form=full_time","form=full_time"," form = part_time ","","basis:budget"],
		"title":"Санкт\u2011Петербург\u00a0сейчас","links":[{"title":"Сайт","url":" http://a.example/x "}]}]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	c := got[0].Card
	if c.ID != "x" || c.Priority != 100 || len(c.Steps) != 1 || strings.Join(c.Match["form"], ",") != "full_time,part_time" {
		t.Errorf("card %+v", c)
	}
	// Validation sends a malformed rule back to the model.
	if _, kept := c.Match["basis:budget"]; !kept || len(c.Match) != 2 {
		t.Errorf("match %v", c.Match)
	}
	if c.Links[0].URL != "https://a.example/x" {
		t.Errorf("link %q", c.Links[0].URL)
	}
	if c.Title != "Санкт-Петербург сейчас" {
		t.Errorf("typographic spaces and hyphens must be plain: %q", c.Title)
	}
	if _, err := parseAnswer("не знаю"); err == nil {
		t.Error("expected an error")
	}
}
