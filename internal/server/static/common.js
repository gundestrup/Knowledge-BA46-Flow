// Shared helpers for the three workspace pages (index, knowledge, report).
// Each Alpine component spreads `...window.kbm` into its data object, so
// `this` still resolves to the component and its own state — the fields
// below are per-component copies, not cross-page globals. Plain script —
// no modules — so `script-src 'self'` and load order stay trivial. Load
// before the page script.

// Chart theming: scriptable colors re-read the --kbm-* CSS vars on every
// draw, so charts follow a data-theme flip without a page reload. Guarded
// for the Node-VM test harness where no window exists.
const chartCssVar = (name, fallback) => () => {
  if (typeof window === "undefined") {
    return fallback;
  }
  const value = window
    .getComputedStyle(document.documentElement)
    .getPropertyValue(name)
    .trim();
  return value || fallback;
};
const chartInk = chartCssVar("--kbm-ink", "#68756e");
const chartLine = chartCssVar("--kbm-line", "#d9d4c6");

window.kbm = {
  // Shared UI state — every page runs the same navbar/locale/sidebar
  // machinery, so the initial values live here once.
  messages: {},
  locale:
    typeof localStorage !== "undefined"
      ? localStorage.getItem("kbm.locale") || ""
      : "",
  locales: ["en"],
  localeOpen: false,
  navOpen: false,
  resolvedLocale: "en",

  async loadI18n() {
    try {
      const query = this.locale
        ? `?lang=${encodeURIComponent(this.locale)}`
        : "";
      const data = await this.requestJSON(`/api/i18n${query}`);
      this.messages = data.messages || {};
      this.resolvedLocale = data.locale || "en";
      // Keep <html lang> honest for assistive tech — the pages ship lang="en"
      // but the negotiated locale may differ.
      if (typeof document !== "undefined") {
        document.documentElement.lang = this.resolvedLocale;
      }
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
    const code =
      { en: "gb", da: "dk" }[tag] || (tag.split("-")[1] || tag).toLowerCase();
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

  // postJSON wraps the repeated POST+JSON call shape — every mutating
  // endpoint takes a JSON body, so call sites stay one line.
  postJSON(url, body) {
    return this.requestJSON(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
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

  formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
  },

  // lineChartOptions is the shared Chart.js option block — both pages draw
  // the same themed line chart (index-mode tooltips, bottom legend, theme-
  // aware tick/grid colors via the chartCssVar thunks above).
  lineChartOptions() {
    return {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: "index", intersect: false },
      scales: {
        x: { ticks: { color: chartInk }, grid: { color: chartLine } },
        y: {
          beginAtZero: true,
          ticks: { precision: 0, color: chartInk },
          grid: { color: chartLine },
        },
      },
      plugins: {
        legend: { position: "bottom", labels: { color: chartInk } },
      },
    };
  },

  // lineDataset builds a themed line-chart dataset; per-page extras merge in
  // (dashboard passes seriesKey so updates map back to series fields, report
  // passes spanGaps/tension overrides).
  lineDataset(label, data, color, extra = {}) {
    return {
      label,
      data,
      borderColor: color,
      backgroundColor: color,
      tension: 0.2,
      ...extra,
    };
  },

  jobTagClass(status) {
    if (status === "done") return "is-success is-light";
    if (status === "failed") return "is-danger is-light";
    return "is-warning is-light";
  },

  // Shared dialog state — the Bulma modal on each page reads these.
  dialog: null,
  dialogInput: "",
  dialogReturnFocus: null,

  // confirmDialog/promptDialog replace window.confirm/window.prompt: they
  // open the shared modal and resolve a Promise — confirm → bool,
  // prompt → string, or null when cancelled. Callers await, so the flow in
  // decide()/applyAccepted()/stopServer() reads like the native dialogs but
  // never blocks the renderer (which hung headless e2e runs).
  confirmDialog(message, title) {
    return this.openDialog("confirm", message, title);
  },

  promptDialog(message, title) {
    return this.openDialog("prompt", message, title);
  },

  // openDialog remembers the triggering element so resolveDialog can return
  // focus to it (modal a11y), then moves focus into the dialog once Alpine
  // has rendered it. document/$nextTick/$refs are absent in the Node VM
  // tests — every access is guarded.
  openDialog(kind, message, title) {
    return new Promise((resolve) => {
      this.dialogReturnFocus =
        typeof document !== "undefined" ? document.activeElement : null;
      this.dialogInput = "";
      this.dialog = {
        kind,
        message,
        title: title || this.t("ui.common.confirm", "Confirm"),
        resolve,
      };
      if (typeof this.$nextTick === "function") {
        this.$nextTick(() => this.focusDialog());
      }
    });
  },

  // focusDialog puts keyboard users inside the dialog: the text field for
  // prompts, the confirm button otherwise (Tab reaches Cancel/backdrop).
  focusDialog() {
    const target = this.dialogIsPrompt()
      ? this.$refs?.dialogInputEl
      : this.$refs?.dialogConfirmEl;
    if (target && typeof target.focus === "function") target.focus();
  },

  // trapDialogFocus keeps Tab inside the modal card — Shift+Tab on the
  // first focusable wraps to the last, Tab on the last wraps to the first.
  trapDialogFocus(event) {
    if (!this.dialog || typeof document === "undefined") return;
    const focusables = [
      ...event.currentTarget.querySelectorAll("button, input"),
    ].filter((el) => el.offsetParent !== null);
    if (!focusables.length) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  },

  // dialogIsPrompt/dialogConfirmLabel keep the dialog kind's vocabulary out
  // of template expressions — template_globals_test word-scans Alpine
  // attributes for forbidden globals and trips on the string literals.
  dialogIsPrompt() {
    return this.dialog?.kind === "prompt";
  },

  // Optional chaining lives in methods, not templates — Alpine's CSP
  // evaluator can't parse "?." in attribute expressions.
  dialogTitle() {
    return this.dialog?.title ?? "";
  },

  dialogMessage() {
    return this.dialog?.message ?? "";
  },

  dialogConfirmLabel() {
    return this.dialogIsPrompt()
      ? this.t("ui.common.save", "Save")
      : this.t("ui.common.confirm", "Confirm");
  },

  // resolveDialog settles the open dialog — the backdrop, close and cancel
  // buttons pass false; the confirm button (and Enter in a prompt) passes
  // true. Focus returns to the element that opened the dialog.
  resolveDialog(ok) {
    const pending = this.dialog;
    this.dialog = null;
    const returnTo = this.dialogReturnFocus;
    this.dialogReturnFocus = null;
    if (!pending) return;
    pending.resolve(
      pending.kind === "prompt" ? (ok ? this.dialogInput : null) : ok,
    );
    if (returnTo && typeof returnTo.focus === "function") returnTo.focus();
  },
};
