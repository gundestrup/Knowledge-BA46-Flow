// Command KBM-flow is the CLI entrypoint for Knowledge-BA46-Flow.
// Subcommands: scan, analyze, run, report, list, review, accept, reject, apply, prune, serve, version.
// Intended flow: run -> list/review -> apply. Nothing on disk changes
// until a suggestion is explicitly accepted and `apply` is run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

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
	"knowledge-ba46-flow/internal/server"
	"knowledge-ba46-flow/internal/taxonomy"
	"knowledge-ba46-flow/internal/validate"
	appversion "knowledge-ba46-flow/internal/version"
	"knowledge-ba46-flow/internal/websearch"
)

// configPath is bound to the persistent --config flag, usable before or after
// the subcommand (a cobra improvement over the old hand-rolled flag parsing).
var configPath string

func main() {
	// SilenceErrors: we print the error ourselves below; SilenceUsage: a failing
	// command shouldn't dump help text.
	if err := execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return rootCmd().ExecuteContext(ctx)
}

// rootCmd builds the full command tree. Each subcommand gets free --help and
// shell completion via `KBM-flow completion <shell>`.
func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "KBM-flow",
		Short:   "Maintain a markdown knowledge repository with a local LLM",
		Version: appversion.Version,
		Long: `Knowledge-BA46-Flow scans a markdown repository, asks a local Ollama
model for maintenance suggestions (move/merge/expand/delete), and only
changes files after explicit human acceptance.

Flow: run -> list/review -> apply.
report refreshes the separate local repository health report.
serve starts an HTTP API with a home page and separate knowledge/report workspaces.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "config.json", "path to config file")
	root.AddCommand(
		scanCmd(),
		linkCheckCmd(),
		validateCmd(),
		analyzeCmd(),
		runCmd(),
		enrichCmd(),
		embedCmd(),
		expandCmd(),
		freshnessCmd(),
		tasksCmd(),
		reportCmd(),
		listCmd(),
		reviewCmd(),
		decideCmd("accept"),
		decideCmd("reject"),
		applyCmd(),
		pruneCmd(),
		serveCmd(),
		versionCmd(),
	)
	return root
}

// report: refresh and persist the configured local repository health snapshot.
func reportCmd() *cobra.Command {
	var force, noRemote, remote, jsonOutput bool
	var repoName string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Refresh the local repository health report",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if noRemote && remote {
				return fmt.Errorf("--remote and --no-remote cannot be combined")
			}
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			useRemote := c.Report.WantsRemote()
			if remote {
				useRemote = true
			}
			if noRemote {
				useRemote = false
			}
			snapshot, err := repositoryreport.RunWithOptions(cmd.Context(), c.Report, db, repositoryreport.RunOptions{
				Remote: useRemote, Force: force, Repository: repoName,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(snapshot)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Reported %d repositories; %d problems.\n",
				snapshot.Summary.Repositories, snapshot.Summary.Problems)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "refresh one registered repository by name")
	cmd.Flags().BoolVar(&force, "force", false, "force remote checks and refresh package registry caches")
	cmd.Flags().BoolVar(&noRemote, "no-remote", false, "run local checks only and retain cached remote results")
	cmd.Flags().BoolVar(&remote, "remote", false, "enable remote checks for this run")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the complete report snapshot as JSON")
	return cmd
}

// versionCmd prints the application version embedded at build time.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the application version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "KBM-flow %s\n", appversion.Version)
		},
	}
}

// validate: the read-only structural gate — taxonomy, frontmatter coverage,
// index/back-link and language↔catalog edges, orphan files. No DB, no Ollama.
func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check repository structure against taxonomy.yaml and linking conventions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load(configPath)
			if err != nil {
				return err
			}
			vocabulary, err := taxonomy.Load(c.RepositoryPath)
			if err != nil {
				return err
			}
			violations, err := validate.Run(cmd.Context(), c, vocabulary)
			if err != nil {
				return err
			}
			for _, v := range violations {
				fmt.Fprintln(cmd.OutOrStdout(), v)
			}
			if len(violations) > 0 {
				return fmt.Errorf("validate: %d violation(s)", len(violations))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "validate: ok")
			return nil
		},
	}
}

// openAll loads the config and opens the database — the shared setup every
// command needs. Callers must close the returned DB.
func openAll(ctx context.Context) (config.Config, *database.DB, error) {
	c, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	if err := logging.Init(c.LogFile, c.LogLevel); err != nil {
		return config.Config{}, nil, err
	}
	db, err := database.Open(ctx, c.DatabasePath)
	if err != nil {
		return config.Config{}, nil, err
	}
	return c, db, nil
}

// scan: walk the repo, hash markdown files, upsert into the documents table. No Ollama calls.
func scanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Scan the repository and sync SQLite (no Ollama calls)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			ctx := cmd.Context()
			job, stop, err := claimJob(ctx, db, "scan")
			if err != nil {
				return err
			}
			defer stop()
			detail := ""
			defer finishJob(db, job.ID, &detail, &runErr)
			r, err := scanner.Scan(ctx, c, db)
			detail = fmt.Sprintf("Scanned %d markdown files; %d new/changed.", r.Seen, r.Changed)
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), detail)
			}
			return err
		},
	}
}

// check-links probes the external http(s) URLs referenced by the corpus and
// records the verdict in each note's tool-owned kbm.link_check frontmatter
// block — the one documented file write outside apply, since the block is
// operational metadata the canonical content hash ignores.
func linkCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-links",
		Short: "Check external links in documents and record verdicts (network calls; no LLM)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			ctx := cmd.Context()
			job, stop, err := claimJob(ctx, db, "link-check")
			if err != nil {
				return err
			}
			defer stop()
			detail := ""
			defer finishJob(db, job.ID, &detail, &runErr)
			r, err := linkcheck.Run(ctx, c, db)
			detail = r.Summary()
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), detail)
			}
			return err
		},
	}
}

// claimJob refuses to start when another process (CLI or serve) already holds
// a live job, then creates the job and starts its heartbeat ticker — the beat
// is what lets RunningJob tell live work from a killed process.
func claimJob(ctx context.Context, db *database.DB, op string) (database.Job, func(), error) {
	running, ok, err := db.RunningJob(ctx)
	if err != nil {
		return database.Job{}, nil, err
	}
	if ok {
		return database.Job{}, nil, fmt.Errorf("job #%d (%s) is already running — wait for it to finish", running.ID, running.Op)
	}
	job, err := db.CreateJob(ctx, op)
	if err != nil {
		return database.Job{}, nil, err
	}
	return job, db.RunHeartbeat(ctx, job.ID), nil
}

// finishJob centralizes the deferred job-completion write used by the scan,
// analyze and run commands: status follows runErr, and a FinishJob failure is
// only surfaced when the command itself succeeded.
func finishJob(db *database.DB, id int64, detail *string, runErr *error) {
	status, errText := "done", ""
	if *runErr != nil {
		status, errText = "failed", (*runErr).Error()
	}
	if err := db.FinishJob(context.Background(), id, status, *detail, errText); err != nil && *runErr == nil {
		*runErr = err
	}
}

// progressPrinter returns a callback that prints one line per document
// event, so a long analyze run shows visible forward motion. Slow models can
// take minutes per document — silence looks identical to a hang.
func progressPrinter(w io.Writer) func(analyzer.Progress) {
	return func(p analyzer.Progress) {
		if p.Outcome == "analyzing" || p.Outcome == "enriching" {
			fmt.Fprintf(w, "[%d/%d] %s\n", p.Index, p.Total, p.Path)
			return
		}
		fmt.Fprintf(w, "    %s: %s\n", p.Outcome, p.Detail)
	}
}

// analyze sends changed documents to Ollama. --reanalyze includes model drift;
// --reanalyze-all explicitly refreshes every non-missing document. --external
// opts into the enhanced lane (ollama_model_external, e.g. a cloud model):
// results stamp the external_* markers and leave the local lineage untouched.
func analyzeCmd() *cobra.Command {
	var reanalyze, reanalyzeAll, external, allowCloud bool
	var model, embedModel string
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze changed documents with Ollama (LLM calls)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			c, err = modelselection.Prepare(cmd.Context(), c, "analyze", modelselection.Options{
				Model: model, EmbedModel: embedModel, External: external, AllowCloud: allowCloud,
			})
			if err != nil {
				return err
			}
			analysisModel, _ := c.TaskModel(config.TaskAnalyze, external, model)
			ctx := cmd.Context()
			job, stop, err := claimJob(ctx, db, "analyze")
			if err != nil {
				return err
			}
			defer stop()
			detail := ""
			defer finishJob(db, job.ID, &detail, &runErr)
			print := progressPrinter(cmd.OutOrStdout())
			detail, err = runAnalyze(ctx, c, db, analysisModel, analyzer.RunOptions{Reanalyze: reanalyze, ReanalyzeAll: reanalyzeAll, External: external, Model: model, EmbedModel: embedModel, AllowCloud: allowCloud, Progress: func(p analyzer.Progress) {
				print(p)
				_ = db.UpdateJobDetail(ctx, job.ID, p.String())
			}})
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), detail)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, "also re-analyze documents whose last analysis used a different model")
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, "force every non-missing document through enrichment and analysis (expensive)")
	cmd.Flags().BoolVar(&external, "external", false, "analyze with ollama_model_external (enhanced lane; results stamp external_* and keep the local lineage)")
	cmd.Flags().StringVar(&model, "model", "", "run generation stages with this configured or installed Ollama model")
	cmd.Flags().StringVar(&embedModel, "embed-model", "", "use this embedding model independently of --model")
	cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, "allow cloud/remote embeddings and enrichment (sends document content off-machine)")
	return cmd
}

// run: convenience combo = scan followed by analyze.
func runCmd() *cobra.Command {
	var reanalyze, reanalyzeAll, external, allowCloud bool
	var model, embedModel string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Scan, then analyze changed documents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			c, err = modelselection.Prepare(cmd.Context(), c, "run", modelselection.Options{
				Model: model, EmbedModel: embedModel, External: external, AllowCloud: allowCloud,
			})
			if err != nil {
				return err
			}
			analysisModel, _ := c.TaskModel(config.TaskAnalyze, external, model)
			ctx := cmd.Context()
			job, stop, err := claimJob(ctx, db, "run")
			if err != nil {
				return err
			}
			defer stop()
			detail := ""
			defer finishJob(db, job.ID, &detail, &runErr)

			r, err := scanner.Scan(ctx, c, db)
			detail = fmt.Sprintf("Scanned %d markdown files; %d new/changed.", r.Seen, r.Changed)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), detail)
			print := progressPrinter(cmd.OutOrStdout())
			progress := func(p analyzer.Progress) {
				print(p)
				_ = db.UpdateJobDetail(ctx, job.ID, p.String())
			}
			analysisDetail, err := runAnalyze(ctx, c, db, analysisModel, analyzer.RunOptions{
				Reanalyze: reanalyze, ReanalyzeAll: reanalyzeAll, External: external, Model: model,
				EmbedModel: embedModel, AllowCloud: allowCloud, Progress: progress,
			})
			detail += " " + analysisDetail
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), analysisDetail)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, "also re-analyze documents whose last analysis used a different model")
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, "force every non-missing document through enrichment and analysis (expensive)")
	cmd.Flags().BoolVar(&external, "external", false, "analyze with ollama_model_external (enhanced lane; results stamp external_* and keep the local lineage)")
	cmd.Flags().StringVar(&model, "model", "", "run generation stages with this configured or installed Ollama model")
	cmd.Flags().StringVar(&embedModel, "embed-model", "", "use this embedding model independently of --model")
	cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, "allow cloud/remote embeddings and enrichment (sends document content off-machine)")
	return cmd
}

// taskCmd builds the shared command shape for the optional per-task passes
// (enrich, embed, expand, freshness): each resolves its model through
// ollama_task_models/lane defaults or --model, preflights model capability and
// privacy, supports --reanalyze model drift and, for expand/freshness,
// --external.
func taskCmd(use, short, op string, allowExternal bool, run func(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error)) *cobra.Command {
	var reanalyze, reanalyzeAll, external, allowCloud, webVerify bool
	var model, embedModel string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			c, err = modelselection.Prepare(cmd.Context(), c, op, modelselection.Options{
				Model: model, EmbedModel: embedModel, External: external, AllowCloud: allowCloud,
				WebVerify: webVerify,
			})
			if err != nil {
				return err
			}
			if webVerify && c.OllamaAPIKey == "" {
				return errors.New("--web-verify requires OLLAMA_API_KEY in the environment or a .env file next to the config")
			}
			override := model
			if op == config.TaskEmbed && embedModel != "" {
				override = embedModel
			}
			resolved, err := c.TaskModel(op, external, override)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			job, stop, err := claimJob(ctx, db, op)
			if err != nil {
				return err
			}
			defer stop()
			detail := ""
			defer finishJob(db, job.ID, &detail, &runErr)
			print := progressPrinter(cmd.OutOrStdout())
			detail, err = run(ctx, c, db, resolved, analyzer.RunOptions{
				Reanalyze: reanalyze, ReanalyzeAll: reanalyzeAll, External: external,
				Model: model, EmbedModel: embedModel, AllowCloud: allowCloud,
				WebVerify: webVerify,
				Progress: func(p analyzer.Progress) {
					print(p)
					_ = db.UpdateJobDetail(ctx, job.ID, p.String())
				},
			})
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), detail)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, "also reprocess documents whose last pass used a different model")
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, "force every non-missing document through the pass (expensive)")
	cmd.Flags().StringVar(&model, "model", "", "run this pass with a configured or installed Ollama model")
	if op == config.TaskEmbed {
		cmd.Flags().StringVar(&embedModel, "embed-model", "", "embedding model override (alternative to --model)")
	}
	if allowExternal {
		cmd.Flags().BoolVar(&external, "external", false, "use ollama_model_external for this pass")
	}
	if op == "enrich" || op == "embed" {
		cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, "allow cloud/remote models for this pass (sends content off-machine)")
	}
	if op == config.TaskFreshness {
		cmd.Flags().BoolVar(&webVerify, "web-verify", false, "verify flagged claims via Ollama web search (claim text is sent to ollama.com; requires OLLAMA_API_KEY)")
	}
	return cmd
}

// taskRunner executes one LLM pass with an already-resolved model.
type taskRunner func(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error)

func runEnrich(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	gens := analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud),
		Enrich:   ollama.New(c.OllamaURL, model, int(c.EnrichmentTimeout().Seconds()), c.OllamaNumCtx, false, c.OllamaKeepAlive).WithOutputLimit(c.EnrichmentOutputTokens()).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud),
	}
	opts.EnrichOnly = true
	opts.Model = model
	r, err := analyzer.RunProgress(ctx, c, db, gens, opts)
	return r.Summary(model, model), err
}

func runEmbed(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	r, err := analyzer.RunEmbed(ctx, c, db, analyzer.Generators{
		Embed: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, 0, false, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityEmbedding, opts.AllowCloud),
	}, opts)
	return r.Summary("embed", model), err
}

func runAnalyze(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	fast, err := c.TaskModel(config.TaskEnrich, false, opts.Model)
	if err != nil {
		return "", err
	}
	gens := analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External),
		Enrich:   ollama.New(c.OllamaURL, fast, int(c.EnrichmentTimeout().Seconds()), c.OllamaNumCtx, false, c.OllamaKeepAlive).WithOutputLimit(c.EnrichmentOutputTokens()).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud),
	}
	embed, err := c.TaskModel(config.TaskEmbed, false, opts.EmbedModel)
	if err != nil && !errors.Is(err, config.ErrNoTaskModel) {
		return "", err
	}
	if embed != "" {
		gens.Embed = ollama.New(c.OllamaURL, embed, c.RequestTimeoutSeconds, 0, false, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityEmbedding, opts.AllowCloud)
	}
	r, err := analyzer.RunProgress(ctx, c, db, gens, opts)
	return r.Summary(model, fast), err
}

func runExpand(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	r, err := analyzer.RunTask(ctx, c, db, analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External),
	}, "expand", opts)
	return r.Summary("expand", model), err
}

func runFreshness(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	gens := analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External),
	}
	if opts.WebVerify {
		gens.Search = websearch.New(c.OllamaAPIKey)
		verifyModel := c.VerifyModel(opts.External, opts.Model, model)
		gens.Verify = ollama.New(c.OllamaURL, verifyModel, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External)
	}
	r, err := analyzer.RunTask(ctx, c, db, gens, "freshness", opts)
	return r.Summary("freshness", model), err
}

func enrichCmd() *cobra.Command {
	return taskCmd("enrich", "Run only the fast enrichment pass (title/tags metadata)", "enrich", false, runEnrich)
}

func embedCmd() *cobra.Command {
	return taskCmd("embed", "Recompute embedding vectors for new/changed documents", "embed", false, runEmbed)
}

func expandCmd() *cobra.Command {
	return taskCmd("expand", "Expand stub documents using their category STUB.md templates", "expand", true, runExpand)
}

func freshnessCmd() *cobra.Command {
	return taskCmd("freshness", "Flag documents that may no longer be current (heuristic review)", "freshness", true, runFreshness)
}

// tasks runs every LLM-backed pass in one job — enrichment, embedding
// backfill, stub expansion, freshness, then full analysis — each on its
// configured lane, or generation stages through one model when --model is
// given. Embedding uses its independent override or configured default. Tasks without
// a configured model are skipped, not failed.
func tasksCmd() *cobra.Command {
	var reanalyze, reanalyzeAll, external, allowCloud, webVerify bool
	var model, embedModel string
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "Run every configured LLM task (enrich, embed, expand, freshness, analyze)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			c, err = modelselection.Prepare(cmd.Context(), c, "all", modelselection.Options{
				Model: model, EmbedModel: embedModel, External: external, AllowCloud: allowCloud,
				WebVerify: webVerify,
			})
			if err != nil {
				return err
			}
			if webVerify && c.OllamaAPIKey == "" {
				return errors.New("--web-verify requires OLLAMA_API_KEY in the environment or a .env file next to the config")
			}
			ctx := cmd.Context()
			job, stop, err := claimJob(ctx, db, "all")
			if err != nil {
				return err
			}
			defer stop()
			detail := ""
			defer finishJob(db, job.ID, &detail, &runErr)
			print := progressPrinter(cmd.OutOrStdout())
			opts := analyzer.RunOptions{
				Reanalyze: reanalyze, ReanalyzeAll: reanalyzeAll, External: external,
				Model: model, EmbedModel: embedModel, AllowCloud: allowCloud,
				WebVerify: webVerify,
				Progress: func(p analyzer.Progress) {
					print(p)
					_ = db.UpdateJobDetail(ctx, job.ID, p.String())
				},
			}
			runners := []struct {
				name string
				run  taskRunner
			}{
				{"enrich", runEnrich},
				{"embed", runEmbed},
				{"expand", runExpand},
				{"freshness", runFreshness},
				{"analyze", runAnalyze},
			}
			var parts []string
			for _, t := range runners {
				// Generation and embedding overrides are independent.
				taskModel, taskOpts := model, opts
				if t.name == "embed" {
					taskModel, taskOpts.Model = embedModel, ""
				}
				m, err := c.TaskModel(t.name, external, taskModel)
				if err != nil {
					if errors.Is(err, config.ErrNoTaskModel) && (model == "" || t.name == "embed") {
						continue
					}
					detail = strings.Join(parts, "; ")
					return err
				}
				summary, err := t.run(ctx, c, db, m, taskOpts)
				if err != nil {
					detail = strings.Join(parts, "; ")
					return fmt.Errorf("%s: %w", t.name, err)
				}
				parts = append(parts, t.name+": "+summary)
			}
			detail = strings.Join(parts, "; ")
			fmt.Fprintln(cmd.OutOrStdout(), detail)
			return nil
		},
	}
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, "also reprocess documents whose last pass used a different model")
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, "force every non-missing document through every pass (expensive)")
	cmd.Flags().BoolVar(&external, "external", false, "allow cloud/remote generation models and use ollama_model_external when selected")
	cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, "allow cloud/remote embeddings and enrichment (sends document content off-machine)")
	cmd.Flags().BoolVar(&webVerify, "web-verify", false, "verify freshness flags via Ollama web search (claim text is sent to ollama.com; requires OLLAMA_API_KEY)")
	cmd.Flags().StringVar(&model, "model", "", "run every generation task with this configured or installed Ollama model")
	cmd.Flags().StringVar(&embedModel, "embed-model", "", "use this embedding model independently of --model")
	return cmd
}

// prune: bound the jobs table — finished scan/analyze/run/report history is
// otherwise unbounded. New job creation already trims to the newest 200
// finished rows; this command is for a tighter cut on demand.
func pruneCmd() *cobra.Command {
	var keep, trashDays int
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete finished job history beyond the newest --keep rows, optionally purge old trash",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			ctx := cmd.Context()
			// Also sweep stale runners (frozen heartbeats) to 'failed' so a
			// corpse doesn't linger as 'running' in the job history.
			if _, _, err := db.RunningJob(ctx); err != nil {
				return err
			}
			removed, err := db.PruneFinishedJobs(ctx, keep)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pruned %d finished job(s); kept the newest %d.\n", removed, keep)
			if trashDays > 0 {
				removed, err := review.PurgeTrash(c, time.Now().AddDate(0, 0, -trashDays))
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Purged %d trash entrie(s) older than %d days.\n", removed, trashDays)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&keep, "keep", 200, "number of newest finished jobs to retain")
	cmd.Flags().IntVar(&trashDays, "trash-days", 0, "also permanently delete trash entries older than this many days (0 = keep all)")
	return cmd
}

// list: print all pending suggestions non-interactively.
func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show pending suggestions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			return review.List(cmd.Context(), db)
		},
	}
}

// review: prompt y/n/s/q for each pending suggestion.
func reviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "review",
		Short: "Interactive accept/reject of pending suggestions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			return review.Interactive(cmd.Context(), db)
		},
	}
}

// accept/reject ID: non-interactive variant — decide a single suggestion by id.
func decideCmd(name string) *cobra.Command {
	status := "accepted"
	if name == "reject" {
		status = "rejected"
	}
	cmd := &cobra.Command{
		Use:   name + " ID",
		Short: name + " a single pending suggestion",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			note, _ := cmd.Flags().GetString("note")
			return review.Decide(cmd.Context(), db, args[0], status, note)
		},
	}
	if status == "rejected" {
		cmd.Flags().String("note", "", "why the suggestion is wrong — stored on the row and shown back to the model on later runs")
	}
	return cmd
}

// apply: execute every accepted suggestion (move/merge/expand/delete) on disk.
func applyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Execute all accepted suggestions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			results, err := review.Apply(cmd.Context(), c, db)
			if err != nil {
				return err
			}
			for _, r := range results {
				if r.Status == "superseded" {
					fmt.Printf("SKIPPED #%d: %s\n", r.ID, r.Error)
					continue
				}
				if r.Error != "" {
					status := "ERROR"
					if r.Applied {
						status = "WARNING"
					}
					fmt.Printf("%s #%d: %s\n", status, r.ID, r.Error)
				} else {
					fmt.Printf("Applied #%d (%s)\n", r.ID, r.Action)
				}
			}
			return nil
		},
	}
}

// serve: HTTP API + web UI for listing/deciding/applying suggestions.
// Binds to config http_addr (default 127.0.0.1:8080 — localhost only).
func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "HTTP API + web UI for the review workflow",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A context cancelled before startup means "don't run" — exit
			// cleanly rather than failing the first context-aware DB call.
			if cmd.Context().Err() != nil {
				return nil
			}
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			return server.Listen(cmd.Context(), c, db)
		},
	}
}
