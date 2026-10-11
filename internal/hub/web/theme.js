// theme.js — applies the portal user's look (theme + accent) before the
// first paint, from the copy app.js keeps in localStorage (the hub's
// /api/prefs is the source of truth; app.js refreshes it on load).
"use strict";
(function () {
  // mix: blend #rrggbb a toward b by t (0..1).
  const rgb = (hx) => [1, 3, 5].map((i) => parseInt(hx.slice(i, i + 2), 16));
  const hex = (c) => "#" + c.map((v) => Math.round(Math.max(0, Math.min(255, v))).toString(16).padStart(2, "0")).join("");
  const mix = (a, b, t) => { const x = rgb(a), y = rgb(b); return hex(x.map((v, i) => v + (y[i] - v) * t)); };
  // ink: text for a fill of colour c — white unless black reads clearly
  // better (WCAG contrast, biased to white, which suits brand colours).
  const lum = (c) => { const [r, g, b] = rgb(c).map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
  const ink = (c) => ((lum(c) + 0.05) / 0.05 > 1.05 / (lum(c) + 0.05) * 1.6 ? "#111214" : "#ffffff");
  window.applyLook = function (p) {
    const root = document.documentElement;
    p = p || {};
    if (p.theme === "light" || p.theme === "dark") root.dataset.theme = p.theme; else delete root.dataset.theme;
    if (/^#[0-9a-f]{6}$/i.test(p.accent || "")) {
      const a = p.accent.toLowerCase(), d = mix(a, "#ffffff", 0.22);
      root.style.setProperty("--accent-l", a);
      root.style.setProperty("--accent-l-text", ink(a));
      root.style.setProperty("--accent-d", d);
      root.style.setProperty("--accent-d-text", ink(d));
    } else {
      for (const k of ["--accent-l", "--accent-l-text", "--accent-d", "--accent-d-text"]) root.style.removeProperty(k);
    }
    const dark = p.theme === "dark" || (p.theme !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
    for (const m of document.querySelectorAll('meta[name="theme-color"]')) m.content = dark ? "#0a0b0d" : "#f2f3f5";
  };
  try { window.applyLook(JSON.parse(localStorage.getItem("ccdash.look") || "{}")); } catch { window.applyLook({}); }
})();
