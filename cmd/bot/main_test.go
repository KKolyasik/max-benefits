package main

import (
	"slices"
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
	for value, want := range map[string]bool{"": false, "false": false, "true": true, "1": true} {
		t.Setenv("MAX_BOT_TOKEN", "token")
		t.Setenv("ADMIN_FOR_ALL", value)
		cfg, err := loadConfig()
		if err != nil || cfg.AdminForAll != want {
			t.Errorf("ADMIN_FOR_ALL=%q: got %v, %v", value, cfg.AdminForAll, err)
		}
	}
	t.Setenv("ADMIN_FOR_ALL", "yes please")
	if _, err := loadConfig(); err == nil {
		t.Error("a value that is not a boolean must be rejected")
	}
}
