window.landingDashboard = () => ({
  overview: null,
  loading: true,
  error: "",

  async init() {
    await Promise.all([this.loadI18n(), this.refresh()]);
  },

  ...window.kbm,

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
    if (days === null || days === undefined)
      return this.t("ui.landing.cycle_never", "Never");
    if (days === 0) return this.t("ui.landing.cycle_today", "Today");
    return this.t("ui.landing.cycle_days", "{{.Count}} day(s)", {
      Count: days,
    });
  },

  lastCycleText() {
    const at = this.knowledge.last_cycle_at;
    return at
      ? this.t("ui.landing.last_run", "Last successful run · {{.Time}}", {
          Time: this.formatTime(at),
        })
      : this.t("ui.landing.no_full_run", "No full run recorded yet");
  },

  reportFreshness() {
    if (!this.report.generated_at)
      return this.t("ui.landing.no_snapshot", "No repository snapshot yet");
    const reference = this.report.comparison;
    const local = this.t("ui.landing.local_scan", "Local scan · {{.Time}}", {
      Time: this.formatTime(this.report.generated_at),
    });
    const stale = this.report.summary?.remote_stale_repositories || 0;
    const staleText =
      stale > 0
        ? this.t(
            "ui.landing.stale_remotes",
            " · {{.Count}} stale remote repos",
            {
              Count: stale,
            },
          )
        : "";
    if (this.report.remote_enabled) {
      return `${local}${this.t("ui.landing.remote_checks", " · remote checks · {{.Time}}", { Time: this.formatTime(this.report.remote_generated_at) })}${staleText}`;
    }
    if (this.report.remote_generated_at) {
      return `${local}${this.t("ui.landing.remote_refreshed", " · remote last refreshed · {{.Time}}", { Time: this.formatTime(this.report.remote_generated_at) })}${staleText}`;
    }
    if (!reference?.available)
      return `${local}${this.t("ui.landing.remote_unavailable", " · remote results unavailable")}`;
    return `${local}${this.t("ui.landing.ruby_reference", " · Ruby reference · {{.Time}}", { Time: this.formatTime(reference.generated_at) })}`;
  },
});

if (typeof document !== "undefined") {
  document.addEventListener("alpine:init", () => {
    Alpine.data("landingDashboard", window.landingDashboard);
  });
}
