// Package search finds pages for the agent's queries: through Yandex Search
// API, or through a SearXNG instance for free runs without a key.
package search

import (
	"errors"
	"net/url"
	"strings"
)

// ErrUnavailable means no query can succeed right now: the key is rejected
// or every engine refuses to answer. The agent stops instead of wasting the
// remaining queries.
var ErrUnavailable = errors.New("search is unavailable")

// Result is a found page.
type Result struct {
	URL   string
	Title string
	// Snippets are fragments of the page text around the query words.
	Snippets []string
}

// onePerSite keeps the first result from each site, up to limit, so the
// sources are not all from one domain.
func onePerSite(results []Result, limit int) []Result {
	var out []Result
	seen := map[string]bool{}
	for _, r := range results {
		u, err := url.Parse(r.URL)
		if err != nil || u.Host == "" {
			continue
		}
		site := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		if seen[site] {
			continue
		}
		seen[site] = true
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out
}
