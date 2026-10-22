window.landingDashboard = () => ({
  overview: null,
  loading: true,
  error: "",

  async init() {
    await this.refresh();
  },

  async requestJSON(url) {
    const response = await fetch(url);
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

  async refresh() {
    this.loading = true;
    this.error = "";
    try {
      this.overview = await this.requestJSON("/api/dashboard");
    } catch (error) {
      this.error = error.message;
    } finally {
      this.loading = false;
    }
  },

  get knowledge() {
    return this.overview?.knowledge || {};
  },

  get report() {
    return this.overview?.report || {};
  },

  knowledgeValue(key) {
    if (key === "changed_files" && !this.knowledge.last_cycle_at) return "—";
    return this.knowledge[key] ?? "—";
  },

  reportValue(key) {
    if (!this.report.generated_at) return "—";
    return this.report.summary?.[key] ?? "—";
  },

  comparisonValue(key) {
    const comparison = this.report.comparison;
    if (!comparison?.available) return "—";
    const value = comparison[key];
    return value === null || value === undefined ? "—" : value;
  },

  reportMetric(key) {
    const value = this.report.summary?.[key];
    return value === null || value === undefined
      ? this.comparisonValue(key)
      : value;
  },

  cycleAge() {
    const days = this.knowledge.days_since_last_cycle;
    if (days === null || days === undefined) return "Never";
    if (days === 0) return "Today";
    return `${days} ${days === 1 ? "day" : "days"}`;
  },

  lastCycleText() {
    const at = this.knowledge.last_cycle_at;
    return at
      ? `Last successful run · ${this.formatTime(at)}`
      : "No full run recorded yet";
  },

  reportFreshness() {
    if (!this.report.generated_at) return "No repository snapshot yet";
    const reference = this.report.comparison;
    const local = `Local scan · ${this.formatTime(this.report.generated_at)}`;
    const stale = this.report.summary?.remote_stale_repositories || 0;
    const staleText = stale > 0 ? ` · ${stale} stale remote repos` : "";
    if (this.report.remote_enabled) {
      return `${local} · remote checks · ${this.formatTime(this.report.remote_generated_at)}${staleText}`;
    }
    if (this.report.remote_generated_at) {
      return `${local} · remote last refreshed · ${this.formatTime(this.report.remote_generated_at)}${staleText}`;
    }
    if (!reference?.available) return `${local} · remote results unavailable`;
    return `${local} · Ruby reference · ${this.formatTime(reference.generated_at)}`;
  },

  formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
  },
});

if (typeof document !== "undefined") {
  document.addEventListener("alpine:init", () => {
    Alpine.data("landingDashboard", window.landingDashboard);
  });
}
