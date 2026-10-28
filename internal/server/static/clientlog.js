// Relays uncaught errors and unhandled rejections to POST /api/client-log so
// browser-side failures land in the server log (stderr / configured
// log_file) with their stack — a JS error never reaches the Go process
// otherwise. Fire-and-forget; reporting must never break the page.
(() => {
  const report = (payload) => {
    try {
      fetch("/api/client-log", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      }).catch(() => {
        /* best effort */
      });
    } catch {
      /* best effort */
    }
  };
  window.addEventListener("error", (event) => {
    report({
      message: String(event.message || "unknown error"),
      source: String(event.filename || ""),
      line: event.lineno || 0,
      stack: String(event.error?.stack || "").slice(0, 4000),
    });
  });
  window.addEventListener("unhandledrejection", (event) => {
    const reason = event.reason;
    report({
      message: `unhandled rejection: ${reason?.message || reason}`,
      stack: String(reason?.stack || "").slice(0, 4000),
    });
  });
})();
