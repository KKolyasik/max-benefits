// Package fetch downloads found pages and turns them into plain text for the
// model: the main content of HTML pages without menus and footers, and the
// text of PDF documents.
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html/charset"
)

// UserAgent tells sites who is reading them.
const UserAgent = "Mozilla/5.0 (compatible; max-benefits-agent; +https://github.com/KKolyasik/max-benefits)"

const (
	maxHTMLBytes = 5 << 20
	maxPDFBytes  = 20 << 20
	// A PDF with less text than this is a scan without a text layer.
	minPDFText = 200
)

var (
	// ErrUnsupported means the document type can't be read, e.g. a Word file.
	ErrUnsupported = errors.New("unsupported document type")
	// ErrNoText means the page has no text to read, e.g. a scanned PDF.
	ErrNoText = errors.New("no text in the document")
)

// Page is the readable content of a URL.
type Page struct {
	Title string
	Text  string
	// Published is when the page was published or last changed, if it says.
	Published time.Time
	PDF       bool
}

// Fetcher downloads pages.
type Fetcher struct {
	HTTP *http.Client
	// MaxChars limits the text of one page; longer texts keep the parts most
	// relevant to the query.
	MaxChars int
	// PDFToText is the pdftotext binary from poppler-utils; empty disables
	// PDFs.
	PDFToText string
}

// Fetch downloads a page and extracts its text, keeping the parts most
// relevant to the query if it is too long.
func (f *Fetcher) Fetch(ctx context.Context, rawURL, query string) (Page, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Page{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	// Some sites, metro.spb.ru among them, refuse requests without Accept.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru,en;q=0.5")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Page{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPDFBytes))
	if err != nil {
		return Page{}, err
	}

	contentType := resp.Header.Get("Content-Type")
	var page Page
	switch {
	case bytes.HasPrefix(body, []byte("%PDF-")):
		page, err = f.pdf(ctx, body)
	case strings.Contains(contentType, "html") || contentType == "":
		page, err = htmlPage(body[:min(len(body), maxHTMLBytes)], contentType, u)
	default:
		return Page{}, fmt.Errorf("%w: %s", ErrUnsupported, contentType)
	}
	if err != nil {
		return Page{}, err
	}
	page.Text = Relevant(page.Text, query, f.MaxChars)
	return page, nil
}

// minArticle is how much text readability must find for its article to be
// trusted. University pages keep sums and dates in collapsed sections that
// readability drops, so with less the whole page is taken, and Relevant
// keeps the parts close to the query.
const minArticle = 2000

func htmlPage(body []byte, contentType string, u *url.URL) (Page, error) {
	// Some official sites still serve windows-1251.
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return Page{}, fmt.Errorf("decode page: %w", err)
	}
	decoded, err := io.ReadAll(r)
	if err != nil {
		return Page{}, fmt.Errorf("decode page: %w", err)
	}
	article, err := readability.FromReader(bytes.NewReader(decoded), u)
	if err != nil {
		return Page{}, fmt.Errorf("extract article: %w", err)
	}
	text := markdown(article.Content, article.TextContent)
	if n := utf8.RuneCountInString(text); n < minArticle {
		// A page much longer than its article lost something; one about
		// as short is a script shell, and its menu would only add noise.
		if whole := markdown(string(decoded), ""); utf8.RuneCountInString(whole) > 2*max(n, minArticle/2) {
			text = whole
		}
	}
	page := Page{Title: article.Title, Text: text}
	if article.ModifiedTime != nil {
		page.Published = *article.ModifiedTime
	} else if article.PublishedTime != nil {
		page.Published = *article.PublishedTime
	}
	if page.Text == "" {
		return Page{}, ErrNoText
	}
	return page, nil
}

func (f *Fetcher) pdf(ctx context.Context, body []byte) (Page, error) {
	if f.PDFToText == "" {
		return Page{}, fmt.Errorf("%w: PDF (pdftotext is not installed)", ErrUnsupported)
	}
	cmd := exec.CommandContext(ctx, f.PDFToText, "-enc", "UTF-8", "-nopgbrk", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return Page{}, fmt.Errorf("pdftotext: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	text := tidy(out.String())
	if len([]rune(strings.Join(strings.Fields(text), ""))) < minPDFText {
		return Page{}, fmt.Errorf("%w: a scanned PDF", ErrNoText)
	}
	return Page{Title: firstLine(text), Text: text, PDF: true}, nil
}

var (
	blankLines = regexp.MustCompile(`\n{3,}`)
	// Links and images only cost tokens: the model gets the page URL anyway.
	mdImage = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdLink  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// markdown turns HTML into text. Markdown keeps lists and tables, which often
// hold the steps and sums.
func markdown(html, fallback string) string {
	text, err := htmltomarkdown.ConvertString(html)
	if err != nil || strings.TrimSpace(text) == "" {
		text = fallback
	}
	return tidy(mdLink.ReplaceAllString(mdImage.ReplaceAllString(text, ""), "$1"))
}

// tidy trims lines and collapses runs of blank lines.
func tidy(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	line = strings.Join(strings.Fields(line), " ")
	if r := []rune(line); len(r) > 120 {
		line = string(r[:120])
	}
	return line
}
