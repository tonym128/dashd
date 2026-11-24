// Command dashd serves the cost dashboard and keeps its database up to date.
//
// The two halves are deliberately separate. The scanner is a background loop that
// owns writing; the web server only ever reads. SQLite runs in WAL mode, so a
// page load gets a consistent snapshot while a scan is in progress and neither
// half waits on the other. Either can also be run alone — `dashd scan` for a
// cron-driven machine, `dashd serve` when the data is maintained elsewhere.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/scan"
	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

// stopTimeout bounds how long shutdown waits for an in-flight scan pass to
// notice the cancellation. Long enough for the pass that is nearly done, short
// enough that ctrl-c still feels like ctrl-c.
const stopTimeout = 30 * time.Second

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

// runCLI parses the command line, handles the two options that print and exit,
// and otherwise runs the command. Kept separate from main so the exit code and
// the streams are testable without starting a server.
func runCLI(argv []string, stdout, stderr io.Writer) int {
	o, command, err := parseArgs(argv)
	switch {
	case errors.Is(err, flag.ErrHelp):
		// `-h` is a request, not a failure.
		printUsage(stderr)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "dashd: %v\n\n", err)
		printUsage(stderr)
		return 2
	}

	if o.showVer {
		fmt.Fprintln(stdout, versionString())
		return 0
	}

	if err := run(o, command, stdout); err != nil {
		if errors.Is(err, errRestart) {
			// A new binary was installed; the supervisor restarts us into it.
			fmt.Fprintln(stderr, "dashd: restarting into the updated binary")
			return exitRestart
		}
		fmt.Fprintln(stderr, "dashd:", err)
		return 1
	}
	return 0
}

func run(o options, command string, stdout io.Writer) error {
	if err := resolveSecrets(&o); err != nil {
		return err
	}
	if o.remote == "" {
		o.remote = os.Getenv("DASHD_REMOTE")
	}
	if o.webhookURL == "" {
		o.webhookURL = os.Getenv("DASHD_WEBHOOK_URL")
	}
	if o.tz == "" {
		o.tz = os.Getenv("DASHD_TZ")
	}
	if o.tz != "" {
		loc, err := time.LoadLocation(o.tz)
		if err != nil {
			return fmt.Errorf("-tz %q: %w", o.tz, err)
		}
		// Every day bucket, date filter and burn window is computed in
		// time.Local, so setting it here is what makes them agree.
		time.Local = loc
	}
	if command == "push" || command == "status" {
		// A push agent keeps only cursors locally, in its own database, so it
		// never mixes with a dashboard database on the same machine. An
		// explicit -db is still honoured for installs that predate -state-db.
		if home, err := os.UserHomeDir(); err == nil && o.db == filepath.Join(home, ".local/share/dashd/dashboard.db") {
			o.db = o.stateDB
		}
	}

	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log.Debug("starting", "version", version, "commit", commit, "date", date, "command", command)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "update-models":
		n, err := source.UpdateModels(ctx, source.OpenRouterModelsURL, o.models)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "updated %s (%d models)\n", o.models, n)
		return nil
	case "doctor":
		return runDoctor(ctx, o, stdout)
	case "self-update":
		_, err := runSelfUpdate(ctx, stdout)
		return err
	}

	if command == "serve" || command == "serve-and-scan" {
		if err := checkExposure(o); err != nil {
			return err
		}
	}

	// The store reports a database it could not chmod 0600 through this hook,
	// rather than failing: a filesystem without permission bits should not stop
	// the dashboard, and a database any local account can read should not start
	// silently. It is wired before Open, because that is when the chmod happens.
	store.Warn = log.Warn

	st, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer st.Close()

	switch command {
	case "status":
		return runStatus(ctx, o, st, stdout)
	case "backup":
		if o.out == "" {
			o.out = o.db + "." + time.Now().Format("20060102-150405") + ".bak"
		}
		if err := st.Backup(ctx, o.out); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "backed up %s to %s\n", o.db, o.out)
		return nil
	case "export":
		return runExport(ctx, o, st, stdout)
	case "stats":
		return printStats(st, stdout)
	case "insights":
		return runInsights(ctx, o, st, stdout)
	case "digest":
		return runDigest(ctx, o, st, stdout)
	case "prune":
		if o.before == "" {
			return fmt.Errorf("-before YYYY-MM-DD is required for prune")
		}
		cutoff, err := time.ParseInLocation("2006-01-02", o.before, time.Local)
		if err != nil {
			return fmt.Errorf("invalid -before date %q: %w", o.before, err)
		}
		n, err := st.PruneBefore(ctx, cutoff, store.PruneOptions{IncludeUndated: o.includeUndated, Vacuum: o.vacuum})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "pruned %d calls before %s\n", n, o.before)
		return nil
	}

	// The pricer is built here on purpose: every remaining command that costs
	// anything needs it, so this is the single place the fallback warning can
	// be — once per process, for scan, reprice, push and serve.
	pricer, err := source.NewPricer(o.models, "")
	if err != nil {
		return fmt.Errorf("%w\n\nPricing is required. Fetch it with:\n  "+
			"dashd update-models", err)
	}
	warnIfFallbackOnly(pricer, o.models, log)
	log.Debug("pricing configured", "models", o.models)
	priceFn := priceAt(pricer)

	if command != "push" {
		if command == "reprice" && o.currentPrices {
			if err := st.ClearPrices(""); err != nil {
				return err
			}
		}
		if err := pricer.SyncHistory(ctx, st, time.Now()); err != nil {
			return err
		}
	}

	if command == "reprice" {
		n, err := st.Reprice(ctx, priceFn)
		if err != nil {
			return err
		}
		log.Info("repriced", "changed_calls", n, "prices", o.models)
		return printStats(st, stdout)
	}

	sources, openCodeDB := scan.DefaultSources(o.home)
	// The flag wins over the location under -home. It used to be read here and
	// then thrown away, so a user pointing -opencode at a database elsewhere
	// scanned the default path instead and was shown an empty OpenCode panel.
	if o.opencode != "" {
		openCodeDB = o.opencode
	}
	var sink scan.SessionSink = st
	var rsink *remoteSink
	if command == "push" {
		if o.remote == "" {
			return fmt.Errorf("-remote URL (or DASHD_REMOTE) is required for push")
		}
		warnInsecureRemote(o.remote, log)
		pushToken := o.ingestToken
		if pushToken == "" {
			pushToken = o.authToken
		}
		redact, err := parseRedactMode(o.redactPaths)
		if err != nil {
			return err
		}
		rsink = newRemoteSink(o.remote, pushToken, o.machine).
			withModelsSync(o.models, pricer, log).
			withState(st).
			withRedaction(redact, o.redactTitles)
		if o.resync {
			if err := rsink.resetCursors("requested with -resync"); err != nil {
				return err
			}
		}
		rsink.SyncModels()
		sink = rsink
	}

	scanner := scan.New(scan.Config{
		Store:      st,
		Sink:       sink,
		Pricer:     pricer,
		Logger:     log,
		Sources:    sources,
		Machine:    o.machine,
		OpenCodeDB: openCodeDB,
	})

	switch command {
	case "scan":
		// One pass and out, for a machine that would rather be driven by cron
		// or a systemd timer than by a resident process. The totals are
		// printed even after a failed pass: the per-source lines are where the
		// failure is named, and what did go in is real.
		scanErr := scanner.RunOnce(ctx)
		if err := printStats(st, stdout); err != nil {
			return errors.Join(scanErr, err)
		}
		return scanErr

	case "push":
		if o.once || o.interval <= 0 {
			err := rsink.runPass(ctx, scanner)
			if err == nil {
				fmt.Fprintf(stdout, "pushed %s to %s\n", o.machine, o.remote)
			}
			return err
		}
		if o.modelsRefresh > 0 {
			go runModelsRefresher(ctx, o.models, o.modelsRefresh, pricer, nil, nil, log)
		}
		if o.autoUpdate > 0 {
			go runAutoUpdater(ctx, o.autoUpdate, log, stop)
		}
		log.Info("pushing continuously", "machine", o.machine, "remote", o.remote, "interval", o.interval)
		runPushLoop(ctx, scanner, rsink, o.interval, log)
		if restartRequested.Load() {
			return errRestart
		}
		return nil
	}

	srv, err := web.New(st, log)
	if err != nil {
		return err
	}
	srv.ModelsPath = o.models
	srv.DailyBudget = o.dailyBudget
	srv.MonthlyBudget = o.monthlyBudget
	srv.Version = version
	srv.TrustProxy = o.trustProxy
	srv.CachePayloads = true
	srv.Pricer = priceFn
	srv.Tags, err = web.LoadTags(o.tagsFile)
	if err != nil {
		return err
	}
	srv.IngestEnabled = ingestEnabled(o, command)
	srv.RepriceFunc = func(rctx context.Context) (int, error) { return st.Reprice(rctx, priceFn) }
	srv.ScanInfo = scanner.Status
	srv.ScanDuration = scanner.LastDuration
	srv.AuthEnabled = o.authToken != ""
	srv.PriceAdmin = priceAdmin{st, pricer}
	srv.CatalogueModels = pricer.LiveModelKeys
	srv.PriceStatus = &web.PriceStatus{}
	srv.PriceStatus.Configure(o.modelsRefresh, pricer.LiveCatalogueSize())
	if command == "serve" {
		// No local scanner: report the last ingest instead.
		srv.ScanInfo = nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if o.modelsRefresh > 0 {
		go runModelsRefresher(ctx, o.models, o.modelsRefresh, pricer, st, srv.PriceStatus, log)
	}
	if o.webhookURL != "" {
		go web.RunNotifier(ctx, st, web.NotifierConfig{
			WebhookURL:    o.webhookURL,
			DailyBudget:   o.dailyBudget,
			MonthlyBudget: o.monthlyBudget,
			DigestEvery:   o.digestEvery,
			Tags:          srv.Tags,
			Log:           log,
		})
	}

	if command == "serve-and-scan" {
		var scannerDone sync.WaitGroup
		scannerDone.Add(1)
		go func() {
			defer scannerDone.Done()
			scanner.Run(ctx, o.interval)
		}()
		// Deferred after the store's Close above, so it runs *before* it on every
		// exit path, including a listener that failed to bind. The scanner is the
		// only writer, so it has to be stopped before the database goes away.
		defer func() { waitFor(&scannerDone, log) }()
	}

	logExposure(o, log)

	machineTokens, err := loadMachineTokens(o.machineTokensFile)
	if err != nil {
		return err
	}
	// Authentication is a wrapper rather than something the dashboard knows
	// about, so this file decides policy and web/ stays a dashboard.
	var handler http.Handler = web.WithAuthConfig(web.AuthConfig{
		UIToken:       o.authToken,
		IngestToken:   o.ingestToken,
		AdminToken:    o.adminToken,
		MachineTokens: machineTokens,
		TrustProxy:    o.trustProxy,
		Log:           log,
	}, srv.Handler())
	if o.authToken == "" {
		// With no token the Host header is the only thing standing between a
		// DNS-rebinding page and the whole dashboard.
		handler = web.WithHostCheck(splitList(o.allowedHosts), handler)
	}

	httpSrv := &http.Server{
		Addr:    o.addr,
		Handler: handler,
		// Generous, because a first render over a large history is genuinely
		// slower than a static page but not unbounded. Exports stream and set
		// their own deadline.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", o.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.addr, err)
	}
	scheme := "http"
	if o.tlsCert != "" {
		scheme = "https"
	}
	log.Info("serving",
		"url", scheme+"://"+ln.Addr().String(),
		"version", version,
		"db", o.db,
		"scanning", command == "serve-and-scan",
		"ingest", srv.IngestEnabled,
		"auth", o.authToken != "",
		"interval", o.interval,
		"pid", os.Getpid())
	log.Info("press ctrl-c to stop; -verbose for per-request detail")

	errCh := make(chan error, 1)
	go func() {
		var err error
		if o.tlsCert != "" {
			err = httpSrv.ServeTLS(ln, o.tlsCert, o.tlsKey)
		} else {
			err = httpSrv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Stop accepting requests and let in-flight ones finish. The scanner is
	// stopped by the deferred wait above, after Shutdown returns and before
	// the store closes.
	log.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", "error", err)
	}
	return nil
}

// ingestEnabled decides whether this server accepts pushes. A central
// `serve` exists to receive them; a local serve-and-scan does not, unless the
// operator asks or configures an ingest credential.
func ingestEnabled(o options, command string) bool {
	return o.acceptIngest || command == "serve" || o.ingestToken != "" || o.machineTokensFile != ""
}

// checkExposure refuses to publish an unauthenticated dashboard to the
// network. A warning in a log nobody reads was the old behaviour; Docker's port
// publishing also bypasses host firewalls, so "it's behind ufw" is not a
// mitigation either.
func checkExposure(o options) error {
	if o.tlsCert != "" && o.tlsKey == "" || o.tlsCert == "" && o.tlsKey != "" {
		return fmt.Errorf("-tls-cert and -tls-key must be given together")
	}
	if !exposedToNetwork(o.addr) || o.authToken != "" || o.insecureNoAuth {
		return nil
	}
	return fmt.Errorf("refusing to serve on %s without -auth-token: that would publish every project path, "+
		"session title and cost to the network.\n  Set -auth-token (or DASHD_AUTH_TOKEN / -auth-token-file), "+
		"bind to 127.0.0.1, or pass -insecure-no-auth if this network is trusted", o.addr)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// priceAt adapts a pricer to the store's and server's price function, which
// carry the call's time as epoch seconds, 0 when unknown.
func priceAt(pricer *source.Pricer) func(m string, in, out, cacheRead, cacheWrite, cacheWrite1h, ts int64) (float64, bool) {
	return func(m string, in, out, cacheRead, cacheWrite, cacheWrite1h, ts int64) (float64, bool) {
		var at time.Time
		if ts > 0 {
			at = time.Unix(ts, 0)
		}
		return pricer.CostCallAt(at, m, int(in), int(out), int(cacheRead), int(cacheWrite), int(cacheWrite1h))
	}
}

func runModelsRefresher(ctx context.Context, path string, interval time.Duration, pricer *source.Pricer, st *store.Store, status *web.PriceStatus, log *slog.Logger) {
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if st != nil {
			// Models first used since the last sync are recorded at the prices
			// they were charged, before those change.
			if err := pricer.SyncHistory(ctx, st, time.Now()); err != nil {
				log.Warn("record prices failed", "error", err)
			}
		}
		n, err := source.UpdateModels(rctx, source.OpenRouterModelsURL, path)
		if status != nil {
			status.Attempt(time.Now(), n, err)
		}
		if err != nil {
			log.Warn("auto-update models failed", "path", path, "error", err)
			return
		}
		if err := pricer.ReloadOpenRouter(path); err != nil {
			log.Warn("reload models failed", "path", path, "error", err)
			return
		}
		log.Info("auto-updated models", "path", path, "models", n)
		if st != nil {
			if err := pricer.SyncHistory(ctx, st, time.Now()); err != nil {
				log.Warn("record prices failed", "error", err)
				return
			}
			repriced, err := st.Reprice(ctx, priceAt(pricer))
			if err != nil {
				log.Warn("auto-reprice after models update failed", "error", err)
			} else {
				log.Info("auto-repriced calls", "calls", repriced)
			}
		}
	}

	if st, err := os.Stat(path); err != nil || time.Since(st.ModTime()) >= interval {
		refresh()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// waitFor blocks until the scanner has stopped, or stopTimeout passes.
//
// It is a timeout and not an unbounded wait because a scan pass reads every
// transcript on the machine; on a cold home directory a stop could take a minute,
// and a process that ignores ctrl-c is worse than one that exits having rolled
// back. SQLite transactions are atomic, so a pass killed mid-flight leaves a
// readable database and the next start re-reads the same files.
func waitFor(wg *sync.WaitGroup, log *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopTimeout):
		log.Warn("scanner did not stop within the timeout; exiting mid-pass",
			"timeout", stopTimeout)
	}
}

// logExposure says plainly what the bind address implies. `dashd -addr 0.0.0.0`
// on a cloud VM is a documented, and reasonable, thing for someone to do — which
// is exactly why the unauthenticated case needs to be loud at startup rather
// than discovered later.
func logExposure(o options, log *slog.Logger) {
	exposed := exposedToNetwork(o.addr)
	switch {
	case !exposed && o.authToken == "":
		// Nothing to say. Localhost, no token: the normal case, and a warning
		// here would train the operator to ignore warnings.
	case exposed && o.authToken == "":
		log.Warn("serving on a non-loopback address with NO authentication (-insecure-no-auth)",
			"addr", o.addr,
			"exposes", "project paths, session titles, model names and per-session cost to anyone who can reach this port",
			"reachable_from", "every interface on this host, including its public one",
			"fix", "set -auth-token, or bind to 127.0.0.1 and reach it over an SSH tunnel")
	case exposed && o.authToken != "":
		log.Info("serving on a non-loopback address, protected by -auth-token",
			"addr", o.addr, "accepts", "Authorization: Bearer, or a /login cookie",
			"exempt", "/healthz")
	default:
		log.Info("authentication enabled", "accepts", "Authorization: Bearer, or a /login cookie",
			"exempt", "/healthz")
	}
}

// exposedToNetwork reports whether addr binds somewhere other than this machine.
func exposedToNetwork(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Unparseable is not a reason to be reassuring.
		return true
	}
	switch strings.ToLower(host) {
	case "":
		// ":8753" and "0.0.0.0:8753" both mean every interface.
		return true
	case "localhost":
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// warnIfFallbackOnly reports the one degraded-but-working state: the OpenRouter
// price dump did not load and every price is coming from the embedded table.
//
// This is not fatal, and deliberately so. The fallback table exists so that a
// binary without a dump beside it still runs, and refusing to start would turn a
// rough number into no number. It is also the reason this is loud: with the dump
// absent, a model the fallback knows is priced correctly, but a model it does
// not know is *unpriced* — and a stale row is worse than a missing one, because
// a missing one is at least visible in the unpriced count. Silence here is how a
// `$0.00` with `priced=1` reaches the dashboard.
func warnIfFallbackOnly(pricer *source.Pricer, modelsPath string, log *slog.Logger) {
	if !pricer.FallbackOnly() {
		return
	}
	log.Warn("OpenRouter price dump not loaded; pricing accuracy is degraded",
		"reason", "price dump missing or unreadable",
		"models", modelsPath,
		"priced_from", "embedded fallback table",
		"fallback_models", pricer.FallbackSize(),
		"live_models", 0,
		"effect", "models absent from the fallback table are reported unpriced; "+
			"fallback rows may be older than the provider's current rates",
		"fix", "run `dashd update-models` (or pass -models /path/to/models.json)",
	)
}

// printStats reports the totals, including how many calls could not be priced at
// all — the number that says the rate table is missing something.
func printStats(st *store.Store, out io.Writer) error {
	ctx := context.Background()
	totals, err := st.Totals(ctx, store.Filter{})
	if err != nil {
		return err
	}
	statuses, _ := st.ScanStatuses()
	fmt.Fprintf(out, "calls      %d\n", totals.Calls)
	fmt.Fprintf(out, "sessions   %d\n", totals.Sessions)
	fmt.Fprintf(out, "projects   %d\n", totals.Projects)
	fmt.Fprintf(out, "tokens     %d\n", totals.TotalTokens)
	fmt.Fprintf(out, "cost       $%.2f\n", totals.Cost)
	if totals.UnpricedCalls > 0 {
		fmt.Fprintf(out, "unpriced   %d calls (no rate found; run `dashd update-models`)\n", totals.UnpricedCalls)
		if unpriced, err := st.UnpricedModels(ctx); err == nil {
			for _, u := range unpriced {
				fmt.Fprintf(out, "  %-32s %d calls\n", u.Model, u.Calls)
			}
		}
	}
	if totals.FirstTS > 0 {
		oldest, newest, _ := st.HistoryRange(ctx, store.Filter{})
		fmt.Fprintf(out, "history    %s to %s\n",
			oldest.Local().Format("2006-01-02"), newest.Local().Format("2006-01-02"))
	}
	models, err := st.Models(ctx, store.Filter{})
	if err == nil && len(models) > 0 {
		providers := model.RollupProviders(models, totals.Cost)
		if len(providers) > 0 {
			fmt.Fprintln(out, "\nproviders:")
			for _, p := range providers {
				fmt.Fprintf(out, "  %-12s $%10.2f (%5.1f%%)  %7d calls\n",
					p.Name, p.Cost, p.Pct, p.Calls)
			}
		}
	}
	fmt.Fprintln(out)
	for _, s := range statuses {
		state := "ok"
		if s.Error != "" {
			state = s.Error
		}
		fmt.Fprintf(out, "%-9s %6d files  %6d changed  %6d calls  %s\n",
			s.Agent, s.FilesSeen, s.FilesChanged, s.CallsIngested, state)
	}
	return nil
}

// defaultModelsPath looks for the price dump next to the repository, which is
// where it lives in a checkout, and under the data directory otherwise.
func defaultModelsPath() string {
	exe, _ := os.Executable()
	wd, _ := os.Getwd()
	return modelsPathFrom(exe, wd)
}

// modelsPathFrom is defaultModelsPath with the two environment lookups handed in,
// so the search order can be tested against a real filesystem instead of
// whatever directory the test binary happens to sit in.
func modelsPathFrom(exePath, workDir string) string {
	if exePath != "" {
		// The binary sits beside models.json in a checkout, and one level above
		// it when installed into ~/bin or /usr/local/bin.
		dir := filepath.Dir(exePath)
		for _, candidate := range []string{
			filepath.Join(dir, "models.json"),
			filepath.Join(filepath.Dir(dir), "models.json"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if workDir != "" {
		for _, candidate := range []string{
			filepath.Join(workDir, "models.json"),
			filepath.Join(workDir, "..", "models.json"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	// Where the installers and the .deb/.rpm/Homebrew packages put it.
	var shared []string
	if home, err := os.UserHomeDir(); err == nil {
		shared = append(shared, filepath.Join(home, ".local/share/dashd/models.json"))
	}
	shared = append(shared, "/usr/share/dashd/models.json", "/usr/local/share/dashd/models.json")
	for _, candidate := range shared {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "models.json"
}
