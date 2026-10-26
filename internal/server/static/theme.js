// Theme toggle: cycles auto → light → dark on [data-theme-toggle] clicks and
// persists the choice in localStorage. It drives Bulma's built-in theming —
// an explicit `data-theme` on <html> forces a palette, while "auto" removes
// the attribute so Bulma's prefers-color-scheme rules follow the OS.
// Loaded without `defer` in <head> so a stored choice applies before first
// paint (no flash of the wrong theme).
(() => {
  const STORAGE_KEY = "kbm-theme";
  const ORDER = ["auto", "light", "dark"];
  const ICONS = {
    auto: "icon-monitor",
    light: "icon-sun",
    dark: "icon-moon",
  };

  const stored = () => {
    try {
      const value = localStorage.getItem(STORAGE_KEY);
      return ORDER.includes(value) ? value : "auto";
    } catch {
      return "auto";
    }
  };

  const apply = (theme) => {
    if (theme === "auto") {
      document.documentElement.removeAttribute("data-theme");
    } else {
      document.documentElement.setAttribute("data-theme", theme);
    }
    document.querySelectorAll("[data-theme-icon]").forEach((icon) => {
      icon.classList.remove("icon-monitor", "icon-sun", "icon-moon");
      icon.classList.add(ICONS[theme]);
    });
  };

  document.addEventListener("click", (event) => {
    if (event.target.closest("[data-theme-toggle]")) {
      const next = ORDER[(ORDER.indexOf(stored()) + 1) % ORDER.length];
      try {
        localStorage.setItem(STORAGE_KEY, next);
      } catch {
        /* best effort */
      }
      apply(next);
    }
  });

  apply(stored());
  document.addEventListener("DOMContentLoaded", () => apply(stored()));
})();
