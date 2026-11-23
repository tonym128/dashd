package source

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestAgyLabelsFromGenMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER, data BLOB)`); err != nil {
		t.Fatal(err)
	}
	// Shaped like the real blob: length-prefixed protobuf strings.
	blob := append([]byte{0x0a, 0x16}, []byte("MODEL_PLACEHOLDER_M999")...)
	blob = append(blob, 0x12, 0x17)
	blob = append(blob, []byte("Gemini 9 Xenon (Medium)\"")...)
	blob = append(blob, 0x1a, 0x0f)
	blob = append(blob, []byte("gemini-9-xenon")...)
	if _, err := db.Exec(`INSERT INTO gen_metadata VALUES (1, ?)`, blob); err != nil {
		t.Fatal(err)
	}
	got := agyLabelsFromGenMetadata(db)
	if got[1999] != "Gemini 9 Xenon (Medium)" {
		t.Fatalf("labels = %v", got)
	}
	if Hyphenate(got[1999]) != "gemini-9-xenon" {
		t.Fatalf("label does not normalise to the API id: %q", Hyphenate(got[1999]))
	}
}

// TestAgyLabelsFromARealConversation runs against a local Antigravity
// conversation when DASHD_AGY_CONVERSATION points at one.
func TestAgyLabelsFromARealConversation(t *testing.T) {
	p := os.Getenv("DASHD_AGY_CONVERSATION")
	if p == "" {
		t.Skip("set DASHD_AGY_CONVERSATION to a conversation .db to run")
	}
	db, err := OpenReadOnly(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Logf("labels: %v", agyLabelsFromGenMetadata(db))
}

// Antigravity's own label beats the built-in table, and prompt text that
// happens to name another model does not outvote it.
func TestAgyRecordedLabelWins(t *testing.T) {
	field := func(s string) []byte { return append([]byte{0x12, byte(len(s))}, s...) }
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER, data BLOB)`); err != nil {
		t.Fatal(err)
	}
	row := func(fields ...string) {
		var b []byte
		for _, f := range fields {
			b = append(b, field(f)...)
			b = append(b, 0)
		}
		if _, err := db.Exec(`INSERT INTO gen_metadata VALUES (1, ?)`, b); err != nil {
			t.Fatal(err)
		}
	}
	row("MODEL_PLACEHOLDER_M20", "Gemini 8 Zinc (Low)")
	row("MODEL_PLACEHOLDER_M20", "Gemini 9 Xenon (Medium)")
	row("MODEL_PLACEHOLDER_M20", "Gemini 9 Xenon (Medium)")
	row("MODEL_PLACEHOLDER_M20", "MODEL_PLACEHOLDER_M21", "Gemini 8 Zinc (Low)")
	labels := agyLabelsFromGenMetadata(db)
	if len(labels) != 1 || labels[1020] != "Gemini 9 Xenon (Medium)" {
		t.Fatalf("labels = %v", labels)
	}

	pricer, err := NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	p := &AntigravityParser{labels: labels}
	b := newSessionBuilder("u", "agy", "")
	p.consumeStep(b, 1, agyStepLLM1, 3, agyStepMetadata(1_700_000_000, nil, 1020, 10, 5, 0, 0), pricer)
	calls := b.build("x").Calls
	if len(calls) != 1 || calls[0].Model != "gemini-9-xenon" {
		t.Fatalf("calls = %+v", calls)
	}
}
