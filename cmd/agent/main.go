// Command agent keeps the knowledge base fresh: it searches the web by the
// queries from data/queries.yaml, reads the found pages and asks a language
// model to draft cards. Admins review every draft in the bot before it
// reaches the students.
//
// As a service it runs on a schedule and when an admin presses the button in
// the bot, and tells the admins how each run went; it talks to the bot
// through Kafka:
//
//	go run ./cmd/agent serve
//
// It can also run once from the command line. Without Kafka the drafts go
// into drafts/, and the base comes from data/ instead of the bot:
//
//	go run ./cmd/agent collect
//
// Settings come from environment variables or a .env file, see README.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/agent"
	"github.com/KKolyasik/max-benefits/internal/agent/bus"
	"github.com/KKolyasik/max-benefits/internal/agent/fetch"
	"github.com/KKolyasik/max-benefits/internal/agent/llm"
	"github.com/KKolyasik/max-benefits/internal/agent/search"
	"github.com/KKolyasik/max-benefits/internal/knowledge"
	"github.com/KKolyasik/max-benefits/internal/survey"
)

const yandexLLMURL = "https://ai.api.cloud.yandex.net/v1"

// defaultTokenLimit caps the model tokens of a run. A run of all 13 queries
// takes 150–200 thousand; the limit guards against a bloated query list or a
// model that thinks too long, and costs at most about 125 ₽ on YandexGPT Pro
// 5.1 in the asynchronous mode.
const defaultTokenLimit = 300_000

// defaultSchedule runs the agent on Mondays at 3 a.m.: sites change with the
// academic calendar, not daily, admins review a week's drafts at once, and
// the search costs less at night.
const defaultSchedule = "0 3 * * 1"

const usage = `usage:
  agent serve              run on the schedule and on the admins' command (needs Kafka)
  agent collect [-force]   run once: search, read pages and draft cards`

type config struct {
	SurveyFile    string
	KnowledgeFile string
	QueriesFile   string
	DraftsDir     string
	StateFile     string
	// SearchProvider is "yandex" or "searxng".
	SearchProvider string
	SearXNGURL     string
	YandexAPIKey   string
	YandexFolderID string
	// YandexRegion is the search region: 2 is Saint Petersburg.
	YandexRegion int
	LLMBaseURL   string
	LLMAPIKey    string
	LLMModel     string
	LLMProject   string
	// YandexAsync sends requests to a Yandex model in the asynchronous
	// mode: slower, but half the price.
	YandexAsync bool
	// TokenLimit caps the model tokens of a run; 0 means no limit.
	TokenLimit int
	// KafkaBrokers and SchemaRegistryURL connect the agent to the bot: the
	// base comes from it, the drafts and the reports go to it. Without them
	// the base comes from the files, and the drafts go into DraftsDir.
	KafkaBrokers      []string
	SchemaRegistryURL string
	// Schedule is when the service runs by itself, in cron syntax and local
	// time; nil means only on the admins' command.
	Schedule     cron.Schedule
	ScheduleSpec string
	// CAFile is an extra root certificate for reading sites: many Russian
	// official sites use the Russian Trusted Root CA.
	CAFile   string
	LogLevel slog.Level
	LogJSON  bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	if err := loadDotEnv(".env"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "collect":
		fs := flag.NewFlagSet("collect", flag.ContinueOnError)
		force := fs.Bool("force", false, "send pages to the model even if they did not change since the last run")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return collect(ctx, cfg, *force)
	case "serve":
		return serve(ctx, cfg)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

// serve runs the agent as a service until it is stopped.
func serve(ctx context.Context, cfg config) error {
	if len(cfg.KafkaBrokers) == 0 {
		return errors.New("the service takes the admins' commands and the base through Kafka: set KAFKA_BROKERS and SCHEMA_REGISTRY_URL")
	}
	a, err := newApp(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.close()
	a.log.Info("serving", "schedule", cfg.ScheduleSpec)
	return bus.Serve(ctx, bus.Config{Brokers: cfg.KafkaBrokers, Group: "max-benefits-agent", Schedule: cfg.Schedule}, a.runner(), a.log)
}

// collect runs the agent once. With Kafka the run is like one from the
// bot: the base comes from the bot, and the admins get the drafts and the
// report.
func collect(ctx context.Context, cfg config, force bool) error {
	a, err := newApp(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.close()
	var report agent.Report
	if a.kafka != nil {
		report, err = a.runner().Do(ctx, contract.RunTriggerCLI, "", force)
	} else {
		report, err = a.run(ctx, "", force)
	}
	fmt.Printf("\nЗапросов: %d, без изменений: %d, с ошибкой: %d, отложено: %d, новых черновиков: %d.\n",
		report.Queries, report.Unchanged, report.Failed, report.Deferred, len(report.Drafts))
	fmt.Printf("Токены модели: %d на входе, %d на выходе.\n", report.InputTokens, report.OutputTokens)
	switch {
	case len(report.Drafts) == 0:
	case a.kafka != nil:
		fmt.Println("Черновики ушли боту через Kafka: админы увидят их в чате.")
	default:
		fmt.Println("Черновики лежат в " + cfg.DraftsDir + ": без Kafka бот их не увидит.")
	}
	if err != nil {
		return err
	}
	if report.Failed > 0 {
		return fmt.Errorf("%d queries failed, see the log", report.Failed)
	}
	if report.Deferred > 0 {
		return fmt.Errorf("the run spent its limit of %d tokens (LLM_TOKEN_LIMIT): %d queries wait for the next run",
			cfg.TokenLimit, report.Deferred)
	}
	return nil
}

// app is what the runs of the agent share.
type app struct {
	cfg      config
	log      *slog.Logger
	state    *agent.State
	search   agent.Searcher
	fetch    agent.Fetcher
	model    agent.Model
	parallel int
	// kafka and codec are set with Kafka: the base comes from the bot, and
	// the drafts and the reports go to it.
	kafka *kgo.Client
	codec *contract.Codec
}

// newApp checks the settings and sets up the search, the model and Kafka.
// A broken Kafka fails here, before a run costs anything.
func newApp(ctx context.Context, cfg config) (*app, error) {
	if err := cfg.checkCollect(); err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, log: newLogger(cfg)}
	var err error
	if a.state, err = agent.LoadState(cfg.StateFile); err != nil {
		return nil, err
	}
	roots, err := certPool(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	// Found sites often send incomplete certificate chains; APIs don't.
	web := fetch.NewClient(roots, 30*time.Second)
	api := apiClient(roots, 30*time.Second)
	// A local model may think for minutes over a long prompt.
	modelHTTP := apiClient(roots, 10*time.Minute)
	a.search = &search.SearXNG{URL: cfg.SearXNGURL, HTTP: api}
	if cfg.SearchProvider == "yandex" {
		a.search = &search.Yandex{APIKey: cfg.YandexAPIKey, FolderID: cfg.YandexFolderID, Region: cfg.YandexRegion, HTTP: api}
	}
	pdftotext, err := exec.LookPath("pdftotext")
	if err != nil {
		a.log.Warn("pdftotext is not installed: PDF documents are read only by their search snippets (install poppler-utils)")
	}
	a.fetch = &fetch.Fetcher{HTTP: web, MaxChars: 5000, PDFToText: pdftotext}

	// A local model takes one request at a time. In the asynchronous mode a
	// request waits in a queue for seconds or hours, so queries wait side by
	// side.
	a.model = &llm.Client{
		BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel, Project: cfg.LLMProject, HTTP: modelHTTP,
	}
	a.parallel = 1
	mode := cfg.LLMBaseURL
	if cfg.YandexAsync {
		a.model = &llm.Async{APIKey: cfg.LLMAPIKey, Folder: cfg.LLMProject, Model: cfg.LLMModel, HTTP: api}
		a.parallel, mode = 8, "yandex async"
	}

	if len(cfg.KafkaBrokers) > 0 {
		if a.kafka, a.codec, err = connect(ctx, cfg); err != nil {
			return nil, err
		}
	}
	a.log.Info("agent settings", "search", cfg.SearchProvider, "model", cfg.LLMModel, "mode", mode,
		"token_limit", cfg.TokenLimit, "kafka", a.kafka != nil)
	return a, nil
}

func (a *app) close() {
	if a.kafka != nil {
		a.kafka.Close()
	}
}

// runner runs the agent with reports to the bot; it needs Kafka.
func (a *app) runner() *bus.Runner {
	return &bus.Runner{Kafka: a.kafka, Codec: a.codec, Run: a.run, Log: a.log}
}

// run does one run: it reads the base and the queries anew, so a run sees
// the cards admins approved since the last one.
func (a *app) run(ctx context.Context, runID string, force bool) (agent.Report, error) {
	base, err := a.base(ctx)
	if err != nil {
		return agent.Report{}, err
	}
	queries, err := agent.LoadQueries(a.cfg.QueriesFile, base.Survey)
	if err != nil {
		return agent.Report{}, err
	}
	var sink agent.Sink = agent.Drafts{Dir: a.cfg.DraftsDir}
	log := a.log
	if a.kafka != nil {
		sink = &bus.KafkaSink{Client: a.kafka, Codec: a.codec, RunID: runID}
		log = log.With("run", runID)
	}
	log.Info("collecting", "queries", len(queries), "cards", len(base.Cards), "force", force)
	c := &agent.Collector{
		Survey:     base.Survey,
		Base:       base.Cards,
		Rejected:   base.Rejected,
		Queries:    queries,
		Search:     a.search,
		Fetch:      a.fetch,
		Model:      a.model,
		Drafts:     sink,
		State:      a.state,
		Force:      force,
		Parallel:   a.parallel,
		TokenLimit: a.cfg.TokenLimit,
		Log:        log,
	}
	return c.Run(ctx)
}

// base reads the survey and the cards: from the bot through Kafka, or from
// the files without it.
func (a *app) base(ctx context.Context) (bus.Base, error) {
	if a.kafka == nil {
		sv, cards, err := loadBase(a.cfg)
		return bus.Base{Survey: sv, Cards: cards}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return bus.ReadBase(ctx, a.cfg.KafkaBrokers, a.codec)
}

// loadBase reads the survey and the cards in file order, and fails if the
// base itself is broken.
func loadBase(cfg config) (*survey.Survey, []knowledge.Card, error) {
	sv, err := survey.Load(cfg.SurveyFile)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(cfg.KnowledgeFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read knowledge base: %w", err)
	}
	cards, err := knowledge.ParseCards(data)
	if err == nil {
		err = knowledge.Validate(cards, sv)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("knowledge base %s: %w", cfg.KnowledgeFile, err)
	}
	return sv, cards, nil
}

func loadConfig() (config, error) {
	cfg := config{
		SurveyFile:        env("SURVEY_FILE", "data/survey.yaml"),
		KnowledgeFile:     env("KNOWLEDGE_FILE", "data/knowledge.yaml"),
		QueriesFile:       env("QUERIES_FILE", "data/queries.yaml"),
		DraftsDir:         env("DRAFTS_DIR", "drafts"),
		SearXNGURL:        env("SEARXNG_URL", "http://localhost:8888"),
		YandexAPIKey:      os.Getenv("YANDEX_API_KEY"),
		YandexFolderID:    os.Getenv("YANDEX_FOLDER_ID"),
		YandexRegion:      2,
		LLMBaseURL:        env("LLM_BASE_URL", yandexLLMURL),
		CAFile:            env("CA_FILE", "certs/russian_trusted_root_ca.pem"),
		KafkaBrokers:      splitList(os.Getenv("KAFKA_BROKERS")),
		SchemaRegistryURL: os.Getenv("SCHEMA_REGISTRY_URL"),
	}
	cfg.StateFile = env("STATE_FILE", cfg.DraftsDir+"/.seen.json")
	// Without a Yandex key the agent still works, on the free search.
	defaultSearch := "searxng"
	if cfg.YandexAPIKey != "" {
		defaultSearch = "yandex"
	}
	cfg.SearchProvider = strings.ToLower(env("SEARCH_PROVIDER", defaultSearch))

	onYandex := strings.Contains(cfg.LLMBaseURL, "api.cloud.yandex.net")
	if onYandex {
		cfg.LLMAPIKey = env("LLM_API_KEY", cfg.YandexAPIKey)
		cfg.LLMProject = env("LLM_PROJECT", cfg.YandexFolderID)
		if cfg.YandexFolderID != "" {
			cfg.LLMModel = env("LLM_MODEL", "gpt://"+cfg.YandexFolderID+"/yandexgpt-5.1")
		} else {
			cfg.LLMModel = os.Getenv("LLM_MODEL")
		}
	} else {
		// A local model ignores the key, but the header must be there.
		cfg.LLMAPIKey = env("LLM_API_KEY", "local")
		cfg.LLMProject = os.Getenv("LLM_PROJECT")
		cfg.LLMModel = os.Getenv("LLM_MODEL")
	}
	var err error
	if cfg.YandexAsync, err = strconv.ParseBool(env("YANDEX_ASYNC", "true")); err != nil {
		return cfg, fmt.Errorf("YANDEX_ASYNC: %w", err)
	}
	cfg.YandexAsync = cfg.YandexAsync && onYandex
	if cfg.TokenLimit, err = strconv.Atoi(env("LLM_TOKEN_LIMIT", strconv.Itoa(defaultTokenLimit))); err != nil || cfg.TokenLimit < 0 {
		return cfg, fmt.Errorf("LLM_TOKEN_LIMIT must be a number of tokens, 0 for no limit: %q", os.Getenv("LLM_TOKEN_LIMIT"))
	}
	if (len(cfg.KafkaBrokers) > 0) != (cfg.SchemaRegistryURL != "") {
		return cfg, errors.New("KAFKA_BROKERS and SCHEMA_REGISTRY_URL go together: set both or neither")
	}
	if cfg.ScheduleSpec = env("AGENT_SCHEDULE", defaultSchedule); !strings.EqualFold(cfg.ScheduleSpec, "off") {
		spec, err := cron.ParseStandard(cfg.ScheduleSpec)
		if err != nil {
			return cfg, fmt.Errorf("AGENT_SCHEDULE must be in cron syntax, e.g. %q, or off: %w", defaultSchedule, err)
		}
		cfg.Schedule = localTime{spec}
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return cfg, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	cfg.LogJSON = strings.EqualFold(os.Getenv("LOG_FORMAT"), "json")
	return cfg, nil
}

// localTime reads a cron schedule in local time. By itself cron reads a
// schedule without CRON_TZ in the time zone of the moment it is asked
// about, and the start of the latest run comes from Kafka in UTC.
type localTime struct{ cron.Schedule }

func (s localTime) Next(t time.Time) time.Time { return s.Schedule.Next(t.In(time.Local)) }

func (cfg config) checkCollect() error {
	var errs []error
	switch cfg.SearchProvider {
	case "yandex":
		if cfg.YandexAPIKey == "" || cfg.YandexFolderID == "" {
			errs = append(errs, errors.New("search in Yandex needs YANDEX_API_KEY and YANDEX_FOLDER_ID; for the free search set SEARCH_PROVIDER=searxng"))
		}
	case "searxng":
	default:
		errs = append(errs, fmt.Errorf("SEARCH_PROVIDER must be yandex or searxng, got %q", cfg.SearchProvider))
	}
	if cfg.LLMModel == "" || cfg.LLMAPIKey == "" {
		errs = append(errs, errors.New("no model configured: set YANDEX_API_KEY and YANDEX_FOLDER_ID for AI Studio, or LLM_BASE_URL and LLM_MODEL for your own model (e.g. Ollama)"))
	}
	return errors.Join(errs...)
}

// certPool returns the system roots plus the extra CA file, if it exists.
func certPool(caFile string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("read CA file: %w", err)
		case !pool.AppendCertsFromPEM(data):
			return nil, fmt.Errorf("no certificates found in CA file %s", caFile)
		}
	}
	return pool, nil
}

func apiClient(roots *x509.CertPool, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// loadDotEnv sets variables from a .env file for local runs. Variables
// already set in the environment win, as with docker compose.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if i := strings.Index(value, " #"); i >= 0 && !strings.HasPrefix(value, `"`) {
			value = strings.TrimSpace(value[:i])
		}
		value = strings.Trim(value, `"'`)
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newLogger(cfg config) *slog.Logger {
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.LogLevel,
		// Timestamps only clutter an interactive run; docker adds its own.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// connect makes sure the topics and the schemas are there.
func connect(ctx context.Context, cfg config) (*kgo.Client, *contract.Codec, error) {
	// Without a delivery timeout a draft would wait for a dead Kafka forever.
	cl, err := kgo.NewClient(kgo.SeedBrokers(cfg.KafkaBrokers...), kgo.RecordDeliveryTimeout(30*time.Second))
	if err != nil {
		return nil, nil, fmt.Errorf("kafka: %w", err)
	}
	reg, err := sr.NewClient(sr.URLs(cfg.SchemaRegistryURL))
	if err != nil {
		cl.Close()
		return nil, nil, fmt.Errorf("schema registry: %w", err)
	}
	codec := contract.NewCodec(reg)
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := contract.EnsureTopics(setup, kadm.NewClient(cl), contract.Topics...); err != nil {
		cl.Close()
		return nil, nil, fmt.Errorf("kafka: %w", err)
	}
	if err := codec.Register(setup); err != nil {
		cl.Close()
		return nil, nil, fmt.Errorf("schema registry: %w", err)
	}
	return cl, codec, nil
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
