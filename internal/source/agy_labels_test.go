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
	blob = append(blob, 0x12, 0x18)
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
