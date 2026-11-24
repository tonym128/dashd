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
var commands = []string{"serve-and-scan", "serve", "scan", "push", "status", "reprice", "prune", "backup", "export", "digest", "update-models", "self-update", "doctor", "insights", "stats"}

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
	git           bool
	currentPrices bool
	before        string
	dailyBudget   float64
	monthlyBudget float64
	// authToken is empty unless -auth-token was given, which means "no
	// authentication", not "authentication with an empty secret".
	authToken   string
	ingestToken string
	adminToken  string
	// The *File variants read a token from a file, which keeps it out of argv
	// (world-readable via ps on Linux) and out of the environment.
	authTokenFile     string
	ingestTokenFile   string
	adminTokenFile    string
	machineTokensFile string

	acceptIngest   bool
	insecureNoAuth bool
	allowedHosts   string
	trustProxy     bool
	tlsCert        string
	tlsKey         string
	tz             string

	// prune
	includeUndated bool
	vacuum         bool

	// push
	stateDB      string
	resync       bool
	redactPaths  string
	redactTitles bool
	autoUpdate   time.Duration

	// insights / export / digest filters
	since       string
	until       string
	project     string
	agent       string
	modelFilter string
	onlyMachine string
	jsonOut     bool
	format      string
	out         string
	webhookURL  string
	digestEvery time.Duration
	tagsFile    string
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
	fs.StringVar(&o.before, "before", "",
		"for 'prune', delete calls older than this YYYY-MM-DD date")
	fs.Float64Var(&o.dailyBudget, "daily-budget", 0,
		"optional soft daily spending budget in USD for alerts/insights")
	fs.Float64Var(&o.monthlyBudget, "monthly-budget", 0,
		"optional soft monthly spending budget in USD for alerts/insights")
	fs.StringVar(&o.authToken, "auth-token", "",
		"require/send this bearer token (also DASHD_AUTH_TOKEN); empty disables authentication")
	fs.StringVar(&o.ingestToken, "ingest-token", "",
		"optional write-only bearer token for /api/ingest and /api/models (also DASHD_INGEST_TOKEN)")
	fs.StringVar(&o.adminToken, "admin-token", "",
		"optional token required for admin actions (reprice, prune, forget, rename); also DASHD_ADMIN_TOKEN. Without it the -auth-token grants admin")
	fs.StringVar(&o.authTokenFile, "auth-token-file", "", "read -auth-token from this file (keeps it out of argv)")
	fs.StringVar(&o.ingestTokenFile, "ingest-token-file", "", "read -ingest-token from this file")
	fs.StringVar(&o.adminTokenFile, "admin-token-file", "", "read -admin-token from this file")
	fs.StringVar(&o.machineTokensFile, "machine-tokens-file", "",
		"file of `machine token` lines; each token may only push as its machine")
	fs.BoolVar(&o.acceptIngest, "accept-ingest", false,
		"accept POST /api/ingest from push agents (on by default for 'serve' and whenever an ingest token is set)")
	fs.BoolVar(&o.insecureNoAuth, "insecure-no-auth", false,
		"allow a non-loopback -addr without -auth-token (exposes all data to the network)")
	fs.StringVar(&o.allowedHosts, "allowed-hosts", "",
		"comma-separated hostnames accepted in the Host header when no -auth-token is set (DNS-rebinding defence); * disables")
	fs.BoolVar(&o.trustProxy, "trust-proxy", false,
		"trust X-Forwarded-For / X-Forwarded-Proto from a reverse proxy")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "serve HTTPS with this certificate file (with -tls-key)")
	fs.StringVar(&o.tlsKey, "tls-key", "", "serve HTTPS with this key file (with -tls-cert)")
	fs.StringVar(&o.tz, "tz", "",
		"IANA timezone used to bucket calls into days (default: the host's local zone; also DASHD_TZ)")
	fs.BoolVar(&o.includeUndated, "include-undated", false, "for 'prune', also delete calls with no timestamp")
	fs.BoolVar(&o.vacuum, "vacuum", true, "for 'prune', VACUUM afterwards to reclaim disk space")
	fs.StringVar(&o.stateDB, "state-db", filepath.Join(home, ".local/share/dashd/push-state.db"),
		"for 'push' and 'status', the local cursor database")
	fs.BoolVar(&o.currentPrices, "current-prices", false,
		"for 'reprice', forget the recorded price history and apply today's prices to all of it")
	fs.BoolVar(&o.git, "git", false,
		"read recent commits (hash, time, subject) in the projects agents worked in, to show what each cost; 'push' sends them to the server")
	fs.BoolVar(&o.resync, "resync", false, "for 'push', forget local cursors and resend all history")
	fs.StringVar(&o.redactPaths, "redact-paths", "none",
		"for 'push', how project and log paths are sent: none, home (strip $HOME), basename, or hash")
	fs.BoolVar(&o.redactTitles, "redact-titles", false, "for 'push', do not send session titles")
	fs.DurationVar(&o.autoUpdate, "auto-update", 0,
		"for 'push', check for a new release on this interval and restart into it (e.g. 24h); 0 disables")
	fs.StringVar(&o.since, "since", "", "for insights/export/digest, only calls on or after YYYY-MM-DD")
	fs.StringVar(&o.until, "until", "", "for insights/export/digest, only calls on or before YYYY-MM-DD")
	fs.StringVar(&o.project, "project", "", "for insights/export/digest, comma-separated projects")
	fs.StringVar(&o.agent, "agent", "", "for insights/export/digest, comma-separated agents")
	fs.StringVar(&o.modelFilter, "model", "", "for insights/export/digest, comma-separated models")
	fs.StringVar(&o.onlyMachine, "only-machine", "", "for insights/export/digest, comma-separated machines")
	fs.BoolVar(&o.jsonOut, "json", false, "for insights/status, print JSON")
	fs.StringVar(&o.format, "format", "csv", "for 'export', csv or json")
	fs.StringVar(&o.out, "out", "", "for 'export'/'backup', the output file (export defaults to stdout)")
	fs.StringVar(&o.webhookURL, "webhook-url", "",
		"POST budget alerts and digests to this URL (Slack/Discord-compatible JSON; also DASHD_WEBHOOK_URL)")
	fs.DurationVar(&o.digestEvery, "digest-every", 0,
		"with -webhook-url, send a usage digest on this interval (e.g. 168h for weekly); 0 disables")
	fs.StringVar(&o.tagsFile, "tags-file", "",
		"JSON file mapping a tag (client, team, cost centre) to a list of project path prefixes")
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
  status          show this machine's push state: last push, last error, cursors
  reprice         re-cost every stored call at the price in force when it was made
  prune           delete calls older than -before YYYY-MM-DD and vacuum the DB
  backup          write a consistent copy of the database to -out (safe while serving)
  export          write calls as CSV or JSON (-format, -out, filter flags)
  digest          print a Markdown usage digest (or POST it to -webhook-url)
  update-models   download the latest OpenRouter model pricing to -models
  self-update     download and install the latest dashd release binary
  doctor          check local agent logs, pricing, database, and remote server
  insights        print plain-English usage and cost insights from the database
  stats           print totals from the existing database, change nothing

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprintf(out, `
Authentication is off unless -auth-token is set. With a token, API clients send
"Authorization: Bearer <token>" and browsers sign in at /login; /healthz is
always open. A non-loopback -addr without a token is refused unless
-insecure-no-auth is given.
`)
}
