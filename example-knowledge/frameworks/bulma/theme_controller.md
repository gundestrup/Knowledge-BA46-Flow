---
title: Theme controller
tags: [bulma, javascript, theme]
status: active
related: [frameworks/bulma/README.md, frameworks/bulma/theme_setup.md]
---

# Theme controller

A reusable JavaScript pattern for switching between light and dark theme.

```javascript
export function setTheme(name) {
  document.documentElement.dataset.theme = name;
}
```

See also [Bulma README](README.md).
