// Package server exposes the review workflow over HTTP: a JSON API for
// operations and suggestions plus an embedded Bulma/Alpine review UI.
// It binds to localhost by default (see config http_addr) — /api/apply mutates
// the filesystem, so wider exposure must be a deliberate choice.
package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/i18n"
	"knowledge-ba46-flow/internal/logging"
	repositoryreport "knowledge-ba46-flow/internal/report"
)

//go:embed static/index.html
var landingUI []byte

//go:embed static/knowledge.html
var knowledgeUI []byte

//go:embed static/report.html
var reportUI []byte

//go:embed static
var staticFiles embed.FS

// Listen starts the HTTP server on c.HTTPAddr and blocks until the process is
// interrupted (SIGINT/SIGTERM), then shuts down gracefully.
func Listen(ctx context.Context, c config.Config, db *database.DB) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A 'running' job cannot survive a restart — mark leftovers as failed so
	// they don't show up as perpetually running in job history.
	if err := db.FailStaleJobs(context.WithoutCancel(ctx)); err != nil {
		return err
	}

	stopCh := make(chan struct{})
	srv := &http.Server{
		Addr:              c.HTTPAddr,
		Handler:           NewMux(c, db, stopCh),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logging.Infof("KBM-flow listening on http://%s", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
	case <-stopCh:
		logging.Infof("shutdown requested via the web UI — stopping gracefully")
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// NewMux builds the API, landing page, knowledge-review UI, and report UI.
// stop may be nil — the shutdown endpoint then reports unavailable; production
// use goes through Listen, which wires it to graceful server shutdown.
func NewMux(c config.Config, db *database.DB, stop chan<- struct{}) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/documents", documents(db))
	mux.HandleFunc("GET /api/models", modelsData(c, db))
	mux.HandleFunc("GET /api/suggestions", suggestions(db))
	mux.HandleFunc("GET /api/suggestions/{id}", suggestion(db))
	o := &ops{c: c, db: db}
	mux.HandleFunc("POST /api/suggestions/{id}/decide", o.decide)
	mux.HandleFunc("POST /api/apply", o.applyAccepted)
	mux.HandleFunc("POST /api/ops", o.create)
	mux.HandleFunc("GET /api/ops", o.listJobs)
	mux.HandleFunc("GET /api/ops/{id}", o.get)
	mux.HandleFunc("GET /api/report", reportData(c, db))
	mux.HandleFunc("GET /api/report/history", reportHistory(db))
	mux.HandleFunc("GET /api/progress", knowledgeProgress(db))
	mux.HandleFunc("GET /api/i18n", i18nData)
	mux.HandleFunc("GET /api/dashboard", landingData(c, db))
	mux.HandleFunc("POST /api/report/run", o.createReport)
	mux.HandleFunc("GET /api/report/jobs", o.listReportJobs)
	mux.HandleFunc("GET /api/report/jobs/{id}", o.getReportJob)
	mux.HandleFunc("POST /api/shutdown", o.shutdown(stop))
	dev := devStaticFS()
	mux.HandleFunc("GET /knowledge", servePage(dev, "knowledge.html", knowledgeUI))
	mux.HandleFunc("GET /knowledge/{$}", servePage(dev, "knowledge.html", knowledgeUI))
	mux.HandleFunc("GET /report", servePage(dev, "report.html", reportUI))
	mux.HandleFunc("GET /report/{$}", servePage(dev, "report.html", reportUI))
	mux.Handle("GET /static/", staticHandler(dev))
	mux.HandleFunc("POST /api/client-log", clientLog)
	mux.HandleFunc("GET /{$}", servePage(dev, "index.html", landingUI)) // {$} matches the root path only
	return securityHeaders(requestLog(mux))
}

// requestLog emits one DEBUG line per HTTP request — method, path, status,
// duration. Silent unless config log_level is "debug"; static asset hits are
// skipped so polls of the UI don't drown the signal.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logging.Debugf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// clientLog receives the browser's uncaught errors and unhandled rejections
// (see static/clientlog.js) so UI failures land in the server log with their
// stack — a JavaScript error never otherwise reaches the Go process.
func clientLog(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Message string `json:"message"`
		Source  string `json:"source"`
		Line    int    `json:"line"`
		Stack   string `json:"stack"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid client log payload: %w", err))
		return
	}
	where := b.Source
	if b.Line > 0 {
		where = fmt.Sprintf("%s:%d", b.Source, b.Line)
	}
	logging.Infof("client error: %s (%s)", b.Message, where)
	if b.Stack != "" {
		logging.Infof("client stack: %s", b.Stack)
	}
	w.WriteHeader(http.StatusNoContent)
}

// securityHeaders sets baseline response headers on every route. The CSP
// forbids inline scripts and remote origins; script-src is 'self' alone
// because the vendored @alpinejs/csp build evaluates directives without
// `new Function` — 'unsafe-eval' must not return.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// staticHandler serves locally embedded Bulma and Alpine assets — or the dev
// directory when KBM_DEV_STATIC is set. Keeping them in the binary makes the
// dashboard usable offline and avoids third-party requests from the user's
// browser.
func staticHandler(devFS fs.FS) http.Handler {
	files := devFS
	if files == nil {
		var err error
		if files, err = fs.Sub(staticFiles, "static"); err != nil {
			return http.NotFoundHandler()
		}
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(files)))
}

// documents returns every tracked document with its analysis metadata.
func documents(db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		docs, err := db.Documents(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, docs)
	}
}

// suggestions lists suggestions filtered by ?status= (default "pending").
// proposed_content is stripped — it can be huge; GET /api/suggestions/{id} has it.
func suggestions(db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "pending"
		}
		sugs, err := db.Suggestions(r.Context(), status)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for i := range sugs {
			sugs[i].ProposedContent = ""
		}
		writeJSON(w, http.StatusOK, sugs)
	}
}

// suggestion returns one suggestion in full, including proposed_content.
func suggestion(db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := db.Suggestion(r.Context(), pathID(r))
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("suggestion not found"))
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	}
}

// decideBody is the POST body for the decide endpoint.
type decideBody struct {
	Status string `json:"status"` // "accepted" or "rejected"
	// Note is the reviewer's optional "why" — stored on the suggestion and
	// shown back to the model as review feedback on later runs.
	Note string `json:"note"`
}

// decide transitions a pending suggestion to accepted/rejected — the HTTP
// equivalent of `KBM-flow accept|reject ID`.
func decide(db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b decideBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
			return
		}
		if b.Status != "accepted" && b.Status != "rejected" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("status must be 'accepted' or 'rejected'"))
			return
		}
		if err := db.Decide(r.Context(), pathID(r), b.Status, b.Note); err != nil {
			writeErr(w, http.StatusConflict, err) // already decided or unknown id
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": b.Status})
	}
}

// devStaticFS returns a filesystem rooted at the directory named by
// KBM_DEV_STATIC, or nil when the variable is unset or not a directory.
// With it set, the UI pages and /static/ assets are read from disk on every
// request — frontend work then only needs a save and a browser refresh, not
// a rebuild. Development use only.
func devStaticFS() fs.FS {
	dir := os.Getenv("KBM_DEV_STATIC")
	if dir == "" {
		return nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		logging.Infof("KBM_DEV_STATIC=%q is not a directory — serving embedded UI assets", dir)
		return nil
	}
	logging.Infof("serving UI assets from %s (KBM_DEV_STATIC dev mode)", dir)
	return os.DirFS(dir)
}

// servePage serves one of the UI pages: from devFS when KBM_DEV_STATIC is
// set, else the embedded copy.
func servePage(devFS fs.FS, name string, embedded []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if devFS != nil {
			http.ServeFileFS(w, r, devFS, name)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Embedded static page — no user input to escape.
		// nosemgrep: no-direct-write-to-responsewriter
		_, _ = w.Write(embedded)
	}
}

func reportData(c config.Config, db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		empty := repositoryreport.Snapshot{
			Configured:    c.Report.RegistryPath != "",
			RemoteEnabled: c.Report.WantsRemote(),
			Scope:         "local-only",
			Repos:         []repositoryreport.Repository{},
		}
		if !empty.Configured {
			writeJSON(w, http.StatusOK, empty)
			return
		}
		_, data, err := db.RepoReportSnapshot(r.Context())
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusOK, empty)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !json.Valid(data) {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("stored report snapshot is invalid"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// JSON snapshot served as application/json — not rendered as HTML.
		// nosemgrep: no-direct-write-to-responsewriter
		_, _ = w.Write(data)
	}
}

// reportHistory returns per-run report summaries oldest-first for the
// progress-over-time chart on the report page.
func reportHistory(db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		points, err := db.RepoReportHistory(r.Context(), 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if points == nil {
			points = []database.ReportHistoryPoint{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"points": points})
	}
}

// knowledgeProgress aggregates per-day document/suggestion pipeline events
// for the progress chart on the knowledge page.
func knowledgeProgress(db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, err := db.KnowledgeProgress(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if days == nil {
			days = []database.KnowledgeProgressDay{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"days": days})
	}
}

// i18nData serves the UI's message catalog. ?lang=<tag> is the explicit
// override the dashboard's locale picker sends; otherwise the browser's
// Accept-Language negotiates. The response is the flattened key → message
// map Alpine's t() resolves, plus the shipped locale list for the picker.
func i18nData(w http.ResponseWriter, r *http.Request) {
	pref := r.URL.Query().Get("lang")
	if pref == "" {
		pref = r.Header.Get("Accept-Language")
	}
	tag, messages, err := i18n.Messages(pref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Vary", "Accept-Language")
	writeJSON(w, http.StatusOK, map[string]any{
		"locale":   tag,
		"locales":  i18n.Supported(),
		"messages": messages,
	})
}

// pathID parses the {id} path wildcard; non-numeric values yield -1, which can
// never match a real AUTOINCREMENT id.
func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
