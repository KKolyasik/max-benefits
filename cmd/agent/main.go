// Command agent keeps the knowledge base fresh: it searches the web by the
// queries from data/queries.yaml, reads the found pages and asks a language
// model to draft cards. The drafts go to the bot through Kafka, or into
// drafts/ when Kafka is not set up:
//
//	go run ./cmd/agent collect
//
// Admins review every draft in the bot before it reaches the students.
// Settings come from environment variables or a .env file, see README.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
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

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"

	"github.com/KKolyasik/max-benefits/contract"
	"github.com/KKolyasik/max-benefits/internal/agent"
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

const usage = `usage:
  agent collect [-force]   search, read pages and draft cards`

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
	// KafkaBrokers and SchemaRegistryURL send drafts to the bot; without
	// them drafts go into DraftsDir.
	KafkaBrokers      []string
	SchemaRegistryURL string
	// CAFile is an extra root certificate for reading sites: many Russian
	// official sites use the Russian Trusted Root CA.
	CAFile   string
	LogLevel slog.Level
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
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func collect(ctx context.Context, cfg config, force bool) error {
	if err := cfg.checkCollect(); err != nil {
		return err
	}
	log := newLogger(cfg)
	sv, base, err := loadBase(cfg)
	if err != nil {
		return err
	}
	queries, err := agent.LoadQueries(cfg.QueriesFile, sv)
	if err != nil {
		return err
	}
	state, err := agent.LoadState(cfg.StateFile)
	if err != nil {
		return err
	}

	roots, err := certPool(cfg.CAFile)
	if err != nil {
		return err
	}
	// Found sites often send incomplete certificate chains; APIs don't.
	web := fetch.NewClient(roots, 30*time.Second)
	api := apiClient(roots, 30*time.Second)
	// A local model may think for minutes over a long prompt.
	modelHTTP := apiClient(roots, 10*time.Minute)
	var searcher agent.Searcher = &search.SearXNG{URL: cfg.SearXNGURL, HTTP: api}
	if cfg.SearchProvider == "yandex" {
		searcher = &search.Yandex{APIKey: cfg.YandexAPIKey, FolderID: cfg.YandexFolderID, Region: cfg.YandexRegion, HTTP: api}
	}
	pdftotext, err := exec.LookPath("pdftotext")
	if err != nil {
		log.Warn("pdftotext is not installed: PDF documents are read only by their search snippets (install poppler-utils)")
	}

	// A local model takes one request at a time. In the asynchronous mode a
	// request waits in a queue for seconds or hours, so queries wait side by
	// side.
	var model agent.Model = &llm.Client{
		BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel, Project: cfg.LLMProject, HTTP: modelHTTP,
	}
	parallel, mode := 1, cfg.LLMBaseURL
	if cfg.YandexAsync {
		model = &llm.Async{APIKey: cfg.LLMAPIKey, Folder: cfg.LLMProject, Model: cfg.LLMModel, HTTP: api}
		parallel, mode = 8, "yandex async"
	}

	// Before anything costs money: without Kafka the drafts would be lost.
	sink, closeSink, err := openSink(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeSink()

	log.Info("collecting", "queries", len(queries), "search", cfg.SearchProvider, "model", cfg.LLMModel, "mode", mode,
		"token_limit", cfg.TokenLimit, "kafka", len(cfg.KafkaBrokers) > 0)
	c := &agent.Collector{
		Survey:     sv,
		Base:       base,
		Queries:    queries,
		Search:     searcher,
		Fetch:      &fetch.Fetcher{HTTP: web, MaxChars: 5000, PDFToText: pdftotext},
		Model:      model,
		Drafts:     sink,
		State:      state,
		Force:      force,
		Parallel:   parallel,
		TokenLimit: cfg.TokenLimit,
		Log:        log,
	}
	report, err := c.Run(ctx)
	fmt.Printf("\nЗапросов: %d, без изменений: %d, с ошибкой: %d, отложено: %d, новых черновиков: %d.\n",
		report.Queries, report.Unchanged, report.Failed, report.Deferred, len(report.Drafts))
	fmt.Printf("Токены модели: %d на входе, %d на выходе.\n", report.InputTokens, report.OutputTokens)
	switch {
	case len(report.Drafts) == 0:
	case len(cfg.KafkaBrokers) > 0:
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
	if err := cfg.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return cfg, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	return cfg, nil
}

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

// openSink sends drafts to the bot through Kafka: it makes sure the topics
// and the schemas are there, so a broken Kafka stops the run before it
// costs anything. Without Kafka the drafts go into files.
func openSink(ctx context.Context, cfg config) (agent.Sink, func(), error) {
	if len(cfg.KafkaBrokers) == 0 {
		return agent.Drafts{Dir: cfg.DraftsDir}, func() {}, nil
	}
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
	return &agent.KafkaSink{Client: cl, Codec: codec, RunID: runID()}, cl.Close, nil
}

// runID names a run by its start and a few random bytes.
func runID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
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
