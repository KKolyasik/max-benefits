// Command cli lets you talk to the bot in a terminal, without MAX and without
// a token. Handy for checking the questionnaire and the knowledge base:
//
//	go run ./cmd/cli
//
// With a database it also takes feedback and shows the admin screens, e.g.
// to review the agent's drafts and the feedback and to run the agent
// through Kafka:
//
//	go run ./cmd/cli -db postgres://... -admin -kafka localhost:9092 -registry http://localhost:8085
//
// Type a button number to press it, or any text to send a message.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/KKolyasik/max-benefits/internal/bot"
	"github.com/KKolyasik/max-benefits/internal/bus"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/pgstore"
	"github.com/KKolyasik/max-benefits/internal/session"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

const userID = 1

type options struct {
	surveyFile, kbFile string
	// db is a PostgreSQL URL; empty means the YAML file only.
	db string
	// admin gives the console user admin rights.
	admin bool
	// kafka and registry connect to the agent: its drafts come in as they
	// are sent, and approved cards go out.
	kafka, registry string
}

func main() {
	var o options
	flag.StringVar(&o.surveyFile, "survey", "data/survey.yaml", "questionnaire file")
	flag.StringVar(&o.kbFile, "knowledge", "data/knowledge.yaml", "knowledge base file; imported into an empty database")
	flag.StringVar(&o.db, "db", os.Getenv("DATABASE_URL"), "PostgreSQL URL (default $DATABASE_URL)")
	flag.BoolVar(&o.admin, "admin", false, "be an admin: review drafts (needs -db)")
	flag.StringVar(&o.kafka, "kafka", os.Getenv("KAFKA_BROKERS"), "Kafka brokers, comma-separated (default $KAFKA_BROKERS; needs -db)")
	flag.StringVar(&o.registry, "registry", os.Getenv("SCHEMA_REGISTRY_URL"), "Schema Registry URL (default $SCHEMA_REGISTRY_URL)")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	ctx := context.Background()
	sv, err := survey.Load(o.surveyFile)
	if err != nil {
		return err
	}
	static, err := knowledge.LoadStatic(o.kbFile, sv)
	if err != nil {
		return err
	}
	var kb knowledge.Base = static
	var store *pgstore.Store
	if o.db != "" {
		db, err := pgstore.Connect(ctx, o.db)
		if err != nil {
			return err
		}
		defer db.Close()
		if err := pgstore.Migrate(ctx, db); err != nil {
			return err
		}
		store = pgstore.New(db, sv)
		if n, err := store.Seed(ctx, static.Cards(), "import from "+o.kbFile); err != nil {
			return err
		} else if n > 0 {
			fmt.Println(dim(fmt.Sprintf("В пустую базу импортировано карточек: %d.", n)))
		}
		kb = store
	} else if o.admin || o.kafka != "" {
		return errors.New("-admin and -kafka need a database: set -db or DATABASE_URL")
	}

	term := &console{}
	b := bot.New(sv, kb, session.NewMemory(), term, slog.New(slog.DiscardHandler))
	if store != nil {
		b.WithFeedback(store)
	}
	if o.admin {
		b.WithModeration(store, []int64{userID})
	}
	if o.kafka != "" {
		if o.registry == "" {
			return errors.New("-kafka needs -registry or SCHEMA_REGISTRY_URL")
		}
		// A consumer group of its own: a bot running alongside gets every
		// draft and report too.
		busCtx, stopBus := context.WithCancel(ctx)
		agent, err := bus.Start(busCtx, bus.Config{Brokers: strings.Split(o.kafka, ","), RegistryURL: o.registry, Group: "max-benefits-cli"},
			store, sv, b, slog.New(slog.DiscardHandler))
		if err != nil {
			stopBus()
			return err
		}
		defer func() {
			stopBus()
			agent.Wait()
		}()
		b.WithAgent(agent)
		fmt.Println(dim("Kafka подключена: черновики и отчёты агента придут сами, а кнопка «Агент» запустит его."))
	}

	fmt.Println(dim("Номер — нажать кнопку, текст — отправить сообщение, пустая строка или Ctrl+D — выход.\n"))
	if err := b.Handle(ctx, bot.Event{Type: bot.EventStart, UserID: userID}); err != nil {
		return err
	}

	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(dim("> "))
		if !in.Scan() {
			return in.Err()
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			return nil
		}
		ev := bot.Event{Type: bot.EventText, UserID: userID, Text: line}
		if n, err := strconv.Atoi(line); err == nil {
			btn, ok := term.button(n)
			if !ok {
				fmt.Println(dim("Нет кнопки с таким номером."))
				continue
			}
			if btn.URL != "" {
				fmt.Println(dim("Ссылка: " + btn.URL))
				continue
			}
			ev = bot.Event{
				Type:       bot.EventCallback,
				UserID:     userID,
				CallbackID: "cli",
				Payload:    btn.Payload,
				SourceText: term.keyboardText,
			}
		}
		if err := b.Handle(ctx, ev); err != nil {
			fmt.Println(dim("ошибка: " + err.Error()))
		}
	}
}

// console prints bot messages and remembers the latest keyboard, which is
// the one the user can press.
type console struct {
	keyboard     [][]bot.Button
	keyboardText string
}

func (c *console) Send(_ context.Context, _ int64, msg bot.Message) error {
	c.print(msg)
	return nil
}

func (c *console) AnswerCallback(_ context.Context, _ int64, _ string, a bot.CallbackAnswer) error {
	if a.Notification != "" {
		fmt.Println(dim("🔔 " + a.Notification))
	}
	if a.Edit != nil {
		if len(a.Edit.Keyboard) > 0 {
			c.print(*a.Edit)
		} else {
			// A frozen message: only the chosen option is new.
			if i := strings.LastIndex(a.Edit.Text, "👉"); i >= 0 {
				fmt.Println(dim(a.Edit.Text[i:]))
			}
			c.keyboard = nil
		}
	}
	return nil
}

func (c *console) print(msg bot.Message) {
	text := msg.Text
	if msg.Markdown {
		text = renderMarkdown(text)
	}
	fmt.Println("\n" + text)
	if len(msg.Keyboard) == 0 {
		return
	}
	c.keyboard, c.keyboardText = msg.Keyboard, msg.Text
	n := 0
	for _, row := range msg.Keyboard {
		var cells []string
		for _, b := range row {
			n++
			cells = append(cells, fmt.Sprintf("[%d] %s", n, b.Text))
		}
		fmt.Println("  " + strings.Join(cells, "   "))
	}
}

func (c *console) button(n int) (bot.Button, bool) {
	for _, row := range c.keyboard {
		for _, b := range row {
			n--
			if n == 0 {
				return b, true
			}
		}
	}
	return bot.Button{}, false
}

var (
	boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)
	linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

func renderMarkdown(s string) string {
	// Links first: the escape codes of bold have brackets of their own.
	s = linkRe.ReplaceAllString(s, "$1 \033[4m$2\033[0m")
	return boldRe.ReplaceAllString(s, "\033[1m$1\033[0m")
}

func dim(s string) string { return "\033[2m" + s + "\033[0m" }
