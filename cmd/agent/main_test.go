package main

import (
	"os"
	"path/filepath"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"YANDEX_API_KEY", "YANDEX_FOLDER_ID", "SEARCH_PROVIDER", "LLM_BASE_URL",
		"LLM_API_KEY", "LLM_MODEL", "LLM_PROJECT", "DRAFTS_DIR", "STATE_FILE", "YANDEX_ASYNC", "LLM_TOKEN_LIMIT", "KAFKA_BROKERS", "SCHEMA_REGISTRY_URL"} {
		t.Setenv(k, "")
	}
}

func TestConfigWithoutKeysUsesFreeSearchAndOwnModel(t *testing.T) {
	clearEnv(t)
	t.Setenv("LLM_BASE_URL", "http://localhost:11434/v1")
	t.Setenv("LLM_MODEL", "qwen3:14b")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SearchProvider != "searxng" || cfg.LLMAPIKey != "local" || cfg.LLMProject != "" || cfg.StateFile != "drafts/.seen.json" ||
		cfg.YandexAsync {
		t.Errorf("config %+v", cfg)
	}
	if err := cfg.checkCollect(); err != nil {
		t.Error(err)
	}
}

func TestConfigWithYandexKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("YANDEX_API_KEY", "key")
	t.Setenv("YANDEX_FOLDER_ID", "folder")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SearchProvider != "yandex" || cfg.LLMAPIKey != "key" || cfg.LLMProject != "folder" ||
		cfg.LLMModel != "gpt://folder/yandexgpt-5.1" || !cfg.YandexAsync || cfg.TokenLimit != defaultTokenLimit {
		t.Errorf("config %+v", cfg)
	}
	if err := cfg.checkCollect(); err != nil {
		t.Error(err)
	}
}

func TestConfigModeAndLimit(t *testing.T) {
	clearEnv(t)
	t.Setenv("YANDEX_API_KEY", "key")
	t.Setenv("YANDEX_FOLDER_ID", "folder")
	t.Setenv("YANDEX_ASYNC", "false")
	t.Setenv("LLM_TOKEN_LIMIT", "0")
	cfg, err := loadConfig()
	if err != nil || cfg.YandexAsync || cfg.TokenLimit != 0 {
		t.Errorf("config %+v, err %v", cfg, err)
	}

	for _, bad := range []string{"много", "-5"} {
		t.Setenv("LLM_TOKEN_LIMIT", bad)
		if _, err := loadConfig(); err == nil {
			t.Errorf("LLM_TOKEN_LIMIT=%s must be an error", bad)
		}
	}
}

func TestConfigNeedsAModel(t *testing.T) {
	clearEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.checkCollect(); err == nil {
		t.Error("expected an error without any model")
	}
}

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\nexport LLM_MODEL=\"qwen3:14b\"\nLLM_BASE_URL=http://pc:11434/v1   # my PC\nYANDEX_API_KEY=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	clearEnv(t)
	t.Setenv("YANDEX_API_KEY", "from-env")
	// t.Setenv("", …) sets an empty value, which counts as set; unset these
	// two so the file can fill them in.
	for _, k := range []string{"LLM_MODEL", "LLM_BASE_URL"} {
		_ = os.Unsetenv(k)
	}

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LLM_MODEL"); got != "qwen3:14b" {
		t.Errorf("LLM_MODEL=%q", got)
	}
	if got := os.Getenv("LLM_BASE_URL"); got != "http://pc:11434/v1" {
		t.Errorf("LLM_BASE_URL=%q", got)
	}
	if got := os.Getenv("YANDEX_API_KEY"); got != "from-env" {
		t.Errorf("the environment must win over the file: %q", got)
	}
}

func TestConfigKafka(t *testing.T) {
	clearEnv(t)
	t.Setenv("KAFKA_BROKERS", "localhost:9092, other:9092")
	if _, err := loadConfig(); err == nil {
		t.Error("KAFKA_BROKERS without SCHEMA_REGISTRY_URL must be an error")
	}
	t.Setenv("SCHEMA_REGISTRY_URL", "http://localhost:8085")
	cfg, err := loadConfig()
	if err != nil || len(cfg.KafkaBrokers) != 2 || cfg.KafkaBrokers[1] != "other:9092" {
		t.Errorf("config %+v, err %v", cfg.KafkaBrokers, err)
	}
}
