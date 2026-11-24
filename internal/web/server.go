// Package web serves the dashboard.
//
// The handler is read-only with respect to the store and holds no scanner state,
// so a scan in progress is invisible to a page load: WAL gives each request a
// consistent snapshot while the writer continues. That is what keeps the
// website responsive while the background half of the process is busy, without
// either half needing to know about the other.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

//go:embed assets/dashboard.css assets/dashboard.js assets/app.js assets/session.js
var assets embed.FS

//go:embed templates/*.html
var templateFS embed.FS

// Server serves the dashboard from a store.
type Server struct {
	store *store.Store
	log   *slog.Logger
	tmpl  *template.Template
	// Generated is injectable so a page can show a stable time in tests.
	Generated func() time.Time
	// ScanDuration reports how long the last scan pass took, for /metrics.
	ScanDuration func() time.Duration
	// ScanInfo reports scanner state for the status line. Optional.
	ScanInfo func() (lastRun time.Time, running bool)
	// HealthCacheTTL is how long /healthz reuses its aggregate. Defaults to
	// healthCacheTTL; zero queries the store on every request, which is what a
	// test wants and what a network-exposed instance does not.
	HealthCacheTTL time.Duration
	// ModelsPath, when set, is served at GET /api/models and advertised via
	// models_etag in /api/ingest responses so push clients stay in sync.
	ModelsPath string
	// PriceStatus and PriceAdmin power the Prices panel. Optional.
	PriceStatus *PriceStatus
	PriceAdmin  PriceAdmin
	// RepriceFunc, when set, powers POST /api/reprice.
	RepriceFunc func(context.Context) (int, error)
	// Optional soft spending thresholds for burn-rate alerts & insights.
	DailyBudget   float64
	MonthlyBudget float64

	// AuthEnabled shows the Sign out control.
	AuthEnabled bool
	// CachePayloads reuses a rendered payload until the data generation
	// changes (an ingest, an admin write or a completed scan pass) or 30s pass.
	// Off by default so a test that writes to the store directly sees it.
	CachePayloads bool
	// IngestEnabled turns POST /api/ingest on. New sets it; cmd/dashd turns
	// it off for a local serve-and-scan that has not asked to receive pushes.
	IngestEnabled bool
	// Tags groups projects into clients, teams or cost centres for the
	// Tags breakdown and the ?tag= filter.
	Tags Tags
	// Pricer, when set, prices every ingested call on the server, ignoring
	// whatever cost the client computed.
	Pricer PriceFunc
	// Version is reported to push clients.
	Version string
	// TrustProxy uses X-Forwarded-For for rate limiting.
	TrustProxy bool
	// PruneFunc, BackupFunc power the admin endpoints. Optional.
	PruneFunc func(ctx context.Context, before time.Time, opt store.PruneOptions) (int64, error)

	health         healthCache
	ingestMu       sync.Mutex
	ingestLimiter  rateLimiter
	ingestCalls    atomic.Int64
	ingestRequests atomic.Int64
	generation     atomic.Int64
	cache          payloadCache
}

// PriceFunc prices one call from its token counts. out includes reasoning.
type PriceFunc func(model string, in, out, cacheRead, cacheWrite, cacheWrite1h, ts int64) (float64, bool)

// bumpGeneration invalidates cached page payloads after a write.
func (s *Server) bumpGeneration() { s.generation.Add(1) }

// New builds a Server.
func New(st *store.Store, log *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(Funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: st, log: log, tmpl: tmpl, Generated: time.Now,
		HealthCacheTTL: healthCacheTTL, IngestEnabled: true,
		ingestLimiter: rateLimiter{rate: 20, burst: 100}}, nil
}

// Handler returns the HTTP handler for the dashboard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/assets/", s.handleAsset)
	mux.HandleFunc("/api/activity", s.handleActivity)
	mux.HandleFunc("/api/ingest", s.handleIngest)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/reprice", s.handleReprice)
	mux.HandleFunc("/api/admin/price", s.handleAdminPrice)
	mux.HandleFunc("/api/admin/prune", s.handleAdminPrune)
	mux.HandleFunc("/api/admin/forget-session", s.handleAdminForgetSession)
	mux.HandleFunc("/api/admin/rename-machine", s.handleAdminRenameMachine)
	mux.HandleFunc("/api/sessions", s.handleSessionsAPI)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.Handle("/session", http.HandlerFunc(s.handleSession))
	// Outermost, so the headers are on every response the dashboard produces —
	// including the assets, which is where a policy that only covered documents
	// would be one bypass away from useless.
	return withSecurityHeaders(withRequestGuards(logRequests(s.log, withGzip(mux))))
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/" {
			log.Debug("request", "method", r.Method, "path", r.URL.Path,
				"duration", time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	switch name {
	case "dashboard.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case "dashboard.js", "app.js", "session.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// A stale stylesheet is a confusing bug report, so the browser revalidates
	// every time and an unchanged asset costs an empty 304.
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", tag)
	if strings.Contains(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(data)
}

// handleHealth answers the unauthenticated liveness probe.
//
// Everything here is either a count or a timestamp. That is not incidental: the
// endpoint is open on purpose so a container healthcheck does not need the auth
// token in its command, and the reason that is defensible is that there is
// nothing here to disclose. No paths, no models, no titles.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := map[string]any{"ok": true}
	// The aggregate is cached, because this endpoint is unauthenticated and
	// would otherwise let anyone who can reach the port run a full scan of the
	// call table as often as they like. See healthCacheTTL.
	if counts, ok := s.healthCountsFor(ctx); ok {
		status["calls"] = counts.calls
		status["sessions"] = counts.sessions
	} else {
		status["ok"] = false
	}
	if s.ScanInfo != nil {
		lastRun, running := s.ScanInfo()
		if lastRun.IsZero() {
			// No pass has completed yet. The zero time would otherwise render
			// as 0001-01-01T00:00:00Z, which parses as a valid timestamp two
			// thousand years in the past.
			status["last_scan"] = nil
		} else {
			status["last_scan"] = lastRun.Format(time.RFC3339)
		}
		status["scan_running"] = running
	}
	writeJSON(w, status)
}

// ---------------------------------------------------------------- index

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// A browser opening the bare dashboard lands on its remembered range.
	if r.URL.RawQuery == "" && acceptsHTML(r) {
		rng := defaultRange
		if c, err := r.Cookie(rangeCookie); err == nil && validRange(c.Value) {
			rng = c.Value
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/?range="+rng, http.StatusFound)
		return
	}
	ctx := r.Context()
	filter, _, err := parseFilter(r.URL.Query())
	if err != nil {
		s.errorPage(w, http.StatusBadRequest, "Bad filter", err)
		return
	}

	var data pageData
	var err2 error
	gen := ""
	cached := false
	if s.CachePayloads {
		gen = s.dataGeneration()
		data, cached = s.cache.get(r.URL.RawQuery, gen, time.Now())
	}
	if !cached {
		data, err2 = s.buildPayload(ctx, filter, r.URL.Query())
		if err2 == nil && s.CachePayloads {
			s.cache.put(r.URL.RawQuery, gen, time.Now(), data)
		}
	}
	err = err2
	if err != nil {
		s.log.Error("could not build dashboard", "error", err)
		s.errorPage(w, http.StatusInternalServerError, "Could not read the database",
			fmt.Errorf("the scan may still be populating it; try again in a moment"))
		return
	}
	// The payload goes into two inline <script> blocks, so it needs the nonce
	// from withSecurityHeaders: the CSP permits those blocks and nothing else.
	data.Nonce = nonceFrom(ctx)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.Error("template failed", "error", err)
	}
}

func (s *Server) errorPage(w http.ResponseWriter, code int, title string, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	s.tmpl.ExecuteTemplate(w, "error.html", map[string]any{
		"Title": title, "Error": err.Error(), "Code": code,
	})
}

// pageData is what the template renders from.
type pageData struct {
	Generated string
	StatCards []statCard
	// Nonce is the per-response CSP nonce, carried to the two inline <script>
	// blocks that hold the payload and the filter state.
	Nonce       string
	PayloadJSON template.JS
	FilterState filterState
	Facets      facets
	Scan        []scanStatusView
	ScanSummary string
	// Burn is the cost headline: Today, 7d, 30d and month-to-date, each with
	// the change against the equivalent prior period.
	Burn     []burnCard
	BurnNote string
	// Sources is the source-detection panel: one row per agent this build can
	// read, and whether its logs were found. SourcesNeedAction decides whether
	// it is shown at all.
	Sources           []sourceView
	SourcesSummary    string
	SourcesNeedAction bool
	Machines          []machineStatusView
	Prices            priceView
	PriceChanges      []priceChangeView
	Unpriced          []store.UnpricedModel
	Insights          []Insight
	TopInsights       []Insight
	GeneratedEpoch    int64
	AuthEnabled       bool
	// Empty is a fresh install: no calls at all and no filter hiding them.
	Empty bool
}

type machineStatusView struct {
	Machine        string
	LastSeenAgo    string
	SessionsPushed int
	CallsPushed    int
	LastError      string
	LastErrorAgo   string
	Version        string
	// Stale is a machine not heard from in over an hour.
	Stale bool
}

type facets struct {
	Tags     []string
	Models   []string
	Agents   []string
	Projects []string
	Machines []string
	MinDate  string
	MaxDate  string
}

type scanStatusView struct {
	Agent        string
	LastScan     string
	LastScanAgo  string
	FilesSeen    int
	FilesChanged int
	CallsAdded   int
	Error        string
}

func (s *Server) buildPayload(ctx context.Context, filter store.Filter, query url.Values) (pageData, error) {
	var totals store.Totals
	var daily, burnDaily []store.DayBucket
	var models []store.ModelStat
	var projects []store.ProjectStat
	var machines []store.MachineStat
	var tools []store.ToolStat
	var projectModels []store.ProjectModel
	var projectTools []store.ProjectTool
	var modelNames, agentNames, projectNames []string
	var minTS, maxTS int64
	var signals store.InsightSignals
	var comparison map[string]any
	var yesterdayByNow *store.Totals

	// A ?tag= filter expands into the projects the tag covers.
	s.applyTagFilter(ctx, &filter, query)

	// The page renders a dozen rollups; each must observe the same database
	// state, so they all run inside one read transaction. Under WAL this does
	// not block the writer, and a scan in progress cannot tear the payload.
	if err := s.store.ReadTx(ctx, func(st *store.Store) error {
		var err error
		if totals, err = st.Totals(ctx, filter); err != nil {
			return err
		}
		if daily, err = st.Daily(ctx, filter); err != nil {
			return err
		}
		// The burn cards are about "now" — today, the last 7 and 30 days,
		// this month — so a date range must not truncate them. Every other
		// filter still applies.
		burnDaily = daily
		if filter.DateFrom != nil || filter.DateTo != nil {
			bf := filter
			bf.DateFrom, bf.DateTo = nil, nil
			if burnDaily, err = st.Daily(ctx, bf); err != nil {
				return err
			}
		}
		{
			at := s.Generated()
			from, to := localDay(at).AddDate(0, 0, -1), at.AddDate(0, 0, -1)
			yf := filter
			yf.DateFrom, yf.DateTo = &from, &to
			t, err := st.Totals(ctx, yf)
			if err != nil {
				return err
			}
			yesterdayByNow = &t
		}
		if models, err = st.Models(ctx, filter); err != nil {
			return err
		}
		if projects, err = st.Projects(ctx, filter); err != nil {
			return err
		}
		if machines, err = st.MachinesBreakdown(ctx, filter); err != nil {
			return err
		}
		if tools, err = st.Tools(ctx, filter); err != nil {
			return err
		}
		if projectModels, err = st.ProjectModels(ctx, filter); err != nil {
			return err
		}
		if projectTools, err = st.ProjectTools(ctx, filter); err != nil {
			return err
		}
		modelNames, agentNames, projectNames, minTS, maxTS, err = st.Facets(ctx)
		if err != nil {
			return err
		}
		if signals, err = st.InsightSignals(ctx, filter, totals); err != nil {
			return err
		}
		comparison, err = comparisonJSON(ctx, st, filter, s.Generated())
		return err
	}); err != nil {
		return pageData{}, err
	}

	now := s.Generated()
	burn := burnWindows(burnDaily, now)
	if yesterdayByNow != nil {
		// A part day against a whole one always looks like a saving.
		burn[0].PriorCost, burn[0].PriorLabel = yesterdayByNow.Cost, "yesterday by this time"
	}
	fc := monthForecast(burn, now)
	savings := cacheSavings(models, s.Pricer)
	machineViews := s.machineStatusViews(ctx, now)

	// The headline figures sit at the top level rather than under a key: they are
	// what the stat cards read, and there is one set of them per page.
	provStats := model.RollupProviders(models, totals.Cost)
	payload := map[string]any{
		"generatedEpoch":  now.Unix(),
		"dailyStats":      dailyJSON(daily),
		"models":          modelsJSON(models, totals.Cost),
		"providers":       providersJSON(provStats),
		"projects":        projectsJSON(projects, projectModels, projectTools),
		"machines":        machinesJSON(machines, totals.Cost),
		"tools":           toolsJSON(tools, totals.ToolSeconds),
		"tags":            tagsJSON(s.Tags, projects, totals.Cost),
		"unpricedCalls":   totals.UnpricedCalls,
		"firstActivity":   totals.FirstTS,
		"lastActivity":    totals.LastTS,
		"cacheSavingsUSD": savings,
		"timezone":        tzName(now),
		"authEnabled":     s.AuthEnabled,
		"comparison":      comparison,
	}
	for k, v := range totalsJSON(totals) {
		payload[k] = v
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return pageData{}, fmt.Errorf("encode payload: %w", err)
	}

	machineNames, err := s.store.Machines(ctx)
	if err != nil {
		return pageData{}, err
	}
	view := facets{Models: modelNames, Agents: agentNames, Projects: projectNames, Machines: machineNames,
		Tags: s.Tags.Names()}
	if minTS > 0 {
		view.MinDate = time.Unix(minTS, 0).Local().Format("2006-01-02")
	}
	if maxTS > 0 {
		view.MaxDate = time.Unix(maxTS, 0).Local().Format("2006-01-02")
	}

	views, _ := s.scanViews(ctx)
	sources := sourceViews(views)
	prices := s.PriceStatus.view(now)
	pricePoints, err := s.store.PriceHistory(ctx)
	if err != nil {
		return pageData{}, err
	}
	unpriced, err := s.store.UnpricedModels(ctx)
	if err != nil {
		return pageData{}, err
	}
	insights := BuildInsightsFull(InsightInputs{
		Signals: signals, Totals: totals, Burn: burn, Forecast: fc,
		DailyBudget: s.DailyBudget, MonthlyBudget: s.MonthlyBudget,
		CacheSavings: savings, Machines: machineViews, PriceError: prices.LastError, Query: query, Now: now,
	})
	cards := burnCards(burn)
	if fc.OK {
		cards = append(cards, forecastCard(fc, s.MonthlyBudget))
	}
	stat := statCards(totals)
	if savings > 0 {
		for i := range stat {
			if stat[i].Label == "Total Tokens" {
				stat[i].Detail = append(stat[i].Detail, statDetail{"Cache saved", "~" + usd(savings)})
			}
		}
	}
	burnNoteText := burnNote(burn)
	if filter.DateFrom != nil || filter.DateTo != nil {
		burnNoteText += " Burn cards ignore the date range so they always describe now; other filters apply."
	}
	burnNoteText += " Days are bucketed in " + tzName(now) + "."
	return pageData{
		Generated:      now.Format("2006-01-02 15:04:05"),
		GeneratedEpoch: now.Unix(),
		StatCards:      stat,
		PayloadJSON:    template.JS(encoded),
		FilterState:    parseFilterState(query),
		Facets:         view,
		Scan:           views,
		ScanSummary:    scanSummary(views, machineViews),
		Burn:           cards,
		BurnNote:       burnNoteText,
		Sources:        sources,
		SourcesSummary: sourceSummary(sources),
		Machines:       machineViews,
		Prices:         prices,
		PriceChanges:   priceChanges(pricePoints),
		Unpriced:       unpriced,
		Insights:       insights,
		TopInsights:    topInsights(insights),
		AuthEnabled:    s.AuthEnabled,
		Empty:          totals.Calls == 0 && maxTS == 0,
		// Shown whenever a local scan ran and a source needs attention, or when
		// the database holds no calls at all (fresh install). A serve-only
		// instance receiving remote pushes has no local scan_status rows and
		// already has calls, so it should not warn about missing local logs.
		SourcesNeedAction: (len(views) > 0 && anyNeedsAttention(sources)) || (totals.Calls == 0 && maxTS == 0),
	}, nil
}

func filterActive(f store.Filter) bool {
	return len(f.Models) > 0 || len(f.Agents) > 0 || len(f.Projects) > 0 || len(f.Machines) > 0 ||
		f.UnpricedOnly || f.DateFrom != nil || f.DateTo != nil
}

// topInsights is the Key Insights strip: actionable items first (warn, then
// tip), falling back to info only when there is nothing to act on.
func topInsights(all []Insight) []Insight {
	var out []Insight
	for _, sev := range []string{"warn", "tip"} {
		for _, in := range all {
			if in.Severity == sev {
				out = append(out, in)
			}
		}
	}
	if len(out) == 0 {
		out = all
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func tzName(now time.Time) string {
	name, _ := now.Local().Zone()
	if loc := time.Local.String(); loc != "" && loc != "Local" {
		return loc + " (" + name + ")"
	}
	return name
}

func (s *Server) machineStatusViews(ctx context.Context, now time.Time) []machineStatusView {
	rows, err := s.store.MachineStatuses(ctx)
	if err != nil {
		return nil
	}
	out := make([]machineStatusView, 0, len(rows))
	for _, r := range rows {
		ago := "never"
		if r.LastSeenAt > 0 {
			ago = humanAge(now.Sub(time.Unix(r.LastSeenAt, 0)))
		}
		v := machineStatusView{
			Machine:        r.Machine,
			LastSeenAgo:    ago,
			SessionsPushed: r.SessionsPushed,
			CallsPushed:    r.CallsPushed,
			LastError:      r.LastError,
			Version:        r.Version,
			Stale:          r.LastSeenAt > 0 && now.Sub(time.Unix(r.LastSeenAt, 0)) > time.Hour,
		}
		if r.LastError != "" && r.LastErrorAt > 0 {
			v.LastErrorAgo = humanAge(now.Sub(time.Unix(r.LastErrorAt, 0)))
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) scanViews(ctx context.Context) ([]scanStatusView, error) {
	statuses, err := s.store.ScanStatuses()
	if err != nil {
		return nil, err
	}
	now := s.Generated()
	out := make([]scanStatusView, 0, len(statuses))
	for _, st := range statuses {
		views := scanStatusView{
			Agent:        st.Agent,
			FilesSeen:    st.FilesSeen,
			FilesChanged: st.FilesChanged,
			CallsAdded:   st.CallsIngested,
			Error:        st.Error,
		}
		if !st.LastScanAt.IsZero() {
			views.LastScan = st.LastScanAt.Format("15:04:05")
			views.LastScanAgo = humanAge(now.Sub(st.LastScanAt))
		}
		out = append(out, views)
	}
	return out, nil
}

func scanSummary(views []scanStatusView, machines []machineStatusView) string {
	if len(views) == 0 {
		if len(machines) > 0 {
			noun := "machine"
			if len(machines) != 1 {
				noun = "machines"
			}
			return fmt.Sprintf("%d %s · last push %s", len(machines), noun, machines[0].LastSeenAgo)
		}
		return "no scan has run yet"
	}
	newest := views[0].LastScanAgo
	var errs []string
	for _, v := range views {
		if v.Error != "" {
			errs = append(errs, v.Agent)
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return "last scan " + newest + "; failing: " + strings.Join(errs, ", ")
	}
	return "last scan " + newest
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		http.Error(w, "missing uid", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	row, ok, err := s.store.Session(ctx, uid)
	if err != nil {
		s.log.Error("could not load session", "uid", uid, "error", err)
		s.errorPage(w, http.StatusInternalServerError, "Could not load session",
			fmt.Errorf("the database could not be read; see the server log"))
		return
	}
	if !ok {
		s.errorPage(w, http.StatusNotFound, "Unknown session",
			fmt.Errorf("no session with id %q is stored; it may have been pruned", uid))
		return
	}
	calls, err := s.store.SessionCalls(ctx, uid)
	if err != nil {
		s.log.Error("could not load session calls", "uid", uid, "error", err)
		s.errorPage(w, http.StatusInternalServerError, "Could not load session",
			fmt.Errorf("the database could not be read; see the server log"))
		return
	}
	tools, err := s.store.SessionTools(ctx, uid)
	if err != nil {
		s.log.Warn("could not load session tools", "uid", uid, "error", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "session.html", map[string]any{
		"Generated":   s.Generated().Format("2006-01-02 15:04:05"),
		"Session":     sessionView(row),
		"Calls":       callViews(calls),
		"ModelSplit":  sessionModelSplit(calls, row.Cost),
		"Tools":       sessionToolViews(tools),
		"ToolSummary": sessionToolSummary(tools),
		"Spark":       sessionSpark(calls),
		"ResumeCmd":   resumeCommand(row),
		"ProjectURL":  "/?project=" + url.QueryEscape(row.Project),
		"MachineURL":  "/?machine=" + url.QueryEscape(row.Machine),
		"Nonce":       nonceFrom(ctx),
	}); err != nil {
		s.log.Error("session template failed", "error", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		slog.Default().Error("json encode failed", "error", err)
	}
}

// ---------------------------------------------------------------- filtering

type filterState struct {
	Models       []string
	Agents       []string
	Projects     []string
	Machines     []string
	UnpricedOnly bool
	DateFrom     string
	DateTo       string
	Range        string
	Active       bool
}

func parseFilter(q url.Values) (store.Filter, filterState, error) {
	multi := func(key string) []string {
		var out []string
		for _, v := range q[key] {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
		return out
	}
	unpricedOnly := q.Get("unpriced") == "1" || q.Get("unpriced") == "true"
	state := filterState{
		Models:       multi("model"),
		Agents:       multi("agent"),
		Projects:     multi("project"),
		Machines:     multi("machine"),
		UnpricedOnly: unpricedOnly,
		DateFrom:     q.Get("date_from"),
		DateTo:       q.Get("date_to"),
	}
	state.Active = len(state.Models) > 0 || len(state.Agents) > 0 ||
		len(state.Projects) > 0 || len(state.Machines) > 0 ||
		state.UnpricedOnly || state.DateFrom != "" || state.DateTo != ""

	filter := store.Filter{
		Models:       state.Models,
		Agents:       state.Agents,
		Projects:     state.Projects,
		Machines:     state.Machines,
		UnpricedOnly: state.UnpricedOnly,
	}

	// A rolling range stands in for dates it would otherwise go stale as, so a
	// bookmark or saved view of "30d" still means 30 days next week. Explicit
	// dates win.
	if state.DateFrom == "" && state.DateTo == "" {
		if from, to, ok := rangeDates(q.Get("range"), time.Now()); ok {
			filter.DateFrom, filter.DateTo = &from, &to
			state.Range = q.Get("range")
		}
	}

	// A date is interpreted in the reader's own timezone, and a "to" date
	// covers the whole of that day rather than stopping at midnight.
	if state.DateFrom != "" {
		t, err := time.ParseInLocation("2006-01-02", state.DateFrom, time.Local)
		if err != nil {
			return filter, state, fmt.Errorf("date_from %q is not a date", state.DateFrom)
		}
		filter.DateFrom = &t
	}
	if state.DateTo != "" {
		t, err := time.ParseInLocation("2006-01-02", state.DateTo, time.Local)
		if err != nil {
			return filter, state, fmt.Errorf("date_to %q is not a date", state.DateTo)
		}
		end := t.AddDate(0, 0, 1).Add(-time.Second)
		filter.DateTo = &end
	}
	return filter, state, nil
}

// defaultRange is the date range of a bare "/" for a browser that has not
// chosen one. All time is a figure nobody acts on.
const defaultRange = "30d"

// rangeCookie remembers the last preset a browser picked.
const rangeCookie = "dashd_range"

func validRange(name string) bool {
	_, _, ok := rangeDates(name, time.Now())
	return ok || name == "all"
}

// rangeDates resolves a rolling range to the local days it covers today.
func rangeDates(name string, now time.Time) (from, to time.Time, ok bool) {
	today := localDay(now)
	to = today.AddDate(0, 0, 1).Add(-time.Second)
	switch name {
	case "today":
		from = today
	case "7d":
		from = today.AddDate(0, 0, -6)
	case "30d":
		from = today.AddDate(0, 0, -29)
	case "90d":
		from = today.AddDate(0, 0, -89)
	case "mtd":
		from = today.AddDate(0, 0, 1-today.Day())
	default:
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// parseFilterState re-reads the query for the template, which needs the raw
// strings the selects are pre-filled from rather than the parsed form.
func parseFilterState(query url.Values) filterState {
	_, state, err := parseFilter(query)
	if err != nil {
		return filterState{}
	}
	return state
}

// payloadCache keeps the most recent rendered payloads, keyed by the query and
// the data generation, so repeated page loads between scans skip the dozen
// rollups a page costs.
type payloadCache struct {
	mu      sync.Mutex
	entries map[string]cachedPayload
}

type cachedPayload struct {
	gen  string
	at   time.Time
	data pageData
}

const payloadCacheTTL = 30 * time.Second

func (c *payloadCache) get(key, gen string, now time.Time) (pageData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.gen != gen || now.Sub(e.at) > payloadCacheTTL {
		return pageData{}, false
	}
	return e.data, true
}

func (c *payloadCache) put(key, gen string, now time.Time, d pageData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) > 64 {
		c.entries = map[string]cachedPayload{}
	}
	c.entries[key] = cachedPayload{gen: gen, at: now, data: d}
}

// dataGeneration changes whenever stored data may have: an ingest or admin
// write bumps the counter, and a completed scan pass moves lastRun.
func (s *Server) dataGeneration() string {
	g := fmt.Sprint(s.generation.Load())
	if s.ScanInfo != nil {
		last, _ := s.ScanInfo()
		g += ":" + fmt.Sprint(last.UnixNano())
	}
	return g
}
