package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"knowledge-ba46-flow/internal/analyzer"
	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/linkcheck"
	"knowledge-ba46-flow/internal/logging"
	"knowledge-ba46-flow/internal/modelselection"
	"knowledge-ba46-flow/internal/ollama"
	repositoryreport "knowledge-ba46-flow/internal/report"
	"knowledge-ba46-flow/internal/review"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/websearch"
)

// opRequest is the POST body for starting an operation job.
type opRequest struct {
	// Op names the work: deterministic scan and link-check, the LLM passes
	// enrich/analyze/run, and the optional tasks embed, expand and freshness.
	Op string `json:"op"`
	// Reanalyze (analyze/run/tasks only) also reprocesses documents whose last
	// pass ran under a different model than the configured one.
	Reanalyze bool `json:"reanalyze"`
	// ReanalyzeAll explicitly refreshes all documents after prompt/taxonomy changes.
	ReanalyzeAll bool `json:"reanalyze_all"`
	// External (analyze/run/expand/freshness) opts into the enhanced lane:
	// the pass uses ollama_model_external (e.g. a cloud model). For analyze/run
	// it stamps the external_* markers, leaving the local analyzed_* lineage
	// untouched; expand and freshness keep their own per-task lineage.
	External bool `json:"external"`
	// Model overrides every generation stage of this run with one configured or
	// installed model. Capability and remote-consent checks run before the job
	// starts. Empty means each stage uses its lane default or task route.
	Model string `json:"model"`
	// EmbedModel independently selects an embedding model for analyze/run/all.
	EmbedModel string `json:"embed_model"`
	// AllowCloud opts into remote embeddings/enrichment; remote generation uses
	// External instead.
	AllowCloud bool `json:"allow_cloud"`
	// AllowRemote consents to remote/cloud execution for the models this run
	// resolves — defaults or overrides — on every lane, without switching to
	// the external lane model. Routing and lineage stamps stay unchanged.
	AllowRemote bool `json:"allow_remote"`
	// WebVerify (freshness/all only) sends each flagged claim to Ollama's
	// hosted web search so a second pass can judge it against live evidence.
	// Claim text leaves the machine; requires OLLAMA_API_KEY.
	WebVerify bool `json:"web_verify"`
	// DryRun returns a queue estimate — documents pending per stage, the
	// analyze lane's evidence-dependents expansion and model-mismatch
	// warnings — instead of starting a job.
	DryRun bool `json:"dry_run"`
}

// validOps are the job ops /api/ops accepts.
var validOps = map[string]bool{
	"scan": true, "analyze": true, "run": true, "link-check": true,
	"enrich": true, "embed": true, "expand": true, "freshness": true,
	"all": true,
}

// ValidOps returns the accepted op names sorted — the single source of truth
// shared by request validation, the error message and the CLI/API parity test.
func ValidOps() []string {
	ops := make([]string, 0, len(validOps))
	for op := range validOps {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return ops
}

// ops runs scan/analyze/run as background jobs persisted in the jobs table —
// analyze can take minutes against a local LLM, far too long for a synchronous
// request, and durable rows mean job history survives a server restart.
// The mutex serialises ops: SQLite is single-writer and ops must not interleave.
// (Restart safety: Listen marks leftover 'running' jobs failed at startup.)
type ops struct {
	c       config.Config
	db      *database.DB
	mu      sync.Mutex
	running bool
}

// create validates the op and starts it as a background job. Returns 202 with
// the job, or 409 if another op is already running.
func (o *ops) create(w http.ResponseWriter, r *http.Request) {
	var b opRequest
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	if !validOps[b.Op] {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("op must be one of %s", strings.Join(ValidOps(), ", ")))
		return
	}
	if b.WebVerify {
		if b.Op != "freshness" && b.Op != "all" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("web_verify only applies to the freshness and all ops"))
			return
		}
		if o.c.OllamaAPIKey == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("web_verify requires OLLAMA_API_KEY in the environment or a .env file next to the config"))
			return
		}
	}
	if b.DryRun {
		c, err := modelselection.Prepare(r.Context(), o.c, b.Op, modelselection.Options{
			Model: b.Model, EmbedModel: b.EmbedModel, External: b.External, AllowCloud: b.AllowCloud,
			AllowRemote: b.AllowRemote, WebVerify: b.WebVerify,
		})
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		o.estimate(w, r, c, b)
		return
	}
	if !o.begin() {
		writeErr(w, http.StatusConflict, fmt.Errorf("another operation is already running"))
		return
	}
	if o.conflict(w) {
		o.end()
		return
	}
	c, err := modelselection.Prepare(r.Context(), o.c, b.Op, modelselection.Options{
		Model: b.Model, EmbedModel: b.EmbedModel, External: b.External, AllowCloud: b.AllowCloud,
		AllowRemote: b.AllowRemote, WebVerify: b.WebVerify,
	})
	if err != nil {
		o.end()
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	j, err := o.db.CreateJob(r.Context(), b.Op)
	if err != nil {
		o.end()
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The job intentionally outlives this request — durable background jobs
	// are the feature (see exec's context.Background and the job heartbeat).
	go o.exec(j, c, b) // #nosec G118
	writeJSON(w, http.StatusAccepted, j)
}

// createReport starts an isolated report job. The report shares the process
// operation guard and durable job storage, but has separate API/UI routes.
func (o *ops) createReport(w http.ResponseWriter, r *http.Request) {
	if o.c.Report.RegistryPath == "" {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("report is not configured"))
		return
	}
	query := r.URL.Query()
	if query.Get("no_remote") == "1" && query.Get("remote") == "1" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("remote and no_remote cannot both be enabled"))
		return
	}
	options := repositoryreport.RunOptions{
		Remote:     o.c.Report.WantsRemote(),
		Force:      query.Get("force") == "1",
		Repository: query.Get("repo"),
	}
	if query.Get("remote") == "1" {
		options.Remote = true
	}
	if query.Get("no_remote") == "1" {
		options.Remote = false
	}
	if options.Repository != "" {
		if err := repositoryreport.ValidateRepository(o.c.Report, options.Repository); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if !o.begin() {
		writeErr(w, http.StatusConflict, fmt.Errorf("another operation is already running"))
		return
	}
	if o.conflict(w) {
		o.end()
		return
	}
	j, err := o.db.CreateJob(r.Context(), "report")
	if err != nil {
		o.end()
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Report jobs likewise run past the end of the request (see exec).
	go o.execReport(j, options) // #nosec G118
	writeJSON(w, http.StatusAccepted, j)
}

// applyAccepted executes accepted suggestions synchronously. It uses the same
// single-operation guard as scan/analyze/run so an HTTP apply cannot overlap a
// background scan or another apply request.
func (o *ops) applyAccepted(w http.ResponseWriter, r *http.Request) {
	if !o.begin() {
		writeErr(w, http.StatusConflict, fmt.Errorf("another operation is already running"))
		return
	}
	defer o.end()
	// Apply mutates files — refuse while a CLI scan/analyze/run holds a job,
	// or reads and writes could race mid-flight.
	if o.conflict(w) {
		return
	}

	results, err := review.Apply(context.WithoutCancel(r.Context()), o.c, o.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// decide serializes review-state changes with scans, analyses, and applies.
func (o *ops) decide(w http.ResponseWriter, r *http.Request) {
	if !o.begin() {
		writeErr(w, http.StatusConflict, fmt.Errorf("another operation is already running"))
		return
	}
	defer o.end()
	decide(o.c, o.db).ServeHTTP(w, r)
}

func (o *ops) begin() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running {
		return false
	}
	o.running = true
	return true
}

func (o *ops) end() {
	o.mu.Lock()
	o.running = false
	o.mu.Unlock()
}

func (o *ops) isRunning() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.running
}

// shutdown stops the server gracefully on an explicit loopback request. It
// refuses while this process runs an operation unless ?force=1 — an orphaned
// job would be swept to 'failed' by its frozen heartbeat, but cutting live
// LLM work should be a deliberate choice.
func (o *ops) shutdown(stop chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if stop == nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("shutdown is not wired"))
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			writeErr(w, http.StatusForbidden, fmt.Errorf("shutdown is only accepted from loopback clients"))
			return
		}
		if o.isRunning() && r.URL.Query().Get("force") != "1" {
			writeErr(w, http.StatusConflict, fmt.Errorf("an operation is running; wait for it or retry with ?force=1"))
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
		go func() {
			time.Sleep(150 * time.Millisecond) // let the response flush first
			select {
			case stop <- struct{}{}:
			default: // already stopping
			}
		}()
	}
}

// conflict reports 409 when a job from another process (e.g. a CLI command or
// a previous server run) is live. Call only while holding begin(); the caller
// owns releasing the guard, including on a conflict.
func (o *ops) conflict(w http.ResponseWriter) bool {
	running, ok, err := o.db.RunningJob(context.Background())
	switch {
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	case ok:
		writeErr(w, http.StatusConflict, fmt.Errorf("job #%d (%s) is already running", running.ID, running.Op))
	}
	return err != nil || ok
}

// listJobs returns the 100 most recent jobs from the jobs table, newest first.
func (o *ops) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := o.db.JobsForOps(r.Context(), 100, "scan", "analyze", "run", "link-check", "enrich", "embed", "expand", "freshness", "all")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (o *ops) listReportJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := o.db.JobsForOps(r.Context(), 100, "report")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// get returns one job by id.
func (o *ops) get(w http.ResponseWriter, r *http.Request) {
	o.jobByID(w, r, false)
}

func (o *ops) getReportJob(w http.ResponseWriter, r *http.Request) {
	o.jobByID(w, r, true)
}

// jobByID returns one job filtered by kind: report jobs only when reportOnly
// is set, non-report jobs otherwise — so the two API surfaces never leak into
// each other.
func (o *ops) jobByID(w http.ResponseWriter, r *http.Request, reportOnly bool) {
	label := "job"
	if reportOnly {
		label = "report job"
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	j, err := o.db.Job(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s not found", label))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if (j.Op == "report") != reportOnly {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s not found", label))
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// exec runs the operation and records the outcome in the jobs table. Runs on a
// background context — the job outlives the request that created it.
func (o *ops) exec(j database.Job, c config.Config, request opRequest) {
	ctx := context.Background()
	stop := o.db.RunHeartbeat(ctx, j.ID)
	defer stop()
	runner := &ops{c: c, db: o.db}
	logging.Debugf("job #%d op=%s started", j.ID, request.Op)
	detail, err := runner.runOp(ctx, j, request)
	status, errText := "done", ""
	if err != nil {
		status, errText = "failed", err.Error()
	}
	if ferr := o.db.FinishJob(ctx, j.ID, status, detail, errText); ferr != nil {
		logging.Infof("WARNING: could not record job #%d result: %v", j.ID, ferr)
	}
	logging.Debugf("job #%d op=%s finished status=%s err=%q", j.ID, request.Op, status, errText)
	o.end()
}

func (o *ops) execReport(j database.Job, options repositoryreport.RunOptions) {
	ctx := context.Background()
	stop := o.db.RunHeartbeat(ctx, j.ID)
	defer stop()
	logging.Debugf("report job #%d started", j.ID)
	options.Progress = func(detail string) {
		if uerr := o.db.UpdateJobDetail(ctx, j.ID, detail); uerr != nil {
			logging.Infof("WARNING: could not update report job #%d progress: %v", j.ID, uerr)
		}
	}
	snapshot, err := repositoryreport.RunWithOptions(ctx, o.c.Report, o.db, options)
	detail, status, errText := "", "done", ""
	if err != nil {
		status, errText = "failed", err.Error()
	} else {
		detail = fmt.Sprintf("Reported %d repositories; %d problems.", snapshot.Summary.Repositories, snapshot.Summary.Problems)
	}
	if ferr := o.db.FinishJob(ctx, j.ID, status, detail, errText); ferr != nil {
		logging.Infof("WARNING: could not record report job #%d result: %v", j.ID, ferr)
	}
	logging.Debugf("report job #%d finished status=%s err=%q", j.ID, status, errText)
	o.end()
}

// runOp executes an operation and returns a one-line summary like the CLI prints.
func (o *ops) runOp(ctx context.Context, j database.Job, request opRequest) (string, error) {
	switch j.Op {
	case "scan":
		return o.scanRepository(ctx)
	case "analyze":
		return o.scanThen(ctx, func() (string, error) { return o.analyze(ctx, j, request) })
	case "run":
		return o.scanThen(ctx, func() (string, error) { return o.analyze(ctx, j, request) })
	case "link-check":
		r, err := linkcheck.Run(ctx, o.c, o.db)
		return r.Summary(), err
	case "enrich":
		return o.scanThen(ctx, func() (string, error) {
			return o.enrich(ctx, j, o.runOptions(ctx, j, request))
		})
	case "embed":
		return o.scanThen(ctx, func() (string, error) {
			return o.embed(ctx, j, o.runOptions(ctx, j, request))
		})
	case "expand", "freshness":
		return o.scanThen(ctx, func() (string, error) {
			return o.task(ctx, j, j.Op, o.runOptions(ctx, j, request))
		})
	case "all":
		return o.scanThen(ctx, func() (string, error) {
			opts := o.runOptions(ctx, j, request)
			analyze := func() (string, error) { return o.analyze(ctx, j, request) }
			return o.runAll(ctx, j, opts, request.Model, analyze)
		})
	case "report":
		r, err := repositoryreport.Run(ctx, o.c.Report, o.db)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Reported %d repositories; %d problems.", r.Summary.Repositories, r.Summary.Problems), nil
	}
	return "", fmt.Errorf("unknown op %q", j.Op)
}

func (o *ops) scanThen(ctx context.Context, run func() (string, error)) (string, error) {
	scanDetail, err := o.scanRepository(ctx)
	if err != nil {
		return scanDetail, err
	}
	detail, err := run()
	return scanDetail + " " + detail, err
}

func (o *ops) scanRepository(ctx context.Context) (string, error) {
	result, err := scanner.Scan(ctx, o.c, o.db)
	detail := fmt.Sprintf("Scanned %d markdown files; %d new/changed.", result.Seen, result.Changed)
	if result.IDsAssigned > 0 {
		detail += fmt.Sprintf(" %d document IDs assigned.", result.IDsAssigned)
	}
	return detail, err
}

func (o *ops) runOptions(ctx context.Context, job database.Job, request opRequest) analyzer.RunOptions {
	return analyzer.RunOptions{
		Reanalyze: request.Reanalyze, ReanalyzeAll: request.ReanalyzeAll, External: request.External,
		Model: request.Model, EmbedModel: request.EmbedModel, AllowCloud: request.AllowCloud,
		AllowRemote: request.AllowRemote, WebVerify: request.WebVerify,
		Progress: func(p analyzer.Progress) {
			if err := o.db.UpdateJobDetail(ctx, job.ID, p.String()); err != nil {
				fmt.Printf("WARNING: could not update job #%d progress: %v\n", job.ID, err)
			}
		},
	}
}

func (o *ops) analyze(ctx context.Context, job database.Job, request opRequest) (string, error) {
	model, err := o.c.TaskModel(config.TaskAnalyze, request.External, request.Model)
	if err != nil {
		return "", err
	}
	fastModel, err := o.c.TaskModel(config.TaskEnrich, false, request.Model)
	if err != nil {
		return "", err
	}
	generation := ollama.New(o.c.OllamaURL, model, o.c.RequestTimeoutSeconds,
		o.c.OllamaNumCtx, o.c.OllamaThink, o.c.OllamaKeepAlive).
		WithModelCheck(ollama.CapabilityCompletion, request.External || request.AllowRemote)
	generators := analyzer.Generators{
		Analysis: generation,
		Enrich: ollama.New(o.c.OllamaURL, fastModel, int(o.c.EnrichmentTimeout().Seconds()),
			o.c.OllamaNumCtx, false, o.c.OllamaKeepAlive).
			WithOutputLimit(o.c.EnrichmentOutputTokens()).
			WithModelCheck(ollama.CapabilityCompletion, request.External || request.AllowCloud || request.AllowRemote),
	}
	embedModel, err := o.c.TaskModel(config.TaskEmbed, false, request.EmbedModel)
	if err != nil && !errors.Is(err, config.ErrNoTaskModel) {
		return "", err
	}
	if embedModel != "" {
		generators.Embed = ollama.New(o.c.OllamaURL, embedModel, o.c.RequestTimeoutSeconds,
			0, false, o.c.OllamaKeepAlive).
			WithModelCheck(ollama.CapabilityEmbedding, request.AllowCloud || request.AllowRemote)
	}
	result, err := analyzer.RunProgress(ctx, o.c, o.db, generators, o.runOptions(ctx, job, request))
	return result.Summary(model, fastModel), err
}

func (o *ops) runAll(
	ctx context.Context,
	j database.Job,
	opts analyzer.RunOptions,
	model string,
	analyze func() (string, error),
) (string, error) {
	var parts []string
	for _, name := range []string{"enrich", "embed", "expand", "freshness", "analyze"} {
		var detail string
		var err error
		switch name {
		case "enrich":
			detail, err = o.enrich(ctx, j, opts)
		case "embed":
			// Generation and embedding overrides are independent.
			embedOpts := opts
			embedOpts.Model = ""
			detail, err = o.embed(ctx, j, embedOpts)
		case "analyze":
			detail, err = analyze()
		default:
			detail, err = o.task(ctx, j, name, opts)
		}
		if err != nil {
			if errors.Is(err, config.ErrNoTaskModel) && (model == "" || name == "embed") {
				continue
			}
			return strings.Join(append(parts, name+" failed"), "; ") + ": " + err.Error(), err
		}
		parts = append(parts, name+": "+detail)
	}
	return strings.Join(parts, "; "), nil
}

// enrich runs only the fast-model metadata pass.
func (o *ops) enrich(ctx context.Context, j database.Job, opts analyzer.RunOptions) (string, error) {
	m, err := o.c.TaskModel(config.TaskEnrich, false, opts.Model)
	if err != nil {
		return "", err
	}
	r, err := analyzer.RunProgress(ctx, o.c, o.db, analyzer.Generators{
		Analysis: ollama.New(o.c.OllamaURL, m, o.c.RequestTimeoutSeconds, o.c.OllamaNumCtx, o.c.OllamaThink, o.c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud || opts.AllowRemote),
		Enrich:   ollama.New(o.c.OllamaURL, m, int(o.c.EnrichmentTimeout().Seconds()), o.c.OllamaNumCtx, false, o.c.OllamaKeepAlive).WithOutputLimit(o.c.EnrichmentOutputTokens()).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud || opts.AllowRemote),
	}, analyzer.RunOptions{
		Reanalyze: opts.Reanalyze, ReanalyzeAll: opts.ReanalyzeAll, EnrichOnly: true, Model: m, Progress: opts.Progress,
	})
	return r.Summary(m, m), err
}

// embed recomputes embedding vectors for documents that lack a current one.
func (o *ops) embed(ctx context.Context, j database.Job, opts analyzer.RunOptions) (string, error) {
	override := opts.EmbedModel
	if override == "" {
		override = opts.Model
	}
	m, err := o.c.TaskModel(config.TaskEmbed, false, override)
	if err != nil {
		return "", err
	}
	r, err := analyzer.RunEmbed(ctx, o.c, o.db, analyzer.Generators{
		Embed: ollama.New(o.c.OllamaURL, m, o.c.RequestTimeoutSeconds, 0, false, o.c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityEmbedding, opts.AllowCloud || opts.AllowRemote),
	}, opts)
	return r.Summary("embed", m), err
}

// estimateStages maps each job op to the stage keys a dry run reports on —
// run all LLM tasks expands to the same fixed order runAll executes.
var estimateStages = map[string][]string{
	"analyze":   {"analyze"},
	"run":       {"analyze"},
	"enrich":    {"enrich"},
	"embed":     {"embed"},
	"expand":    {"expand"},
	"freshness": {"freshness"},
	"all":       {"enrich", "embed", "expand", "freshness", "analyze"},
}

// estimate reports what a request would queue per stage — pending counts,
// the analyze lane's evidence-dependents expansion and model-mismatch
// warnings — without creating a job. Selection validation already ran in
// create(); this answers "what would happen" before anything runs.
func (o *ops) estimate(w http.ResponseWriter, r *http.Request, c config.Config, b opRequest) {
	stages, ok := estimateStages[b.Op]
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("op %q queues no LLM stage; nothing to estimate", b.Op))
		return
	}
	ctx := r.Context()
	stamps, err := o.db.ModelStamps(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	total, err := o.documentCount(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pending := map[string]int{}
	warnings := []string{"Counts use the latest scan snapshot; the task will scan before running, so pending counts may change if files changed since that scan."}
	var analyzeEst analyzer.Estimate
	for _, name := range stages {
		model, stampKey, skip, err := estimateStageModel(c, b, name)
		if skip {
			continue
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		warnStamp(&warnings, name, model, stamps[stampKey])
		if name == "analyze" {
			est, err := analyzer.EstimateAnalysis(ctx, c, o.db, analyzer.RunOptions{
				Reanalyze: b.Reanalyze, ReanalyzeAll: b.ReanalyzeAll, External: b.External,
				Model: b.Model, EmbedModel: b.EmbedModel, AllowCloud: b.AllowCloud,
			})
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			pending["analyze"] = est.Pending
			analyzeEst = est
			continue
		}
		n, err := o.stagePending(ctx, name, model, b)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		pending[name] = n
	}
	if analyzeEst.Dependents > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"The analysis stage would also re-analyze %d unchanged document(s) whose evidence context changed (queue total %d of %d).",
			analyzeEst.Dependents, analyzeEst.Total, total))
	}
	if analyzeEst.Pending > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"Analysis queue reasons (overlapping facets): semantic changes %d; prompt/policy changes %d; model drift %d; previously unstamped %d.",
			analyzeEst.SemanticChanges, analyzeEst.PromptChanges, analyzeEst.ModelDrift, analyzeEst.Unstamped))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"op":            b.Op,
		"documents":     total,
		"pending":       pending,
		"dependents":    analyzeEst.Dependents,
		"analyze_total": analyzeEst.Total,
		"analysis_reasons": map[string]int{
			"semantic_changes":      analyzeEst.SemanticChanges,
			"prompt_policy_changes": analyzeEst.PromptChanges,
			"model_drift":           analyzeEst.ModelDrift,
			"unstamped":             analyzeEst.Unstamped,
		},
		"warnings": warnings,
		"stamps":   stamps,
	})
}

// documentCount is the non-missing corpus size the estimate reports against.
func (o *ops) documentCount(ctx context.Context) (int, error) {
	docs, err := o.db.Documents(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, doc := range docs {
		if doc.Status != "missing" {
			total++
		}
	}
	return total, nil
}

// estimateStageModel resolves the model one stage would run with and the
// lineage stamp the UI compares it against. skip mirrors runAll: an
// unconfigured task model aborts a single-op request, but inside a batch only
// a generation override keeps the stage.
func estimateStageModel(c config.Config, b opRequest, name string) (model, stampKey string, skip bool, err error) {
	stampKey = name
	var m string
	switch name {
	case "enrich":
		stampKey = "enriched"
		m, err = c.TaskModel(config.TaskEnrich, false, b.Model)
	case "embed":
		stampKey = "embedded"
		override := b.EmbedModel
		if b.Op == "embed" && override == "" {
			override = b.Model // standalone embed accepts --model
		}
		m, err = c.TaskModel(config.TaskEmbed, false, override)
	case "analyze":
		stampKey = "analyzed"
		if b.External {
			stampKey = "external"
		}
		m, err = c.TaskModel(config.TaskAnalyze, b.External, b.Model)
	default: // expand, freshness
		m, err = c.TaskModel(name, b.External, b.Model)
	}
	if err != nil {
		return "", stampKey, errors.Is(err, config.ErrNoTaskModel) && (b.Model == "" || name == "embed"), err
	}
	return m, stampKey, false, nil
}

// stagePending counts the queue for the single-model stages; the analyze lane
// needs EstimateAnalysis instead because its sweep adds dependents.
func (o *ops) stagePending(ctx context.Context, name, model string, b opRequest) (int, error) {
	var docs []database.Document
	var err error
	switch name {
	case "enrich":
		docs, err = o.db.PendingEnrich(ctx, model, b.Reanalyze, b.ReanalyzeAll)
	case config.TaskFreshness:
		verifyModel := o.c.VerifyModel(b.External, b.Model, model)
		verifyPromptSHA := ""
		if b.WebVerify {
			verifyPromptSHA, err = analyzer.FreshnessVerifyPromptFingerprint(o.c)
			if err != nil {
				return 0, err
			}
		}
		docs, err = o.db.PendingTaskForRun(ctx, name, model, b.Reanalyze,
			database.TaskRunOptions{
				WebVerify: b.WebVerify, VerifyModel: verifyModel, VerifyPromptSHA: verifyPromptSHA,
			}, b.ReanalyzeAll)
	default:
		docs, err = o.db.PendingTask(ctx, name, model, b.Reanalyze, b.ReanalyzeAll)
	}
	return len(docs), err
}

// warnStamp flags a selection that differs from the recorded lineage — the
// embedding lane especially, where a different model re-vectorizes the whole
// corpus and can enlarge dependent analysis.
func warnStamp(warnings *[]string, stage, resolved string, stamp database.ModelStamp) {
	if resolved == "" || stamp.Model == "" || stamp.Model == "deterministic" || resolved == stamp.Model {
		return
	}
	note := fmt.Sprintf("%s uses %s, but the latest pass on %d document(s) ran with %s", stage, resolved, stamp.Docs, stamp.Model)
	if stage == "embed" {
		note += " — every embedded document is re-vectorized and dependent analysis can expand"
	}
	*warnings = append(*warnings, note)
}

// task runs a suggestion-producing pass (expand or freshness) on its lane.
func (o *ops) task(ctx context.Context, j database.Job, name string, opts analyzer.RunOptions) (string, error) {
	m, err := o.c.TaskModel(name, opts.External, opts.Model)
	if err != nil {
		return "", err
	}
	gens := analyzer.Generators{
		Analysis: ollama.New(o.c.OllamaURL, m, o.c.RequestTimeoutSeconds, o.c.OllamaNumCtx, o.c.OllamaThink, o.c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowRemote),
	}
	if name == config.TaskFreshness && opts.WebVerify {
		gens.Search = websearch.New(o.c.OllamaAPIKey)
		verifyModel := o.c.VerifyModel(opts.External, opts.Model, m)
		gens.Verify = ollama.New(o.c.OllamaURL, verifyModel, o.c.RequestTimeoutSeconds, o.c.OllamaNumCtx, o.c.OllamaThink, o.c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowRemote)
	}
	r, err := analyzer.RunTask(ctx, o.c, o.db, gens, name, opts)
	return r.Summary(name, m), err
}
