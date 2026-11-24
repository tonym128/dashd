package gitlog

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCollectReadsOwnCommitsOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "me@example.com")
	run("config", "user.name", "me")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "mine "+strings.Repeat("é", 300))
	run("commit", "-q", "--allow-empty", "--author=x <you@example.com>", "-m", "theirs")
	got := Collect(context.Background(), []string{dir, "relative", dir + "/gone", t.TempDir()}, time.Now().Add(-time.Hour))
	if len(got) != 1 || !strings.HasPrefix(got[0].Subject, "mine") || len(got[0].Subject) > maxSubject || len(got[0].Hash) != 40 {
		t.Fatalf("%+v", got)
	}
	if n := len(Collect(context.Background(), []string{dir}, time.Now().Add(time.Hour))); n != 0 {
		t.Errorf("%d commits since the future", n)
	}
}
