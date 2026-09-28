package main

import (
	"slices"
	"strings"
	"testing"
)

func TestParseIDs(t *testing.T) {
	ids, err := parseIDs("1, 22;333 4444")
	if err != nil || !slices.Equal(ids, []int64{1, 22, 333, 4444}) {
		t.Errorf("got %v, %v", ids, err)
	}
	if ids, err := parseIDs(""); err != nil || len(ids) != 0 {
		t.Errorf("empty: %v, %v", ids, err)
	}
	for _, bad := range []string{"abc", "-5", "1,,x"} {
		if _, err := parseIDs(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestKafkaConfig(t *testing.T) {
	cases := map[string]struct {
		brokers, registry, db string
		ok                    bool
	}{
		"no kafka":         {"", "", "", true},
		"kafka":            {"kafka:9092, other:9092", "http://schema-registry:8081", "postgres://db", true},
		"no registry":      {"kafka:9092", "", "postgres://db", false},
		"registry only":    {"", "http://schema-registry:8081", "postgres://db", false},
		"kafka without db": {"kafka:9092", "http://schema-registry:8081", "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MAX_BOT_TOKEN", "token")
			t.Setenv("KAFKA_BROKERS", tc.brokers)
			t.Setenv("SCHEMA_REGISTRY_URL", tc.registry)
			t.Setenv("DATABASE_URL", tc.db)
			cfg, err := loadConfig()
			if (err == nil) != tc.ok {
				t.Fatalf("err %v", err)
			}
			if name == "kafka" && !slices.Equal(cfg.KafkaBrokers, []string{"kafka:9092", "other:9092"}) {
				t.Errorf("brokers %q", cfg.KafkaBrokers)
			}
		})
	}
}

func TestAdminForAllConfig(t *testing.T) {
	cases := map[string]struct {
		value string
		want  bool
		ok    bool
	}{
		"unset":   {"", false, true},
		"false":   {"false", false, true},
		"true":    {"true", true, true},
		"one":     {"1", true, true},
		"garbage": {"yes please", false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MAX_BOT_TOKEN", "token")
			// CI sets these for the integration tests; they are not what is
			// under test here.
			t.Setenv("KAFKA_BROKERS", "")
			t.Setenv("SCHEMA_REGISTRY_URL", "")
			t.Setenv("DATABASE_URL", "")
			t.Setenv("ADMIN_FOR_ALL", tc.value)
			cfg, err := loadConfig()
			if (err == nil) != tc.ok || cfg.AdminForAll != tc.want {
				t.Errorf("got %v, %v", cfg.AdminForAll, err)
			}
			if err != nil && !strings.Contains(err.Error(), "ADMIN_FOR_ALL") {
				t.Errorf("the error must be about ADMIN_FOR_ALL: %v", err)
			}
		})
	}
}
