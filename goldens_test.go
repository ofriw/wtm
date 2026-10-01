package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goGoldensDir mirrors the `test/scenarios/**/expected.*` rule in
// .gitattributes: goldens are byte-compared, so a CRLF checkout must fail
// loudly instead of producing confusing cross-platform diffs.
const goGoldensDir = "test/scenarios"

// TestGoldensUseLF fails if any golden contains CRLF line endings.
func TestGoldensUseLF(t *testing.T) {
	checked := 0
	err := filepath.WalkDir(goGoldensDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "expected.") {
			return nil
		}
		checked++
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("\r\n")) {
			t.Errorf("%s contains CRLF; goldens must be LF-only", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", goGoldensDir, err)
	}
	if checked == 0 {
		t.Fatalf("no goldens found under %s", goGoldensDir)
	}
}
