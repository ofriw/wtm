package core

import (
	"reflect"
	"testing"
)

// TestSettingsUnusedTTLTagMatchesKey pins the JSON tag spelling to the cli
// writer's key. A Go tag cannot reference a const, so the two spellings live
// independently; this is the guard that keeps them from drifting.
func TestSettingsUnusedTTLTagMatchesKey(t *testing.T) {
	f, ok := reflect.TypeOf(Settings{}).FieldByName("UnusedTTL")
	if !ok {
		t.Fatal("Settings.UnusedTTL field missing")
	}
	if got := f.Tag.Get("json"); got != UnusedTTLKey {
		t.Fatalf("Settings.UnusedTTL json tag = %q, want %q", got, UnusedTTLKey)
	}
}
