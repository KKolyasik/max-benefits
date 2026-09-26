package fetch

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// chunkChars is the size of the pieces a long text is cut into.
	chunkChars = 600
	// stemLen crudely strips Russian endings: "стипендия", "стипендии" and
	// "стипендиальный" all start with "стипе".
	stemLen = 5
	gap     = "\n\n[…]\n\n"
)

// Relevant shortens a text to about limit characters. A regulation of 15
// pages usually has the sums and the procedure in the middle or in an
// appendix, so instead of the beginning it keeps the pieces that share the
// most words with the query, in their original order. The opening piece is
// always kept: it names the document and often its date.
func Relevant(text, query string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	chunks := split(text)
	terms := stems(query)
	scores := make([]int, len(chunks))
	for i, c := range chunks {
		scores[i] = score(c, terms)
	}
	order := make([]int, len(chunks)-1)
	for i := range order {
		order[i] = i + 1
	}
	// Best first; the earlier piece wins a tie.
	slices.SortStableFunc(order, func(a, b int) int { return scores[b] - scores[a] })

	picked := []int{0}
	used := utf8.RuneCountInString(chunks[0])
	for _, i := range order {
		n := utf8.RuneCountInString(chunks[i]) + utf8.RuneCountInString(gap)
		if used+n > limit {
			continue
		}
		picked = append(picked, i)
		used += n
	}
	slices.Sort(picked)

	var b strings.Builder
	for k, i := range picked {
		if k > 0 {
			if i == picked[k-1]+1 {
				b.WriteString("\n\n")
			} else {
				b.WriteString(gap)
			}
		}
		b.WriteString(chunks[i])
	}
	if picked[len(picked)-1] != len(chunks)-1 {
		b.WriteString(gap)
	}
	return b.String()
}

// split cuts a text into pieces of about chunkChars, preferring paragraph
// breaks. A piece is never cut inside a line, unless the line alone is too
// long.
func split(text string) []string {
	var chunks []string
	var cur []string
	size := 0
	flush := func() {
		if s := strings.TrimSpace(strings.Join(cur, "\n")); s != "" {
			chunks = append(chunks, s)
		}
		cur, size = nil, 0
	}
	for _, line := range strings.Split(text, "\n") {
		n := utf8.RuneCountInString(line)
		switch {
		case strings.TrimSpace(line) == "" && size >= chunkChars/3:
			flush()
			continue
		case size > 0 && size+n > chunkChars:
			flush()
		}
		for n > chunkChars {
			r := []rune(line)
			chunks = append(chunks, string(r[:chunkChars]))
			line = string(r[chunkChars:])
			n -= chunkChars
		}
		cur = append(cur, line)
		size += n + 1
	}
	flush()
	return chunks
}

func stems(text string) []string {
	var out []string
	for _, w := range words(text) {
		if utf8.RuneCountInString(w) < 4 {
			continue
		}
		s := stem(w)
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// score counts the query stems that occur in the piece.
func score(chunk string, terms []string) int {
	have := map[string]bool{}
	for _, w := range words(chunk) {
		have[stem(w)] = true
	}
	n := 0
	for _, t := range terms {
		if have[t] {
			n++
		}
	}
	return n
}

func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func stem(w string) string {
	if r := []rune(w); len(r) > stemLen {
		return string(r[:stemLen])
	}
	return w
}
