window.landingDashboard = () => ({
  overview: null,
  loading: true,
  error: "",
  messages: {},
  locale:
    typeof localStorage !== "undefined"
      ? localStorage.getItem("kbm.locale") || ""
      : "",
  locales: ["en"],
  localeOpen: false,
  resolvedLocale: "en",

  async init() {
    await Promise.all([this.loadI18n(), this.refresh()]);
  },

  async loadI18n() {
    try {
      const query = this.locale
        ? `?lang=${encodeURIComponent(this.locale)}`
        : "";
      const data = await this.requestJSON(`/api/i18n${query}`);
      this.messages = data.messages || {};
      this.resolvedLocale = data.locale || "en";
      if (data.locales?.length) this.locales = data.locales;
    } catch {
      this.messages = {};
    }
  },

  // setLocale persists the UI locale choice across pages; "" negotiates the
  // browser's Accept-Language again.
  async setLocale(value) {
    this.localeOpen = false;
    this.locale = value;
    try {
      if (value) localStorage.setItem("kbm.locale", value);
      else localStorage.removeItem("kbm.locale");
    } catch {
      /* storage may be unavailable; choice just won't persist */
    }
    await this.loadI18n();
  },

  localeLabel(tag) {
    return { en: "English", da: "Dansk" }[tag] || tag;
  },

  // The flag shown on the picker: the explicit choice, else the negotiated
  // one — "" (automatic) resolves to whatever /api/i18n last served.
  displayLocale() {
    return this.locale || this.resolvedLocale;
  },

  // localeFlag maps a locale tag to a vendored flag-icons country code:
  // "en" shows the Union Jack; other tags use their region suffix or the
  // tag itself ("da-DK" -> "dk", "da" -> "dk").
  localeFlag(tag) {
    const code = { en: "gb" }[tag] || (tag.split("-")[1] || tag).toLowerCase();
    return `/static/vendor/flags/${code}.svg`;
  },

  // t resolves a catalog id from /api/i18n — the English fallback keeps the
  // page readable before the fetch lands and if the endpoint ever fails.
  t(id, fallback, params) {
    let msg = this.messages[id];
    if (msg != null && typeof msg === "object") {
      msg = msg[params?.Count === 1 ? "one" : "other"] ?? msg.other;
    }
    const out = typeof msg === "string" ? msg : fallback;
    if (out == null) return id;
    return out.replace(
      /\{\{\.(\w+)\}\}/g,
      (_, name) => params?.[name] ?? `{{.${name}}}`,
    );
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
      throw new Error(
        body.error ||
          this.t(
            "ui.common.request_failed",
            "Request failed (HTTP {{.Status}})",
            {
              Status: response.status,
            },
          ),
      );
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
