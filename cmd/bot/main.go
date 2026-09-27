// Command bot runs the "Навигатор студента" bot for the MAX messenger.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/KKolyasik/max-benefits/internal/bot"
	"github.com/KKolyasik/max-benefits/internal/bus"
	"github.com/KKolyasik/max-benefits/internal/dispatch"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/maxapi"
	"github.com/KKolyasik/max-benefits/internal/pgstore"
	"github.com/KKolyasik/max-benefits/internal/session"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

// handleTimeout bounds a single event, including the pauses required by the
// MAX rate limits when a long result is sent in several messages.
const handleTimeout = 20 * time.Second

// webhookReadTimeout bounds reading one webhook request. It is shorter than
// webhookStopTimeout, so every request finishes before the pool is closed.
const (
	webhookReadTimeout = 10 * time.Second
	webhookStopTimeout = 15 * time.Second
)

// MAX accepts webhook secrets of this form.
var webhookSecret = regexp.MustCompile(`^[\w-]{5,256}$`)

type config struct {
	Token         string
	APIURL        string
	CAFile        string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	SessionTTL    time.Duration
	SurveyFile    string
	KnowledgeFile string
	// DatabaseURL switches the knowledge base from the YAML file to
	// PostgreSQL, where admins publish the agent's drafts.
	DatabaseURL string
	AdminIDs    []int64
	// KafkaBrokers and SchemaRegistryURL connect the bot to the agent: it
	// takes the drafts and publishes the cards and the admins' decisions.
	KafkaBrokers      []string
	SchemaRegistryURL string
	Workers           int
	LogLevel          slog.Level
	LogJSON           bool
	// WebhookURL switches the bot from long polling to a webhook.
	WebhookURL    string
	WebhookPath   string
	WebhookSecret string
	HTTPAddr      string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sv, err := survey.Load(cfg.SurveyFile)
	if err != nil {
		return err
	}
	static, err := knowledge.LoadStatic(cfg.KnowledgeFile, sv)
	if err != nil {
		return err
	}
	var kb knowledge.Base = static
	var cards *pgstore.Store
	if cfg.DatabaseURL != "" {
		db, err := pgstore.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer db.Close()
		if cards, err = openCards(ctx, db, sv, static, cfg, log); err != nil {
			return err
		}
		kb = cards
	} else {
		log.Warn("DATABASE_URL is not set: cards come from the YAML file, admins can't review drafts")
	}

	store, closeStore, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeStore()

	client, err := maxapi.New(maxapi.Config{Token: cfg.Token, BaseURL: cfg.APIURL, CAFile: cfg.CAFile}, log)
	if err != nil {
		return err
	}
	name, err := client.Me(ctx)
	if err != nil {
		return fmt.Errorf("connect to MAX API (check MAX_BOT_TOKEN and MAX_CA_FILE): %w", err)
	}

	b := bot.New(sv, kb, store, client, log)
	if cards != nil {
		b.WithModeration(cards, cfg.AdminIDs)
		if len(cfg.KafkaBrokers) > 0 {
			// Stopped before the database closes: the bus works with it.
			busCtx, stopBus := context.WithCancel(ctx)
			agent, err := bus.Start(busCtx,
				bus.Config{Brokers: cfg.KafkaBrokers, RegistryURL: cfg.SchemaRegistryURL, Group: "max-benefits-bot"},
				cards, sv, b, log)
			if err != nil {
				stopBus()
				return err
			}
			defer func() {
				stopBus()
				agent.Wait()
			}()
			b.WithAgent(agent)
		} else {
			log.Warn("KAFKA_BROKERS is not set: the agent's drafts don't come, and admins can't run it")
		}
	}
	pool := dispatch.New(cfg.Workers, 64,
		func(ev bot.Event) int64 { return ev.UserID },
		func(ev bot.Event) {
			// Not derived from ctx: events already taken from MAX are
			// finished even during shutdown.
			ctx, cancel := context.WithTimeout(context.Background(), handleTimeout)
			defer cancel()
			if err := b.Handle(ctx, ev); err != nil {
				log.Error("handle event", "user", ev.UserID, "type", ev.Type, "err", err)
			}
		})

	submit := func(ev bot.Event) bool { return pool.Submit(ctx, ev) }
	var wait func() error
	if cfg.WebhookURL != "" {
		wait, err = startWebhook(ctx, cfg, client, submit)
	} else {
		wait, err = startPolling(ctx, client, submit)
	}
	if err == nil {
		log.Info("bot is running", "bot", name, "workers", cfg.Workers, "webhook", cfg.WebhookURL)
		err = wait()
	}

	log.Info("shutting down, finishing queued events")
	pool.Close()
	return err
}

// startWebhook serves MAX webhook requests on cfg.HTTPAddr and subscribes the
// bot to cfg.WebhookURL. The returned wait blocks until ctx is done and then
// stops the server, letting requests in flight hand over their events.
//
// The subscription is kept on shutdown: MAX redelivers updates for hours, so
// nothing sent during a restart is lost.
func startWebhook(ctx context.Context, cfg config, client *maxapi.Client, submit func(bot.Event) bool) (func() error, error) {
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("POST "+cfg.WebhookPath, client.Webhook(cfg.WebhookSecret, submit))
	srv := &http.Server{Handler: mux, ReadTimeout: webhookReadTimeout}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	// The server is up before MAX starts sending to it.
	if err := client.Subscribe(ctx, cfg.WebhookURL, cfg.WebhookSecret); err != nil {
		_ = srv.Close()
		return nil, err
	}
	return func() error {
		select {
		case err := <-served:
			return fmt.Errorf("webhook server: %w", err)
		case <-ctx.Done():
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), webhookStopTimeout)
		defer cancel()
		return srv.Shutdown(stopCtx)
	}, nil
}

// startPolling refuses to run while the bot has a webhook: MAX sends updates
// there, and a local copy with the production token would compete for them.
func startPolling(ctx context.Context, client *maxapi.Client, submit func(bot.Event) bool) (func() error, error) {
	hooks, err := client.Webhooks(ctx)
	if err != nil {
		return nil, err
	}
	if len(hooks) > 0 {
		return nil, fmt.Errorf("the bot gets updates by webhook %s: set WEBHOOK_URL, or run locally with a separate test bot",
			strings.Join(hooks, ", "))
	}
	return func() error {
		client.Poll(ctx, func(ev bot.Event) { submit(ev) })
		return nil
	}, nil
}

func openStore(ctx context.Context, cfg config, log *slog.Logger) (session.Store, func(), error) {
	if cfg.RedisAddr == "" {
		log.Warn("REDIS_ADDR is not set: sessions are kept in memory and will be lost on restart")
		return session.NewMemory(), func() {}, nil
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("connect to redis at %s: %w", cfg.RedisAddr, err)
	}
	return session.NewRedis(rdb, cfg.SessionTTL), func() { _ = rdb.Close() }, nil
}

func loadConfig() (config, error) {
	cfg := config{
		Token:             os.Getenv("MAX_BOT_TOKEN"),
		APIURL:            os.Getenv("MAX_API_URL"),
		CAFile:            os.Getenv("MAX_CA_FILE"),
		RedisAddr:         os.Getenv("REDIS_ADDR"),
		RedisPassword:     os.Getenv("REDIS_PASSWORD"),
		SurveyFile:        env("SURVEY_FILE", "data/survey.yaml"),
		KnowledgeFile:     env("KNOWLEDGE_FILE", "data/knowledge.yaml"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		KafkaBrokers:      splitList(os.Getenv("KAFKA_BROKERS")),
		SchemaRegistryURL: os.Getenv("SCHEMA_REGISTRY_URL"),
		LogJSON:           strings.EqualFold(os.Getenv("LOG_FORMAT"), "json"),
		WebhookURL:        os.Getenv("WEBHOOK_URL"),
		WebhookSecret:     os.Getenv("WEBHOOK_SECRET"),
		HTTPAddr:          env("HTTP_ADDR", ":8080"),
	}
	var errs []error
	if cfg.Token == "" {
		errs = append(errs, errors.New("MAX_BOT_TOKEN is required"))
	}
	var err error
	if cfg.RedisDB, err = strconv.Atoi(env("REDIS_DB", "0")); err != nil {
		errs = append(errs, fmt.Errorf("REDIS_DB: %w", err))
	}
	if cfg.SessionTTL, err = time.ParseDuration(env("SESSION_TTL", "4320h")); err != nil {
		errs = append(errs, fmt.Errorf("SESSION_TTL: %w", err))
	}
	if cfg.Workers, err = strconv.Atoi(env("WORKERS", "32")); err != nil || cfg.Workers < 1 {
		errs = append(errs, fmt.Errorf("WORKERS must be a positive integer, got %q", os.Getenv("WORKERS")))
	}
	if cfg.AdminIDs, err = parseIDs(os.Getenv("ADMIN_IDS")); err != nil {
		errs = append(errs, fmt.Errorf("ADMIN_IDS: %w", err))
	}
	switch {
	case (len(cfg.KafkaBrokers) > 0) != (cfg.SchemaRegistryURL != ""):
		errs = append(errs, errors.New("KAFKA_BROKERS and SCHEMA_REGISTRY_URL go together: set both or neither"))
	case len(cfg.KafkaBrokers) > 0 && cfg.DatabaseURL == "":
		errs = append(errs, errors.New("KAFKA_BROKERS needs DATABASE_URL: drafts and the outbox live in the database"))
	}
	if err = cfg.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if cfg.WebhookURL != "" {
		// MAX delivers only to https on the default port.
		if u, err := url.Parse(cfg.WebhookURL); err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" {
			errs = append(errs, fmt.Errorf("WEBHOOK_URL must be an https URL without a port, got %q", cfg.WebhookURL))
		} else {
			cfg.WebhookPath = cmp.Or(u.Path, "/")
		}
		if !webhookSecret.MatchString(cfg.WebhookSecret) {
			errs = append(errs, errors.New("WEBHOOK_SECRET must be 5-256 characters A-Z, a-z, 0-9, _ or -, e.g. from openssl rand -hex 32"))
		}
	}
	return cfg, errors.Join(errs...)
}

// openCards prepares the database: applies the migrations and, on the first
// start, imports the cards from the YAML file.
func openCards(ctx context.Context, db *pgxpool.Pool, sv *survey.Survey, static *knowledge.Static, cfg config, log *slog.Logger) (*pgstore.Store, error) {
	if err := pgstore.Migrate(ctx, db); err != nil {
		return nil, err
	}
	cards := pgstore.New(db, sv)
	n, err := cards.Seed(ctx, static.Cards(), "import from "+cfg.KnowledgeFile)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		log.Info("imported cards into the empty database", "count", n, "file", cfg.KnowledgeFile)
	}
	broken, err := cards.Broken(ctx)
	if err != nil {
		return nil, err
	}
	for id, errs := range broken {
		log.Warn("card doesn't match the survey and is hidden", "card", id, "err", errors.Join(errs...))
	}
	if len(cfg.AdminIDs) == 0 {
		log.Warn("ADMIN_IDS is not set: nobody can review drafts")
	}
	return cards, nil
}

// parseIDs reads MAX user IDs separated by commas or spaces.
func parseIDs(s string) ([]int64, error) {
	var ids []int64
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a user ID", f)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// splitList splits a comma-separated list, dropping the blanks.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newLogger(cfg config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
