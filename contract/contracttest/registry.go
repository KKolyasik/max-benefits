// Package contracttest helps test code that uses the contract without a real
// Schema Registry.
package contracttest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/twmb/franz-go/pkg/sr"
)

// Registry fakes the few Schema Registry calls the contract's codec makes:
// setting the compatibility of a subject, registering a schema and getting a
// schema by ID.
type Registry struct {
	url    string
	client *sr.Client

	mu       sync.Mutex
	schemas  []string       // by ID - 1
	subjects map[string]int // subject → the ID of its latest schema
	compat   map[string]string
}

// NewRegistry starts a fake registry that lives until the test ends.
func NewRegistry(t testing.TB) *Registry {
	t.Helper()
	r := &Registry{subjects: map[string]int{}, compat: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(srv.Close)
	cl, err := sr.NewClient(sr.URLs(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	r.url, r.client = srv.URL, cl
	return r
}

// URL returns the address of the registry.
func (r *Registry) URL() string { return r.url }

// Client returns a client of the registry.
func (r *Registry) Client() *sr.Client { return r.client }

// Subjects returns the ID of the latest schema of every subject.
func (r *Registry) Subjects() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.subjects))
	for s, id := range r.subjects {
		out[s] = id
	}
	return out
}

// Schema returns the schema with the ID, or "" if there is none.
func (r *Registry) Schema(id int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id < 1 || id > len(r.schemas) {
		return ""
	}
	return r.schemas[id-1]
}

// Compatibility returns the compatibility level of the subject.
func (r *Registry) Compatibility(subject string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.compat[subject]
}

// Add registers a schema under the subject, as another service would, and
// returns its ID. The same schema always gets the same ID.
func (r *Registry) Add(subject, schema string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	switch {
	case req.Method == http.MethodPut && parts[0] == "config" && len(parts) == 2:
		var body struct {
			Compatibility string `json:"compatibility"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.compat[parts[1]] = body.Compatibility
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	case req.Method == http.MethodPost && parts[0] == "subjects" && len(parts) == 3:
		var body struct {
			Schema string `json:"schema"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]int{"id": r.Add(parts[1], body.Schema)})
	case req.Method == http.MethodGet && parts[0] == "schemas" && len(parts) == 3:
		id, _ := strconv.Atoi(parts[2])
		schema := r.Schema(id)
		if schema == "" {
			http.Error(w, `{"error_code":40403,"message":"Schema not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"schema": schema})
	default:
		http.NotFound(w, req)
	}
}
