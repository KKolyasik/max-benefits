package bot

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// MAX rejects messages longer than 4000 characters; keep a margin for markup.
const maxMessageRunes = 3500

// Messages with callback buttons are plain text on purpose: when a button is
// pressed the bot "freezes" the message by re-sending its plain text from the
// update, and markdown would be lost at that point anyway.

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

	labelMenu        = "⬅️ В меню"
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

// questionMessage renders a question. preface is shown above it, e.g. the
// category intro before the first question.
func (b *Bot) questionMessage(c *survey.Category, q *survey.Question, pos int, selected []string, preface string) Message {
	var sb strings.Builder
	if preface != "" {
		sb.WriteString(preface)
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(&sb, "Вопрос %d из %d\n%s", pos, len(c.Questions), q.Text)
	if q.Multi {
		sb.WriteString("\n\nМожно выбрать несколько вариантов, потом нажми «Готово».")
	}

	buttons := make([]Button, 0, len(q.Options))
	for _, o := range q.Options {
		if q.Multi {
			mark := "▫️ "
			if slices.Contains(selected, o.ID) {
				mark = "✅ "
			}
			buttons = append(buttons, Button{Text: mark + o.Title, Payload: payload(actToggle, q.ID, o.ID)})
		} else {
			buttons = append(buttons, Button{Text: o.Title, Payload: payload(actAnswer, q.ID, o.ID)})
		}
	}
	kb := layout(buttons)
	if q.Multi {
		kb = append(kb, []Button{{Text: labelDone, Payload: payload(actDone, q.ID)}})
	}
	kb = append(kb, []Button{{Text: labelMenu, Payload: actMenu}})
	return Message{Text: sb.String(), Keyboard: kb}
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
			{Text: labelCancel, Payload: actMenu},
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

// resultMessages renders the found entries, splitting them into several
// messages when they do not fit into one, and ends with a "what next" menu.
func resultMessages(c *survey.Category, entries []knowledge.Entry) []Message {
	next := Message{
		Text: textDisclaimer + "\n\nЧто дальше?",
		Keyboard: [][]Button{
			{{Text: labelOtherTopics, Payload: actMenu}},
			{{Text: labelEditAnswers, Payload: payload(actRedo, c.ID)}},
		},
	}
	if len(entries) == 0 {
		next.Text = textNothingFound + "\n\nЧто дальше?"
		return []Message{next}
	}

	header := fmt.Sprintf("**%s: подборка для тебя**\nНашёл пунктов: %d. В каждом: что это, как оформить и куда идти.", c.Title, len(entries))
	blocks := []string{header}
	for _, e := range entries {
		blocks = append(blocks, renderEntry(e))
	}
	var out []Message
	for _, text := range pack(blocks, "\n\n", maxMessageRunes) {
		out = append(out, Message{Text: text, Markdown: true})
	}
	return append(out, next)
}

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
	if len(e.Links) > 0 {
		links := make([]string, 0, len(e.Links))
		for _, l := range e.Links {
			links = append(links, fmt.Sprintf("[%s](%s)", l.Title, l.URL))
		}
		sb.WriteString("\n\n🔗 " + strings.Join(links, " · "))
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
