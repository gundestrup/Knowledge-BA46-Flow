// Chart instances live in module scope, not on the reactive component — a
// reactive proxy around a Chart (which holds circular refs and a DOM canvas)
// is a known "Maximum call stack size exceeded" trigger.
// Chart.getChart(canvas) is Chart.js's own instance registry — used below
// instead of a hand-rolled map so update-vs-recreate follows the library's
// documented lifecycle. Theme-aware chart options come from common.js.

// Compact counts pin the "en" locale so the suffix stays K/M regardless of
// browser language — the rest of the UI is English-only.
const compactCount = new Intl.NumberFormat("en", {
  notation: "compact",
  maximumFractionDigits: 1,
});

// lcsLines diffs two line lists with a classic LCS table, emitting ordered
// ctx/add/del ops. Callers bound the input size (see diffLines) so the
// (n+1)×(m+1) table stays small.
function lcsLines(a, b) {
  const n = a.length;
  const m = b.length;
  const width = m + 1;
  const dp = new Int32Array((n + 1) * width);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i * width + j] =
        a[i] === b[j]
          ? dp[(i + 1) * width + j + 1] + 1
          : Math.max(dp[(i + 1) * width + j], dp[i * width + j + 1]);
    }
  }
  const ops = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      ops.push({ t: "ctx", s: a[i] });
      i++;
      j++;
    } else if (dp[(i + 1) * width + j] >= dp[i * width + j + 1]) {
      ops.push({ t: "del", s: a[i] });
      i++;
    } else {
      ops.push({ t: "add", s: b[j] });
      j++;
    }
  }
  for (; i < n; i++) ops.push({ t: "del", s: a[i] });
  for (; j < m; j++) ops.push({ t: "add", s: b[j] });
  return ops;
}

window.knowledgeDashboard = () => ({
  documents: [],
  suggestions: [],
  jobs: [],
  progressDays: [],
  proposals: {},
  proposalOpen: {},
  proposalView: {},
  statusFilter: "pending",
  originFilter: "all",
  typeFilter: "all",
  queuePlan: null,
  reconciling: false,
  pendingCount: 0,
  acceptedCount: 0,
  busy: false,
  applying: false,
  applyResults: [],
  reanalyze: false,
  external: false,
  allowCloud: false,
  allowRemote: false,
  webVerify: false,
  stopping: false,
  llmConfigOpen: false,
  error: "",
  notice: "",
  pollTimer: null,
  task: "expand",
  model: "",
  embedModel: "",
  generationModels: [],
  embeddingModels: [],
  unverifiedModels: [],
  discovering: false,
  discovered: false,
  modelNotice: "",
  taskInfo: {},
  stamps: {},
  externalDefault: "",
  estimate: null,
  estimating: false,
  async init() {
    await Promise.all([
      this.loadI18n(),
      this.loadDocuments(),
      this.loadSuggestions(),
      this.loadAcceptedCount(),
      this.refreshJobs(),
      this.loadModels(),
      this.loadProgress(),
    ]);
  },

  ...window.kbm,

  async loadModels(discover = false) {
    this.discovering = discover;
    try {
      const data = await this.requestJSON(
        discover ? "/api/models?discover=1" : "/api/models",
      );
      if (data.discovery_error) throw new Error(data.discovery_error);
      this.generationModels = data.generation_models || [];
      this.embeddingModels = data.embedding_models || [];
      this.unverifiedModels = data.unverified_models || [];
      this.taskInfo = data.tasks || {};
      this.stamps = data.stamps || {};
      this.externalDefault = data.external_default || "";
      this.discovered = Boolean(data.discovered);
      this.modelNotice = discover
        ? this.t(
            "ui.kb.models_refreshed",
            "Ollama models refreshed. {{.Count}} unverified model(s) excluded. No models downloaded.",
            { Count: this.unverifiedModels.length },
          )
        : "";
    } catch (error) {
      this.error = this.t(
        "ui.kb.models_query_failed",
        "Could not query models: {{.Error}}",
        { Error: error.message },
      );
    } finally {
      this.discovering = false;
    }
  },

  modelChoices(models, selected) {
    if (!selected || models.some((m) => m.name === selected)) return models;
    return [
      ...models,
      {
        name: selected,
        locality: "unknown",
        error: this.t(
          "ui.kb.model_unavailable_selected",
          "Selected model is no longer available; choose another model.",
        ),
      },
    ];
  },

  generationChoices() {
    return this.modelChoices(this.generationModels, this.model);
  },

  embeddingChoices() {
    return this.modelChoices(this.embeddingModels, this.embedModel);
  },

  modelLabel(model) {
    const source = model.configured
      ? this.t("ui.kb.model_source_configured", "configured")
      : this.t("ui.kb.model_source_discovered", "discovered");
    const verified = model.verified
      ? ""
      : this.t("ui.kb.model_unverified_suffix", "; unverified");
    return `${model.name} (${model.locality}; ${source}${verified})`;
  },

  // generationLabel/embedLabel mark the option matching the recorded lane
  // stamp so the dropdown shows which model ran "last time".
  generationLabel(model) {
    const stamp = this.generationStamp();
    const used = stamp && this.sameModel(model.name, stamp.model);
    return (
      this.modelLabel(model) +
      (used ? this.t("ui.kb.last_used_suffix", " — last used") : "")
    );
  },

  embedLabel(model) {
    const stamp = this.stamps.embedded;
    const used = stamp && this.sameModel(model.name, stamp.model);
    return (
      this.modelLabel(model) +
      (used ? this.t("ui.kb.last_used_suffix", " — last used") : "")
    );
  },

  // `discovered` is passed in from the template: reads through `this`
  // inside x-for-scoped method calls are not tracked by the CSP evaluator.
  modelUnavailable(model, discovered) {
    return Boolean(model.error || (discovered && !model.verified));
  },

  embeddingDefaultLabel() {
    const name = this.taskInfo.embed?.default;
    return name
      ? this.t("ui.kb.configured_default", "Configured default: {{.Model}}", {
          Model: name,
        })
      : this.t("ui.kb.no_embedding_model", "No configured embedding model");
  },

  // sameModel compares names loosely: "model" and "model:latest" are the
  // same Ollama tag.
  sameModel(a, b) {
    const norm = (name) => (name || "").replace(/:latest$/, "");
    return norm(a) === norm(b);
  },

  // taskStampKey maps the task dropdown to the lineage column its run stamps —
  // the analysis lane splits into local (analyzed) and external stamps.
  taskStampKey() {
    if (["analyze", "run", "all"].includes(this.task)) {
      return this.external ? "external" : "analyzed";
    }
    return (
      { expand: "expand", freshness: "freshness", enrich: "enriched" }[
        this.task
      ] || ""
    );
  },

  generationStamp() {
    return this.stamps[this.taskStampKey()] || null;
  },

  generationStampLine() {
    const stamp = this.generationStamp();
    return stamp
      ? this.t(
          "ui.kb.last_run_task",
          "Last run for this task: {{.Model}} ({{.Docs}} docs)",
          { Model: stamp.model, Docs: stamp.docs },
        )
      : "";
  },

  // effectiveGenerationModel is what the run would actually use: the per-run
  // override, else the configured default — the external lane default when the
  // enhanced lane is on, otherwise the task default (analyze's for batch ops).
  effectiveGenerationModel() {
    if (this.model) return this.model;
    const laneCapable = [
      "analyze",
      "run",
      "all",
      "expand",
      "freshness",
    ].includes(this.task);
    if (this.external && laneCapable) return this.externalDefault;
    if (["expand", "freshness", "enrich"].includes(this.task)) {
      return this.taskInfo[this.task]?.default || "";
    }
    return this.taskInfo.analyze?.default || "";
  },

  generationMismatch() {
    const stamp = this.generationStamp();
    const effective = this.effectiveGenerationModel();
    return Boolean(
      effective && stamp && !this.sameModel(effective, stamp.model),
    );
  },

  effectiveEmbedModel() {
    return this.embedModel || this.taskInfo.embed?.default || "";
  },

  embedStampLine() {
    const stamp = this.stamps.embedded;
    return stamp
      ? this.t(
          "ui.kb.last_embedding_run",
          "Last embedding run: {{.Model}} ({{.Docs}} docs)",
          { Model: stamp.model, Docs: stamp.docs },
        )
      : "";
  },

  embedMismatch() {
    const effective = this.effectiveEmbedModel();
    const stamp = this.stamps.embedded;
    return Boolean(
      effective && stamp && !this.sameModel(effective, stamp.model),
    );
  },

  // previewWork asks the server what the current task selection would queue —
  // pending counts per stage plus the dependents the evidence sweep would add.
  async previewWork() {
    this.error = "";
    this.estimating = true;
    const op = this.task;
    const extOps = ["analyze", "run", "expand", "freshness", "all"];
    try {
      this.checkModelSelection(op);
      this.estimate = await this.postJSON("/api/ops", {
        op,
        reanalyze: this.reanalyze,
        external: extOps.includes(op) && this.external,
        model: op !== "embed" ? this.model : "",
        embed_model: ["analyze", "run", "all", "embed"].includes(op)
          ? this.embedModel
          : "",
        allow_cloud: this.allowCloud,
        allow_remote: this.allowRemote,
        web_verify: ["freshness", "all"].includes(op) && this.webVerify,
        dry_run: true,
      });
    } catch (error) {
      this.error = error.message;
      this.estimate = null;
    } finally {
      this.estimating = false;
    }
  },

  estimateLines() {
    const e = this.estimate;
    if (!e) return [];
    const lines = [];
    for (const stage of ["enrich", "embed", "expand", "freshness", "analyze"]) {
      if (!e.pending || !(stage in e.pending)) continue;
      let line = this.t(
        "ui.kb.estimate_docs_queued",
        "{{.Stage}}: {{.Count}} document(s) queued",
        { Stage: stage, Count: e.pending[stage] },
      );
      if (stage === "analyze" && e.dependents > 0) {
        line += this.t(
          "ui.kb.estimate_dependents",
          " +{{.Dependents}} unchanged via changed evidence → {{.Total}} total",
          { Dependents: e.dependents, Total: e.analyze_total },
        );
      }
      lines.push(line);
    }
    if (lines.length === 0)
      lines.push(
        this.t("ui.kb.estimate_empty", "Nothing queued for this selection."),
      );
    return lines;
  },

  estimateWarnings() {
    return this.estimate?.warnings || [];
  },

  checkModelSelection(op) {
    const check = (models, name, cloudConsent, hint) => {
      if (!name) return;
      const model = models.find((m) => m.name === name);
      if (!model || this.modelUnavailable(model, this.discovered)) {
        throw new Error(
          this.t(
            "ui.kb.model_unavailable",
            "{{.Model}} is unavailable or unverified. Refresh models or choose another.",
            { Model: name },
          ),
        );
      }
      const remote =
        model.locality === "cloud" ||
        model.locality === "remote" ||
        (model.verified && model.locality === "unknown");
      if (remote && !cloudConsent) {
        throw new Error(
          this.t(
            "ui.kb.model_off_machine",
            "{{.Model}} may send document content off-machine. {{.Hint}}",
            { Model: name, Hint: hint },
          ),
        );
      }
    };
    if (op !== "embed") {
      check(
        this.generationModels,
        this.model,
        (op === "enrich" ? this.allowCloud : this.external) || this.allowRemote,
        op === "enrich"
          ? this.t(
              "ui.kb.hint_enable_cloud",
              "Enable cloud/remote embeddings and enrichment.",
            )
          : this.t(
              "ui.kb.hint_enable_external",
              "Enable the external generation lane.",
            ),
      );
    }
    if (["analyze", "run", "all", "embed"].includes(op)) {
      check(
        this.embeddingModels,
        this.embedModel,
        this.allowCloud || this.allowRemote,
        this.t(
          "ui.kb.hint_enable_cloud",
          "Enable cloud/remote embeddings and enrichment.",
        ),
      );
    }
  },

  // isRemoteName mirrors the backend's knownRemoteModel heuristic: a verified
  // locality wins, else the -cloud tag suffix or the external lane's model.
  isRemoteName(name) {
    if (!name) return false;
    const hit = [...this.generationModels, ...this.embeddingModels].find(
      (m) => m.name === name,
    );
    if (hit?.locality === "cloud" || hit?.locality === "remote") return true;
    const tag = name.slice(name.lastIndexOf(":") + 1);
    return (
      tag === "cloud" || tag.endsWith("-cloud") || name === this.externalDefault
    );
  },

  // needsRemoteConsent drives the inline allow_remote checkbox: it appears
  // when any lane the current selection would run resolves to a remote model
  // not already consented by the external lane or allow_cloud checkboxes.
  needsRemoteConsent() {
    const laneCapable = ["analyze", "expand", "freshness", "verify"];
    const genLanes = new Set(["enrich", "analyze"]); // Analyse / Scan+analyse buttons
    (
      ({
        all: ["enrich", "expand", "freshness", "analyze"],
        expand: ["expand"],
        freshness: ["freshness"],
        enrich: ["enrich"],
      })[this.task] || []
    ).forEach((t) => {
      genLanes.add(t);
    });
    if (this.webVerify && ["freshness", "all"].includes(this.task)) {
      genLanes.add("verify");
    }
    const uncovered = (lane, kind) => {
      const model =
        kind === "embed"
          ? this.effectiveEmbedModel()
          : this.model ||
            (this.external && laneCapable.includes(lane)
              ? this.externalDefault
              : this.taskInfo[lane]?.default || "");
      if (!this.isRemoteName(model)) return false;
      if (this.external && kind === "gen" && laneCapable.includes(lane))
        return false;
      if (this.allowCloud && (lane === "enrich" || kind === "embed"))
        return false;
      return true;
    };
    if ([...genLanes].some((lane) => uncovered(lane, "gen"))) return true;
    return uncovered("embed", "embed");
  },

  async refreshData() {
    await Promise.all([
      this.loadDocuments(),
      this.loadSuggestions(),
      this.loadAcceptedCount(),
      this.loadProgress(),
    ]);
  },

  // refreshQueue re-pulls only what a review decision can change — the queue
  // list and the accepted/pending badges. Documents and the progress chart
  // do not move on a decide, so they stay out of this path.
  async refreshQueue() {
    await Promise.all([this.loadSuggestions(), this.loadAcceptedCount()]);
  },

  // usage.md is split by "### slug" headings into help fragments; each
  // fragment is a list of blocks rendered by x-for/x-text (no x-html, so
  // nothing needs sanitizing). helpOpen holds one slug at a time.
  helpOpen: null,
  usageFrags: null,

  async loadUsage() {
    if (this.usageFrags) return;
    try {
      const res = await fetch("/static/usage.md");
      if (!res.ok) return;
      this.usageFrags = this.parseUsage(await res.text());
    } catch {
      /* help stays unavailable */
    }
  },

  parseUsage(md) {
    const frags = {};
    let slug = null;
    for (const line of md.split("\n")) {
      const h = line.match(/^###\s+(\S+)/);
      if (h) {
        slug = h[1];
        frags[slug] = [];
        continue;
      }
      if (/^#{1,2}\s/.test(line)) slug = null;
      if (slug) frags[slug].push(line);
    }
    for (const k of Object.keys(frags)) {
      const blocks = [];
      for (const line of frags[k].join("\n").split(/\n\n+/)) {
        const t = line.trim();
        if (!t) continue;
        if (t.startsWith("- ")) {
          for (const item of t.split(/\n- /)) {
            blocks.push({ t: "li", s: item.replace(/^- /, "") });
          }
        } else {
          blocks.push({ t: "p", s: t });
        }
      }
      frags[k] = blocks;
    }
    return frags;
  },

  toggleHelp(id) {
    this.helpOpen = this.helpOpen === id ? null : id;
    this.loadUsage();
  },

  helpBlocks(id) {
    return (this.usageFrags && this.usageFrags[id]) || [];
  },

  async loadProgress() {
    try {
      const data = await this.requestJSON("/api/progress");
      this.progressDays = data.days || [];
      this.renderProgressChart();
    } catch (error) {
      if (!this.stopping) {
        this.error = error.message;
      }
    }
  },

  // progressSeries accumulates per-day pipeline counts into cumulative
  // series — monotone lines make "progress over time" readable. It is pure
  // (no Chart dependency) so the frontend test can assert it without a canvas.
  progressSeries() {
    const totals = {
      notes: 0,
      analyzed: 0,
      suggestions: 0,
      handled: 0,
      applied: 0,
    };
    const series = {
      labels: [],
      notes: [],
      analyzed: [],
      suggestions: [],
      handled: [],
      applied: [],
      backlog: [],
    };
    for (const day of this.progressDays) {
      totals.notes += day.notes || 0;
      totals.analyzed += day.analyzed || 0;
      totals.suggestions += day.suggestions || 0;
      totals.handled += day.handled || 0;
      totals.applied += day.applied || 0;
      series.labels.push(day.date);
      series.notes.push(totals.notes);
      series.analyzed.push(totals.analyzed);
      series.suggestions.push(totals.suggestions);
      series.handled.push(totals.handled);
      series.applied.push(totals.applied);
      series.backlog.push(totals.suggestions - totals.handled);
    }
    return series;
  },

  renderProgressChart() {
    if (typeof window === "undefined" || typeof window.Chart === "undefined") {
      return;
    }
    const canvas = document.getElementById("knowledgeProgressChart");
    if (!canvas) return;
    const series = this.progressSeries();
    const existing = window.Chart.getChart(canvas);
    if (series.labels.length === 0) {
      existing?.destroy();
      return;
    }
    if (existing) {
      existing.data.labels = series.labels;
      existing.data.datasets.forEach((entry) => {
        entry.data = series[entry.seriesKey] || [];
      });
      existing.update();
      return;
    }
    const dataset = (label, key, color) =>
      this.lineDataset(label, series[key], color, { seriesKey: key });
    new window.Chart(canvas.getContext("2d"), {
      type: "line",
      data: {
        labels: series.labels,
        datasets: [
          dataset("Notes", "notes", "#285444"),
          dataset("Analyzed", "analyzed", "#18352d"),
          dataset("Applied", "applied", "#5b7f95"),
          dataset("Review backlog", "backlog", "#a64035"),
        ],
      },
      options: this.lineChartOptions(),
    });
  },

  // reviewTotalsText renders the all-time suggestion counters as text: they
  // are monotone totals that never trend back to zero, so they sit beside
  // the review stat instead of cluttering the progress chart.
  reviewTotalsText() {
    const series = this.progressSeries();
    const total = (key) => series[key].at(-1) ?? 0;
    return this.t(
      "ui.kb.review_totals",
      "{{.Suggestions}} suggestions · {{.Handled}} handled",
      {
        Suggestions: compactCount.format(total("suggestions")),
        Handled: compactCount.format(total("handled")),
      },
    );
  },

  async loadDocuments() {
    this.documents = await this.requestJSON("/api/documents");
  },

  filteredSuggestions() {
    return this.suggestions.filter(
      (s) =>
        (this.originFilter === "all" ||
          (s.origin || "analysis") === this.originFilter) &&
        (this.typeFilter === "all" ||
          (s.change_types || []).includes(this.typeFilter)),
    );
  },

  noteGroups() {
    const matches = new Set(this.filteredSuggestions().map((s) => s.id));
    const notes = new Map();
    for (const suggestion of this.suggestions) {
      const path = suggestion.document_path;
      if (!notes.has(path)) {
        notes.set(path, {
          path,
          documentId: suggestion.document_id || "",
          suggestions: [],
          matched: false,
        });
      }
      const note = notes.get(path);
      suggestion.first_in_note = note.suggestions.length === 0;
      note.suggestions.push(suggestion);
      note.matched ||= matches.has(suggestion.id);
    }
    // Filters select notes, not isolated payload fragments. Keep alternatives
    // visible so selecting a "frontmatter" facet never hides a conflicting edit.
    return [...notes.values()]
      .filter((note) => note.matched)
      .sort((a, b) => a.path.localeCompare(b.path));
  },

  changeTypeLabel(kind) {
    switch (kind) {
      case "frontmatter":
        return this.t("ui.kb.type_frontmatter", "Frontmatter");
      case "links":
        return this.t("ui.kb.type_links", "Links");
      case "body":
        return this.t("ui.kb.type_body", "Body");
      case "identity":
        return this.t("ui.kb.type_identity", "Identity");
      case "file":
        return this.t("ui.kb.type_file", "File operations");
      default:
        return this.t("ui.kb.type_unknown", "Unclassified");
    }
  },

  async previewReconciliation() {
    this.error = "";
    this.reconciling = true;
    try {
      this.queuePlan = await this.requestJSON("/api/reconcile");
    } catch (error) {
      this.error = error.message;
    } finally {
      this.reconciling = false;
    }
  },

  retireItems() {
    return (this.queuePlan?.items || []).filter((item) => item.retire);
  },

  queueCounts() {
    return Object.entries(this.queuePlan?.counts || {}).map(
      ([state, count]) => ({
        state,
        count,
      }),
    );
  },

  async confirmReconciliation() {
    if (!this.queuePlan || this.queuePlan.retire_count === 0) return;
    if (
      !(await this.confirmDialog(
        this.t(
          "ui.kb.reconcile_confirm",
          "Retire {{.Count}} obsolete proposals? Notes and valid/blocked proposals will not change.",
          { Count: this.queuePlan.retire_count },
        ),
      ))
    ) {
      return;
    }
    this.error = "";
    this.reconciling = true;
    try {
      const result = await this.postJSON("/api/reconcile", {
        confirm: true,
        token: this.queuePlan.token,
      });
      this.queuePlan = null;
      this.notice = this.t(
        "ui.kb.reconcile_finished",
        "Retired {{.Count}} obsolete proposals. No notes were changed.",
        { Count: result.retired },
      );
      await this.refreshQueue();
    } catch (error) {
      this.error = error.message;
    } finally {
      this.reconciling = false;
    }
  },

  async loadSuggestions() {
    const query = new URLSearchParams({ status: this.statusFilter });
    this.suggestions = await this.requestJSON(`/api/suggestions?${query}`);
    if (this.statusFilter === "pending") {
      this.pendingCount = this.suggestions.length;
    } else {
      await this.loadPendingCount();
    }
  },

  async loadPendingCount() {
    if (this.statusFilter === "pending") return;
    const rows = await this.requestJSON("/api/suggestions?status=pending");
    this.pendingCount = rows.length;
  },

  async loadAcceptedCount() {
    const rows = await this.requestJSON("/api/suggestions?status=accepted");
    this.acceptedCount = rows.length;
  },

  async startOperation(op) {
    this.error = "";
    this.notice = "";
    this.estimate = null;
    const llmOps = [
      "analyze",
      "run",
      "enrich",
      "embed",
      "expand",
      "freshness",
      "all",
    ];
    const extOps = ["analyze", "run", "expand", "freshness", "all"];
    const reanalyzeOps = [
      "analyze",
      "run",
      "enrich",
      "embed",
      "expand",
      "freshness",
      "all",
    ];
    try {
      if (llmOps.includes(op)) {
        if (this.discovering)
          throw new Error(
            this.t(
              "ui.kb.wait_discovery",
              "Wait for model discovery to finish.",
            ),
          );
        this.checkModelSelection(op);
      }
      await this.postJSON("/api/ops", {
        op,
        reanalyze: reanalyzeOps.includes(op) && this.reanalyze,
        external: extOps.includes(op) && this.external,
        model: llmOps.includes(op) && op !== "embed" ? this.model : "",
        embed_model: ["analyze", "run", "all", "embed"].includes(op)
          ? this.embedModel
          : "",
        allow_cloud: llmOps.includes(op) && this.allowCloud,
        allow_remote: llmOps.includes(op) && this.allowRemote,
        web_verify: ["freshness", "all"].includes(op) && this.webVerify,
      });
      this.busy = true;
      this.notice = this.t(
        "ui.kb.op_started",
        "{{.Op}} started. The page will refresh when it finishes.",
        { Op: op },
      );
      await this.refreshJobs();
    } catch (error) {
      this.error = error.message;
    }
  },

  async stopServer() {
    this.error = "";
    const forced = await this.confirmDialog(
      this.busy
        ? this.t(
            "ui.kb.stop_busy_confirm",
            "An operation is running — stop the server anyway? Its job will be marked failed.",
          )
        : this.t(
            "ui.kb.stop_confirm",
            "Stop the KBM-flow server? This page will go offline.",
          ),
    );
    if (!forced) return;
    try {
      await this.requestJSON(`/api/shutdown${this.busy ? "?force=1" : ""}`, {
        method: "POST",
      });
      this.stopping = true;
      clearTimeout(this.pollTimer);
      this.notice = this.t(
        "ui.kb.stopping_notice",
        "Server is stopping — this page will go offline. Restart with `KBM-flow serve`.",
      );
    } catch (error) {
      this.error = error.message;
    }
  },

  runningJobs() {
    return (this.jobs || []).filter((j) => j.status === "running");
  },

  jobPercent(job) {
    const m = /(\d+)\/(\d+)/.exec(job?.detail || "");
    return m ? Math.round((+m[1] / +m[2]) * 100) : null;
  },

  async refreshJobs() {
    try {
      this.jobs = await this.requestJSON("/api/ops");
      const wasBusy = this.busy;
      this.busy = this.jobs.some((job) => job.status === "running");
      // While an op runs, keep documents/suggestions live so progress shows
      // up per document instead of only after the job finishes. The busy→
      // idle transition refreshes once more and clears the op notice —
      // startOperation promises the page will refresh when it finishes.
      if (this.busy || wasBusy) {
        await this.refreshData();
        if (!this.busy) this.notice = "";
      }
    } catch (error) {
      // Once the server is stopping, poll failures are expected — don't
      // overwrite the "server is stopping" notice with fetch errors.
      if (!this.stopping) {
        this.error = error.message;
      }
    } finally {
      // Poll jobs even when idle so work started via the CLI shows up here.
      clearTimeout(this.pollTimer);
      if (!this.stopping) {
        this.pollTimer = setTimeout(
          () => this.refreshJobs(),
          this.busy ? 1500 : 10000,
        );
      }
    }
  },

  async toggleProposal(id) {
    this.proposalOpen[id] = !this.proposalOpen[id];
    if (!this.proposalOpen[id] || Object.hasOwn(this.proposals, id)) return;
    this.proposals[id] = null;
    try {
      const suggestion = await this.requestJSON(`/api/suggestions/${id}`);
      const p = {
        proposed: suggestion.proposed_content || "",
        current: suggestion.current_content,
        currentError: suggestion.current_content_error,
      };
      if (this.proposalHasDiff(p)) {
        p.diff = this.diffLines(p.current, p.proposed);
        p.unchanged = p.diff.every((o) => o.t === "ctx");
      }
      this.proposals[id] = p;
      this.proposalView[id] = p.diff ? "diff" : "full";
    } catch (error) {
      this.proposals[id] = {
        proposed: this.t(
          "ui.kb.proposal_load_failed",
          "Unable to load proposed content: {{.Error}}",
          { Error: error.message },
        ),
      };
      this.proposalView[id] = "full";
    }
  },

  // Alpine's CSP evaluator calls template-invoked methods with `this`
  // bound to a scope object, not the reactive proxy — `this.proposals`
  // reads inside a method are never tracked, so the binding went stale.
  // Pass the reactive value in from the expression instead.
  proposalText(p) {
    if (p === undefined || p === null)
      return this.t("ui.kb.loading", "Loading…");
    const content = typeof p === "string" ? p : p.proposed;
    return (
      content || this.t("ui.kb.no_proposed_content", "(No proposed content)")
    );
  },

  // A diff is only meaningful when the handler could read the current file —
  // absent field (old server) or a read error falls back to full text.
  proposalHasDiff(p) {
    return (
      p !== null &&
      typeof p === "object" &&
      typeof p.current === "string" &&
      !p.currentError
    );
  },

  // Line diff for the inspector: common prefix/suffix lines are context, the
  // middle runs an LCS so removed/added/changed regions are highlighted in
  // order. Oversized middles degrade to whole-block del/add rather than
  // allocating a quadratic matrix.
  diffLines(oldText, newText) {
    const a = oldText.split("\n");
    const b = newText.split("\n");
    let lo = 0;
    while (lo < a.length && lo < b.length && a[lo] === b[lo]) lo++;
    let hi = 0;
    while (
      hi < a.length - lo &&
      hi < b.length - lo &&
      a[a.length - 1 - hi] === b[b.length - 1 - hi]
    )
      hi++;
    const aMid = a.slice(lo, a.length - hi);
    const bMid = b.slice(lo, b.length - hi);
    const ops = a.slice(0, lo).map((s) => ({ t: "ctx", s }));
    if (aMid.length * bMid.length > 4000000) {
      for (const s of aMid) ops.push({ t: "del", s });
      for (const s of bMid) ops.push({ t: "add", s });
    } else {
      ops.push(...lcsLines(aMid, bMid));
    }
    for (const s of a.slice(a.length - hi)) ops.push({ t: "ctx", s });
    return ops;
  },

  // The ops list is computed once at fetch time (p.diff) — CSP Alpine forbids
  // x-html, so the template walks ops with x-for + x-text (inherently safe).
  proposalDiffOps(p) {
    return p?.diff || [];
  },

  async decide(id, status) {
    this.error = "";
    // A short "why" on rejections is stored with the suggestion and shown
    // back to the model on later runs — the feedback channel for stale facts.
    let note = "";
    if (status === "rejected") {
      const answer = await this.promptDialog(
        this.t(
          "ui.kb.reject_prompt",
          "Optional: why is this wrong? (shown to the model on future runs)",
        ),
        this.t("ui.kb.reject", "Reject"),
      );
      if (answer === null) return; // Cancel aborts the rejection, not just the note
      note = answer;
    }
    try {
      await this.postJSON(`/api/suggestions/${id}/decide`, { status, note });
      // Optimistic removal: splice the decided card out of the visible list
      // so the queue updates without a refetch — a background queue-only
      // refresh then reconciles server truth (sibling supersessions).
      const i = this.suggestions.findIndex((s) => s.id === id);
      if (i >= 0) {
        const prev = this.suggestions[i].status;
        this.suggestions.splice(i, 1);
        if (prev === "pending") this.pendingCount--;
        if (prev === "accepted") this.acceptedCount--;
        if (status === "accepted") this.acceptedCount++;
        if (status === "pending") this.pendingCount++;
        delete this.proposals[id];
        delete this.proposalOpen[id];
        delete this.proposalView[id];
      }
      this.queuePlan = null;
      this.notice = this.t(
        "ui.kb.suggestion_decided",
        "Suggestion #{{.Id}} {{.Status}}.",
        {
          Id: id,
          Status: status,
        },
      );
      this.refreshQueue().catch((error) => {
        this.error = error.message;
      });
    } catch (error) {
      this.error = error.message;
    }
  },

  async applyAccepted() {
    if (this.acceptedCount === 0 || this.applying) return;
    if (
      !(await this.confirmDialog(
        this.t(
          "ui.kb.apply_confirm",
          "Apply {{.Count}} accepted suggestion(s) to the repository?",
          { Count: this.acceptedCount },
        ),
      ))
    ) {
      return;
    }
    this.error = "";
    this.applyResults = [];
    this.applying = true;
    try {
      const results = await this.requestJSON("/api/apply", { method: "POST" });
      this.applyResults = results;
      const { applied, failed, superseded, blocked } =
        this.applySummary(results);
      this.notice = this.t(
        "ui.kb.apply_finished",
        "Apply finished: {{.Applied}} applied, {{.Failed}} failed, {{.Superseded}} superseded, {{.Blocked}} blocked.",
        {
          Applied: applied,
          Failed: failed,
          Superseded: superseded,
          Blocked: blocked,
        },
      );
      const trigger = results.find(
        (result) => result.action === "reindex-trigger",
      );
      if (trigger && !trigger.applied) {
        this.error = this.t(
          "ui.kb.apply_reindex_failed",
          "Suggestions were applied, but the reindex trigger failed: {{.Error}}",
          { Error: trigger.error },
        );
      }
      await this.refreshData();
    } catch (error) {
      this.error = error.message;
    } finally {
      this.applying = false;
    }
  },

  applySummary(results) {
    const suggestions = results.filter(
      (result) => result.action !== "reindex-trigger",
    );
    return {
      applied: suggestions.filter((result) => result.applied).length,
      failed: suggestions.filter((result) => result.status === "failed").length,
      superseded: suggestions.filter((result) => result.status === "superseded")
        .length,
      blocked: suggestions.filter((result) => result.status === "blocked")
        .length,
    };
  },

  applyLabel() {
    if (this.applying) return this.t("ui.kb.applying", "Applying…");
    return this.t("ui.kb.apply_accepted", "Apply accepted ({{.Count}})", {
      Count: this.acceptedCount,
    });
  },

  documentTags(document) {
    try {
      return JSON.parse(document.tags_json).join(", ") || "—";
    } catch {
      return "—";
    }
  },

  frontmatterIssues(document) {
    try {
      const issues = JSON.parse(document.frontmatter_issues || "[]");
      return Array.isArray(issues) ? issues : [];
    } catch {
      return [];
    }
  },

  kbmBlock(document) {
    try {
      const fields = JSON.parse(document.frontmatter_json || "{}");
      return fields && typeof fields.kbm === "object" ? fields.kbm : {};
    } catch {
      return {};
    }
  },

  linkCheckBadge(document) {
    const check = this.kbmBlock(document).link_check;
    if (!check) return "";
    const dead = (check.dead || []).length;
    const unverified = (check.unverified || []).length;
    if (dead > 0)
      return this.t("ui.kb.dead_links", "{{.Count}} dead link(s)", {
        Count: dead,
      });
    if (unverified > 0)
      return this.t("ui.kb.unverified_links", "{{.Count}} unverified link(s)", {
        Count: unverified,
      });
    return this.t("ui.kb.links_ok", "links ok");
  },

  freshnessBadge(document) {
    const fresh = this.kbmBlock(document).freshness;
    if (!fresh) return "";
    const flags = (fresh.flags || []).length;
    if (fresh.verdict === "review" || flags > 0) {
      return this.t("ui.kb.freshness_flags", "freshness: {{.Count}} flag(s)", {
        Count: flags,
      });
    }
    return this.t("ui.kb.freshness_current", "freshness: current");
  },

  confidenceLabel(suggestion) {
    return this.t("ui.kb.confidence", "{{.Pct}}% confidence", {
      Pct: Math.round(suggestion.confidence * 100),
    });
  },

  // actionIcon maps a suggestion action to its icon-* registry class so the
  // tag reads at a glance. Unknown/future actions fall back to the generic
  // file mark.
  actionIcon(action) {
    const icons = {
      move: "icon-move",
      merge: "icon-merge",
      expand: "icon-file-plus",
      delete: "icon-trash-2",
      none: "icon-file-text",
    };
    return icons[action] || "icon-file-text";
  },

  // Tooltip helpers — Alpine expressions cannot reach globals like JSON,
  // so pretty-printing for :title happens here in component scope.
  linkCheckTooltip(document) {
    return JSON.stringify(this.kbmBlock(document).link_check || {});
  },

  freshnessTooltip(document) {
    return JSON.stringify(this.kbmBlock(document).freshness || {});
  },

  // changeKind distinguishes a content edit from a metadata-only touch:
  // the split fm/body hashes are compared against what the last analysis saw.
  changeKind(document) {
    if (!document.analyzed_sha256 || !document.body_sha256) return "";
    if (document.sha256 === document.analyzed_sha256) return "";
    if (
      document.seen_body_sha256 &&
      document.body_sha256 !== document.seen_body_sha256
    )
      return this.t("ui.kb.change_content", "content changed");
    if (
      document.seen_fm_sha256 &&
      document.fm_sha256 !== document.seen_fm_sha256
    )
      return this.t("ui.kb.change_metadata", "metadata changed");
    return this.t("ui.kb.change_generic", "changed");
  },
});

if (typeof document !== "undefined") {
  document.addEventListener("alpine:init", () => {
    Alpine.data("knowledgeDashboard", window.knowledgeDashboard);
  });
}
