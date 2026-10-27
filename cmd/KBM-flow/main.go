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
	"knowledge-ba46-flow/internal/i18n"
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
	i18n.SetDefault(i18n.EnvLocale(os.Getenv))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return rootCmd().ExecuteContext(ctx)
}

// rootCmd builds the full command tree. Each subcommand gets free --help and
// shell completion via `KBM-flow completion <shell>`.
func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "KBM-flow",
		Short:         i18n.T("cli.root.short"),
		Version:       appversion.Version,
		Long:          i18n.T("cli.root.long"),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "config.json", i18n.T("cli.flag.config"))
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
		decideCmd("withdraw"),
		applyCmd(),
		reconcileCmd(),
		identifyCmd(),
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
		Short: i18n.T("cli.report.short"),
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
	cmd.Flags().StringVar(&repoName, "repo", "", i18n.T("cli.flag.repo"))
	cmd.Flags().BoolVar(&force, "force", false, i18n.T("cli.flag.force"))
	cmd.Flags().BoolVar(&noRemote, "no-remote", false, i18n.T("cli.flag.no_remote"))
	cmd.Flags().BoolVar(&remote, "remote", false, i18n.T("cli.flag.remote"))
	cmd.Flags().BoolVar(&jsonOutput, "json", false, i18n.T("cli.flag.json"))
	return cmd
}

// versionCmd prints the application version embedded at build time.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: i18n.T("cli.version.short"),
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
		Short: i18n.T("cli.validate.short"),
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
		Short: i18n.T("cli.scan.short"),
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
			if r.IDsAssigned > 0 {
				detail += fmt.Sprintf(" %d document IDs assigned.", r.IDsAssigned)
			}
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), detail)
			}
			return err
		},
	}
}

func scanSummary(result scanner.Result) string {
	detail := fmt.Sprintf("Scanned %d markdown files; %d new/changed.", result.Seen, result.Changed)
	if result.IDsAssigned > 0 {
		detail += fmt.Sprintf(" %d document IDs assigned.", result.IDsAssigned)
	}
	return detail
}

// check-links probes the external http(s) URLs referenced by the corpus and
// records the verdict in each note's tool-owned kbm.link_check frontmatter
// block — the one documented file write outside apply, since the block is
// operational metadata the canonical content hash ignores.
func linkCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-links",
		Short: i18n.T("cli.link_check.short"),
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
	var reanalyze, reanalyzeAll, external, allowCloud, allowRemote bool
	var model, embedModel string
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: i18n.T("cli.analyze.short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			c, err = modelselection.Prepare(cmd.Context(), c, "analyze", modelselection.Options{
				Model: model, EmbedModel: embedModel, External: external, AllowCloud: allowCloud, AllowRemote: allowRemote,
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
			scanResult, err := scanner.Scan(ctx, c, db)
			if err != nil {
				return err
			}
			scanDetail := scanSummary(scanResult)
			fmt.Fprintln(cmd.OutOrStdout(), scanDetail)
			print := progressPrinter(cmd.OutOrStdout())
			taskDetail, err := runAnalyze(ctx, c, db, analysisModel, analyzer.RunOptions{Reanalyze: reanalyze, ReanalyzeAll: reanalyzeAll, External: external, Model: model, EmbedModel: embedModel, AllowCloud: allowCloud, AllowRemote: allowRemote, Progress: func(p analyzer.Progress) {
				print(p)
				_ = db.UpdateJobDetail(ctx, job.ID, p.String())
			}})
			if err == nil {
				detail = scanDetail + " " + taskDetail
				fmt.Fprintln(cmd.OutOrStdout(), taskDetail)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, i18n.T("cli.flag.reanalyze"))
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, i18n.T("cli.flag.reanalyze_all"))
	cmd.Flags().BoolVar(&external, "external", false, i18n.T("cli.flag.external_analyze"))
	cmd.Flags().StringVar(&model, "model", "", i18n.T("cli.flag.model"))
	cmd.Flags().StringVar(&embedModel, "embed-model", "", i18n.T("cli.flag.embed_model"))
	cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, i18n.T("cli.flag.allow_cloud"))
	cmd.Flags().BoolVar(&allowRemote, "allow-remote", false, i18n.T("cli.flag.allow_remote"))
	return cmd
}

// run: convenience combo = scan followed by analyze.
func runCmd() *cobra.Command {
	var reanalyze, reanalyzeAll, external, allowCloud, allowRemote bool
	var model, embedModel string
	cmd := &cobra.Command{
		Use:   "run",
		Short: i18n.T("cli.run.short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			c, err = modelselection.Prepare(cmd.Context(), c, "run", modelselection.Options{
				Model: model, EmbedModel: embedModel, External: external, AllowCloud: allowCloud, AllowRemote: allowRemote,
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
			if r.IDsAssigned > 0 {
				detail += fmt.Sprintf(" %d document IDs assigned.", r.IDsAssigned)
			}
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
				EmbedModel: embedModel, AllowCloud: allowCloud, AllowRemote: allowRemote, Progress: progress,
			})
			detail += " " + analysisDetail
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), analysisDetail)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, i18n.T("cli.flag.reanalyze"))
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, i18n.T("cli.flag.reanalyze_all"))
	cmd.Flags().BoolVar(&external, "external", false, i18n.T("cli.flag.external_analyze"))
	cmd.Flags().StringVar(&model, "model", "", i18n.T("cli.flag.model"))
	cmd.Flags().StringVar(&embedModel, "embed-model", "", i18n.T("cli.flag.embed_model"))
	cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, i18n.T("cli.flag.allow_cloud"))
	cmd.Flags().BoolVar(&allowRemote, "allow-remote", false, i18n.T("cli.flag.allow_remote"))
	return cmd
}

// taskCmd builds the shared command shape for the optional per-task passes
// (enrich, embed, expand, freshness): each resolves its model through
// ollama_task_models/lane defaults or --model, preflights model capability and
// privacy, supports --reanalyze model drift and, for expand/freshness,
// --external.
func taskCmd(use, short, op string, allowExternal bool, run func(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error)) *cobra.Command {
	var reanalyze, reanalyzeAll, external, allowCloud, allowRemote, webVerify bool
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
				AllowRemote: allowRemote, WebVerify: webVerify,
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
			scanResult, err := scanner.Scan(ctx, c, db)
			if err != nil {
				return err
			}
			detail = scanSummary(scanResult)
			fmt.Fprintln(cmd.OutOrStdout(), detail)
			print := progressPrinter(cmd.OutOrStdout())
			taskDetail, err := run(ctx, c, db, resolved, analyzer.RunOptions{
				Reanalyze: reanalyze, ReanalyzeAll: reanalyzeAll, External: external,
				Model: model, EmbedModel: embedModel, AllowCloud: allowCloud,
				AllowRemote: allowRemote, WebVerify: webVerify,
				Progress: func(p analyzer.Progress) {
					print(p)
					_ = db.UpdateJobDetail(ctx, job.ID, p.String())
				},
			})
			if err == nil {
				detail += " " + taskDetail
				fmt.Fprintln(cmd.OutOrStdout(), taskDetail)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, i18n.T("cli.flag.task_reanalyze"))
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, i18n.T("cli.flag.task_reanalyze_all"))
	cmd.Flags().StringVar(&model, "model", "", i18n.T("cli.flag.task_model"))
	if op == config.TaskEmbed {
		cmd.Flags().StringVar(&embedModel, "embed-model", "", i18n.T("cli.flag.task_embed_model"))
	}
	if allowExternal {
		cmd.Flags().BoolVar(&external, "external", false, i18n.T("cli.flag.task_external"))
	}
	if op == "enrich" || op == "embed" {
		cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, i18n.T("cli.flag.task_allow_cloud"))
	}
	cmd.Flags().BoolVar(&allowRemote, "allow-remote", false, i18n.T("cli.flag.task_allow_remote"))
	if op == config.TaskFreshness {
		cmd.Flags().BoolVar(&webVerify, "web-verify", false, i18n.T("cli.flag.web_verify"))
	}
	return cmd
}

// taskRunner executes one LLM pass with an already-resolved model.
type taskRunner func(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error)

func runEnrich(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	gens := analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud || opts.AllowRemote),
		Enrich:   ollama.New(c.OllamaURL, model, int(c.EnrichmentTimeout().Seconds()), c.OllamaNumCtx, false, c.OllamaKeepAlive).WithOutputLimit(c.EnrichmentOutputTokens()).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud || opts.AllowRemote),
	}
	opts.EnrichOnly = true
	opts.Model = model
	r, err := analyzer.RunProgress(ctx, c, db, gens, opts)
	return r.Summary(model, model), err
}

func runEmbed(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	r, err := analyzer.RunEmbed(ctx, c, db, analyzer.Generators{
		Embed: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, 0, false, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityEmbedding, opts.AllowCloud || opts.AllowRemote),
	}, opts)
	return r.Summary("embed", model), err
}

func runAnalyze(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	fast, err := c.TaskModel(config.TaskEnrich, false, opts.Model)
	if err != nil {
		return "", err
	}
	gens := analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowRemote),
		Enrich:   ollama.New(c.OllamaURL, fast, int(c.EnrichmentTimeout().Seconds()), c.OllamaNumCtx, false, c.OllamaKeepAlive).WithOutputLimit(c.EnrichmentOutputTokens()).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowCloud || opts.AllowRemote),
	}
	embed, err := c.TaskModel(config.TaskEmbed, false, opts.EmbedModel)
	if err != nil && !errors.Is(err, config.ErrNoTaskModel) {
		return "", err
	}
	if embed != "" {
		gens.Embed = ollama.New(c.OllamaURL, embed, c.RequestTimeoutSeconds, 0, false, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityEmbedding, opts.AllowCloud || opts.AllowRemote)
	}
	r, err := analyzer.RunProgress(ctx, c, db, gens, opts)
	return r.Summary(model, fast), err
}

func runExpand(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	r, err := analyzer.RunTask(ctx, c, db, analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowRemote),
	}, "expand", opts)
	return r.Summary("expand", model), err
}

func runFreshness(ctx context.Context, c config.Config, db *database.DB, model string, opts analyzer.RunOptions) (string, error) {
	gens := analyzer.Generators{
		Analysis: ollama.New(c.OllamaURL, model, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowRemote),
	}
	if opts.WebVerify {
		gens.Search = websearch.New(c.OllamaAPIKey)
		verifyModel := c.VerifyModel(opts.External, opts.Model, model)
		gens.Verify = ollama.New(c.OllamaURL, verifyModel, c.RequestTimeoutSeconds, c.OllamaNumCtx, c.OllamaThink, c.OllamaKeepAlive).WithModelCheck(ollama.CapabilityCompletion, opts.External || opts.AllowRemote)
	}
	r, err := analyzer.RunTask(ctx, c, db, gens, "freshness", opts)
	return r.Summary("freshness", model), err
}

func enrichCmd() *cobra.Command {
	return taskCmd("enrich", i18n.T("cli.enrich.short"), "enrich", false, runEnrich)
}

func embedCmd() *cobra.Command {
	return taskCmd("embed", i18n.T("cli.embed.short"), "embed", false, runEmbed)
}

func expandCmd() *cobra.Command {
	return taskCmd("expand", i18n.T("cli.expand.short"), "expand", true, runExpand)
}

func freshnessCmd() *cobra.Command {
	return taskCmd("freshness", i18n.T("cli.freshness.short"), "freshness", true, runFreshness)
}

// tasks runs every LLM-backed pass in one job — enrichment, embedding
// backfill, stub expansion, freshness, then full analysis — each on its
// configured lane, or generation stages through one model when --model is
// given. Embedding uses its independent override or configured default. Tasks without
// a configured model are skipped, not failed.
func tasksCmd() *cobra.Command {
	var reanalyze, reanalyzeAll, external, allowCloud, allowRemote, webVerify bool
	var model, embedModel string
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: i18n.T("cli.tasks.short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			c, err = modelselection.Prepare(cmd.Context(), c, "all", modelselection.Options{
				Model: model, EmbedModel: embedModel, External: external, AllowCloud: allowCloud,
				AllowRemote: allowRemote, WebVerify: webVerify,
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
			scanResult, err := scanner.Scan(ctx, c, db)
			if err != nil {
				return err
			}
			detail = scanSummary(scanResult)
			fmt.Fprintln(cmd.OutOrStdout(), detail)
			print := progressPrinter(cmd.OutOrStdout())
			opts := analyzer.RunOptions{
				Reanalyze: reanalyze, ReanalyzeAll: reanalyzeAll, External: external,
				Model: model, EmbedModel: embedModel, AllowCloud: allowCloud,
				AllowRemote: allowRemote, WebVerify: webVerify,
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
			parts := []string{detail}
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
	cmd.Flags().BoolVar(&reanalyze, "reanalyze", false, i18n.T("cli.flag.task_reanalyze"))
	cmd.Flags().BoolVar(&reanalyzeAll, "reanalyze-all", false, i18n.T("cli.flag.tasks_reanalyze_all"))
	cmd.Flags().BoolVar(&external, "external", false, i18n.T("cli.flag.tasks_external"))
	cmd.Flags().BoolVar(&allowCloud, "allow-cloud", false, i18n.T("cli.flag.allow_cloud"))
	cmd.Flags().BoolVar(&allowRemote, "allow-remote", false, i18n.T("cli.flag.allow_remote"))
	cmd.Flags().BoolVar(&webVerify, "web-verify", false, i18n.T("cli.flag.web_verify"))
	cmd.Flags().StringVar(&model, "model", "", i18n.T("cli.flag.tasks_model"))
	cmd.Flags().StringVar(&embedModel, "embed-model", "", i18n.T("cli.flag.embed_model"))
	return cmd
}

// prune: bound the jobs table — finished scan/analyze/run/report history is
// otherwise unbounded. New job creation already trims to the newest 200
// finished rows; this command is for a tighter cut on demand.
func pruneCmd() *cobra.Command {
	var keep, trashDays int
	cmd := &cobra.Command{
		Use:   "prune",
		Short: i18n.T("cli.prune.short"),
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
	cmd.Flags().IntVar(&keep, "keep", 200, i18n.T("cli.flag.keep"))
	cmd.Flags().IntVar(&trashDays, "trash-days", 0, i18n.T("cli.flag.trash_days"))
	return cmd
}

// list: print all pending suggestions non-interactively.
func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: i18n.T("cli.list.short"),
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
		Short: i18n.T("cli.review.short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			return review.InteractiveCurrent(cmd.Context(), c, db)
		},
	}
}

// accept/reject ID: non-interactive variant — decide a single suggestion by id.
func decideCmd(name string) *cobra.Command {
	status := "accepted"
	if name == "reject" {
		status = "rejected"
	}
	if name == "withdraw" {
		status = "pending"
	}
	cmd := &cobra.Command{
		Use:   name + " ID",
		Short: i18n.T("cli.decide.short", map[string]any{"Action": name}),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, db, err := openAll(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()
			note, _ := cmd.Flags().GetString("note")
			return review.DecideCurrent(cmd.Context(), c, db, args[0], status, note)
		},
	}
	if status == "rejected" {
		cmd.Flags().String("note", "", i18n.T("cli.flag.note"))
	}
	return cmd
}

// apply: execute every accepted suggestion (move/merge/expand/delete) on disk.
func applyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: i18n.T("cli.apply.short"),
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
				if r.Status == "blocked" {
					fmt.Printf("BLOCKED #%d: %s\n", r.ID, r.Error)
					continue
				}
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
		Short: i18n.T("cli.serve.short"),
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
