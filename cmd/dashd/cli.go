// Command line parsing for dashd.
//
// This file exists separately from main.go because the decision of *what* to run
// is worth testing on its own: the whole of a user's intent is `argv`, and the
// bug this replaces was entirely a bug in reading it. parseArgs is a pure
// function of its arguments, so the table test needs no process, no port and no
// database.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// defaultCommand runs when no command is named. Preserved deliberately: `dashd`
// on its own is a documented way to start the dashboard and scan in the
// background.
const defaultCommand = "serve-and-scan"

// commands is the complete set. It is a slice rather than inline cases in a
// switch so that the same list can be printed in the error message and the
// usage text, which is the only way those two stay in agreement.
var commands = []string{"serve-and-scan", "serve", "scan", "push", "reprice", "update-models", "doctor", "stats"}

// options is the parsed command line.
type options struct {
	db            string
	addr          string
	interval      time.Duration
	home          string
	models        string
	modelsRefresh time.Duration
	opencode      string
	machine       string
	remote        string
	once          bool
	// authToken is empty unless -auth-token was given, which means "no
	// authentication", not "authentication with an empty secret".
	authToken   string
	ingestToken string
	verbose     bool
	showVer     bool
}

// newFlagSet builds the flag set. It is separate so that both parseArgs and
// printUsage describe the same flags; usage text cannot drift from the parser
// because it is generated from this.
func newFlagSet(o *options, out io.Writer) *flag.FlagSet {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("dashd", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.db, "db", filepath.Join(home, ".local/share/dashd/dashboard.db"),
		"path to the SQLite database")
	fs.StringVar(&o.addr, "addr", "127.0.0.1:8753",
		"address to serve on; 0.0.0.0 exposes the dashboard to the network, so pair it with -auth-token")
	fs.DurationVar(&o.interval, "interval", 30*time.Second,
		"how often to scan for new activity")
	fs.StringVar(&o.home, "home", home, "directory to look for agent logs under")
	fs.StringVar(&o.models, "models", defaultModelsPath(),
		"path to the OpenRouter models.json price dump")
	fs.DurationVar(&o.modelsRefresh, "models-refresh", 0,
		"auto-update models.json from OpenRouter on this interval (e.g. 24h); 0 disables")
	fs.StringVar(&o.opencode, "opencode", "",
		"path to the OpenCode database (default: inside -home)")
	fs.StringVar(&o.machine, "machine", defaultHostname(),
		"machine name attached to scanned sessions and calls")
	fs.StringVar(&o.remote, "remote", "",
		"remote dashd base URL for 'push' (e.g. https://dashd.example.com; also DASHD_REMOTE)")
	fs.BoolVar(&o.once, "once", false,
		"for 'push', run a single pass and exit instead of looping on -interval")
	fs.StringVar(&o.authToken, "auth-token", "",
		"require/send this bearer token (also DASHD_AUTH_TOKEN); empty disables authentication")
	fs.StringVar(&o.ingestToken, "ingest-token", "",
		"optional write-only bearer token for /api/ingest and /api/models (also DASHD_INGEST_TOKEN)")
	fs.BoolVar(&o.verbose, "verbose", false, "log every request and scan detail")
	fs.BoolVar(&o.showVer, "version", false, "print the version, commit and build date, then exit")
	return fs
}

// parseArgs reads argv into options and a command.
//
// The command may appear before the flags, after them, or between them:
//
//	dashd stats
//	dashd -verbose stats
//	dashd stats -verbose
//
// flag.Parse stops at the first non-flag argument and, worse, quietly ignores
// everything after it, so the naive "look at os.Args[1], then hand the rest to
// flag" arrangement turned `dashd -models /nope.json -db foo.db stats` into an
// unattended `serve-and-scan` of the user's real home directory with none of
// their flags applied. So instead of parsing once, this loops: parse, lift the
// leading positional off the remainder, parse again. Interleaving costs nothing
// and every flag is honoured wherever the user typed it.
func parseArgs(argv []string) (options, string, error) {
	var o options
	// The flag package's own output is suppressed so that there is exactly one
	// place that writes usage, and it is the one that knows about commands.
	// Otherwise `-h` prints the bare flag list first and the useful text second,
	// and a bad flag prints its error twice.
	fs := newFlagSet(&o, io.Discard)

	var positional []string
	rest := argv
	for {
		if err := fs.Parse(rest); err != nil {
			return o, "", err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}

	if len(positional) > 1 {
		return o, "", fmt.Errorf("unexpected argument %q: dashd takes one command and any number of flags",
			positional[1])
	}

	command := defaultCommand
	if len(positional) == 1 {
		command = positional[0]
	}
	// An unrecognised command is an error, never a fallback to the default.
	// Falling back here would be the worst possible failure: a typo in a cron
	// entry becomes a resident process serving a dashboard nobody asked for,
	// pointed at a real home directory.
	if !slices.Contains(commands, command) {
		return o, "", fmt.Errorf("unknown command %q (want one of: %s)",
			command, strings.Join(commands, ", "))
	}
	return o, command, nil
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// printUsage writes the commands and the real flag set.
func printUsage(out io.Writer) {
	var o options
	fs := newFlagSet(&o, out)

	fmt.Fprint(out, `dashd serves the cost dashboard and keeps its database up to date.

Usage:
  dashd [flags] [command]

The command may come before or after the flags. With no command at all, dashd
serves the dashboard and scans in the background (serve-and-scan).

Commands:
  serve-and-scan  serve the dashboard and scan on an interval (default)
  serve           serve the dashboard only; data is maintained elsewhere
  scan            one scan pass, print totals, exit (for cron or a timer)
  push            scan local logs and push updates to a remote dashd server
  reprice         re-cost every stored call with the current price dump
  update-models   download the latest OpenRouter model pricing to -models
  doctor          check local agent logs, pricing, database, and remote server
  stats           print totals from the existing database, change nothing

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprintf(out, `
Authentication is off unless -auth-token is set. With a token, requests need
"Authorization: Bearer <token>" or "X-Auth-Token: <token>", except /healthz.
`)
}
