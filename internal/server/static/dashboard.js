// Chart instances live in module scope, not on the reactive component — a
// reactive proxy around a Chart (which holds circular refs and a DOM canvas)
// is a known "Maximum call stack size exceeded" trigger.
const charts = new Map();

window.knowledgeDashboard = () => ({
  documents: [],
  suggestions: [],
  jobs: [],
  progressDays: [],
  proposals: {},
  proposalOpen: {},
  statusFilter: "pending",
  originFilter: "all",
  pendingCount: 0,
  acceptedCount: 0,
  busy: false,
  applying: false,
  reanalyze: false,
  external: false,
  allowCloud: false,
  stopping: false,
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
      this.loadDocuments(),
      this.loadSuggestions(),
      this.loadAcceptedCount(),
      this.refreshJobs(),
      this.loadModels(),
      this.loadProgress(),
    ]);
  },

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
        ? `Ollama models refreshed. ${this.unverifiedModels.length} unverified model(s) excluded. No models downloaded.`
        : "";
    } catch (error) {
      this.error = `Could not query models: ${error.message}`;
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
        error: "Selected model is no longer available; choose another model.",
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
    const source = model.configured ? "configured" : "discovered";
    const verified = model.verified ? "" : "; unverified";
    return `${model.name} (${model.locality}; ${source}${verified})`;
  },

  // generationLabel/embedLabel mark the option matching the recorded lane
  // stamp so the dropdown shows which model ran "last time".
  generationLabel(model) {
    const stamp = this.generationStamp();
    const used = stamp && this.sameModel(model.name, stamp.model);
    return this.modelLabel(model) + (used ? " — last used" : "");
  },

  embedLabel(model) {
    const stamp = this.stamps.embedded;
    const used = stamp && this.sameModel(model.name, stamp.model);
    return this.modelLabel(model) + (used ? " — last used" : "");
  },

  // `discovered` is passed in from the template: reads through `this`
  // inside x-for-scoped method calls are not tracked by the CSP evaluator.
  modelUnavailable(model, discovered) {
    return Boolean(model.error || (discovered && !model.verified));
  },

  embeddingDefaultLabel() {
    const name = this.taskInfo.embed?.default;
    return name
      ? `Configured default: ${name}`
      : "No configured embedding model";
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
      ? `Last run for this task: ${stamp.model} (${stamp.docs} docs)`
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
      ? `Last embedding run: ${stamp.model} (${stamp.docs} docs)`
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
      this.estimate = await this.requestJSON("/api/ops", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          op,
          reanalyze: this.reanalyze,
          external: extOps.includes(op) && this.external,
          model: op !== "embed" ? this.model : "",
          embed_model: ["analyze", "run", "all", "embed"].includes(op)
            ? this.embedModel
            : "",
          allow_cloud: this.allowCloud,
          dry_run: true,
        }),
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
      let line = `${stage}: ${e.pending[stage]} document(s) queued`;
      if (stage === "analyze" && e.dependents > 0) {
        line += ` +${e.dependents} unchanged via changed evidence → ${e.analyze_total} total`;
      }
      lines.push(line);
    }
    if (lines.length === 0) lines.push("Nothing queued for this selection.");
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
          `${name} is unavailable or unverified. Refresh models or choose another.`,
        );
      }
      const remote =
        model.locality === "cloud" ||
        model.locality === "remote" ||
        (model.verified && model.locality === "unknown");
      if (remote && !cloudConsent) {
        throw new Error(
          `${name} may send document content off-machine. ${hint}`,
        );
      }
    };
    if (op !== "embed") {
      check(
        this.generationModels,
        this.model,
        op === "enrich" ? this.allowCloud : this.external,
        op === "enrich"
          ? "Enable cloud/remote embeddings and enrichment."
          : "Enable the external generation lane.",
      );
    }
    if (["analyze", "run", "all", "embed"].includes(op)) {
      check(
        this.embeddingModels,
        this.embedModel,
        this.allowCloud,
        "Enable cloud/remote embeddings and enrichment.",
      );
    }
  },

  async requestJSON(url, options = {}) {
    const response = await fetch(url, options);
    let body;
    try {
      body = await response.json();
    } catch {
      body = {};
    }
    if (!response.ok) {
      throw new Error(body.error || `Request failed (HTTP ${response.status})`);
    }
    return body;
  },

  async refreshData() {
    await Promise.all([
      this.loadDocuments(),
      this.loadSuggestions(),
      this.loadAcceptedCount(),
      this.loadProgress(),
    ]);
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
    const existing = charts.get("knowledgeProgressChart");
    if (series.labels.length === 0) {
      if (existing) {
        existing.destroy();
        charts.delete("knowledgeProgressChart");
      }
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
    const dataset = (label, key, color) => ({
      label,
      seriesKey: key,
      data: series[key],
      borderColor: color,
      backgroundColor: color,
      tension: 0.2,
    });
    charts.set(
      "knowledgeProgressChart",
      new window.Chart(canvas.getContext("2d"), {
        type: "line",
        data: {
          labels: series.labels,
          datasets: [
            dataset("Notes", "notes", "#285444"),
            dataset("Analyzed", "analyzed", "#18352d"),
            dataset("Suggestions", "suggestions", "#d87536"),
            dataset("Handled", "handled", "#68756e"),
            dataset("Applied", "applied", "#5b7f95"),
            dataset("Review backlog", "backlog", "#a64035"),
          ],
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          interaction: { mode: "index", intersect: false },
          scales: { y: { beginAtZero: true, ticks: { precision: 0 } } },
          plugins: { legend: { position: "bottom" } },
        },
      }),
    );
  },

  async loadDocuments() {
    this.documents = await this.requestJSON("/api/documents");
  },

  filteredSuggestions() {
    if (this.originFilter === "all") return this.suggestions;
    return this.suggestions.filter(
      (s) => (s.origin || "analysis") === this.originFilter,
    );
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
          throw new Error("Wait for model discovery to finish.");
        this.checkModelSelection(op);
      }
      await this.requestJSON("/api/ops", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          op,
          reanalyze: reanalyzeOps.includes(op) && this.reanalyze,
          external: extOps.includes(op) && this.external,
          model: llmOps.includes(op) && op !== "embed" ? this.model : "",
          embed_model: ["analyze", "run", "all", "embed"].includes(op)
            ? this.embedModel
            : "",
          allow_cloud: llmOps.includes(op) && this.allowCloud,
        }),
      });
      this.busy = true;
      this.notice = `${op} started. The page will refresh when it finishes.`;
      await this.refreshJobs();
    } catch (error) {
      this.error = error.message;
    }
  },

  async stopServer() {
    this.error = "";
    const forced = this.busy
      ? window.confirm(
          "An operation is running — stop the server anyway? Its job will be marked failed.",
        )
      : window.confirm("Stop the KBM-flow server? This page will go offline.");
    if (!forced) return;
    try {
      await this.requestJSON(`/api/shutdown${this.busy ? "?force=1" : ""}`, {
        method: "POST",
      });
      this.stopping = true;
      clearTimeout(this.pollTimer);
      this.notice =
        "Server is stopping — this page will go offline. Restart with `KBM-flow serve`.";
    } catch (error) {
      this.error = error.message;
    }
  },

  async refreshJobs() {
    try {
      this.jobs = await this.requestJSON("/api/ops");
      this.busy = this.jobs.some((job) => job.status === "running");
      // While an op runs, keep documents/suggestions live so progress shows
      // up per document instead of only after the job finishes.
      if (this.busy) {
        await this.refreshData();
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
      this.proposals[id] = suggestion.proposed_content || "";
    } catch (error) {
      this.proposals[id] = `Unable to load proposed content: ${error.message}`;
    }
  },

  // Alpine's CSP evaluator calls template-invoked methods with `this`
  // bound to a scope object, not the reactive proxy — `this.proposals`
  // reads inside a method are never tracked, so the binding went stale.
  // Pass the reactive value in from the expression instead.
  proposalText(content) {
    if (content === undefined || content === null) return "Loading…";
    return content || "(No proposed content)";
  },

  async decide(id, status) {
    this.error = "";
    // A short "why" on rejections is stored with the suggestion and shown
    // back to the model on later runs — the feedback channel for stale facts.
    let note = "";
    if (status === "rejected") {
      const answer = window.prompt(
        "Optional: why is this wrong? (shown to the model on future runs)",
        "",
      );
      if (answer === null) return; // Cancel aborts the rejection, not just the note
      note = answer;
    }
    try {
      await this.requestJSON(`/api/suggestions/${id}/decide`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ status, note }),
      });
      this.notice = `Suggestion #${id} ${status}.`;
      await this.refreshData();
    } catch (error) {
      this.error = error.message;
    }
  },

  async applyAccepted() {
    if (this.acceptedCount === 0 || this.applying) return;
    if (
      !window.confirm(
        `Apply ${this.acceptedCount} accepted suggestion(s) to the repository?`,
      )
    ) {
      return;
    }
    this.error = "";
    this.applying = true;
    try {
      const results = await this.requestJSON("/api/apply", { method: "POST" });
      const applied = results.filter((result) => result.applied).length;
      const failed = results.filter(
        (result) => result.status === "failed",
      ).length;
      const superseded = results.filter(
        (result) => result.status === "superseded",
      ).length;
      this.notice = `Apply finished: ${applied} applied, ${failed} failed, ${superseded} superseded.`;
      await this.refreshData();
    } catch (error) {
      this.error = error.message;
    } finally {
      this.applying = false;
    }
  },

  applyLabel() {
    if (this.applying) return "Applying…";
    return `Apply accepted (${this.acceptedCount})`;
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
    if (dead > 0) return `${dead} dead link(s)`;
    if (unverified > 0) return `${unverified} unverified link(s)`;
    return "links ok";
  },

  freshnessBadge(document) {
    const fresh = this.kbmBlock(document).freshness;
    if (!fresh) return "";
    const flags = (fresh.flags || []).length;
    if (fresh.verdict === "review" || flags > 0) {
      return `freshness: ${flags} flag(s)`;
    }
    return "freshness: current";
  },

  confidenceLabel(suggestion) {
    return Math.round(suggestion.confidence * 100) + "% confidence";
  },

  // actionIcon maps a suggestion action to its Lucide mark so the tag reads
  // at a glance. Unknown/future actions fall back to the generic file mark.
  actionIcon(action) {
    const icons = {
      move: "move.svg",
      merge: "merge.svg",
      expand: "file-plus.svg",
      delete: "trash-2.svg",
      none: "file-text.svg",
    };
    return `/static/icons/${icons[action] || "file-text.svg"}`;
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
      return "content changed";
    if (
      document.seen_fm_sha256 &&
      document.fm_sha256 !== document.seen_fm_sha256
    )
      return "metadata changed";
    return "changed";
  },

  jobTagClass(status) {
    if (status === "done") return "is-success is-light";
    if (status === "failed") return "is-danger is-light";
    return "is-warning is-light";
  },
});

if (typeof document !== "undefined") {
  document.addEventListener("alpine:init", () => {
    Alpine.data("knowledgeDashboard", window.knowledgeDashboard);
  });
}
