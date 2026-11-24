package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

func TestBackupRotationKeepsOnlyItsOwnNewest(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "dashboard.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mine := []string{"dashboard.db.bak-v10", "dashboard.db.auto-notes.bak", "other.db.auto-20200101-000000.bak"}
	for _, n := range mine {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 4 {
		if _, err := backupOnce(context.Background(), st, db, dir, 2, at.AddDate(0, 0, i)); err != nil {
			t.Fatal(err)
		}
	}
	got := autoBackups(db, dir)
	if len(got) != 2 || filepath.Base(got[1]) != "dashboard.db.auto-20260104-000000.bak" {
		t.Errorf("kept %v", got)
	}
	for _, n := range mine {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("rotation removed %s", n)
		}
	}
}
