package knowledge

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// summaryWidth is where summaries wrap, like the hand-written ones.
const summaryWidth = 72

// plainID matches IDs that YAML reads as strings without quotes.
var plainID = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// MarshalCard formats a card the way cards are written by hand in
// knowledge.yaml: flow lists for categories and conditions, quoted texts and
// a folded summary. The result is a top-level mapping.
func MarshalCard(c Card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", scalar(c.ID))
	fmt.Fprintf(&b, "categories: %s\n", flowList(c.Categories))
	fmt.Fprintf(&b, "priority: %d\n", c.Priority)
	if len(c.Match) > 0 {
		b.WriteString("match:\n")
		for _, q := range slices.Sorted(maps.Keys(c.Match)) {
			fmt.Fprintf(&b, "  %s: %s\n", scalar(q), flowList(c.Match[q]))
		}
	}
	fmt.Fprintf(&b, "title: %s\n", strconv.Quote(c.Title))
	writeSummary(&b, c.Summary)
	writeList(&b, "steps", c.Steps)
	writeList(&b, "documents", c.Documents)
	if c.Where != "" {
		fmt.Fprintf(&b, "where: %s\n", strconv.Quote(c.Where))
	}
	if len(c.Links) > 0 {
		b.WriteString("links:\n")
		for _, l := range c.Links {
			fmt.Fprintf(&b, "  - title: %s\n    url: %s\n", strconv.Quote(l.Title), strconv.Quote(l.URL))
			if len(l.When) > 0 {
				fmt.Fprintf(&b, "    when: %s\n", flowMap(l.When))
			}
		}
	}
	return b.String()
}

func writeSummary(b *strings.Builder, text string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		b.WriteString("summary: \"\"\n")
		return
	}
	// A folded block turns line breaks back into spaces, so wrapping at
	// word boundaries keeps the text as is.
	b.WriteString("summary: >-\n")
	line := ""
	for _, w := range words {
		if line != "" && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > summaryWidth {
			fmt.Fprintf(b, "  %s\n", line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	fmt.Fprintf(b, "  %s\n", line)
}

func writeList(b *strings.Builder, key string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", key)
	for _, s := range items {
		fmt.Fprintf(b, "  - %s\n", strconv.Quote(s))
	}
}

func flowList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = scalar(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func flowMap(cond Condition) string {
	parts := make([]string, 0, len(cond))
	for _, q := range slices.Sorted(maps.Keys(cond)) {
		parts = append(parts, scalar(q)+": "+flowList(cond[q]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// scalar writes IDs plain when that is safe, and quotes the rest, e.g. "1"
// or "3plus", which YAML would otherwise read as a number or misparse.
func scalar(s string) string {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "y", "n", "null":
		return strconv.Quote(s)
	}
	if plainID.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}
