package bot

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// MAX rejects messages longer than 4000 characters; keep a margin for markup.
const maxMessageRunes = 3500

// MAX allows 30 rows of buttons under a message and 128 characters on a
// button. The list of results shows at most maxListed cards: the rest are a
// flip away. A button text is cut at maxButtonRunes, with room for emoji,
// which may count twice.
const (
	maxListed      = 25
	maxButtonRunes = 100
)

// A pressed button turns its message into the next screen. The menu and the
// questions are plain text: a tick in a multi-choice question keeps the text
// MAX sends with the press, and that text has no markup. The results are
// markdown, and the links of a card are buttons: MAX gives an edited message
// a preview of the first link in its text.

const textWelcome = `Привет! Я Навигатор студента 🧭

Помогаю первокурсникам, которые переехали учиться в Петербург, разобраться, какие льготы, стипендии и скидки им положены. Расскажу по шагам, что оформить, какие документы собрать и куда идти.

Выбери раздел, ответь на пару вопросов, и я соберу подборку для тебя 👇`

const textMenu = "Выбери раздел 👇"

const textAbout = `ℹ️ Навигатор студента: MVP проекта для студентов Петербурга.

Как это работает: ты выбираешь раздел, отвечаешь на несколько вопросов кнопками, а я подбираю из базы знаний то, что подходит именно тебе: что это, зачем, как оформить и куда идти.

Базу собираем из официальных источников: сайтов вузов, метрополитена, Госуслуг. Правила иногда меняются, поэтому перед походом в инстанцию сверься со ссылкой в карточке.

Ответы анкеты хранятся только затем, чтобы не спрашивать тебя повторно. Удалить их можно в разделе «Моя анкета».`

const (
	textUseButtons   = "Я пока понимаю только кнопки 🙂"
	textAnswerButton = "Ответь, пожалуйста, кнопкой под вопросом 👇"
	textStale        = "Эта кнопка уже неактуальна"
	textPickOne      = "Отметь хотя бы один вариант"
	textDeleted      = "Готово, я удалил все твои ответы и прогресс. Если захочешь вернуться, просто выбери раздел 👇"
	textFailed       = "Что-то пошло не так 😔 Попробуй ещё раз чуть позже."
	textNothingFound = "По твоим ответам в этом разделе пока ничего не нашлось 🤷 Мы пополняем базу, загляни позже или попробуй другой раздел."
	textDisclaimer   = "⚠️ Правила иногда меняются: перед походом в инстанцию сверься с официальным источником по ссылке."

	labelMenu        = "🏠 В меню"
	labelBack        = "◀️ Назад"
	labelNext        = "Далее ▶️"
	labelList        = "📋 Список"
	labelProfile     = "👤 Моя анкета"
	labelAbout       = "ℹ️ О боте"
	labelDelete      = "🗑 Удалить мои данные"
	labelDeleteYes   = "Да, удалить"
	labelCancel      = "Отмена"
	labelUseSaved    = "✅ Показать подборку"
	labelRedo        = "✏️ Заполнить заново"
	labelOtherTopics = "🗂 Другие разделы"
	labelEditAnswers = "✏️ Изменить анкету"
	labelDone        = "✔️ Готово"

	labelDrafts    = "🛠 Черновики"
	labelReview    = "📝 Разобрать"
	labelApprove   = "✅ Одобрить"
	labelReject    = "❌ Отклонить"
	labelSkip      = "⏭ Пропустить"
	labelFromStart = "🔄 Сначала"
	labelAgent     = "🤖 Агент"
	labelRun       = "▶️ Запустить"
	labelRunForce  = "🔁 Перечитать всё"

	textNoDrafts    = "Черновиков на проверке нет 🎉"
	textLastDraft   = "Это был последний черновик. Пропущенные ждут проверки: %d."
	textApproved    = "✅ Одобрено: студенты уже видят карточку"
	textUpdated     = "✅ Одобрено: студенты уже видят новую версию карточки"
	textRejected    = "❌ Отклонено"
	textSkipped     = "⏭ Пропущено"
	textDecided     = "Этот черновик уже разобран"
	textCantApprove = "⛔ Одобрить нельзя: база или анкета изменились"

	textAgentSent  = "Команда ушла агенту: он напишет, когда начнёт прогон, а по итогам пришлёт отчёт."
	textAgentDown  = "Не получилось передать команду агенту: нет связи с Kafka 😔 Попробуй чуть позже."
	textRunStarted = "▶️ Агент начал прогон. Когда закончит, пришлю отчёт."
	textRunBusy    = "⏳ Агент занят другим прогоном и эту команду пропустил. Отчёт о текущем прогоне придёт, когда он закончится."
)

const textAgent = `🤖 Агент ищет в интернете свежие сведения для разделов бота и присылает черновики карточек на проверку. По расписанию он запускается сам, а здесь его можно запустить сейчас.

Страницы, которые не изменились с прошлого прогона, агент модели не отправляет, поэтому обычный прогон обходится дёшево. «Перечитать всё» отправит модели все страницы заново: это дороже, зато пригодится, если агента настроили по-новому.`

// menuMessage is the main menu; admins also get the drafts and the agent
// buttons.
func (b *Bot) menuMessage(userID int64, text string) Message {
	var kb [][]Button
	for _, c := range b.survey.Categories {
		kb = append(kb, []Button{{Text: c.Title, Payload: payload(actCategory, c.ID)}})
	}
	if b.isAdmin(userID) {
		row := []Button{{Text: labelDrafts, Payload: actDrafts}}
		if b.agent != nil {
			row = append(row, Button{Text: labelAgent, Payload: actAgent})
		}
		kb = append(kb, row)
	}
	kb = append(kb, []Button{
		{Text: labelProfile, Payload: actProfile},
		{Text: labelAbout, Payload: actAbout},
	})
	return Message{Text: text, Keyboard: kb}
}

func aboutMessage() Message {
	return Message{Text: textAbout, Keyboard: [][]Button{{{Text: labelMenu, Payload: actMenu}}}}
}

// questionMessage renders a question under the title of its category. note
// goes under the title, e.g. the category intro before the first question.
// The selected options are ticked: the ones picked in a multi-choice
// question, or the old answer when the user comes back to a question.
func (b *Bot) questionMessage(c *survey.Category, q *survey.Question, pos int, selected []string, note string) Message {
	var sb strings.Builder
	sb.WriteString(c.Title)
	if note != "" {
		sb.WriteString("\n")
		sb.WriteString(note)
	}
	fmt.Fprintf(&sb, "\n\nВопрос %d из %d\n%s", pos, len(c.Questions), q.Text)
	if q.Multi {
		sb.WriteString("\n\nМожно выбрать несколько вариантов, потом нажми «Готово».")
	}

	buttons := make([]Button, 0, len(q.Options))
	for _, o := range q.Options {
		btn := Button{Text: o.Title, Payload: payload(actAnswer, q.ID, o.ID)}
		if q.Multi {
			btn = Button{Text: "▫️ " + o.Title, Payload: payload(actToggle, q.ID, o.ID)}
		}
		if slices.Contains(selected, o.ID) {
			btn.Text = "✅ " + o.Title
		}
		buttons = append(buttons, btn)
	}
	kb := layout(buttons)
	if q.Multi {
		kb = append(kb, []Button{{Text: labelDone, Payload: payload(actDone, q.ID)}})
	}
	nav := []Button{{Text: labelMenu, Payload: actMenu}}
	if pos > 1 {
		nav = append([]Button{{Text: labelBack, Payload: payload(actBack, q.ID)}}, nav...)
	}
	return Message{Text: sb.String(), Keyboard: append(kb, nav)}
}

// layout puts short buttons two per row and long ones on their own row, so
// titles are not truncated on narrow phone screens.
func layout(buttons []Button) [][]Button {
	const shortRunes = 16
	short := true
	for _, btn := range buttons {
		if utf8.RuneCountInString(btn.Text) > shortRunes {
			short = false
			break
		}
	}
	perRow := 1
	if short {
		perRow = 2
	}
	var rows [][]Button
	for chunk := range slices.Chunk(buttons, perRow) {
		rows = append(rows, chunk)
	}
	return rows
}

func (b *Bot) confirmMessage(c *survey.Category, answers map[string][]string) Message {
	text := fmt.Sprintf("%s\n\nУ меня уже есть твои ответы для этого раздела:\n\n%s\n\nПоказать подборку по ним или заполнить анкету заново?",
		c.Title, b.summary(c.Questions, answers))
	return Message{Text: text, Keyboard: [][]Button{
		{{Text: labelUseSaved, Payload: payload(actUseSaved, c.ID)}},
		{{Text: labelRedo, Payload: payload(actRedo, c.ID)}},
		{{Text: labelMenu, Payload: actMenu}},
	}}
}

func (b *Bot) profileMessage(answers map[string][]string) Message {
	var qids []string
	for _, q := range b.survey.AllQuestions() {
		if len(answers[q.ID]) > 0 {
			qids = append(qids, q.ID)
		}
	}
	back := []Button{{Text: labelMenu, Payload: actMenu}}
	if len(qids) == 0 {
		return Message{
			Text:     "👤 Твоя анкета пока пустая. Она заполнится, когда ты выберешь раздел и ответишь на вопросы.",
			Keyboard: [][]Button{back},
		}
	}
	text := "👤 Твоя анкета\n\n" + b.summary(qids, answers) +
		"\n\nЧтобы поменять ответы, выбери раздел в меню и нажми «Заполнить заново»."
	return Message{Text: text, Keyboard: [][]Button{
		{{Text: labelDelete, Payload: actDelete}},
		back,
	}}
}

func deleteConfirmMessage() Message {
	return Message{
		Text: "Удалить все твои ответы и прогресс? Это нельзя отменить.",
		Keyboard: [][]Button{{
			{Text: labelDeleteYes, Payload: actDeleteConfirm},
			{Text: labelCancel, Payload: actProfile},
		}},
	}
}

// summary lists answers as "Вуз: СПбГУ" lines.
func (b *Bot) summary(qids []string, answers map[string][]string) string {
	lines := make([]string, 0, len(qids))
	for _, qid := range qids {
		q, ok := b.survey.Question(qid)
		if !ok {
			continue
		}
		value := strings.Join(q.Titles(answers[qid]), ", ")
		if value == "" {
			value = "не заполнено"
		}
		lines = append(lines, fmt.Sprintf("• %s: %s", q.Label, value))
	}
	return strings.Join(lines, "\n")
}

// resultsMessage lists the cards found, most important first, as buttons: a
// card opens in place of the list.
func resultsMessage(c *survey.Category, entries []knowledge.Entry) Message {
	next := [][]Button{
		{{Text: labelOtherTopics, Payload: actMenu}},
		{{Text: labelEditAnswers, Payload: payload(actRedo, c.ID)}},
	}
	if len(entries) == 0 {
		return Message{Text: c.Title + "\n\n" + textNothingFound, Keyboard: next}
	}

	found := fmt.Sprintf("Нашёл %d %s, самые важные — сверху. Открой любой: расскажу, что это, как оформить и куда идти.",
		len(entries), plural(len(entries), "пункт", "пункта", "пунктов"))
	if len(entries) == 1 {
		found = "Нашёл 1 пункт. Открой его: расскажу, что это, как оформить и куда идти."
	}
	var kb [][]Button
	for _, e := range entries[:min(len(entries), maxListed)] {
		kb = append(kb, []Button{{Text: clip(e.Title), Payload: cardPayload(c, e.ID, 0)}})
	}
	if rest := len(entries) - maxListed; rest > 0 {
		kb = append(kb, []Button{{
			Text:    fmt.Sprintf("Ещё %d %s ▶️", rest, plural(rest, "пункт", "пункта", "пунктов")),
			Payload: cardPayload(c, entries[maxListed].ID, 0),
		}})
	}
	return Message{
		Text:     fmt.Sprintf("**%s: подборка для тебя**\n%s\n\n%s", c.Title, found, textDisclaimer),
		Markdown: true,
		Keyboard: append(kb, next...),
	}
}

// page is a screen of a card: a long card takes a few.
type page struct {
	entry, part, parts int
	text               string
}

// cardPages splits the cards found into screens, in the order they are flipped
// through.
func cardPages(entries []knowledge.Entry) []page {
	var out []page
	for i, e := range entries {
		parts := pack([]string{renderEntry(e)}, "\n\n", maxMessageRunes)
		for j, text := range parts {
			out = append(out, page{entry: i, part: j, parts: len(parts), text: text})
		}
	}
	return out
}

// cardMessage shows a part of the card with the given ID, with the card's
// links and the arrows to the pages around it. ok is false if the card is
// not among the entries.
func cardMessage(c *survey.Category, entries []knowledge.Entry, id string, part int) (msg Message, ok bool) {
	all := cardPages(entries)
	i := slices.IndexFunc(all, func(p page) bool { return entries[p.entry].ID == id && p.part == part })
	if i < 0 {
		return Message{}, false
	}
	p := all[i]
	header := fmt.Sprintf("%s · %d из %d", c.Title, p.entry+1, len(entries))
	if p.parts > 1 {
		header += fmt.Sprintf(", часть %d из %d", p.part+1, p.parts)
	}
	to := func(pg page) string { return cardPayload(c, entries[pg.entry].ID, pg.part) }

	var nav []Button
	if i > 0 {
		nav = append(nav, Button{Text: labelBack, Payload: to(all[i-1])})
	}
	nav = append(nav, Button{Text: labelList, Payload: payload(actResults, c.ID)})
	if i < len(all)-1 {
		nav = append(nav, Button{Text: labelNext, Payload: to(all[i+1])})
	}
	return Message{
		Text:     header + "\n\n" + p.text,
		Markdown: true,
		Keyboard: append(linkButtons(entries[p.entry].Links), nav),
	}, true
}

func cardPayload(c *survey.Category, id string, part int) string {
	return payload(actCard, c.ID, strconv.Itoa(part), id)
}

// linkButtons puts the links of a card under it, a link a row.
func linkButtons(links []knowledge.Link) [][]Button {
	var rows [][]Button
	for _, l := range links {
		rows = append(rows, []Button{{Text: clip("🔗 " + l.Title), URL: l.URL}})
	}
	return rows
}

// clip cuts a text from the knowledge base to fit a button.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxButtonRunes {
		return s
	}
	return string([]rune(s)[:maxButtonRunes-1]) + "…"
}

// plural picks the form of a noun for n: 1 пункт, 3 пункта, 7 пунктов.
func plural(n int, one, few, many string) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return few
	}
	return many
}

// renderEntry is the text of a card. Its links are buttons: linkButtons.
func renderEntry(e knowledge.Entry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "**%s**\n%s", e.Title, strings.TrimSpace(e.Summary))
	if len(e.Steps) > 0 {
		sb.WriteString("\n\n**Как оформить:**")
		for i, step := range e.Steps {
			fmt.Fprintf(&sb, "\n%d. %s", i+1, strings.TrimSpace(step))
		}
	}
	if len(e.Documents) > 0 {
		sb.WriteString("\n\n**Документы:**")
		for _, doc := range e.Documents {
			fmt.Fprintf(&sb, "\n• %s", strings.TrimSpace(doc))
		}
	}
	if e.Where != "" {
		fmt.Fprintf(&sb, "\n\n**Куда идти:** %s", strings.TrimSpace(e.Where))
	}
	return sb.String()
}

// pack greedily joins blocks into texts of at most limit runes. A block that
// alone exceeds the limit is split by lines (and, as a last resort, by runes).
func pack(blocks []string, sep string, limit int) []string {
	var out []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if curLen > 0 {
			out = append(out, cur.String())
			cur.Reset()
			curLen = 0
		}
	}
	add := func(s string) {
		n := utf8.RuneCountInString(s)
		sepLen := utf8.RuneCountInString(sep)
		if curLen > 0 && curLen+sepLen+n > limit {
			flush()
		}
		if curLen > 0 {
			cur.WriteString(sep)
			curLen += sepLen
		}
		cur.WriteString(s)
		curLen += n
	}
	for _, block := range blocks {
		if utf8.RuneCountInString(block) <= limit {
			add(block)
			continue
		}
		flush()
		out = append(out, pack(splitLong(block, limit), "\n", limit)...)
	}
	flush()
	return out
}

func splitLong(block string, limit int) []string {
	var parts []string
	for line := range strings.SplitSeq(block, "\n") {
		for utf8.RuneCountInString(line) > limit {
			r := []rune(line)
			parts = append(parts, string(r[:limit]))
			line = string(r[limit:])
		}
		parts = append(parts, line)
	}
	return parts
}
