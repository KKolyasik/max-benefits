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
