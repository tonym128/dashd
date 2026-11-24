package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

// autoBackupRe matches only the names runBackups writes, so rotation can never
// delete a backup someone made by hand or before a migration.
var autoBackupRe = regexp.MustCompile(`\.auto-\d{8}-\d{6}\.bak$`)

func autoBackups(db, dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.bak"))
	prefix := filepath.Base(db) + ".auto-"
	var out []string
	for _, m := range matches {
		base := filepath.Base(m)
		if len(base) > len(prefix) && base[:len(prefix)] == prefix && autoBackupRe.MatchString(base) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// backupOnce writes a backup and removes the oldest automatic ones beyond keep.
func backupOnce(ctx context.Context, st *store.Store, db, dir string, keep int, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, filepath.Base(db)+".auto-"+now.Format("20060102-150405")+".bak")
	if err := st.Backup(ctx, dest); err != nil {
		return "", err
	}
	all := autoBackups(db, dir)
	for len(all) > max(keep, 1) {
		if err := os.Remove(all[0]); err != nil {
			return dest, fmt.Errorf("rotate backups: %w", err)
		}
		all = all[1:]
	}
	return dest, nil
}

// runBackups keeps rotating backups until ctx is done. The newest file's age
// decides when the next is due, so restarts neither skip nor repeat one.
func runBackups(ctx context.Context, st *store.Store, db, dir string, every time.Duration, keep int, log *slog.Logger) {
	if dir == "" {
		dir = filepath.Dir(db)
	}
	for {
		wait := time.Duration(0)
		if all := autoBackups(db, dir); len(all) > 0 {
			if fi, err := os.Stat(all[len(all)-1]); err == nil {
				wait = every - time.Since(fi.ModTime())
			}
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if ctx.Err() != nil {
			return
		}
		dest, err := backupOnce(ctx, st, db, dir, keep, time.Now())
		if err == nil {
			log.Info("backed up the database", "path", dest, "keeping", keep)
			continue
		}
		log.Warn("automatic backup failed", "dir", dir, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(min(every, time.Hour)):
		}
	}
}
