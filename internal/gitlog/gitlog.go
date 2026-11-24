// Package gitlog reads recent commits of the projects agents worked in, so a
// commit can be shown with what it cost. It is off unless asked for.
package gitlog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonym128/dashd/internal/model"
)

const maxSubject = 200

// Collect returns the commits made since a time in each project directory that
// is a git repository. A directory that is gone, or is not one, is skipped.
func Collect(ctx context.Context, projects []string, since time.Time) []model.Commit {
	var out []model.Commit
	for _, p := range projects {
		if !filepath.IsAbs(p) {
			continue
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, read(ctx, p, since)...)
	}
	return out
}

func read(ctx context.Context, dir string, since time.Time) []model.Commit {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// The directory comes from an agent's log, so its repository config is not
	// trusted: the settings that would run a program during a log are off. The
	// author filter keeps a pulled history from being charged to local sessions.
	base := []string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "log.showSignature=false", "-c", "core.hooksPath=/dev/null", "--no-pager"}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append(append([]string{}, base...), args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")
		b, err := cmd.Output()
		return string(b), err
	}
	args := []string{"log", "--branches", "--no-merges", "--no-ext-diff", "--no-textconv",
		"--since=" + since.UTC().Format(time.RFC3339), "--max-count=2000", "--format=%H%x1f%ct%x1f%s"}
	if email, err := git("config", "user.email"); err == nil && strings.TrimSpace(email) != "" {
		args = append(args, "--fixed-strings", "--author=<"+strings.TrimSpace(email)+">")
	}
	text, err := git(args...)
	if err != nil {
		return nil
	}
	return parse(dir, text)
}

func parse(project, text string) []model.Commit {
	var out []model.Commit
	for _, line := range strings.Split(text, "\n") {
		f := strings.SplitN(line, "\x1f", 3)
		if len(f) != 3 {
			continue
		}
		ts, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || ts <= 0 {
			continue
		}
		subject := strings.ToValidUTF8(f[2], "")
		for len(subject) > maxSubject {
			_, n := utf8.DecodeLastRuneInString(subject)
			subject = subject[:len(subject)-n]
		}
		out = append(out, model.Commit{Project: project, Hash: f[0], TS: ts, Subject: subject})
	}
	return out
}
