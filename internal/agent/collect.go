package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/KKolyasik/max-benefits/internal/agent/fetch"
	"github.com/KKolyasik/max-benefits/internal/agent/llm"
	"github.com/KKolyasik/max-benefits/internal/agent/search"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// ErrContextTooSmall means the model saw only the start of the prompt: its
// context window is smaller than the prompt, and every answer would be
// garbage.
var ErrContextTooSmall = errors.New("the model's context window is too small for the prompt")

// errNoTokens means the run spent its token limit.
var errNoTokens = errors.New("the run has no tokens left")

// validID is what the agent accepts as a new card ID: it also names the
// draft file.
var validID = regexp.MustCompile(`^[a-z0-9_]+$`)

// Searcher finds pages for a query.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]search.Result, error)
}

// Fetcher reads a page.
type Fetcher interface {
	Fetch(ctx context.Context, url, query string) (fetch.Page, error)
}

// Model answers with JSON matching a schema.
type Model interface {
	Complete(ctx context.Context, msgs []llm.Message, schemaName string, schema any) (llm.Answer, error)
}

// Collector runs the agent over all queries.
type Collector struct {
	Survey  *survey.Survey
	Base    []knowledge.Card
	Queries []Query
	Search  Searcher
	Fetch   Fetcher
	Model   Model
	Drafts  Sink
	State   *State
	// Force sends pages to the model even if they did not change.
	Force bool
	// Parallel is how many queries run at once: the asynchronous mode may
	// keep a request for hours, but a local model takes one at a time.
	Parallel int
	// TokenLimit caps the model tokens of a run; 0 means no limit.
	TokenLimit int
	Log        *slog.Logger
	// Now is overridable in tests.
	Now func() time.Time
}

// Report sums up a run.
type Report struct {
	Queries int
	// Unchanged counts queries whose pages were the same as last time.
	Unchanged int
	Failed    int
	// Deferred counts queries left for the next run for lack of tokens.
	Deferred     int
	Drafts       []string
	InputTokens  int
	OutputTokens int
}

// run is what the queries of one Run share.
type run struct {
	mu     sync.Mutex
	report Report
	// tokens counts what the model calls spent or reserved.
	tokens int
	limit  int
	// drafts keeps two queries from writing a draft of one card at once.
	drafts sync.Mutex
}

// reserve books the most a model call can spend, so calls running at once
// can't overshoot the limit together.
func (r *run) reserve(n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.limit > 0 && r.tokens+n > r.limit {
		return false
	}
	r.tokens += n
	return true
}

// spent replaces a reservation with what the call took.
func (r *run) spent(reserved int, a llm.Answer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens += a.InputTokens + a.OutputTokens - reserved
	r.report.InputTokens += a.InputTokens
	r.report.OutputTokens += a.OutputTokens
}

// exhausted tells that not even an answer fits in the limit any more.
func (r *run) exhausted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.limit > 0 && r.tokens+llm.MaxAnswerTokens > r.limit
}

func (r *run) update(f func(*Report)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.report)
}

// Run processes the queries, Parallel at a time. A failed query is logged
// and skipped; errors after which no query can succeed (a rejected key, an
// unreachable model) stop the run. Queries the token limit leaves out wait
// for the next run.
func (c *Collector) Run(ctx context.Context) (Report, error) {
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	r := &run{limit: c.TokenLimit}
	slots := make(chan struct{}, max(c.Parallel, 1))
	var wg sync.WaitGroup
	for _, q := range c.Queries {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		r.update(func(rep *Report) { rep.Queries++ })
		wg.Go(func() {
			defer func() { <-slots }()
			log := c.Log.With("query", q.Text)
			err := c.query(ctx, q, r, log)
			switch {
			case err == nil:
			case errors.Is(err, errNoTokens):
				log.Warn("the run has no tokens left, the query waits for the next run")
				r.update(func(rep *Report) { rep.Deferred++ })
			case fatal(err):
				stop(err)
			default:
				log.Error("query failed", "err", err)
				r.update(func(rep *Report) { rep.Failed++ })
			}
		})
	}
	wg.Wait()
	return r.report, context.Cause(ctx)
}

func fatal(err error) bool {
	return errors.Is(err, search.ErrUnavailable) || errors.Is(err, llm.ErrUnreachable) || errors.Is(err, ErrSink) ||
		errors.Is(err, llm.ErrUnauthorized) || errors.Is(err, ErrContextTooSmall) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *Collector) query(ctx context.Context, q Query, r *run, log *slog.Logger) error {
	// Searching costs money too, and without tokens it would be for nothing.
	if r.exhausted() {
		return errNoTokens
	}
	results, err := c.Search.Search(ctx, q.Text, q.Results)
	if err != nil {
		return err
	}
	sources := c.read(ctx, q, results, log)
	if len(sources) == 0 {
		log.Info("nothing found")
		return nil
	}
	digest := digest(sources)
	if !c.Force && c.State.Seen(q.Text) == digest {
		log.Info("pages did not change since the last run")
		r.update(func(rep *Report) { rep.Unchanged++ })
		return nil
	}

	proposals, problems, err := c.extract(ctx, q, sources, r)
	if err != nil {
		return err
	}
	urls := make([]string, len(sources))
	for i, s := range sources {
		urls[i] = s.URL
	}
	for _, p := range proposals {
		r.drafts.Lock()
		path, written, err := c.Drafts.Write(ctx, Draft{
			Query:   q.Text,
			FoundAt: c.now(),
			Updates: p.Updates,
			Sources: urls,
			Notes:   problems[p.Card.ID],
			Card:    p.Card,
		})
		r.drafts.Unlock()
		if err != nil {
			return err
		}
		if !written {
			log.Info("a draft of this card already waits for review", "card", p.Card.ID)
			continue
		}
		log.Info("draft written", "path", path)
		r.update(func(rep *Report) { rep.Drafts = append(rep.Drafts, path) })
	}
	return c.State.Remember(q.Text, digest)
}

// read downloads the found pages in parallel, so one slow site doesn't hold
// up the rest. A page that can't be read is replaced by its search snippets.
func (c *Collector) read(ctx context.Context, q Query, results []search.Result, log *slog.Logger) []Source {
	sources := make([]Source, len(results))
	var wg sync.WaitGroup
	for i, res := range results {
		wg.Go(func() {
			src := Source{URL: res.URL, Title: res.Title}
			page, err := c.Fetch.Fetch(ctx, res.URL, q.Text)
			if err != nil {
				log.Info("page not read, using search snippets", "url", res.URL, "err", err)
				src.Text = strings.Join(res.Snippets, "\n")
			} else {
				src.Text, src.Published, src.PDF = page.Text, page.Published, page.PDF
				if src.Title == "" {
					src.Title = page.Title
				}
			}
			sources[i] = src
		})
	}
	wg.Wait()
	return slices.DeleteFunc(sources, func(s Source) bool { return strings.TrimSpace(s.Text) == "" })
}

// extract asks the model for cards and, if some are invalid, asks once more
// with the list of problems.
func (c *Collector) extract(ctx context.Context, q Query, sources []Source, r *run) ([]proposal, map[string][]string, error) {
	msgs := buildMessages(q, sources, c.Survey, c.Base, c.now())
	proposals, answer, err := c.complete(ctx, msgs, r)
	if err != nil {
		return nil, nil, err
	}
	proposals, problems, upToDate := check(proposals, c.Survey, c.Base, sources)
	if len(problems) > 0 {
		c.Log.Info("asking the model to fix its cards", "query", q.Text, "problems", problems)
		fixed, _, err := c.complete(ctx, repairMessages(msgs, answer, problems), r)
		switch {
		case errors.Is(err, errNoTokens):
			// The first answer is paid for: its drafts go to people with
			// the problems listed.
			c.Log.Warn("no tokens left to fix the cards, the drafts keep their problems", "query", q.Text)
		case err != nil:
			return nil, nil, err
		default:
			proposals, problems, upToDate = check(fixed, c.Survey, c.Base, sources)
		}
	}
	if len(upToDate) > 0 {
		c.Log.Info("the model found nothing new for these cards", "query", q.Text, "cards", upToDate)
	}
	return proposals, problems, nil
}

func (c *Collector) complete(ctx context.Context, msgs []llm.Message, r *run) ([]proposal, string, error) {
	chars := 0
	for _, m := range msgs {
		chars += utf8.RuneCountInString(m.Content)
	}
	// A token takes 2.5–4 characters of Russian text, so half the
	// characters is more than the prompt can cost.
	reserved := chars/2 + llm.MaxAnswerTokens
	if !r.reserve(reserved) {
		return nil, "", errNoTokens
	}
	answer, err := c.Model.Complete(ctx, msgs, "cards", schema(c.Survey))
	r.spent(reserved, answer)
	if err != nil {
		return nil, "", err
	}
	// Fewer tokens than a fifth of the characters means the server cut the
	// prompt: Ollama does it silently when the prompt exceeds its context
	// window.
	if answer.InputTokens > 0 && answer.InputTokens < chars/5 {
		return nil, "", fmt.Errorf("%w: it read %d tokens of a %d-character prompt (for Ollama set OLLAMA_CONTEXT_LENGTH=32768)",
			ErrContextTooSmall, answer.InputTokens, chars)
	}
	if answer.Truncated {
		return nil, "", errors.New("the answer hit the token limit; lower `results` of the query")
	}
	proposals, err := parseAnswer(answer.Content)
	return proposals, answer.Content, err
}

// check validates the model's cards. It returns the cards worth drafting, the
// problems of each by card ID and the IDs of the updates that change nothing.
func check(proposals []proposal, s *survey.Survey, base []knowledge.Card, sources []Source) ([]proposal, map[string][]string, []string) {
	byID := map[string]knowledge.Card{}
	for _, c := range base {
		byID[c.ID] = c
	}
	sourceURLs := map[string]bool{}
	for _, src := range sources {
		sourceURLs[normURL(src.URL)] = true
	}
	var kept []proposal
	var upToDate []string
	problems := map[string][]string{}
	seen := map[string]bool{}
	for _, p := range proposals {
		var errs []string
		// The same ID is the same card: models forget to fill in updates,
		// and asked to fix it, they may make a duplicate with another ID.
		if _, taken := byID[p.Card.ID]; taken && p.Updates == "" {
			p.Updates = p.Card.ID
		}
		// Links come from the sources, and an update may keep the links of
		// the card it rewrites. Any other, e.g. an address written in a page's
		// text, may be stale, and asking the model to fix it would double the
		// price of the query, so it is dropped.
		old, isUpdate := byID[p.Updates]
		oldLinks := map[string]bool{}
		for _, l := range old.Links {
			oldLinks[normURL(l.URL)] = true
		}
		p.Card.Links = slices.DeleteFunc(p.Card.Links, func(l knowledge.CardLink) bool {
			u := normURL(l.URL)
			return !sourceURLs[u] && !oldLinks[u]
		})
		if p.Updates != "" {
			if !isUpdate {
				errs = append(errs, fmt.Sprintf("updates: there is no card %q in the base", p.Updates))
			} else {
				p.Card.ID = p.Updates
				keepConditions(&p.Card, old)
				if knowledge.MarshalCard(p.Card) == knowledge.MarshalCard(old) {
					upToDate = append(upToDate, old.ID) // not worth a review
					continue
				}
			}
		}
		if !validID.MatchString(p.Card.ID) {
			errs = append(errs, fmt.Sprintf("id %q: use lowercase latin letters, digits and _", p.Card.ID))
		}
		if seen[p.Card.ID] {
			errs = append(errs, fmt.Sprintf("id %q appears twice in the answer", p.Card.ID))
		}
		seen[p.Card.ID] = true
		for _, err := range knowledge.ValidateCard(p.Card, s) {
			errs = append(errs, err.Error())
		}
		kept = append(kept, p)
		if len(errs) > 0 {
			problems[p.Card.ID] = append(problems[p.Card.ID], errs...)
		}
	}
	return kept, problems, upToDate
}

// keepConditions keeps who sees the card and its links, e.g. a guide of one
// university: people decide it. The model drops the conditions its sources
// don't mention and can't write link conditions at all.
func keepConditions(c *knowledge.Card, old knowledge.Card) {
	c.Match = old.Match
	for _, l := range old.Links {
		if len(l.When) == 0 {
			continue
		}
		i := slices.IndexFunc(c.Links, func(n knowledge.CardLink) bool { return normURL(n.URL) == normURL(l.URL) })
		if i < 0 {
			c.Links = append(c.Links, l)
		} else {
			c.Links[i].When = l.When
		}
	}
}

// normURL makes the addresses of one page equal: the model may drop the
// trailing slash or switch http to https, which links require.
func normURL(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
}

func digest(sources []Source) string {
	h := sha256.New()
	for _, s := range sources {
		h.Write([]byte(s.URL))
		h.Write([]byte{0})
		h.Write([]byte(s.Text))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// State remembers a digest of the pages each query found last time, so
// unchanged pages are not sent to the model again. Queries running at once
// share it.
type State struct {
	mu      sync.Mutex
	path    string
	digests map[string]string
}

// LoadState reads the state file; a missing file is an empty state.
func LoadState(path string) (*State, error) {
	s := &State{path: path, digests: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent state: %w", err)
	}
	if err := json.Unmarshal(data, &s.digests); err != nil {
		return nil, fmt.Errorf("agent state %s: %w", path, err)
	}
	return s, nil
}

// Seen returns the digest remembered for the query.
func (s *State) Seen(query string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digests[query]
}

// Remember stores the digest of the query's pages and saves the state file.
func (s *State) Remember(query, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digests[query] = digest
	data, err := json.MarshalIndent(maps.Clone(s.digests), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("save agent state: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o644); err != nil {
		return fmt.Errorf("save agent state: %w", err)
	}
	return nil
}
