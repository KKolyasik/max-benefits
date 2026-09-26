package contract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hamba/avro/v2"
	"github.com/twmb/franz-go/pkg/sr"
)

// registry fakes the few Schema Registry calls the codec makes.
type registry struct {
	mu       sync.Mutex
	schemas  []string       // by ID - 1
	subjects map[string]int // subject → the ID of its latest schema
	compat   map[string]string
}

func newRegistry(t *testing.T) (*registry, *sr.Client) {
	t.Helper()
	r := &registry{subjects: map[string]int{}, compat: map[string]string{}}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	cl, err := sr.NewClient(sr.URLs(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return r, cl
}

func (r *registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	switch {
	case req.Method == http.MethodPut && parts[0] == "config":
		var body struct {
			Compatibility string `json:"compatibility"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.compat[parts[1]] = body.Compatibility
		_ = json.NewEncoder(w).Encode(body)
	case req.Method == http.MethodPost && parts[0] == "subjects":
		var body struct {
			Schema string `json:"schema"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]int{"id": r.add(parts[1], body.Schema)})
	case req.Method == http.MethodGet && parts[0] == "schemas":
		id, _ := strconv.Atoi(parts[2])
		if id < 1 || id > len(r.schemas) {
			http.Error(w, `{"error_code":40403,"message":"Schema not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"schema": r.schemas[id-1]})
	default:
		http.NotFound(w, req)
	}
}

func (r *registry) add(subject, schema string) int {
	for i, s := range r.schemas {
		if s == schema {
			r.subjects[subject] = i + 1
			return i + 1
		}
	}
	r.schemas = append(r.schemas, schema)
	r.subjects[subject] = len(r.schemas)
	return len(r.schemas)
}

func registered(t *testing.T) (*registry, *Codec) {
	t.Helper()
	r, cl := newRegistry(t)
	c := NewCodec(cl)
	if err := c.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r, c
}

var at = time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)

var card = Card{
	ID: "pgas", Categories: []string{"scholarships"}, Priority: 80,
	Match: map[string][]string{"study_basis": {"budget"}, "course": {"2", "3plus"}},
	Title: "🏆 ПГАС", Summary: "Повышенная стипендия.", Steps: []string{"Собери подтверждения."},
	Documents: []string{"Заявление"}, Where: "Деканат",
	Links: []CardLink{{Title: "Положение", URL: "https://spbu.ru/pgas", When: map[string][]string{"university": {"spbu"}}}},
}

// Every message survives a trip through the codec, so the Go types and the
// schemas agree field by field.
func TestRoundTrip(t *testing.T) {
	_, c := registered(t)
	messages := []any{
		&card,
		&Survey{
			Categories: []Category{{ID: "money", Title: "Деньги", Intro: "Пара вопросов", Questions: []string{"form"}}},
			Questions: []Question{{ID: "form", Label: "Форма", Text: "Как учишься?", Multi: true,
				Options: []Option{{ID: "full_time", Title: "Очно"}, {ID: "none", Title: "Ничего", Exclusive: true}}}},
		},
		&Draft{ID: "d1", Card: card, Updates: "pgas", Query: "ПГАС", Sources: []string{"https://spbu.ru"},
			Notes: []string{"проверь сумму"}, FoundAt: at, RunID: "r1"},
		&Decision{DraftID: "d1", CardID: "pgas", Verdict: VerdictRejected, DecidedAt: at},
		&RunCommand{ID: "c1", RequestedAt: at, Force: true},
		&RunReport{RunID: "r1", Trigger: RunTriggerCommand, CommandID: "c1", Status: RunStatusDone,
			StartedAt: at, FinishedAt: at.Add(time.Minute), Queries: 13, Unchanged: 4, Failed: 1, Deferred: 2,
			Drafts: 5, InputTokens: 150_000, OutputTokens: 7_000, Error: "что-то"},
	}
	for _, m := range messages {
		data, err := c.Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		got := reflect.New(reflect.TypeOf(m).Elem())
		if err := c.Decode(context.Background(), data, got.Interface()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Interface(), m) {
			t.Errorf("%T changed:\n got %+v\nwant %+v", m, got.Elem(), reflect.ValueOf(m).Elem())
		}
	}
}

// The registry gets self-contained schemas with their defaults, each under
// its record's name and FULL_TRANSITIVE.
func TestRegisteredSchemas(t *testing.T) {
	r, _ := registered(t)
	want := []string{"maxbenefits.Card", "maxbenefits.Decision", "maxbenefits.Draft", "maxbenefits.RunCommand",
		"maxbenefits.RunReport", "maxbenefits.Survey"}
	var subjects []string
	for s, id := range r.subjects {
		subjects = append(subjects, s)
		if _, err := avro.ParseWithCache(r.schemas[id-1], "", &avro.SchemaCache{}); err != nil {
			t.Errorf("%s does not stand alone: %v", s, err)
		}
		if r.compat[s] != "FULL_TRANSITIVE" {
			t.Errorf("%s is %q", s, r.compat[s])
		}
	}
	slices.Sort(subjects)
	if strings.Join(subjects, " ") != strings.Join(want, " ") {
		t.Errorf("subjects %v", subjects)
	}
	draft := r.schemas[r.subjects["maxbenefits.Draft"]-1]
	for _, part := range []string{`"name":"CardLink"`, `"default":""`, `"doc":"The agent run that made it."`} {
		if !strings.Contains(draft, part) {
			t.Errorf("the Draft schema has no %s:\n%s", part, draft)
		}
	}
}

// written encodes v with another version of a schema, as another service
// would, registering it in the fake registry.
func written(t *testing.T, r *registry, subject, schemaJSON string, v any) []byte {
	t.Helper()
	for _, s := range schemas {
		if s.json == schemaJSON {
			t.Fatal("the schema is not another version")
		}
	}
	s, err := avro.ParseWithCache(schemaJSON, "", &avro.SchemaCache{})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	id := r.add(subject+"-other", schemaJSON)
	r.mu.Unlock()
	data, err := avro.Marshal(s, v)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := (&sr.ConfluentHeader{}).AppendEncode(nil, id, nil)
	return append(b, data...)
}

// A draft from an older agent without the newer fields reads with their
// defaults.
func TestOlderVersionReadsWithDefaults(t *testing.T) {
	r, c := registered(t)
	old := strings.Replace(schemas[reflect.TypeFor[Draft]()].json,
		`{"default":"","doc":"The agent run that made it.","name":"run_id","type":"string"}`, "", 1)
	old = strings.Replace(old, `,]`, `]`, 1)
	type oldDraft struct {
		ID      string    `avro:"id"`
		Card    Card      `avro:"card"`
		Updates string    `avro:"updates"`
		Query   string    `avro:"query"`
		Sources []string  `avro:"sources"`
		Notes   []string  `avro:"notes"`
		FoundAt time.Time `avro:"found_at"`
	}
	data := written(t, r, "maxbenefits.Draft", old, oldDraft{ID: "d1", Card: card, Query: "ПГАС", FoundAt: at})

	var got Draft
	if err := c.Decode(context.Background(), data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "d1" || got.RunID != "" || got.Card.Title != card.Title {
		t.Errorf("draft %+v", got)
	}
}

// A report from a newer agent with an extra field and a new status reads
// too: the field is skipped, the status is UNKNOWN.
func TestNewerVersionReads(t *testing.T) {
	r, c := registered(t)
	newer := strings.Replace(schemas[reflect.TypeFor[RunReport]()].json, `"BUSY"]`, `"BUSY","CANCELLED"]`, 1)
	newer = strings.Replace(newer, `{"default":"","doc":"Why the run failed.","name":"error","type":"string"}`,
		`{"default":"","doc":"Why the run failed.","name":"error","type":"string"},{"default":0,"name":"cost","type":"double"}`, 1)
	type newerReport struct {
		RunID      string    `avro:"run_id"`
		Trigger    string    `avro:"trigger"`
		CommandID  string    `avro:"command_id"`
		Status     string    `avro:"status"`
		StartedAt  time.Time `avro:"started_at"`
		FinishedAt time.Time `avro:"finished_at"`
		Drafts     int       `avro:"drafts"`
		Cost       float64   `avro:"cost"`
	}
	data := written(t, r, "maxbenefits.RunReport", newer,
		newerReport{RunID: "r1", Trigger: "SCHEDULE", Status: "CANCELLED", StartedAt: at, FinishedAt: at, Drafts: 3, Cost: 42})

	var got RunReport
	if err := c.Decode(context.Background(), data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != "r1" || got.Status != RunStatusUnknown || got.Trigger != RunTriggerSchedule || got.Drafts != 3 {
		t.Errorf("report %+v", got)
	}
}

func TestCodecMisuse(t *testing.T) {
	_, cl := newRegistry(t)
	c := NewCodec(cl)
	if _, err := c.Encode(&card); err == nil || !strings.Contains(err.Error(), "call Register") {
		t.Errorf("encode before Register: %v", err)
	}
	if _, err := c.Encode(42); err == nil {
		t.Error("42 is not a message")
	}
	if err := c.Decode(context.Background(), []byte{0, 0, 0, 0, 1}, card); err == nil {
		t.Error("decode into a value must fail")
	}
	if err := c.Decode(context.Background(), []byte("not avro"), &card); err == nil {
		t.Error("data without the header must fail")
	}
}

func TestTopics(t *testing.T) {
	seen := map[string]bool{}
	for _, topic := range Topics {
		if seen[topic.Name] {
			t.Errorf("topic %s twice", topic.Name)
		}
		seen[topic.Name] = true
		compact := topic.Configs["cleanup.policy"] == "compact"
		wantCompact := topic.Name == TopicCards || topic.Name == TopicSurvey || topic.Name == TopicDecisions
		if compact != wantCompact {
			t.Errorf("%s: compacted %v", topic.Name, compact)
		}
	}
}
