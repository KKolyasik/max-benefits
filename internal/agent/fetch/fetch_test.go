package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

const article = `<html><head><title>Проездной для студентов</title>
<meta property="article:modified_time" content="2026-08-01T10:00:00+03:00"></head>
<body>
<nav><a href="/">Главная</a> <a href="/news">Новости</a> <a href="/contacts">Контакты</a></nav>
<article>
<h1>Льготный проездной для студентов</h1>
<p>Студенты очной формы обучения могут купить льготный проездной билет на месяц. В 2026 году он стоит 1599 рублей и действует в метро, автобусах, трамваях и троллейбусах.</p>
<p>Чтобы оформить проездной, приходи в любую кассу метрополитена с паспортом и студенческим билетом. Кассир запишет проездной на Единую карту петербуржца.</p>
<ul><li>Паспорт</li><li>Студенческий билет</li></ul>
<p>Продлевать проездной нужно каждый месяц: в кассах, автоматах или через банк.</p>
</article>
<footer>© Петербургский метрополитен, все права защищены</footer>
</body></html>`

func TestFetchHTMLInWindows1251(t *testing.T) {
	body, err := charmap.Windows1251.NewEncoder().String(article)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != UserAgent || r.Header.Get("Accept") == "" {
			t.Errorf("headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := &Fetcher{HTTP: srv.Client(), MaxChars: 5000}
	page, err := f.Fetch(context.Background(), srv.URL+"/ticket", "проездной")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1599 рублей", "с паспортом и студенческим", "- Паспорт"} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("text has no %q:\n%s", want, page.Text)
		}
	}
	for _, noise := range []string{"Контакты", "все права защищены"} {
		if strings.Contains(page.Text, noise) {
			t.Errorf("menu or footer %q left in:\n%s", noise, page.Text)
		}
	}
	if page.Published.Year() != 2026 || page.PDF {
		t.Errorf("page %+v", page)
	}
}

// Readability skips hidden elements, and university pages keep sums and
// dates in collapsed sections, so a page much longer than its article is
// taken whole.
func TestFetchKeepsWhatReadabilityDrops(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<html><head><title>Стипендия</title></head><body>
<article><h1>Именная стипендия</h1><p>Конкурс для студентов с достижениями в учёбе, науке и спорте. Приём заявок закрыт.</p>
<h2>Размеры стипендии</h2><div class="accordion__content" aria-hidden="true"><p>Размер стипендии: от 2 000 до 7 000 рублей ежемесячно.</p>`)
	for i := range 12 {
		fmt.Fprintf(&b, "<p>Условие %d: студент учится очно, не имеет академических задолженностей и подтверждает достижения за последний год документами, которые принимает стипендиальная комиссия вуза.</p>", i+1)
	}
	b.WriteString("</div></article></body></html>")

	page, err := htmlPage([]byte(b.String()), "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Text, "от 2 000 до 7 000 рублей") || !strings.Contains(page.Text, "Приём заявок закрыт") {
		t.Errorf("text:\n%s", page.Text)
	}
}

func TestFetchUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/doc":
			w.Header().Set("Content-Type", "application/msword")
			_, _ = w.Write([]byte("binary"))
		case "/pdf":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(makePDF("text"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &Fetcher{HTTP: srv.Client()}

	if _, err := f.Fetch(context.Background(), srv.URL+"/doc", ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("word file: %v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/pdf", ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("pdf without pdftotext: %v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/missing", ""); err == nil {
		t.Error("expected an error for 404")
	}
}

func TestFetchPDF(t *testing.T) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("pdftotext is not installed")
	}
	text := strings.Repeat("Scholarship regulation paragraph with the amount and the procedure. ", 5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/scan.pdf" {
			_, _ = w.Write(makePDF("Scan"))
			return
		}
		_, _ = w.Write(makePDF(text))
	}))
	defer srv.Close()
	f := &Fetcher{HTTP: srv.Client(), PDFToText: bin}

	page, err := f.Fetch(context.Background(), srv.URL+"/doc.pdf", "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.PDF || !strings.Contains(page.Text, "Scholarship regulation paragraph") {
		t.Errorf("page %+v", page)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/scan.pdf", ""); !errors.Is(err, ErrNoText) {
		t.Errorf("scan: %v", err)
	}
}

func TestRelevant(t *testing.T) {
	var parts []string
	parts = append(parts, "ПОЛОЖЕНИЕ о материальной помощи обучающимся, 2024 год.")
	for i := range 30 {
		parts = append(parts, fmt.Sprintf("Раздел %d. Общие положения о порядке работы комиссии и хранении документов в архиве университета.", i))
	}
	parts[20] = "Размер материальной помощи студентам составляет до двух стипендий, заявление подаётся в студенческий офис."
	text := strings.Join(parts, "\n\n")

	got := Relevant(text, "материальная помощь студентам размер", 600)

	if !strings.HasPrefix(got, "ПОЛОЖЕНИЕ о материальной помощи") {
		t.Errorf("the opening must stay:\n%s", got)
	}
	if !strings.Contains(got, "до двух стипендий") {
		t.Errorf("the relevant part is lost:\n%s", got)
	}
	if !strings.Contains(got, "[…]") {
		t.Errorf("cuts must be marked:\n%s", got)
	}
	if n := len([]rune(got)); n > 700 {
		t.Errorf("too long: %d", n)
	}
	if short := "короткий текст"; Relevant(short, "текст", 600) != short {
		t.Error("a short text must stay as is")
	}
}

// makePDF builds a one-page PDF with the text in Helvetica.
func makePDF(text string) []byte {
	content := fmt.Sprintf("BT /F1 10 Tf 20 800 Td (%s) Tj ET", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 2000 842] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return []byte(b.String())
}
