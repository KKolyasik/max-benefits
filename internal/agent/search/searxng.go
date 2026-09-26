package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SearXNG searches through a SearXNG instance: a free metasearch engine that
// asks Google, Bing, DuckDuckGo, Yandex and others. No key is needed, which
// makes it handy for trying the agent out, but the engines throttle frequent
// queries, so regular runs are better off with Yandex.
//
// The instance must allow the JSON format; see deploy/searxng/settings.yml.
type SearXNG struct {
	// URL of the instance, e.g. http://localhost:8888.
	URL  string
	HTTP *http.Client
}

type searxngResponse struct {
	Results []struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Content string `json:"content"`
	} `json:"results"`
	// Engines that failed, as [name, reason] pairs.
	Unresponsive [][]string `json:"unresponsive_engines"`
}

// Search returns up to limit results, one per site.
func (s *SearXNG) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	q := url.Values{"q": {query}, "format": {"json"}, "language": {"ru-RU"}, "safesearch": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(s.URL, "/")+"/search?"+q.Encode(), http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: SearXNG at %s: %w (is it running? docker compose --profile agent up -d searxng)",
			ErrUnavailable, s.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("searxng: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: SearXNG refuses the JSON format; allow it in search.formats of settings.yml", ErrUnavailable)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("searxng: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var r searxngResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("searxng: %w", err)
	}
	if len(r.Results) == 0 && len(r.Unresponsive) > 0 {
		// Nothing found because every engine failed, not because there is
		// nothing to find.
		var failed []string
		for _, e := range r.Unresponsive {
			failed = append(failed, strings.Join(e, ": "))
		}
		return nil, fmt.Errorf("%w: search engines did not answer (%s); try again later",
			ErrUnavailable, strings.Join(failed, ", "))
	}
	results := make([]Result, 0, len(r.Results))
	for _, res := range r.Results {
		out := Result{URL: res.URL, Title: res.Title}
		if c := strings.TrimSpace(res.Content); c != "" {
			out.Snippets = []string{c}
		}
		results = append(results, out)
	}
	return onePerSite(results, limit), nil
}
