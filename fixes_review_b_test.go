package main

import "testing"

// fixes_review_b_test.go — regressions for the Phase B fixes from the PR #2
// review: the long-context key table must stay wired to slots, and every ratio
// meta row must carry a JSON projection rather than silently zeroing.

// TestAboveFieldKeysExact pins the five long-context catalog keys. A writer
// adding a sixth key must extend aboveFields with a slot here; without a row,
// the decode silently never reads the key.
func TestAboveFieldKeysExact(t *testing.T) {
	want := []string{
		"input_cost_per_token_above_200k_tokens",
		"output_cost_per_token_above_200k_tokens",
		"cache_read_input_token_cost_above_200k_tokens",
		"cache_creation_input_token_cost_above_200k_tokens",
		"cache_creation_input_token_cost_above_1hr_above_200k_tokens",
	}
	if len(aboveFields) != len(want) {
		t.Fatalf("aboveFields = %d rows, want %d", len(aboveFields), len(want))
	}
	for i, w := range want {
		if aboveFields[i].key != w {
			t.Errorf("aboveFields[%d].key = %q, want %q", i, aboveFields[i].key, w)
		}
	}
}

// TestDetailRatioMetaRowsProject pins that every detailRatioMeta row carries a
// non-nil set projection and that invoking it updates exactly the JSON field
// its jsonKey names — so a silent-zero drift fails here instead of in serialized
// output.
func TestDetailRatioMetaRowsProject(t *testing.T) {
	const probe = 1.25
	for _, d := range detailRatioMeta {
		if d.set == nil {
			t.Errorf("detailRatioMeta row %q has nil set projection", d.label)
			continue
		}
		r := jsonInputOutputRatio{}
		d.set(&r, probe)
		switch d.jsonKey {
		case "total":
			if r.Total != probe || r.Cached != 0 || r.Uncached != 0 {
				t.Errorf("row %q set fields = %v/%v/%v, want total=%v only", d.label, r.Total, r.Cached, r.Uncached, probe)
			}
		case "cached":
			if r.Cached != probe || r.Total != 0 || r.Uncached != 0 {
				t.Errorf("row %q set fields = %v/%v/%v, want cached=%v only", d.label, r.Total, r.Cached, r.Uncached, probe)
			}
		case "uncached":
			if r.Uncached != probe || r.Total != 0 || r.Cached != 0 {
				t.Errorf("row %q set fields = %v/%v/%v, want uncached=%v only", d.label, r.Total, r.Cached, r.Uncached, probe)
			}
		default:
			t.Errorf("detailRatioMeta row %q has unknown jsonKey %q", d.label, d.jsonKey)
		}
	}
}
