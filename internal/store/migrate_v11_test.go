package store

import (
	"path/filepath"
	"testing"
)

func TestV11DropsOnlyAntigravityCursors(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO scan_state (path, agent) VALUES ('a', 'agy'), ('c', 'claude')`); err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := v11RereadAntigravity.apply(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var left string
	if err := st.DB().QueryRow(`SELECT group_concat(agent) FROM scan_state`).Scan(&left); err != nil || left != "claude" {
		t.Fatalf("cursors left = %q, %v; want claude only", left, err)
	}
}
