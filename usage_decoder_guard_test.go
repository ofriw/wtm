package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestDecoderTagGuard pins every hand-written UnmarshalJSON method to its
// struct's json tags. Each decoder re-lists its keys as string literals instead
// of reading encoding/json tags, so a new billed field added later would decode
// to zero silently. Reflecting over the tags and checking each against the keys
// the decoder handles turns that silence into a loud test failure.
func TestDecoderTagGuard(t *testing.T) {
	type guard struct {
		name    string
		typ     reflect.Type
		handled map[string]bool
	}
	keys := func(k ...string) map[string]bool {
		m := make(map[string]bool, len(k))
		for _, s := range k {
			m[s] = true
		}
		return m
	}
	for _, g := range []guard{
		{"piUsage", reflect.TypeOf(piUsage{}), keys("input", "output", "cacheRead", "cacheWrite", "cost")},
		{"piCost", reflect.TypeOf(piCost{}), keys("input", "output", "cacheRead", "cacheWrite", "total")},
		{"piSpawnStats", reflect.TypeOf(piSpawnStats{}), keys("cost", "inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens")},
		{"piSpawnUsage", reflect.TypeOf(piSpawnUsage{}), keys("model", "cost", "input", "output", "cacheRead", "cacheWrite")},
		{"claudeUsage", reflect.TypeOf(claudeUsage{}), keys("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation", "service_tier", "server_tool_use")},
		{"claudeCacheCreation", reflect.TypeOf(claudeCacheCreation{}), keys("ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens")},
		{"claudeServerToolUse", reflect.TypeOf(claudeServerToolUse{}), keys("web_search_requests")},
	} {
		for i := 0; i < g.typ.NumField(); i++ {
			f := g.typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if !g.handled[tag] {
				t.Errorf("%s: json tag %q is not decoded by its hand-written UnmarshalJSON; add a decoder entry", g.name, tag)
			}
		}
	}
}
