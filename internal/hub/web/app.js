// ccdash hub portal — vanilla JS SPA. Routes (hash):
//   #/                      device list
//   #/d/<dev>               one device: approvals, sessions, new session
//   #/d/<dev>/s/<session>   a session: live terminal if hosted, else transcript + resume
//   #/d/<dev>/p/<ptyKey>    a fresh spawn's terminal (before discovery names it)
"use strict";

const $ = (sel, el = document) => el.querySelector(sel);
const view = $("#view");
const enc = new TextEncoder();
let cleanup = []; // run on every route change (timers, sockets, terminals)
let me = null;

// ---------- helpers ----------

function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "html") el.innerHTML = v; // only ever used with constant strings
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

function toast(msg, ms = 2500) {
  const t = $("#toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => (t.hidden = true), ms);
}

function rel(ts) {
  if (!ts) return "";
  const d = (Date.now() - new Date(ts).getTime()) / 1000;
  if (d < 60) return "今";
  if (d < 3600) return `${Math.floor(d / 60)}分前`;
  if (d < 86400) return `${Math.floor(d / 3600)}時間前`;
  return `${Math.floor(d / 86400)}日前`;
}

function shortPath(p, home) {
  if (!p) return "";
  if (home && (p === home || p.startsWith(home + "/"))) return "~" + p.slice(home.length);
  return p;
}

function base(p) {
  return (p || "").replace(/\/+$/, "").split("/").pop() || p;
}

class HTTPError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

let loggingIn = false;

async function api(path, opts = {}) {
  const init = { credentials: "same-origin", ...opts, headers: { ...(opts.headers || {}) } };
  if (opts.json !== undefined) {
    init.body = JSON.stringify(opts.json);
    init.headers["Content-Type"] = "application/json";
    delete init.json;
  }
  const r = await fetch(path, init);
  if (r.status === 401) {
    // Several polls hit the expired session at once; start one login.
    if (!loggingIn) {
      loggingIn = true;
      try { sessionStorage.setItem("ccdash.next", location.hash); } catch {}
      location.href = "/auth/login?next=/";
    }
    throw new HTTPError(401, "login required");
  }
  if (!r.ok) throw new HTTPError(r.status, (await r.text()).trim() || r.statusText);
  if (r.status === 204) return null;
  const ct = r.headers.get("Content-Type") || "";
  return ct.includes("json") ? r.json() : r.text();
}

const dev = (id) => ({
  get: (p) => api(`/api/d/${id}${p}`),
  post: (p, json) => api(`/api/d/${id}${p}`, { method: "POST", json: json ?? {} }),
  del: (p) => api(`/api/d/${id}${p}`, { method: "DELETE" }),
});

function every(ms, fn, bucket = cleanup) {
  let stopped = false, timer;
  const loop = async () => {
    try { await fn(); } catch (e) { if (e.status !== 401) console.warn(e); }
    if (!stopped) timer = setTimeout(loop, ms);
  };
  loop();
  bucket.push(() => { stopped = true; clearTimeout(timer); });
}

// Polling re-renders replace DOM nodes, which drops a text selection the
// operator is making for copy. deferWhileSelecting holds one render of el
// while a selection lives inside it and runs fn once it's cleared.
const selHolds = new Map(); // element → deferred render
const selectingIn = (el) => {
  const sel = getSelection();
  return !!sel && !sel.isCollapsed && sel.rangeCount > 0 && el.contains(sel.getRangeAt(0).commonAncestorContainer);
};
document.addEventListener("selectionchange", () => {
  for (const [el, fn] of selHolds) {
    if (!el.isConnected) { selHolds.delete(el); continue; }
    if (!selectingIn(el)) { selHolds.delete(el); fn(); }
  }
});
function deferWhileSelecting(el, fn) {
  if (!selectingIn(el)) return false;
  selHolds.set(el, fn);
  return true;
}

// crumbs sets the header: breadcrumbs on a wide screen; on a phone a
// native-style bar — a back chevron to the parent crumb and the page title.
function crumbs(...parts) {
  const nav = $("#crumbs");
  nav.replaceChildren();
  // The local portal (ccdash web) has no device list to go back to.
  if (me?.local && parts[0]?.text === "端末") parts[0] = { ...parts[0], text: "ホーム" };
  parts.forEach((p, i) => {
    if (i) nav.append(" / ");
    nav.append(p.href ? h("a", { href: p.href }, p.text) : h("span", {}, p.text));
  });
  const last = parts[parts.length - 1], parent = parts[parts.length - 2];
  $("#navtitle").textContent = last?.text || "";
  const back = $("#navback");
  back.hidden = !parent?.href;
  if (parent?.href) back.href = parent.href;
}

// ---------- app shell (phone) ----------

// haptic: a short tick on Android for actions that change something.
function haptic(ms = 8) { try { navigator.vibrate?.(ms); } catch {} }

// paintShell marks the current tab in the bottom bar and hides the bar in
// detail views (a chat, a terminal, a diff), like a native app's push.
function paintShell(parts) {
  const detail = parts[0] === "d" && (parts[2] === "s" || parts[2] === "p");
  document.body.classList.toggle("detail", detail);
  const tab = parts[0] === "grid" ? "grid" : parts[0] === "tasks" ? "tasks" : parts[0] === "d" ? "sessions" : "home";
  for (const a of document.querySelectorAll("#tabbar a")) a.classList.toggle("on", a.dataset.tab === tab);
  let lastDev = "";
  try { lastDev = localStorage.getItem("ccdash.lastDevice") || ""; } catch {}
  if (parts[0] === "d" && parts[1]) { lastDev = parts[1]; try { localStorage.setItem("ccdash.lastDevice", lastDev); } catch {} }
  const sess = document.querySelector('#tabbar a[data-tab="sessions"]');
  if (sess) sess.href = lastDev ? `#/d/${lastDev}` : "#/";
}

// Page transitions: a push slides the new page in from the right, a pop
// slides back (View Transitions API; phones only, and not after a swipe
// back, which already animated).
let skipTransition = false;
const hashDepth = (hh) => hh.replace(/^#\/?/, "").split("/").filter(Boolean).length;
let lastDepth = hashDepth(location.hash || "#/");
function navigate() {
  const depth = hashDepth(location.hash || "#/");
  const dir = depth > lastDepth ? "push" : depth < lastDepth ? "pop" : "swap";
  lastDepth = depth;
  const animate = document.startViewTransition && !skipTransition && !wideQuery.matches &&
    !matchMedia("(prefers-reduced-motion: reduce)").matches && dir !== "swap";
  skipTransition = false;
  if (!animate) return route();
  document.documentElement.dataset.nav = dir;
  const t = document.startViewTransition(() => route());
  t.finished.finally(() => delete document.documentElement.dataset.nav);
}

// autoFocus focuses el only where a keyboard is at hand (a fine pointer
// that can hover: desktops). On phones, focusing an input on its own pops
// the on-screen keyboard over the page; let the user tap when they mean to
// type.
function autoFocus(el) {
  if (el && matchMedia("(hover: hover) and (pointer: fine)").matches) el.focus();
}

// Drafts: what was typed into a session's composer but not sent, kept per
// device + session (localStorage, so it also survives a reload) until it
// is sent. Switching sessions mid-sentence no longer loses it.
const draftKey = (id, ref) => `ccdash.draft.${id}.${ref}`;
function loadDraft(id, ref) { try { return localStorage.getItem(draftKey(id, ref)) || ""; } catch { return ""; } }
function saveDraft(id, ref, text) {
  try { text ? localStorage.setItem(draftKey(id, ref), text) : localStorage.removeItem(draftKey(id, ref)); } catch {}
}

function dialog(build) {
  const dlg = $("#dlg"), body = $("#dlg-body");
  body.replaceChildren();
  dlg.style.transform = "";
  // Dialogs build with `cond ? el : null`; DOM append would print "null".
  const append = Element.prototype.append.bind(body);
  body.append = (...kids) => append(...kids.flat().filter((k) => k != null && k !== false));
  build(body, () => dlg.close());
  dlg.showModal();
  return dlg;
}

// On a phone the dialog is a bottom sheet (app.css): tap outside or drag
// the grabber down to dismiss it.
function installSheet() {
  const dlg = $("#dlg"), grab = dlg.querySelector(".sheet-grab");
  dlg.addEventListener("click", (e) => { if (e.target === dlg) dlg.close(); });
  let y0 = null, dy = 0;
  grab.addEventListener("pointerdown", (e) => { y0 = e.clientY; dy = 0; grab.setPointerCapture(e.pointerId); dlg.style.transition = "none"; });
  grab.addEventListener("pointermove", (e) => {
    if (y0 == null) return;
    dy = Math.max(0, e.clientY - y0);
    dlg.style.transform = `translateY(${dy}px)`;
  });
  const end = () => {
    if (y0 == null) return;
    y0 = null;
    dlg.style.transition = "";
    if (dy > 90) dlg.close();
    dlg.style.transform = "";
  };
  grab.addEventListener("pointerup", end);
  grab.addEventListener("pointercancel", end);
}

async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast("コピーしました"); }
  catch { toast("コピーできませんでした（手動で選択してください）"); }
}

// ---------- icons ----------
// Stroke icons drawn after Lucide (ISC). docs/design.md: icons come from
// here, never emoji. icon(name) is an inline SVG sized 1em in currentColor.
const ICONS = {
  bell: '<path d="M10.268 21a2 2 0 0 0 3.464 0"/><path d="M3.262 15.326A1 1 0 0 0 4 17h16a1 1 0 0 0 .74-1.673C19.41 13.956 18 12.499 18 8A6 6 0 0 0 6 8c0 4.499-1.411 5.956-2.738 7.326"/>',
  "bell-off": '<path d="M10.268 21a2 2 0 0 0 3.464 0"/><path d="M17 17H4a1 1 0 0 1-.74-1.673C4.59 13.956 6 12.499 6 8a6 6 0 0 1 .258-1.742"/><path d="m2 2 20 20"/><path d="M8.668 3.01A6 6 0 0 1 18 8c0 2.687.77 4.653 1.707 6.05"/>',
  sparkles: '<path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z"/>',
  clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
  pin: '<path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z"/>',
  grid: '<rect width="7" height="7" x="3" y="3" rx="1"/><rect width="7" height="7" x="14" y="3" rx="1"/><rect width="7" height="7" x="14" y="14" rx="1"/><rect width="7" height="7" x="3" y="14" rx="1"/>',
  folder: '<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>',
  mic: '<path d="M12 19v3"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><rect x="9" y="2" width="6" height="13" rx="3"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  image: '<rect width="18" height="18" x="3" y="3" rx="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21"/>',
  more: '<circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/><circle cx="5" cy="12" r="1"/>',
  restart: '<path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/>',
  open: '<path d="M7 7h10v10"/><path d="M7 17 17 7"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  stop: '<rect width="12" height="12" x="6" y="6" rx="1.5" fill="currentColor"/>',
  play: '<path d="M6 4.5v15a1 1 0 0 0 1.5.86l12.5-7.5a1 1 0 0 0 0-1.72L7.5 3.64A1 1 0 0 0 6 4.5z" fill="currentColor"/>',
  edit: '<path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z"/>',
  shuffle: '<path d="m18 14 4 4-4 4"/><path d="m18 2 4 4-4 4"/><path d="M2 18h1.973a4 4 0 0 0 3.3-1.7l5.454-7.6a4 4 0 0 1 3.3-1.7H22"/><path d="M2 6h1.972a4 4 0 0 1 3.6 2.2"/><path d="M22 18h-6.041a4 4 0 0 1-3.3-1.8l-.359-.45"/>',
  up: '<path d="m18 15-6-6-6 6"/>',
  down: '<path d="m6 9 6 6 6-6"/>',
  back: '<path d="m12 19-7-7 7-7"/><path d="M19 12H5"/>',
  terminal: '<path d="m4 17 6-6-6-6"/><path d="M12 19h8"/>',
  home: '<path d="M15 21v-8a1 1 0 0 0-1-1h-4a1 1 0 0 0-1 1v8"/><path d="M3 10a2 2 0 0 1 .709-1.528l7-5.999a2 2 0 0 1 2.582 0l7 5.999A2 2 0 0 1 21 10v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
  list: '<path d="M3 12h.01"/><path d="M3 18h.01"/><path d="M3 6h.01"/><path d="M8 12h13"/><path d="M8 18h13"/><path d="M8 6h13"/>',
  chevron: '<path d="m15 18-6-6 6-6"/>',
  settings: '<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>',
  file: '<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/>',
  search: '<path d="m21 21-4.34-4.34"/><circle cx="11" cy="11" r="8"/>',
  tasks: '<path d="M13 5h8"/><path d="M13 12h8"/><path d="M13 19h8"/><path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/>',
  sidebar: '<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M15 3v18"/>',
  expand: '<path d="M15 3h6v6"/><path d="M9 21H3v-6"/><path d="M21 3l-7 7"/><path d="M3 21l7-7"/>',
  eye: '<path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/>',
  download: '<path d="M12 15V3"/><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/>',
};
function icon(name) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  el.setAttribute("viewBox", "0 0 24 24");
  el.setAttribute("class", "ic");
  el.setAttribute("aria-hidden", "true");
  el.innerHTML = ICONS[name] || "";
  return el;
}

const deviceCache = new Map(); // id → device row, for crumbs

// ---------- router ----------

// mainMount is where a page renders: the whole view, or — on a wide screen
// inside a device — the right pane next to the session list (TUI-style).
let mainMount = view;
let split = null; // { dev, left, right, cleanup }
const wideQuery = matchMedia("(min-width: 1024px)");

function closeSplit() {
  if (!split) return;
  split.cleanup.forEach((f) => { try { f(); } catch {} });
  split = null;
}

function openSplit(id) {
  closeSplit();
  view.replaceChildren();
  const left = h("div", { class: "split-list" });
  const right = h("div", { class: "split-main" });
  const handle = h("div", { class: "split-handle", title: "ドラッグで幅を変更（ダブルクリックで初期値、← → キーでも調整）", tabindex: "0", role: "separator", "aria-orientation": "vertical" });
  const box = h("div", { class: "split" }, left, handle, right);
  // Width of the list in percent: what the operator dragged to, else the
  // device's own TUI preference (Session list size %), else 34.
  let w = 34, saved = null, fallback = 34;
  try { saved = +localStorage.getItem("ccdash.listWidth") || null; } catch {}
  const setW = (v) => { w = Math.min(75, Math.max(15, v)); box.style.setProperty("--list-w", w + "%"); handle.setAttribute("aria-valuenow", String(Math.round(w))); };
  const save = () => { try { localStorage.setItem("ccdash.listWidth", String(Math.round(w))); } catch {} };
  setW(saved || fallback);
  if (!saved) {
    dev(id).get("/hub/settings").then((rows) => {
      const pct = rows?.find?.((r) => r.key === "pane_list_pct")?.value;
      if (typeof pct === "number" && pct > 0) { fallback = pct; if (!saved) setW(pct); }
    }).catch(() => {});
  }
  handle.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    box.classList.add("dragging");
    const rect = box.getBoundingClientRect();
    const move = (ev) => setW(((ev.clientX - rect.left) / rect.width) * 100);
    const up = () => {
      box.classList.remove("dragging");
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", up);
      handle.removeEventListener("pointercancel", up);
      saved = w;
      save();
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", up);
    handle.addEventListener("pointercancel", up);
  });
  handle.addEventListener("dblclick", () => {
    saved = null;
    try { localStorage.removeItem("ccdash.listWidth"); } catch {}
    setW(fallback);
  });
  handle.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    setW(w + (e.key === "ArrowLeft" ? -2 : 2));
    saved = w;
    save();
  });
  view.append(box);
  split = { dev: id, left, right, cleanup: [] };
  devicePage(id, { mount: left, bucket: split.cleanup, compact: true });
}

let beforeGrid = "#/";

function route() {
  cleanup.forEach((f) => { try { f(); } catch {} });
  cleanup = [];
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  paintShell(parts);
  // The header's グリッド is a toggle: on the grid it leads back to where
  // you were before opening it.
  const gl = $("#gridlink");
  if (parts[0] === "grid") {
    gl.classList.add("on");
    gl.href = beforeGrid;
    gl.title = "グリッド表示をやめる";
  } else {
    beforeGrid = location.hash || "#/";
    gl.classList.remove("on");
    gl.href = "#/grid";
    gl.title = "セッションをまとめて表示（グリッド）";
  }
  const inDevice = parts[0] === "d" && parts[1];
  paintTaskScope();
  if (inDevice && wideQuery.matches) {
    if (split?.dev !== parts[1]) openSplit(parts[1]);
    paintListFold();
    split.right.replaceChildren();
    mainMount = split.right;
    split.left.querySelectorAll(".item.selected").forEach((e) => e.classList.remove("selected"));
    if (parts[2] === "s" && parts[3]) {
      split.left.querySelector(`.item[data-sid="${CSS.escape(parts[3])}"]`)?.classList.add("selected");
      if (parts[4] === "diff") return diffPage(parts[1], parts[3]);
      return sessionPage(parts[1], parts[3], parts[4]);
    }
    if (parts[2] === "p" && parts[3]) return spawnPage(parts[1], parts[3], parts[4]);
    crumbs({ text: "端末", href: "#/" }, { text: deviceCache.get(parts[1])?.name || "…" });
    deviceName(parts[1]).then((n) => { if (location.hash.split("/")[2] === parts[1] && !parts[2]) crumbs({ text: "端末", href: "#/" }, { text: n }); });
    split.right.append(h("div", { class: "empty split-empty" }, "左の一覧からセッションを選んでください"));
    return;
  }
  closeSplit();
  paintListFold();
  view.replaceChildren();
  mainMount = view;
  if (parts[0] === "grid") return gridPage();
  if (parts[0] === "tasks") return tasksPage(parts[1]);
  if (inDevice) {
    if (parts[2] === "s" && parts[3] && parts[4] === "diff") return diffPage(parts[1], parts[3]);
    if (parts[2] === "s" && parts[3]) return sessionPage(parts[1], parts[3], parts[4]);
    if (parts[2] === "p" && parts[3]) return spawnPage(parts[1], parts[3], parts[4]);
    return devicePage(parts[1]);
  }
  return devicesPage();
}

// ---------- swipe back ----------

// parentHash is where "back" leads from the current view: diff → its chat,
// a session or spawn → the device, a device → the board, the grid → where
// it was opened from. "" on the board.
function parentHash() {
  const parts = location.hash.replace(/^#\/?/, "").split("/").filter(Boolean);
  if (parts[0] === "grid") return beforeGrid || "#/";
  if (parts[0] !== "d" || !parts[1]) return "";
  const dev = `#/d/${parts[1]}`;
  if (parts[2] === "s" && parts[3] && parts[4] === "diff") return `${dev}/s/${parts[3]}`;
  if (parts[2]) return dev;
  return "#/";
}

// hashTrail mirrors this tab's in-app history so going back can use
// history.back() (keeping the browser's own back / forward consistent)
// when the previous entry is the parent.
const hashTrail = [location.hash || "#/"];
function noteHash() {
  const cur = location.hash || "#/";
  if (hashTrail[hashTrail.length - 2] === cur) hashTrail.pop();
  else if (hashTrail[hashTrail.length - 1] !== cur) hashTrail.push(cur);
}
function goBack() {
  const parent = parentHash();
  if (!parent) return;
  if (hashTrail[hashTrail.length - 2] === parent) history.back();
  else location.replace(parent);
}

// Swipe right to go back, like a native app: the page follows the finger
// and lets go past a third of the width (or a quick flick). Touch screens
// in the single-column layout only. Swipes that start in something that
// scrolls sideways or takes input are left alone, and outside an installed
// app the left edge belongs to the browser's own back gesture.
function blocksSwipe(el) {
  for (let n = el; n && n !== document.body; n = n.parentElement) {
    if (n.matches?.("input, textarea, select, dialog, .xterm, .tabs, pre, .grid")) return true;
    const ox = getComputedStyle(n).overflowX;
    if ((ox === "auto" || ox === "scroll") && n.scrollWidth > n.clientWidth + 2) return true;
  }
  return false;
}
function installSwipeBack() {
  if (!matchMedia("(pointer: coarse)").matches) return;
  const standalone = matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
  let sx = 0, sy = 0, st = 0, dx = 0, state = "idle"; // idle | maybe | drag
  const reset = (animate) => {
    view.style.transition = animate ? "transform .2s ease-out" : "none";
    view.style.transform = "";
    view.classList.remove("swiping");
  };
  addEventListener("touchstart", (e) => {
    state = "idle";
    if (e.touches.length !== 1 || wideQuery.matches || $("#dlg").open || !parentHash()) return;
    const t = e.touches[0];
    if (!standalone && t.clientX < 24) return;
    if (blocksSwipe(e.target)) return;
    sx = t.clientX; sy = t.clientY; st = e.timeStamp; dx = 0; state = "maybe";
  }, { passive: true });
  addEventListener("touchmove", (e) => {
    if (state === "idle") return;
    const t = e.touches[0], mx = t.clientX - sx, my = t.clientY - sy;
    if (state === "maybe") {
      if (Math.abs(my) > 10 && Math.abs(my) >= Math.abs(mx)) { state = "idle"; return; }
      if (mx < 14 || mx < Math.abs(my) * 1.5) return;
      state = "drag";
      view.classList.add("swiping");
    }
    e.preventDefault(); // hold the page still vertically while dragging
    dx = Math.max(0, mx);
    view.style.transition = "none";
    view.style.transform = `translateX(${dx}px)`;
  }, { passive: false });
  const end = (e) => {
    if (state !== "drag") { state = "idle"; return; }
    state = "idle";
    const flick = dx > 40 && dx / Math.max(1, e.timeStamp - st) > 0.6;
    if (dx < innerWidth / 3 && !flick) return reset(true);
    view.style.transition = "transform .15s ease-out";
    view.style.transform = `translateX(${innerWidth}px)`;
    setTimeout(() => { reset(false); skipTransition = true; goBack(); }, 150);
  };
  addEventListener("touchend", end);
  addEventListener("touchcancel", () => { if (state === "drag") reset(true); state = "idle"; });
}

// ---------- devices ----------

// The board: every connected device's sessions that need the operator,
// are working, or finished unread — the first thing to look at.
const BOARD_COLS = [["needs_you", "要対応"], ["working", "作業中"], ["done", "未確認"]];

// What a session declared it hands back at the end of its turn (ccdash mcp
// set_status; model.WaitLabel in Go). answer / action also make it
// needs_you, with "回答待ち: <note>" as the reason.
const WAIT_LABEL = { answer: "回答待ち", action: "作業待ち", report: "報告", background: "自動で続行" };
const WAIT_LABEL_NEEDS = { answer: true, action: true };
function waitDeclared(s) {
  return !!s && !!WAIT_LABEL[s.wait] && s.status !== "active";
}
// waitChip: the declared status as a chip — filled for the two that wait on
// the operator (they ask for action), a quiet word for the rest.
function waitChip(s) {
  if (!waitDeclared(s)) return null;
  return h("span", { class: "chip wait " + s.wait, title: s.wait_note || "" }, WAIT_LABEL[s.wait]);
}

function boardCard(c) {
  const s = c.session;
  const declared = waitDeclared(s) && s.attention !== "needs_you" ? WAIT_LABEL[s.wait] + (s.wait_note ? ": " + s.wait_note : "") : "";
  const reason = s.attention === "needs_you" ? (s.attention_reason || "要対応") : declared || (s.attention === "done" ? "完了" : "作業中");
  return h("a", { class: "card " + (s.attention || "working"), href: `#/d/${c.device_id}/s/${encodeURIComponent(s.session_id)}` },
    h("div", { class: "row" }, h("span", { class: "chip" }, c.device_name), h("span", { class: "grow" }), h("span", { class: "muted small" }, rel(s.last_seen))),
    h("div", { class: titleClass("card-title", s) }, (s.num ? `#${s.num} ` : "") + sessionTitle(s)),
    h("div", { class: "card-reason" }, reason),
    s.attention ? null : beamEl());
}

function devicesPage() {
  crumbs({ text: "端末" });
  const list = h("div", { class: "list" }, h("div", { class: "empty" }, "読み込み中…"));
  const board = h("div", { class: "board" });
  const local = !!me?.local;
  const todo = h("div", { class: "task-body" });
  const todoHead = h("div", { class: "row todo-head" }, h("h2", { class: "grow" }, "やること"),
    h("a", { class: "btn small", href: "#/tasks", onclick: (e) => { if (wideQuery.matches) { e.preventDefault(); setTaskPane(true); } } }, "すべて"));
  view.append(h("div", { class: "page" },
    h("section", { class: "todo" }, todoHead, todo),
    board,
    h("div", { class: "row" },
      h("h1", { class: "grow" }, local ? "このマシン" : "端末"),
      local ? null : h("button", { class: "btn primary", onclick: addDevice }, icon("plus"), "端末を追加")),
    local ? null : h("h2", {}, "登録済み"),
    list,
    local ? null : h("p", { class: "muted small" },
      "端末側で ", h("span", { class: "mono" }, "ccdash hub join"), " を実行すると、その端末の collector がこのハブへ接続します。端末にポートを開ける必要はありません。")));

  taskList(todo, { only: "", buckets: ["today", "tomorrow"], compact: true });
  // Empty columns say what that means (docs/design.md: empty states guide).
  const BOARD_EMPTY = { needs_you: "あなたを待っているセッションはありません", working: "作業中のセッションはありません", done: "未確認の完了はありません" };
  every(4000, async () => {
    const b = await api("/api/board");
    updateBadge(b);
    const cols = BOARD_COLS.map(([k, label]) => {
      const cards = b[k] || [];
      return h("div", { class: "board-col " + k },
        h("div", { class: "board-head" }, label, h("span", { class: "count" }, cards.length)),
        ...(cards.length ? cards.slice(0, 30).map(boardCard) : [h("div", { class: "muted small board-empty" }, BOARD_EMPTY[k] || "なし")]));
    });
    board.replaceChildren(...cols);
  });
  every(5000, async () => {
    const ds = await api("/api/devices");
    ds.forEach((d) => deviceCache.set(d.id, d));
    list.replaceChildren(...(ds.length ? ds.map(deviceRow) : [h("div", { class: "empty" }, local ? "collector に接続しています…" : "まだ端末がありません。「端末を追加」から登録してください。")]));
  });
}

function deviceRow(d) {
  const local = !!me?.local;
  const sub = d.online
    ? (local ? `collector に接続中${d.version ? " · ccdash " + d.version : ""}` : `${d.hostname || ""} · ccdash ${d.version || "?"} · ${rel(d.since)}から接続`)
    : local ? "collector に接続していません（再接続を待っています）" : `オフライン${d.last_seen ? " · 最終接続 " + rel(d.last_seen) : " · 未接続"}`;
  return h("div", { class: "item", onclick: () => d.online ? (location.hash = `#/d/${d.id}`) : toast("この端末はオフラインです") },
    h("span", { class: "dot" + (d.online ? " on" : "") }),
    h("div", { class: "grow" }, h("div", { class: "title" }, d.name), h("div", { class: "sub" }, sub)),
    local ? null : h("button", { class: "btn small", onclick: (e) => { e.stopPropagation(); deviceMenu(d); } }, "管理"));
}

function showToken(body, close, title, res) {
  body.append(
    h("h1", {}, title),
    h("p", {}, "登録する端末で次を実行し、トークンを聞かれたら貼り付けてください。トークンはこの画面でしか表示されません。"),
    h("div", { class: "secret" }, res.join),
    h("div", { class: "row" }, h("span", { class: "grow muted small" }, "トークン"), h("button", { type: "button", class: "btn small", onclick: () => copy(res.token) }, "コピー")),
    h("div", { class: "secret" }, res.token),
    h("p", { class: "muted small" }, "接続には端末側で collector が動いている必要があります（", h("span", { class: "mono" }, "ccdash server"), " か ", h("span", { class: "mono" }, "ccdash -k"), "）。"),
    h("div", { class: "actions" }, h("button", { class: "btn primary", onclick: close }, "閉じる")));
}

function addDevice() {
  dialog((body, close) => {
    const name = h("input", { placeholder: "例: desktop, macbook", autofocus: true, required: true, maxlength: 64 });
    const go = async (e) => {
      e.preventDefault();
      try {
        const res = await api("/api/devices", { method: "POST", json: { name: name.value.trim() } });
        body.replaceChildren();
        showToken(body, close, `「${res.device.name}」を追加しました`, res);
      } catch (err) { toast(err.message); }
    };
    body.append(h("h1", {}, "端末を追加"), h("label", {}, "名前", h("br"), name),
      h("div", { class: "actions" },
        h("button", { class: "btn", value: "cancel", formnovalidate: true }, "キャンセル"),
        h("button", { class: "btn primary", onclick: go }, "追加")));
  });
}

function deviceMenu(d) {
  dialog((body, close) => {
    const name = h("input", { value: d.name, maxlength: 64 });
    body.append(h("h1", {}, d.name),
      h("p", { class: "muted small" }, `ID ${d.id} · 登録 ${new Date(d.created_at).toLocaleString()}`),
      h("label", {}, "名前", h("br"), name),
      h("div", { class: "actions" },
        h("button", { type: "button", class: "btn danger", onclick: async () => {
          if (!confirm(`「${d.name}」を削除しますか？ 接続中のトンネルも切断されます。`)) return;
          try { await api(`/api/devices/${d.id}`, { method: "DELETE" }); close(); toast("削除しました"); route(); } catch (e) { toast(e.message); }
        } }, "削除"),
        h("button", { type: "button", class: "btn", onclick: async () => {
          if (!confirm("トークンを再発行しますか？ 古いトークンは即座に無効になります。")) return;
          try { const res = await api(`/api/devices/${d.id}/rotate`, { method: "POST" }); body.replaceChildren(); showToken(body, close, "トークンを再発行しました", res); } catch (e) { toast(e.message); }
        } }, "トークン再発行"),
        h("span", { class: "grow" }),
        h("button", { class: "btn", value: "cancel" }, "閉じる"),
        h("button", { type: "button", class: "btn primary", onclick: async () => {
          try { await api(`/api/devices/${d.id}/rename`, { method: "POST", json: { name: name.value.trim() } }); close(); route(); } catch (e) { toast(e.message); }
        } }, "保存")));
  });
}

async function deviceName(id) {
  if (!deviceCache.has(id)) {
    try { (await api("/api/devices")).forEach((d) => deviceCache.set(d.id, d)); } catch {}
  }
  return deviceCache.get(id)?.name || id;
}

// ---------- one device ----------

const infoCache = new Map();
async function deviceInfo(id) {
  if (!infoCache.has(id)) infoCache.set(id, await dev(id).get("/hub/info"));
  return infoCache.get(id);
}

function groupOf(s) {
  return s.user_group || s.repo || base(s.cwd) || "(none)";
}

// sessionColor: the session's automatic accent color — the same FNV-1a
// hash and palette as model.Session.Color, so the TUI shows the same color.
// sessionColorOf prefers the operator's pick (s.color, set from ⋯ or the
// TUI's c / C), then the session's #N, then this hash.
const SESSION_PALETTE = [
  "#ef4444", "#f97316", "#f59e0b", "#facc15", "#84cc16", "#22c55e",
  "#10b981", "#14b8a6", "#06b6d4", "#0ea5e9", "#3b82f6", "#6366f1",
  "#8b5cf6", "#a855f7", "#d946ef", "#ec4899", "#f43f5e", "#b45309",
  "#94a3b8", "#fca5a5", "#fde68a", "#86efac", "#93c5fd", "#c4b5fd",
];
function sessionColor(sid) {
  if (!sid) return "";
  let x = 0x811c9dc5;
  for (const b of new TextEncoder().encode(sid)) x = Math.imul(x ^ b, 16777619) >>> 0;
  return SESSION_PALETTE[x % SESSION_PALETTE.length];
}
// By #N first: 7 is coprime with the palette size, so consecutive sessions
// get different, far-apart colors (model.Session.Color does the same).
const sessionColorOf = (s) => s?.color || (s?.num > 0 ? SESSION_PALETTE[(s.num * 7) % SESSION_PALETTE.length] : sessionColor(s?.session_id));

// projectColorOf mirrors model.ProjectColorOf: the project's stored color
// (projects table, on each row as project_color), else an FNV-1a hash of
// the name into the palette.
function projectColorOf(name, stored) {
  if (stored) return stored;
  if (!name) return "";
  let x = 0x811c9dc5;
  for (const b of new TextEncoder().encode(name)) x = Math.imul(x ^ b, 16777619) >>> 0;
  return SESSION_PALETTE[x % SESSION_PALETTE.length];
}
// inkOn mirrors model.InkOn: black or white text on a "#rrggbb" band.
function inkOn(bg) {
  const v = parseInt((bg || "").slice(1), 16);
  if (!(bg?.length === 7) || Number.isNaN(v)) return "#ffffff";
  return 0.299 * (v >> 16 & 255) + 0.587 * (v >> 8 & 255) + 0.114 * (v & 255) > 150 ? "#111111" : "#ffffff";
}
// projectsFirst mirrors the TUI: project members go to the top, one block
// per project, in a stable order that activity never changes — projects by
// their operator-set order (project_order; ▲▼ on the band); inside a block
// pins first (latest pin first), then newest-started; the rest keep their
// order.
// pinOrder: among two pinned sessions, the more recently pinned first.
function pinOrder(a, b) {
  if (!a.favorite || !b.favorite) return 0;
  const t = (v) => (v ? new Date(v).getTime() : 0);
  return t(b.pinned_at) - t(a.pinned_at);
}
// laterFirst mirrors the TUI: "watch later" sessions in their own block at
// the very top, the most recently marked first.
function laterFirst(rows) {
  const t = (v) => (v ? new Date(v).getTime() : 0);
  const later = rows.filter((s) => s.later).sort((a, b) => t(b.later_at) - t(a.later_at));
  return later.length ? [...later, ...rows.filter((s) => !s.later)] : rows;
}
function projectsFirst(rows) {
  const t = (v) => (v ? new Date(v).getTime() : 0);
  if (!rows.some((s) => s.project)) return rows;
  const inP = rows.filter((s) => s.project).sort((a, b) =>
    a.project === b.project ? (!!b.favorite - !!a.favorite) || pinOrder(a, b) || (t(b.first_seen) - t(a.first_seen)) || ((b.num || 0) - (a.num || 0))
      : ((a.project_order || 0) - (b.project_order || 0)) || a.project.localeCompare(b.project));
  return [...inP, ...rows.filter((s) => !s.project)];
}
// projectPicker: choose a project for one or more sessions. Existing
// projects (GET /api/projects — also the ones whose sessions are all
// archived) are chips in their colors; typing filters them, and a name
// that isn't listed is created on 作成. pick(name) receives "" for 外す.
async function projectPicker(id, { current = "", count = 1 } = {}, pick) {
  let projects = [];
  try { projects = (await dev(id).get("/api/projects")) || []; } catch (e) { toast(e.message); }
  dialog((body, close) => {
    const input = h("input", { class: "grow", placeholder: "プロジェクトを検索、または新しい名前を入力" });
    const chips = h("div", { class: "proj-chips" });
    const create = h("button", { type: "button", class: "btn primary" }, "作成");
    const choose = (name) => { close(); pick(name); };
    const paint = () => {
      const q = input.value.trim();
      const ql = q.toLowerCase();
      const hits = projects.filter((p) => p.name !== current && (!ql || p.name.toLowerCase().includes(ql)));
      chips.replaceChildren(...(hits.length ? hits.map((p) => {
        const c = projectColorOf(p.name, p.color);
        return h("button", { type: "button", class: "proj-chip", style: `--proj:${c};--ink:${inkOn(c)}`, onclick: () => choose(p.name) },
          p.name, h("span", { class: "n" }, String(p.sessions)));
      }) : [h("span", { class: "muted small" }, projects.length ? "一致するプロジェクトはありません" : "まだプロジェクトはありません — 名前を入力して作成")]));
      const exists = projects.some((p) => p.name === q);
      create.hidden = !q || exists;
      create.textContent = `「${q}」を作成`;
    };
    input.addEventListener("input", paint);
    input.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" || e.isComposing) return;
      e.preventDefault();
      const q = input.value.trim();
      if (q) choose(q);
    });
    create.addEventListener("click", () => { const q = input.value.trim(); if (q) choose(q); });
    body.append(
      h("h1", {}, count > 1 ? `${count} 件をプロジェクトへ` : "プロジェクトを選ぶ"),
      current ? h("p", { class: "muted small" }, `いまのプロジェクト: ${current}`) : null,
      chips,
      h("div", { class: "row" }, input, create),
      h("div", { class: "actions" },
        current || count > 1 ? h("button", { type: "button", class: "btn", onclick: () => choose("") }, "プロジェクトから外す") : null,
        h("button", { class: "btn" }, "キャンセル")));
    paint();
    setTimeout(() => autoFocus(input), 0);
  });
}

function projectHeader(name, color, members, onNew, onMove) {
  const running = members.filter((s) => s.status === "active" || s.status === "idle").length;
  const needs = members.filter((s) => s.attention === "needs_you").length;
  const fresh = members.filter((s) => s.unseen).length;
  const meta = [`${members.length} セッション`, running ? `${running} 稼働中` : null, needs ? `${needs} 要対応` : null, fresh ? `${fresh} NEW` : null].filter(Boolean).join(" · ");
  return h("div", { class: "bucket project", style: `--proj:${color};--ink:${inkOn(color)}` },
    h("span", { class: "proj-dot" }), h("b", {}, name), h("span", { class: "proj-meta" }, meta),
    onMove ? h("button", { type: "button", class: "proj-new", title: "プロジェクトを上へ", "aria-label": "プロジェクトを上へ", onclick: (e) => { e.stopPropagation(); onMove(-1); } }, icon("up")) : null,
    onMove ? h("button", { type: "button", class: "proj-new", title: "プロジェクトを下へ", "aria-label": "プロジェクトを下へ", onclick: (e) => { e.stopPropagation(); onMove(1); } }, icon("down")) : null,
    onNew ? h("button", { type: "button", class: "proj-new", title: "このプロジェクトで新規セッション", "aria-label": "このプロジェクトで新規セッション", onclick: (e) => { e.stopPropagation(); onNew(); } }, icon("plus")) : null);
}

function sessionTitle(s) {
  return s.custom_title || s.gen_title || s.title || "(無題)";
}

// titleClass adds "titling" (a spinner before the title, app.css) while
// claude -p is generating the session's title.
function titleClass(base, s) {
  return s?.title_status === "running" ? base + " titling" : base;
}
const TITLING_TIP = "タイトルを生成しています（claude -p）";

// Tabs mirror the TUI's strip: "" = 最新 (everything, newest first), then
// user-named groups and — unless the device turned auto_repo_tabs off —
// repo / cwd names, alphabetical. The choice is remembered per device.
function uniqueGroups(sessions, autoRepo) {
  const user = new Set(), auto = new Set();
  for (const s of sessions) {
    if (s.user_group) user.add(s.user_group);
    else if (autoRepo) {
      const g = s.repo || base(s.cwd);
      if (g) auto.add(g);
    }
  }
  for (const u of user) auto.delete(u);
  return ["", ...[...user, ...auto].sort()];
}

// dateBucket follows the TUI's date grouping (pinned sessions on top).
function dateBucket(s) {
  if (s.favorite) return "ピン留め";
  const t = new Date(s.last_seen);
  const now = new Date();
  const day = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const days = Math.round((day(now) - day(t)) / 86400000);
  if (days <= 0) return "今日";
  if (days === 1) return "昨日";
  if (days <= 7) return "今週";
  if (days <= 30) return "今月";
  return `${t.getFullYear()}年${t.getMonth() + 1}月`;
}

// revealInStrip scrolls a horizontal strip so el is visible, without
// touching any other scroll position (unlike scrollIntoView).
function revealInStrip(strip, el) {
  if (!strip || !el) return;
  const sr = strip.getBoundingClientRect(), er = el.getBoundingClientRect();
  if (er.left < sr.left) strip.scrollLeft += er.left - sr.left - 8;
  else if (er.right > sr.right) strip.scrollLeft += er.right - sr.right + 8;
}

const tabKey = (id) => `ccdash.tab.${id}`;
function loadTab(id) { try { return localStorage.getItem(tabKey(id)) || ""; } catch { return ""; } }
function saveTab(id, g) { try { localStorage.setItem(tabKey(id), g); } catch {} }

async function devicePage(id, opts = {}) {
  const mount = opts.mount || mainMount;
  const bucket = opts.bucket || cleanup;
  const name = await deviceName(id);
  if (!opts.compact) crumbs({ text: "端末", href: "#/" }, { text: name });
  const approvals = h("div");
  const spawns = h("div");
  const tabs = h("div", { class: "tabs" });
  tabs.addEventListener("wheel", (e) => {
    if (Math.abs(e.deltaY) > Math.abs(e.deltaX) && tabs.scrollWidth > tabs.clientWidth) {
      e.preventDefault();
      tabs.scrollLeft += e.deltaY;
    }
  }, { passive: false });
  const list = h("div", { class: "list" }, h("div", { class: "empty" }, "読み込み中…"));
  const more = h("div");
  const filter = h("input", { type: "search", placeholder: "タイトル・パス・#番号で絞り込み", "aria-label": "絞り込み" });
  let showArchived = false;
  const archBtn = h("button", { class: "btn small", onclick: () => { showArchived = !showArchived; selected.clear(); paintSel(); archBtn.classList.toggle("on", showArchived); archBtn.textContent = showArchived ? "アーカイブ表示中" : "アーカイブ"; refreshNow(); } }, "アーカイブ");
  const status = h("div", { class: "muted small" });
  const settingsBox = h("details", { class: "settings-box" }, h("summary", {}, "端末の設定（閲覧のみ）"));
  const usageCard = h("div", { class: "usage-card", hidden: true });
  const loadUsage = async () => {
    const u = await dev(id).get("/api/usage?days=7").catch(() => null);
    if (!u) return;
    const days = [];
    for (let i = 6; i >= 0; i--) {
      const dt = new Date(Date.now() - i * 86400000);
      const key = `${dt.getFullYear()}-${String(dt.getMonth() + 1).padStart(2, "0")}-${String(dt.getDate()).padStart(2, "0")}`;
      days.push([`${dt.getMonth() + 1}/${dt.getDate()}`, u.by_day?.[key]?.cost || 0]);
    }
    const max = Math.max(...days.map((x) => x[1]), 0.01);
    usageCard.hidden = !u.range?.messages;
    usageCard.replaceChildren(
      h("div", { class: "row" },
        h("div", { class: "grow" }, h("div", { class: "muted small" }, "今日（API 換算）"), h("b", { class: "big" }, fmtUSD(u.today?.cost || 0))),
        h("div", {}, h("div", { class: "muted small" }, "7 日間"), h("b", {}, fmtUSD(u.range?.cost || 0)), h("span", { class: "muted small" }, ` · ${fmtTok(tokTotal(u.range || {}))} tok`))),
      // Only today's value is printed; the others are in each bar's tooltip.
      h("div", { class: "bars", title: "日別（API 換算）" }, days.map(([d, v], i) => h("div", { class: "bar" + (i === days.length - 1 ? " today" : ""), title: `${d} ${fmtUSD(v)}` },
        h("span", { style: `height:${Math.max(3, Math.round((v / max) * 44))}px` }), h("small", {}, i === days.length - 1 ? "今日" : d)))));
  };
  loadUsage();
  const usageTimer = setInterval(loadUsage, 60000);
  bucket.push(() => clearInterval(usageTimer));
  settingsBox.addEventListener("toggle", async () => {
    if (!settingsBox.open || settingsBox.dataset.loaded) return;
    try {
      const rows = await dev(id).get("/hub/settings");
      settingsBox.dataset.loaded = "1";
      settingsBox.append(h("div", { class: "list" }, rows.map((r) => h("div", { class: "item", style: "cursor:default", title: r.help },
        h("div", { class: "grow" }, h("div", {}, r.label), h("div", { class: "sub" }, r.help)),
        h("span", { class: "chip" + (r.value === true ? " idle" : "") }, r.value === true ? "ON" : r.value === false ? "OFF" : String(r.value ?? "")))),
        h("p", { class: "muted small" }, "変更は端末の TUI（, キー）から。ハブからは変更できません。")));
    } catch (e) { toast(e.message); }
  });
  // Checkboxes on the rows select sessions for bulk actions: put them on
  // the grid, archive (or unarchive), put them in a project, or generate
  // titles with claude -p.
  const selected = new Set();
  const selBar = h("div", { class: "sel-bar", hidden: true });
  const sel = {
    set: selected,
    toggle(sid, on) { on ? selected.add(sid) : selected.delete(sid); paintSel(); render(); },
  };
  const bulk = async (label, fn) => {
    const ids = [...selected];
    let ok = 0, ng = 0, lastErr = "";
    for (const sid of ids) {
      try { await fn(sid); ok++; } catch (e) { ng++; lastErr = e.message; }
    }
    toast(`${label}: ${ok} 件` + (ng ? `（失敗 ${ng} 件: ${lastErr}）` : ""));
    selected.clear();
    paintSel();
    refreshNow();
  };
  const setProject = () => projectPicker(id, { count: selected.size }, (name) =>
    bulk(name ? `プロジェクト「${name}」に追加` : "プロジェクトから外す",
      (sid) => dev(id).post(`/api/sessions/${encodeURIComponent(sid)}/project`, { project: name })));
  const genTitles = async () => {
    const ids = [...selected];
    let ok = 0, lastErr = "";
    for (let i = 0; i < ids.length; i += 10) {
      try { await dev(id).post("/api/titles", { sessionIds: ids.slice(i, i + 10) }); ok += Math.min(10, ids.length - i); }
      catch (e) { lastErr = e.message; }
    }
    toast(ok ? `タイトルを生成しています（claude -p, ${ok} 件）` : "生成できませんでした: " + lastErr);
    selected.clear();
    paintSel();
    render();
  };
  const paintSel = () => {
    selBar.hidden = !selected.size;
    list.classList.toggle("selecting", selected.size > 0);
    if (!selected.size) return;
    const keys = [...selected].map((sid) => id + ":" + sid);
    selBar.replaceChildren(
      h("b", {}, `${selected.size} 件選択`),
      h("button", { class: "btn small", onclick: () => {
        keys.forEach((k) => gridList.put(k, true));
        toast("グリッドに追加しました"); selected.clear(); paintSel(); render();
      } }, icon("grid"), "グリッドに追加"),
      h("button", { class: "btn small", onclick: () => {
        keys.forEach((k) => gridList.put(k, false));
        toast("グリッドから外しました"); selected.clear(); paintSel(); render();
      } }, "グリッドから外す"),
      h("button", { class: "btn small", onclick: () => bulk(showArchived ? "アーカイブ解除" : "アーカイブ",
        (sid) => dev(id).post(`/api/sessions/${encodeURIComponent(sid)}/archive`, { archived: !showArchived })) },
        showArchived ? "アーカイブ解除" : "アーカイブ"),
      h("button", { class: "btn small", title: "複数のセッションを一つのプロジェクトにまとめる", onclick: setProject }, icon("folder"), "プロジェクト"),
      h("button", { class: "btn small", onclick: () => bulk("あとで見る", (sid) => dev(id).post(`/api/sessions/${encodeURIComponent(sid)}/later`, { later: true })) }, icon("clock"), "あとで見る"),
      h("button", { class: "btn small", title: "claude -p でタイトルを生成", onclick: genTitles }, "タイトル生成"),
      h("span", { class: "grow" }),
      h("button", { class: "btn small", onclick: () => { for (const sid of visible) selected.add(sid); paintSel(); render(); } }, "表示中を全選択"),
      h("button", { class: "btn small", onclick: () => { selected.clear(); paintSel(); render(); } }, "選択解除"));
  };
  let visible = [];
  mount.append(h("div", { class: "page" + (opts.compact ? " compact" : "") },
    h("div", { class: "row" },
      h("h1", { class: "grow" }, name),
      h("button", { class: "btn icon", title: "この端末の ccdash collector を再起動（ホスト中のセッションは自動で再開）", onclick: () => restartCollector(id, last), "aria-label": "collector を再起動" }, icon("restart")),
      h("button", { class: "btn", title: "スキル・コマンドを新しいセッションで実行（TUI の S）",
        onclick: () => skillPicker(id, recentCwds(last?.sessions)) }, "スキル実行"),
      h("button", { class: "btn primary", onclick: () => newSession(id) }, icon("plus"), "新規セッション")),
    status, usageCard, approvals, spawns,
    tabs,
    h("div", { class: "row", style: "margin-bottom:8px" }, h("label", { class: "search grow" }, icon("search"), filter), archBtn),
    selBar, list, more, settingsBox));

  let last = null, tab = loadTab(id), limit = 100, briefs = {}, briefsAt = 0, shownTab = null;
  const render = () => {
    if (!last) return;
    if (deferWhileSelecting(list, render)) return;
    const { sessions, ptys, info } = last;
    const live = new Set(ptys.filter((p) => p.alive).map((p) => p.key));
    const groups = uniqueGroups(sessions, info.autoRepoTabs !== false);
    if (!groups.includes(tab)) tab = "";

    tabs.replaceChildren(...groups.map((g) => {
      const n = g ? sessions.filter((s) => groupOf(s) === g).length : sessions.length;
      const active = (s) => s.status === "active" || s.status === "idle";
      const running = (g ? sessions.filter((s) => groupOf(s) === g) : sessions).some(active);
      return h("button", {
        class: "tab" + (g === tab ? " on" : ""),
        onclick: () => { tab = g; limit = 100; saveTab(id, g); render(); },
      }, running ? h("span", { class: "dot on" }) : null, g || "最新", h("span", { class: "count" }, n));
    }));
    // Bring the active tab into view only when it changes, and only by
    // scrolling the strip sideways: scrollIntoView also scrolls the page,
    // which yanked a reader back up to the tabs on every 3 s refresh.
    if (shownTab !== tab) {
      shownTab = tab;
      revealInStrip(tabs, tabs.querySelector(".tab.on"));
    }

    const q = filter.value.trim().toLowerCase();
    const rows = sessions
      .filter((s) => !tab || groupOf(s) === tab)
      .filter((s) => !q || `${sessionTitle(s)} ${s.cwd} #${s.num} ${groupOf(s)} ${s.project || ""}`.toLowerCase().includes(q))
      .sort((a, b) => (!!b.favorite - !!a.favorite) || pinOrder(a, b) || (new Date(b.last_seen) - new Date(a.last_seen)));
    const ordered = laterFirst(projectsFirst(rows));
    const out = [];
    let bucket = null;
    visible = ordered.slice(0, limit).map((s) => s.session_id);
    for (const s of ordered.slice(0, limit)) {
      const b = s.later ? "\0later" : s.project ? "\0" + s.project : dateBucket(s);
      if (b !== bucket) {
        bucket = b;
        out.push(s.later ? h("div", { class: "bucket later" }, icon("clock"), "あとで見る")
          : s.project
          ? projectHeader(s.project, projectColorOf(s.project, s.project_color), ordered.filter((x) => x.project === s.project),
            // The project's directory: where its most recent session works.
            () => newSession(id, { cwd: s.cwd, project: s.project }),
            async (delta) => {
              try { await dev(id).post("/api/projects/order", { project: s.project, delta }); refreshNow(); } catch (e) { toast(e.message); }
            })
          : h("div", { class: "bucket" }, b));
      }
      out.push(sessionRow(id, s, live.has(s.session_id), info, !tab, sel, briefs[s.session_id]));
    }
    list.replaceChildren(...(out.length ? out : [h("div", { class: "empty" }, q ? "該当なし" : "セッションがありません")]));
    more.replaceChildren(...(rows.length > limit
      ? [h("button", { class: "btn", style: "margin-top:8px;width:100%", onclick: () => { limit += 100; render(); } }, `さらに表示（残り ${rows.length - limit}）`)]
      : []));

    // Fresh spawns not yet tied to a session row.
    const byPid = new Map();
    for (const p of ptys) {
      if (!byPid.has(p.pid)) byPid.set(p.pid, []);
      byPid.get(p.pid).push(p.key);
    }
    const fresh = [...byPid.values()].filter((keys) => keys.every((k) => k.startsWith("pid-"))).map((keys) => keys[0]);
    const ptyOf = new Map(ptys.map((p) => [p.key, p]));
    // A spawn can wait on a dialog before claude registers a session (the
    // folder-trust question in a new folder): show it, open it to answer.
    spawns.replaceChildren(...(fresh.length ? [h("h2", {}, "起動中（未登録）"), h("div", { class: "list" },
      fresh.map((k) => {
        const p = ptyOf.get(k) || {};
        return h("div", { class: "item srow", onclick: () => (location.hash = `#/d/${id}/p/${encodeURIComponent(k)}`) },
          h("div", { class: "grow" },
            h("div", { class: "title mono" }, p.cwd ? p.cwd.split("/").filter(Boolean).pop() : k),
            p.prompt ? h("div", { class: "muted small" }, p.prompt) : null),
          h("div", { class: "srow-tail" }, p.prompt ? h("span", { class: "chip pend" }, "要対応") : h("span", { class: "chip live" }, "live")));
      }))] : []));
  };
  filter.addEventListener("input", render);

  const refreshNow = () => tick();
  const tick = async () => {
    const d = dev(id);
    const [sessions, aps, ptys, info] = await Promise.all([
      d.get(showArchived ? "/api/sessions?archived=1" : "/api/sessions").then((x) => x || []), d.get("/api/approvals").then((x) => x || []),
      d.get("/pty/").then((x) => x || []).catch(() => []), deviceInfo(id),
    ]);
    last = { sessions, ptys, info };
    // Model / context / cost per row: a transcript scan, so not every tick.
    if (Date.now() - briefsAt > 30000) {
      briefsAt = Date.now();
      d.get(showArchived ? "/api/usage/sessions?archived=1" : "/api/usage/sessions")
        .then((b) => { briefs = b || {}; render(); }).catch(() => {});
    }
    status.textContent = (info.hostname && info.hostname !== name ? info.hostname + " · " : "") + `ccdash ${info.version}` +
      (info.attachEnabled ? "" : " · attach OFF（閲覧のみ）") + (info.approveEnabled ? "" : " · 承認 OFF");
    renderApprovals(approvals, id, aps, sessions);
    render();
  };
  every(3000, tick, bucket);
}

// restartCollector re-execs the device's collector (POST /api/restart). The
// claudes it hosts are stopped and resumed by the new process; the tunnel
// drops for a few seconds meanwhile.
async function restartCollector(id, last) {
  const hosted = (last?.ptys || []).filter((p) => p.alive && !p.key.startsWith("pid-")).length;
  const fresh = (last?.ptys || []).filter((p) => p.alive && p.key.startsWith("pid-")).length;
  if (!confirm(`この端末の ccdash collector を再起動しますか？\n\n` +
    `・ホスト中のセッション ${hosted} 件はいったん停止し、再起動後に claude --resume で自動的に再開します（応答中の内容は中断されます）\n` +
    (fresh ? `・まだ未登録（最初のメッセージ前）の ${fresh} 件は再開されません\n` : "") +
    `・数秒〜数十秒、この端末に接続できなくなります`)) return;
  try {
    const r = await dev(id).post("/api/restart", {});
    toast(`再起動しています… ${r?.resuming ?? hosted} 件を再開します`, 6000);
  } catch (e) { toast(e.message); }
}

// ---- working beam ----
// While Claude is mid-turn, a gradient comet circles the session's row or
// tile (see .beam in app.css): a conic gradient, masked to a thin ring.
// The gradient is drawn once on a square the size of the box's diagonal
// and turned by transform: rotate — a compositor animation, no repaint per
// frame. The angle keyframes are computed so the comet's head moves along
// the edge at one speed, whatever the box's shape (a plain spin crawls
// along the long sides). Phase follows the clock, so a row rebuilt by a
// refresh carries on where the old one was.
const BEAM_MS = 3200;
const beamOff = () => document.documentElement.classList.contains("no-beam") || matchMedia("(prefers-reduced-motion: reduce)").matches;
const beamRO = new ResizeObserver((entries) => {
  for (const e of entries) {
    if (!e.target.isConnected) { beamRO.unobserve(e.target); stopBeam(e.target); continue; }
    runBeam(e.target);
  }
});
function stopBeam(b) { b._anim?.cancel(); b._anim = null; }
// beamKeyframes: rotate angles (clockwise from 12 o'clock) that aim at
// evenly spaced points round a w×h box, starting at the top centre.
function beamKeyframes(w, ht, n = 72) {
  const P = 2 * (w + ht);
  const at = (d) => { // point at distance d clockwise from the top centre, relative to the centre
    d = (d + w / 2) % P;
    if (d < w) return [d - w / 2, -ht / 2];
    if ((d -= w) < ht) return [w / 2, d - ht / 2];
    if ((d -= ht) < w) return [w / 2 - d, ht / 2];
    return [-w / 2, ht / 2 - (d - w)];
  };
  const out = [];
  let prev = 0;
  for (let k = 0; k <= n; k++) {
    const [x, y] = at((P * k) / n);
    let deg = (Math.atan2(x, -y) * 180) / Math.PI;
    while (deg < prev - 1e-6) deg += 360; // keep turning one way
    if (k === n) deg = 360;
    out.push({ transform: `rotate(${deg}deg)`, offset: k / n });
    prev = deg;
  }
  return out;
}
function runBeam(b) {
  stopBeam(b);
  const w = b.clientWidth, ht = b.clientHeight, i = b.firstChild;
  if (!w || !ht || !i) return;
  const D = Math.ceil(Math.hypot(w, ht)) + 4;
  Object.assign(i.style, { width: D + "px", height: D + "px", marginLeft: -D / 2 + "px", marginTop: -D / 2 + "px" });
  if (beamOff()) return;
  b._anim = i.animate(beamKeyframes(w, ht), { duration: BEAM_MS, iterations: Infinity, easing: "linear" });
  b._anim.currentTime = Date.now() % BEAM_MS;
}
function beamEl() {
  const b = h("span", { class: "beam", "aria-hidden": "true" }, h("i"));
  beamRO.observe(b); // fires once laid out → runBeam
  return b;
}
const refreshBeams = () => document.querySelectorAll(".beam").forEach(runBeam);
// setBeam lights or puts out the beam on a box that stays mounted across
// refreshes (a grid tile, the chat header), keeping a running one going.
function setBeam(el, on) {
  if (on && !el.classList.contains("working")) el.append(beamEl());
  else if (!on) el.querySelector(":scope > .beam")?.remove();
  el.classList.toggle("working", on);
}

// statusTag: a session's state as a dot + Japanese word (docs/design.md:
// status is words, not English pills).
const STATUS_JA = { active: "作業中", idle: "待機中", recent: "停止", stopped: "停止" };
function statusTag(st) {
  return h("span", { class: "stat " + st }, h("i"), STATUS_JA[st] || st);
}

// recentReply mirrors model.Session.RecentReply: finished a turn within
// RECENT_REPLY_MS and already seen — NEW is gone, a quiet "返信 3分前" stays.
const RECENT_REPLY_MS = 30 * 60 * 1000;
// SEEN_DWELL_MS: how long an open chat must be looked at before it counts
// as seen (twin of seenDwell in internal/tui). engagedTile is the grid tile
// last clicked / focused — the only tile that can become seen.
const SEEN_DWELL_MS = 3000;
let engagedTile = null;
function recentReply(s) {
  return !!s && !s.unseen && s.status !== "active" && s.attention !== "needs_you" && !s.pending_count &&
    !!s.last_activity && Date.now() - new Date(s.last_activity).getTime() < RECENT_REPLY_MS;
}
// freshChip: NEW (unread progress) or "返信 3分前" (recently replied, seen).
function freshChip(s) {
  if (s?.unseen) return h("span", { class: "chip new", title: "最後に見た後に進捗あり" }, "NEW");
  if (recentReply(s)) return h("span", { class: "chip recent", title: "最近返信あり（既読）" }, "返信 " + rel(s.last_activity));
  return null;
}

function sessionRow(id, s, live, info, showGroup, sel = null, brief = null) {
  const chips = [];
  const fresh = freshChip(s);
  if (fresh) chips.push(fresh);
  // (watch-later rows sit under the あとで見る band, which says so)
  if (live) chips.push(h("span", { class: "chip live", title: "ccdash 上で動いています（チャットから操作できます）" }, "live"));
  const wc = waitChip(s);
  if (wc && (s.attention === "needs_you" || !WAIT_LABEL_NEEDS[s.wait])) chips.push(wc);
  if (s.attention === "needs_you") { if (!wc) chips.push(h("span", { class: "chip pend", title: s.attention_reason || "" }, "要対応")); }
  else if (s.attention === "done") chips.push(h("span", { class: "chip done" }, "未確認"));
  if (s.account && s.account !== "default") chips.unshift(h("span", { class: "chip" }, s.account));
  const sub = [s.attention === "needs_you" && s.attention_reason ? "? " + s.attention_reason
    : waitDeclared(s) && !WAIT_LABEL_NEEDS[s.wait] && s.wait_note ? s.wait_note : null,
    showGroup ? groupOf(s) : null, shortPath(s.cwd, info?.home), s.branch, rel(s.last_seen)].filter(Boolean).join(" · ");
  const here = decodeURIComponent(location.hash).startsWith(`#/d/${id}/s/${s.session_id}`);
  const gk = id + ":" + s.session_id;
  const mark = h("button", { class: "grid-mark" + (gridList.is(gk, s) ? " on" : ""), type: "button", title: "グリッドに表示する／外す", "aria-label": "グリッドに表示する／外す" }, icon("grid"));
  mark.addEventListener("click", (e) => {
    e.stopPropagation();
    const on = !gridList.is(gk, s);
    gridList.put(gk, on);
    mark.classList.toggle("on", on);
    toast(on ? "グリッドに追加しました" : "グリッドから外しました");
  });
  const check = sel && h("input", { type: "checkbox", class: "row-check", title: "選択してまとめて操作" });
  if (check) {
    check.checked = sel.set.has(s.session_id);
    check.addEventListener("click", (e) => e.stopPropagation());
    check.addEventListener("change", () => sel.toggle(s.session_id, check.checked));
  }
  const proj = s.project ? `;--proj:${projectColorOf(s.project, s.project_color)}` : "";
  return h("div", { class: "item srow" + (here ? " selected" : "") + (check?.checked ? " checked" : "") + (s.project ? " in-project" : "") + (s.status === "active" ? " working" : ""), "data-sid": s.session_id,
    style: `--sess:${sessionColorOf(s)}${proj}`, onclick: (e) => {
    if (selectingIn(e.currentTarget)) return; // a drag to copy text, not a tap
    if (e.currentTarget.dataset.longpressed) { delete e.currentTarget.dataset.longpressed; return; }
    // While some rows are selected (edit mode), a tap toggles the row.
    if (sel && sel.set.size) { sel.toggle(s.session_id, !sel.set.has(s.session_id)); haptic(); return; }
    location.hash = `#/d/${id}/s/${encodeURIComponent(s.session_id)}`;
  }, onpointerdown: (e) => {
    // Long press selects (how a phone list enters edit mode).
    if (!sel || e.pointerType !== "touch") return;
    const row = e.currentTarget;
    const t = setTimeout(() => { row.dataset.longpressed = "1"; haptic(15); sel.toggle(s.session_id, true); }, 480);
    const cancel = () => { clearTimeout(t); row.removeEventListener("pointerup", cancel); row.removeEventListener("pointermove", moved); row.removeEventListener("pointercancel", cancel); };
    const x0 = e.clientX, y0 = e.clientY;
    const moved = (ev) => { if (Math.abs(ev.clientX - x0) + Math.abs(ev.clientY - y0) > 10) cancel(); };
    row.addEventListener("pointerup", cancel);
    row.addEventListener("pointermove", moved);
    row.addEventListener("pointercancel", cancel);
  } },
    check || null,
    h("span", { class: "num" }, s.num ? `#${s.num}` : ""),
    h("div", { class: "grow" },
      h("div", { class: titleClass("title", s), title: s.title_status === "running" ? TITLING_TIP : null }, s.favorite ? h("span", { class: "pin", title: "ピン留め" }, icon("pin")) : null, sessionTitle(s)),
      h("div", { class: "sub" }, statusTag(s.status), sub),
      brief ? h("div", { class: "sub meta" }, usageLine(brief)) : null),
    h("span", { class: "srow-tail" }, ...chips, mark),
    s.status === "active" ? beamEl() : null);
}

function renderApprovals(el, id, aps, sessions) {
  if (!aps?.length) { el.replaceChildren(); return; }
  const byId = new Map(sessions.map((s) => [s.session_id, s]));
  el.replaceChildren(h("h2", {}, `承認待ち (${aps.length})`), ...aps.map((a) => {
    const s = byId.get(a.session_id);
    let input = "";
    try { input = JSON.stringify(a.tool_input, null, 2); } catch {}
    const decide = async (behavior, keep = false) => {
      try { await dev(id).post(`/approvals/${a.id}/decide`, { behavior, keep }); toast(behavior === "allow" ? "許可しました" : "拒否しました"); }
      catch (e) { toast(e.message); }
    };
    return h("div", { class: "approval" },
      h("div", { class: "row" }, h("b", {}, a.tool), h("span", { class: "grow muted small" }, s ? `${s.num ? "#" + s.num + " " : ""}${sessionTitle(s)}` : a.session_id)),
      h("pre", { class: "mono" }, input),
      h("div", { class: "row" },
        h("button", { class: "btn primary", onclick: () => decide("allow") }, "許可"),
        h("button", { class: "btn", onclick: () => decide("allow", true) }, "常に許可"),
        h("button", { class: "btn danger", onclick: () => decide("deny") }, "拒否")));
  }));
}

// newSession picks a directory and starts claude there — optionally with a
// first message, e.g. a skill / slash command like the TUI's skill picker
// (S). A project skill only loads inside its project, so picking one moves
// the directory there.
function newSession(id, preset = {}) {
  dialog(async (body, close) => {
    const info = await deviceInfo(id).catch(() => null);
    if (info && !info.attachEnabled) {
      body.append(h("p", {}, "この端末は attach が OFF のため、新規セッションを起動できません。"), h("div", { class: "actions" }, h("button", { class: "btn" }, "閉じる")));
      return;
    }
    const path = h("input", { class: "grow mono", value: preset.cwd || info?.newSessionDir || "" });
    const dirs = h("div", { class: "dirlist" });
    const prompt = h("textarea", { rows: 2, class: "grow", placeholder: "最初の指示（任意）。/ でスキル・コマンドを選べます" });
    prompt.value = preset.prompt || ""; // a textarea's value attribute isn't its content
    const mode = h("select", { class: "mode" },
      h("option", { value: "" }, "権限モード: 既定"),
      h("option", { value: "manual" }, "manual（毎回確認）"),
      h("option", { value: "acceptEdits" }, "acceptEdits（編集は自動許可）"),
      h("option", { value: "auto" }, "auto（自動判断）"),
      h("option", { value: "plan" }, "plan（計画だけ立てる）"));
    const skillBox = h("div", { class: "dirlist skills", hidden: true });
    let skillList = null;
    const loadSkills = async () => {
      try { skillList = (await dev(id).get(`/hub/skills?dir=${encodeURIComponent(path.value)}`)) || []; } catch { skillList = []; }
    };
    const showSkills = async () => {
      const v = prompt.value;
      if (!v.startsWith("/") || /\s/.test(v)) { skillBox.hidden = true; return; }
      if (!skillList) await loadSkills();
      const q = v.slice(1).toLowerCase();
      const hits = skillList.filter((k) => k.name.toLowerCase().includes(q) || (k.description || "").toLowerCase().includes(q)).slice(0, 40);
      skillBox.hidden = !hits.length;
      skillBox.replaceChildren(...hits.map((k) => h("div", { onclick: () => {
        prompt.value = "/" + k.name + " ";
        if (k.dir) { path.value = k.dir; load(k.dir); }
        skillBox.hidden = true;
        prompt.focus();
      } }, h("b", {}, "/" + k.name), k.source === "project" ? h("span", { class: "chip" }, "project") : null,
        k.description ? h("div", { class: "muted small" }, k.description) : null)));
    };
    prompt.addEventListener("input", showSkills);
    const load = async (p) => {
      try {
        const r = await dev(id).get(`/hub/dirs?path=${encodeURIComponent(p)}`);
        if (path.value !== r.path) skillList = null;
        path.value = r.path;
        dirs.replaceChildren(
          h("div", { onclick: () => load(r.parent) }, "../"),
          ...r.dirs.map((d) => h("div", { onclick: () => load(r.path.replace(/\/$/, "") + "/" + d) }, d + "/")));
      } catch (e) { toast(e.message); }
    };
    path.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); load(path.value); } });
    const start = async (e) => {
      e.preventDefault();
      let first = prompt.value.trim();
      if (first.startsWith("-")) first = " " + first;
      try {
        const r = await dev(id).post("/pty/start", { cwd: path.value, cols: 120, rows: 36, prompt: first, permissionMode: mode.value, project: preset.project || "" });
        close();
        location.hash = `#/d/${id}/p/${encodeURIComponent(r.ptyKey)}`;
      } catch (err) { toast(err.message); }
    };
    body.append(h("h1", {}, "新規セッション"),
      preset.project ? h("p", { class: "muted small" }, `プロジェクト「${preset.project}」に入ります`) : null,
      h("div", { class: "row" }, path, h("button", { type: "button", class: "btn", onclick: () => load(path.value) }, "移動")),
      dirs,
      h("div", { class: "col" }, prompt, skillBox, mode),
      h("div", { class: "actions" },
        h("button", { class: "btn", value: "cancel" }, "キャンセル"),
        h("button", { class: "btn primary", onclick: start }, "ここで claude を起動")));
    load(path.value);
    if (preset.prompt) showSkills();
  });
}

// recentCwds: distinct session directories, most recently seen first.
function recentCwds(sessions) {
  const out = [];
  for (const s of [...(sessions || [])].sort((a, b) => new Date(b.last_seen) - new Date(a.last_seen))) {
    if (s.cwd && !out.includes(s.cwd)) out.push(s.cwd);
    if (out.length >= 40) break;
  }
  return out;
}

// filterSkills mirrors the TUI's (internal/tui/skillpicker.go): name
// prefix (also the part after "plugin:") first, then name substring, then
// description / project directory. Once arguments follow, nothing matches.
function filterSkills(list, input) {
  if (/\S\s/.test(input.trim())) return [];
  const q = input.trim().replace(/^\//, "").toLowerCase();
  if (!q) return list;
  const prefix = [], name = [], desc = [];
  for (const k of list) {
    const ln = k.name.toLowerCase(), short = ln.includes(":") ? ln.slice(ln.indexOf(":") + 1) : "";
    if (ln.startsWith(q) || (short && short.startsWith(q))) prefix.push(k);
    else if (ln.includes(q)) name.push(k);
    else if ((k.description || "").toLowerCase().includes(q) || (k.dir || "").toLowerCase().includes(q)) desc.push(k);
  }
  return [...prefix, ...name, ...desc];
}

// skillPicker is the TUI's `S`: the device's skills and slash commands
// (project skills of the recent session directories first), filtered as
// you type. Picking one opens the new-session dialog with "/<skill> " as the
// first message — in the skill's project for a project skill, since it only
// loads there. Free text with no match (built-ins, arguments) goes as-is.
function skillPicker(id, dirs) {
  dialog(async (body, close) => {
    const input = h("input", { class: "grow mono", placeholder: "スキル名で絞り込み（引数は スペースのあとに）", autocomplete: "off" });
    const list = h("div", { class: "dirlist skills" }, h("div", { class: "muted" }, "読み込み中…"));
    let all = null, sel = 0, hits = [], tabbed = null;
    const go = (k) => {
      let first = input.value.trim();
      if (k) {
        // Keep arguments typed after a completed name.
        if (!first.startsWith("/" + k.name + " ")) first = "/" + k.name;
      } else if (first && !first.startsWith("/")) first = "/" + first;
      if (!first) return;
      close();
      // A bare "/name" gets a trailing space so arguments can follow.
      newSession(id, { cwd: k?.dir, prompt: /\s/.test(first) ? first : first + " " });
    };
    const paint = () => {
      if (!all) return;
      hits = filterSkills(all, input.value);
      sel = Math.min(sel, Math.max(hits.length - 1, 0));
      const rows = hits.slice(0, 80).map((k, i) => h("div", { class: i === sel ? "on" : null, onclick: () => go(k) },
        h("b", {}, "/" + k.name), k.source === "project" ? h("span", { class: "chip" }, "project") : null,
        k.dir ? h("span", { class: "muted small" }, " " + k.dir) : null,
        k.description ? h("div", { class: "muted small" }, k.description) : null));
      const typed = input.value.trim();
      if (!rows.length) rows.push(h("div", { class: "muted", onclick: () => go(null) },
        typed ? `Enter で ${typed.startsWith("/") ? typed : "/" + typed} を実行` : "スキルが見つかりません"));
      list.replaceChildren(...rows);
      list.querySelector(".on")?.scrollIntoView({ block: "nearest" });
    };
    input.addEventListener("input", () => { sel = 0; paint(); });
    input.addEventListener("keydown", (e) => {
      if (e.isComposing) return;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        if (hits.length) sel = (sel + (e.key === "ArrowDown" ? 1 : -1) + hits.length) % hits.length;
        paint();
      } else if (e.key === "Tab" && hits.length) {
        // Complete the name and drop into argument mode, as in the TUI.
        e.preventDefault();
        input.value = "/" + hits[sel].name + " ";
        tabbed = hits[sel];
        paint();
      } else if (e.key === "Enter") {
        e.preventDefault();
        const t = input.value.trim();
        go(hits[sel] || (tabbed && (t + " ").startsWith("/" + tabbed.name + " ") ? tabbed : null));
      }
    });
    body.append(h("h1", {}, "スキル実行"),
      h("p", { class: "muted small" }, "選んだスキルを最初の指示にして新しいセッションを起動します。"),
      h("div", { class: "row" }, input),
      list,
      h("div", { class: "actions" }, h("button", { class: "btn", value: "cancel" }, "キャンセル")));
    input.focus();
    const q = new URLSearchParams();
    for (const d of dirs || []) q.append("dir", d);
    try { all = (await dev(id).get(`/hub/skills?${q}`)) || []; } catch (e) { all = []; toast(e.message); }
    paint();
  });
}

// ---------- session (chat) ----------

// Sessions open as a chat: the transcript JSONL rendered as messages, a
// composer that types into the ccdash-hosted PTY, approval cards, and — for
// TUI-only prompts (permission dialogs without hooks, menus, the folder
// trust prompt) — the bottom of the emulated screen with one-tap answers.
// The xterm view stays one button away (…/term).

function sessionPage(id, sid, mode) {
  return mode === "term" ? termRoute(id, { sid }) : chatPage(id, { sid });
}

function spawnPage(id, key, mode) {
  return mode === "term" ? termRoute(id, { key }) : chatPage(id, { key });
}

// aliasOf finds the session id a fresh pid-<N> spawn was registered under.
function aliasOf(ptys, key) {
  const p = ptys.find((x) => x.key === key);
  if (!p) return null;
  return ptys.find((x) => x.pid === p.pid && !x.key.startsWith("pid-"))?.key || null;
}

async function termRoute(id, { sid, key }) {
  const mount = mainMount;
  const name = await deviceName(id);
  const ptys = (await dev(id).get("/pty/").catch(() => [])) || [];
  const k = key || sid;
  const chatHref = sid ? `#/d/${id}/s/${encodeURIComponent(sid)}` : `#/d/${id}/p/${encodeURIComponent(key)}`;
  crumbs({ text: "端末", href: "#/" }, { text: name, href: `#/d/${id}` }, { text: "ターミナル" });
  if (!ptys.some((p) => p.key === k && p.alive)) {
    toast("このセッションは ccdash 上で動いていません");
    location.replace(chatHref);
    return;
  }
  if (mainMount !== mount) return; // navigated away meanwhile
  terminal(id, k, sid ? "ターミナル" : "新規セッション", chatHref);
}

// ---- markdown-lite (escape first, then a few safe constructs) ----

function esc(s) {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function inlineMd(s) {
  return s
    .replace(/`([^`\n]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>")
    .replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
  // Bare URLs are linked after rendering (linkify), wherever they are.
}

// linkify turns every bare http(s) URL in el's text into a link — after
// Japanese text, inside inline code, code blocks, tool output — leaving
// existing links alone. Trailing punctuation stays text.
const URL_RE = /https?:\/\/[^\s<>"'`\u0080-\uffff]+/g; // raw non-ASCII ends it (日本語 right after a URL)
function linkify(el) {
  const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT, {
    acceptNode: (n) => n.parentElement?.closest("a, textarea, input") ? NodeFilter.FILTER_REJECT
      : /https?:\/\//.test(n.data) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_SKIP,
  });
  const nodes = [];
  for (let n = walker.nextNode(); n; n = walker.nextNode()) nodes.push(n);
  for (const n of nodes) {
    const frag = document.createDocumentFragment();
    let at = 0;
    for (const m of n.data.matchAll(URL_RE)) {
      let url = m[0];
      // Sentence punctuation and an unmatched closing bracket aren't part of it.
      const count = (c) => url.split(c).length - 1;
      for (;;) {
        const ch = url.slice(-1);
        if (/[.,;:!?。、」』）]/.test(ch) || (ch === ")" && count(")") > count("(")) || (ch === "]" && count("]") > count("["))) url = url.slice(0, -1);
        else break;
      }
      if (url.length <= "https://".length) continue;
      frag.append(n.data.slice(at, m.index), h("a", { href: url, target: "_blank", rel: "noopener noreferrer" }, url));
      at = m.index + url.length;
    }
    if (!at) continue;
    frag.append(n.data.slice(at));
    n.replaceWith(frag);
  }
  return el;
}

function md(text) {
  const out = [];
  const lines = text.split("\n");
  let i = 0, list = null, callout = false;
  const closeList = () => { if (list) { out.push(`</${list}>`); list = null; } };
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(/^\s*```(\S*)/);
    if (fence) {
      closeList();
      const buf = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lines[i])) buf.push(lines[i++]);
      i++;
      out.push(`<pre class="code"><code>${esc(buf.join("\n"))}</code></pre>`);
      continue;
    }
    // Tables: consecutive "| a | b |" lines; the |---| row is dropped.
    if (/^\s*\|.*\|\s*$/.test(line)) {
      closeList();
      const rows = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) rows.push(lines[i++]);
      const cells = (r) => r.trim().replace(/^\||\|$/g, "").split("|").map((c) => inlineMd(esc(c.trim())));
      const body = rows.filter((r) => !/^\s*\|[\s:|-]+\|\s*$/.test(r)).map(cells);
      const head = rows.length > 1 && /^\s*\|[\s:|-]+\|\s*$/.test(rows[1]) ? body.shift() : null;
      out.push(`<div class="md-table"><table>${head ? `<thead><tr>${head.map((c) => `<th>${c}</th>`).join("")}</tr></thead>` : ""}<tbody>${body.map((r) => `<tr>${r.map((c) => `<td>${c}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`);
      continue;
    }
    const e = esc(line);
    let m;
    if ((m = e.match(/^(#{1,4})\s+(.*)$/))) {
      closeList();
      out.push(`<div class="md-h">${inlineMd(m[2])}</div>`);
    } else if ((m = e.match(/^\s*[-*]\s+(.*)$/))) {
      if (list !== "ul") { closeList(); out.push("<ul>"); list = "ul"; }
      out.push(`<li>${inlineMd(m[1])}</li>`);
    } else if ((m = e.match(/^\s*\d+[.)]\s+(.*)$/))) {
      if (list !== "ol") { closeList(); out.push("<ol>"); list = "ol"; }
      out.push(`<li>${inlineMd(m[1])}</li>`);
    } else if (/^\s*`?★\s*Insight\s*─+`?\s*$/.test(line)) {
      // Claude's explanatory-style "★ Insight ───" … "───" block: a callout.
      closeList();
      out.push('<div class="md-callout"><div class="md-callout-h">★ Insight</div>');
      callout = true;
    } else if (/^\s*`?─{8,}`?\s*$/.test(line)) {
      closeList();
      out.push(callout ? "</div>" : '<hr class="md-hr">');
      callout = false;
    } else if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) {
      closeList();
      out.push('<hr class="md-hr">');
    } else if (e.trim() === "") {
      closeList();
      out.push('<div class="md-gap"></div>');
    } else {
      closeList();
      out.push(`<div>${inlineMd(e)}</div>`);
    }
    i++;
  }
  closeList();
  if (callout) out.push("</div>");
  return out.join("");
}

// ---- transcript → chat items ----

// decodeEntries turns a transcript chunk into its JSON entries. A tail
// read usually starts mid-line; that fragment just fails to parse.
function decodeEntries(b64) {
  if (!b64) return [];
  const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  const out = [];
  for (const line of new TextDecoder().decode(bytes).split("\n")) {
    try { out.push(JSON.parse(line)); } catch {}
  }
  return out;
}

function parseTranscript(entries, opts = {}) {
  const items = [];
  const tools = new Map(); // tool_use id → item
  for (const e of entries) {
    // A message sent while Claude was working is recorded as a
    // queued_command attachment, not a user turn.
    // Only human-sent ones: background-task notifications use the same
    // attachment type (origin.kind "task-notification").
    if (e.type === "attachment" && e.attachment?.type === "queued_command" && (!e.isSidechain || opts.sidechain)) {
      if ((e.attachment.origin?.kind ?? "human") !== "human") continue;
      const q = e.attachment.prompt;
      const t = typeof q === "string" ? q : Array.isArray(q) ? q.map((x) => x.text || "").join("") : "";
      if (t.trim()) items.push({ kind: "user", text: t.trim(), queued: true });
      continue;
    }
    if ((e.type !== "user" && e.type !== "assistant") || !e.message || (e.isSidechain && !opts.sidechain)) continue;
    // Context compaction writes its summary as a user turn; show a note instead.
    if (e.isCompactSummary) { items.push({ kind: "note", text: "会話が長くなったため要約して続行しました" }); continue; }
    // Files Claude sent the operator (SendUserFile): shown as images /
    // download cards after the tool call.
    const att = e.toolUseResult?.attachments;
    if (e.type === "user" && Array.isArray(att) && att.length) {
      items.push({ kind: "files", caption: e.toolUseResult.caption || "", files: att.filter((a) => a?.path) });
    }
    const c = e.message.content;
    const parts = typeof c === "string" ? [{ type: "text", text: c }] : Array.isArray(c) ? c : [];
    for (const p of parts) {
      if (p.type === "text" && p.text?.trim()) {
        let t = p.text.trim();
        if (e.type === "user") {
          if ((e.origin?.kind ?? "human") !== "human") continue;
          if (e.isMeta || /^<(local-command|system-reminder|task-notification)/.test(t)) continue;
          const cmd = t.match(/<command-name>([^<]*)<\/command-name>/);
          if (cmd) {
            const args = t.match(/<command-args>([\s\S]*?)<\/command-args>/)?.[1]?.trim();
            items.push({ kind: "user", text: cmd[1] + (args ? " " + args : "") });
            continue;
          }
          if (/^\[Request interrupted/.test(t)) { items.push({ kind: "note", text: "中断しました" }); continue; }
          // Bash mode ("! cmd"): the command, then its output as its own entry.
          const bin = t.match(/^<bash-input>([\s\S]*)<\/bash-input>$/);
          if (bin) { items.push({ kind: "user", text: "! " + bin[1].trim() }); continue; }
          if (/^<bash-(stdout|stderr)>/.test(t)) {
            const tag = (k) => t.match(new RegExp(`<bash-${k}>([\\s\\S]*?)</bash-${k}>`))?.[1] || "";
            items.push({ kind: "bashout", stdout: tag("stdout").trimEnd(), stderr: tag("stderr").trimEnd() });
            continue;
          }
          items.push({ kind: "user", text: t });
        } else {
          items.push({ kind: "assistant", text: t });
        }
      } else if (p.type === "thinking" && p.thinking?.trim()) {
        items.push({ kind: "thinking", text: p.thinking.trim() });
      } else if (p.type === "tool_use") {
        const it = { kind: "tool", id: p.id, name: p.name, input: p.input || {}, result: null, error: false };
        tools.set(p.id, it);
        items.push(it);
      } else if (p.type === "tool_result") {
        const it = tools.get(p.tool_use_id);
        if (!it) continue;
        const rc = p.content;
        it.result = typeof rc === "string" ? rc : Array.isArray(rc) ? rc.map((x) => x.text || (x.type === "image" ? "[画像]" : "")).join("\n") : "";
        it.error = !!p.is_error;
        if (e.toolUseResult?.answers) it.answers = e.toolUseResult.answers;
      } else if (p.type === "image") {
        const src = p.source?.type === "base64" && /^image\/(png|jpeg|gif|webp)$/.test(p.source.media_type || "")
          ? `data:${p.source.media_type};base64,${p.source.data}` : null;
        items.push(src ? { kind: "image", role: e.type, src } : { kind: e.type === "user" ? "user" : "assistant", text: "[画像]" });
      }
    }
  }
  return items;
}

function toolArg(name, inp) {
  const v = inp.command || inp.file_path || inp.path || inp.pattern || inp.url || inp.query || inp.description || inp.prompt || "";
  return String(v).split("\n")[0].slice(0, 200);
}

// askCard renders an AskUserQuestion. Pending (no tool_result yet) and the
// session hosted by ccdash → an answerable form; the element is cached by
// tool id so the 1.5 s re-render doesn't wipe half-made choices. Answered →
// a read-only summary from toolUseResult.answers.
function askCard(it, ctx) {
  const qs = Array.isArray(it.input.questions) ? it.input.questions : [];
  if (it.result != null) {
    const ans = it.answers || {};
    return h("div", { class: "ask done" },
      h("div", { class: "ask-head" }, "質問に回答しました"),
      qs.map((q) => h("div", { class: "ask-q" }, h("div", { class: "muted small" }, q.question), h("div", {}, "→ " + (ans[q.question] ?? "（回答なし）")))));
  }
  if (ctx.askCards.has(it.id)) {
    const el = ctx.askCards.get(it.id);
    el.querySelector(".ask-submit").disabled = !ctx.hosted() || el.dataset.busy === "1";
    return el;
  }
  const state = qs.map(() => ({ picks: new Set(), other: "" }));
  const el = h("div", { class: "ask" }, h("div", { class: "ask-head" }, "Claude からの質問"));
  qs.forEach((q, qi) => {
    const multi = !!q.multiSelect;
    const opts = h("div", { class: "ask-opts" });
    const other = h("input", { placeholder: multi ? "" : "その他（自由入力）", class: "ask-other", hidden: multi });
    const paint = () => opts.querySelectorAll(".ask-opt").forEach((b, i) => {
      const on = state[qi].picks.has(i);
      b.classList.toggle("on", on);
      b.querySelector(".ask-mark").textContent = multi ? (on ? "☑" : "☐") : (on ? "●" : "○");
    });
    (q.options || []).forEach((o, oi) => {
      opts.append(h("button", { type: "button", class: "ask-opt", onclick: () => {
        const p = state[qi].picks;
        if (multi) p.has(oi) ? p.delete(oi) : p.add(oi);
        else { p.clear(); p.add(oi); other.value = ""; state[qi].other = ""; }
        paint();
      } }, h("span", { class: "ask-mark" }, multi ? "☐" : "○"), h("span", {}, h("b", {}, o.label), o.description && o.description !== o.label ? h("div", { class: "muted small" }, o.description) : null)));
    });
    other.addEventListener("input", () => { state[qi].other = other.value; if (other.value) { state[qi].picks.clear(); paint(); } });
    el.append(h("div", { class: "ask-q" },
      h("div", { class: "row" }, q.header ? h("span", { class: "chip" }, q.header) : null, multi ? h("span", { class: "muted small" }, "複数選択可") : null),
      h("div", { class: "ask-text" }, q.question), opts, other));
  });
  const submit = h("button", { type: "button", class: "btn primary ask-submit", disabled: !ctx.hosted(), onclick: async () => {
    const missing = state.findIndex((st) => !st.picks.size && !st.other.trim());
    if (missing >= 0) { toast(`「${qs[missing].header || qs[missing].question}」に回答してください`); return; }
    el.dataset.busy = "1";
    submit.disabled = true;
    submit.textContent = "送信中…";
    try {
      await ctx.answer(qs, state);
      submit.textContent = "回答しました";
    } catch (e) {
      toast(e.message);
      el.dataset.busy = "";
      submit.disabled = false;
      submit.textContent = "回答する";
    }
  } }, "回答する");
  el.append(h("div", { class: "row" }, submit, ctx.hosted() ? null : h("span", { class: "muted small" }, "このセッションは ccdash 上で動いていないため、ここからは回答できません")));
  ctx.askCards.set(it.id, el);
  return el;
}

// userText splits a user turn around Claude Code's paste wrappers
// (<pasted_content id="…"> … </pasted_content id="…">): typed text stays
// as is, each paste becomes a collapsed block.
const PASTE_RE = /<pasted_content id="[^"]*">\n?([\s\S]*?)\n?<\/pasted_content id="[^"]*">/g;
function userText(text) {
  const out = [];
  let at = 0;
  for (const m of text.matchAll(PASTE_RE)) {
    const before = text.slice(at, m.index).trim();
    if (before) out.push(h("div", {}, before));
    const body = m[1];
    const n = body.split("\n").length;
    out.push(h("details", { class: "paste" }, h("summary", {}, `📋 貼り付け（${n} 行）`), h("pre", {}, body)));
    at = m.index + m[0].length;
  }
  const rest = text.slice(at).trim();
  if (rest || !out.length) out.push(h("div", {}, rest));
  return out;
}

// ---- links to this portal ----

// followPortalLink keeps links to this hub (a session link from ccdash mcp,
// <hub>/s/<id>, or a /#/… route) in the current tab / installed app,
// moving to the chat in place instead of opening a new browser tab.
async function followPortalLink(e) {
  const a = e.target.closest?.("a[href]");
  if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  let u;
  try { u = new URL(a.href, location.href); } catch { return; }
  if (u.origin !== location.origin) return;
  const sm = u.pathname.match(/^\/s\/([^/]+)\/?$/);
  if (!sm && !(u.pathname === "/" && u.hash.startsWith("#/"))) return;
  e.preventDefault();
  if (!sm) { location.hash = u.hash; return; }
  const sid = decodeURIComponent(sm[1]);
  try {
    const r = await api(`/api/locate/${encodeURIComponent(sid)}`);
    location.hash = `#/d/${r.device_id}/s/${encodeURIComponent(sid)}`;
  } catch {
    toast("このセッションを持つ端末が見つかりません（オフラインかもしれません）", 4000);
  }
}

// ---- document preview (Markdown / HTML reports Claude wrote or sent) ----

// previewKind: "md" | "html" | "txt" for a previewable path, else "".
function previewKind(path) {
  const ext = (path.match(/\.([a-z0-9]+)$/i)?.[1] || "").toLowerCase();
  return { md: "md", markdown: "md", html: "html", htm: "html", txt: "txt" }[ext] || "";
}

// loadScript adds a vendored script once (marked / DOMPurify load only
// when a preview is opened).
const scriptLoads = new Map();
function loadScript(src) {
  if (!scriptLoads.has(src)) scriptLoads.set(src, new Promise((res, rej) => document.head.append(h("script", { src, onload: res, onerror: () => rej(new Error("読み込めませんでした: " + src)) }))));
  return scriptLoads.get(src);
}

// previewFile opens a document: Markdown rendered (marked + DOMPurify),
// an HTML report in a sandboxed frame (scripts run in an opaque origin — no
// access to the portal), plain text as is. The file comes from the device's
// /hub/file (transcript-recorded paths only). It opens either as a large
// viewer or (desktop) in a sidebar beside the chat; the bar's button
// switches, and the choice is remembered.
const previewModeKey = "ccdash.previewMode";
function previewMode() {
  if (!matchMedia("(min-width: 768px)").matches) return "modal";
  try { return localStorage.getItem(previewModeKey) === "side" ? "side" : "modal"; } catch { return "modal"; }
}
async function previewFile(url, path, mode = previewMode()) {
  const kind = previewKind(path), name = path.split("/").pop();
  const content = h("div", { class: "viewer-body" }, h("div", { class: "muted" }, "読み込み中…"));
  let close;
  const swap = h("button", { type: "button", class: "btn small icon hide-sm", title: mode === "side" ? "大きく開く" : "サイドバーで開く", "aria-label": mode === "side" ? "大きく開く" : "サイドバーで開く",
    onclick: () => { const next = mode === "side" ? "modal" : "side"; try { localStorage.setItem(previewModeKey, next); } catch {} close(); previewFile(url, path, next); } },
    icon(mode === "side" ? "expand" : "sidebar"));
  const bar = h("div", { class: "viewer-bar" },
    h("button", { type: "button", class: "btn small icon", "aria-label": "閉じる", title: "閉じる", onclick: () => close() }, icon("x")),
    h("div", { class: "grow viewer-title" }, h("b", {}, name), h("span", { class: "muted small mono" }, path)),
    swap,
    h("a", { class: "btn small", href: url, download: name }, icon("download"), h("span", { class: "hide-sm" }, "ダウンロード")));
  if (mode === "side") {
    document.querySelector("#side")?.remove();
    const side = h("aside", { id: "side", class: "viewer side" }, bar, content);
    close = () => { side.remove(); document.body.classList.remove("side-open"); };
    document.body.append(side);
    document.body.classList.add("side-open");
  } else {
    const dlg = h("dialog", { class: "viewer" }, bar, content);
    close = () => dlg.close();
    dlg.addEventListener("close", () => dlg.remove());
    dlg.addEventListener("click", (e) => { if (e.target === dlg) dlg.close(); });
    document.body.append(dlg);
    dlg.showModal();
  }
  try {
    const r = await fetch(url, { credentials: "same-origin" });
    if (!r.ok) throw new Error(r.status === 404 ? "ファイルが見つかりません（端末から消えたか、移動されました）" : (await r.text()).trim() || r.statusText);
    const text = await r.text();
    if (kind === "html") {
      content.replaceChildren(h("iframe", { class: "viewer-frame", sandbox: "allow-scripts allow-popups", srcdoc: text, title: name }));
    } else if (kind === "md") {
      await Promise.all([loadScript("vendor/marked@12.0.2/marked.min.js"), loadScript("vendor/dompurify@3.4.16/purify.min.js")]);
      const doc = h("article", { class: "doc" });
      doc.innerHTML = DOMPurify.sanitize(marked.parse(text, { gfm: true, breaks: false }));
      for (const a of doc.querySelectorAll("a[href]")) { a.target = "_blank"; a.rel = "noopener noreferrer"; }
      content.replaceChildren(doc);
    } else {
      content.replaceChildren(h("pre", { class: "doc-text" }, text));
    }
  } catch (e) {
    content.replaceChildren(h("div", { class: "empty" }, e.message));
  }
}

// fmtSize: 624633 → "610 KB".
function fmtSize(n) {
  if (!n && n !== 0) return "";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(n < 10 ? 1 : 0) : n) + " " + u[i];
}

// ---- "! command" lines in Claude's replies ----

// bangCmd returns the command of a one-line "! command" (Claude Code's
// bash mode, the way Claude asks you to run something), or "". Prose
// (prose = true) needs the space after "!", so "!important" stays text.
function bangCmd(line, prose) {
  const m = String(line).trim().match(prose ? /^!\s+([^\s!=].*)$/ : /^!\s*([^\s!=].*)$/);
  return m && !m[1].includes("\n") ? m[1].trim() : "";
}

// bangLinks puts a "コマンドを実行する" button next to each "! command" in a
// rendered reply — a plain line, a list item, an inline `! cmd` or a line
// of a code block — and hands the command to run (which asks before
// sending anything). The command text itself stays plain text.
function bangLinks(el, run) {
  const btn = (cmd, label) => h("button", { class: "btn small bang-btn", type: "button", title: "! " + cmd,
    onclick: (e) => { e.preventDefault(); e.stopPropagation(); run(cmd); } }, icon("terminal"), label || "コマンドを実行する");
  const done = new Set();
  for (const c of el.querySelectorAll("code")) {
    if (c.closest("pre")) continue;
    const cmd = bangCmd(c.textContent);
    if (!cmd) continue;
    c.after(" ", btn(cmd));
    let top = c; // the line it sits in: its list item, or the bubble's child
    while (top.parentElement && top.parentElement !== el && top.tagName !== "LI") top = top.parentElement;
    done.add(top);
  }
  for (const n of el.querySelectorAll(":scope > div, li")) {
    if (done.has(n)) continue;
    const cmd = bangCmd(n.textContent, true);
    if (cmd) n.append(" ", btn(cmd));
  }
  for (const pre of el.querySelectorAll("pre.code")) {
    const cmds = pre.textContent.split("\n").map((l) => bangCmd(l)).filter(Boolean);
    if (!cmds.length) continue;
    pre.after(h("div", { class: "bang-row" }, ...cmds.map((cmd) =>
      btn(cmd, cmds.length > 1 ? "実行: " + (cmd.length > 40 ? cmd.slice(0, 40) + "…" : cmd) : ""))));
  }
}

// confirmBang shows the command and sends it only on an explicit yes.
function confirmBang(cmd, send) {
  dialog((body, close) => {
    const yes = h("button", { class: "btn primary", type: "button" }, "はい（実行する）");
    yes.addEventListener("click", async () => {
      yes.disabled = true;
      try { await send(cmd); close(); toast("コマンドを送りました"); }
      catch (e) { toast(e.message); yes.disabled = false; }
    });
    body.append(h("h1", {}, "このコマンドを実行しますか？"),
      h("div", { class: "muted small" }, "このセッションの claude にシェルコマンド（! モード）として送ります。端末上でそのまま実行されます。"),
      h("pre", { class: "mono bang-cmd" }, "! " + cmd),
      h("div", { class: "actions" },
        h("button", { class: "btn", value: "cancel", formnovalidate: true }, "いいえ"),
        yes));
  });
}

function renderItem(it, ctx) {
  const el = renderItemRaw(it, ctx);
  return el ? linkify(el) : el;
}

function renderItemRaw(it, ctx) {
  if (it.kind === "tool" && it.name === "AskUserQuestion" && ctx) return askCard(it, ctx);
  switch (it.kind) {
    case "user":
      return h("div", { class: "bubble user" + (it.queued ? " queued" : ""), title: it.queued ? "作業中に送信" : null }, ...userText(it.text));
    case "image":
      return h("a", { class: "chat-img " + it.role, href: it.src, target: "_blank", rel: "noopener" }, h("img", { src: it.src, alt: "画像", loading: "lazy" }));
    case "assistant": {
      const el = h("div", { class: "bubble assistant" });
      el.innerHTML = md(it.text);
      if (ctx?.runBang) bangLinks(el, ctx.runBang);
      return el;
    }
    case "files": {
      const url = ctx?.fileURL;
      const kids = it.files.map((f) => {
        const name = f.path.split("/").pop();
        const src = url ? url(f.path) : null;
        if (f.isImage && src) return h("a", { class: "sent-img", href: src, target: "_blank", rel: "noopener" }, h("img", { src, alt: name, loading: "lazy" }));
        if (src && previewKind(f.path)) return h("button", { type: "button", class: "sent-file", onclick: () => previewFile(src, f.path) },
          icon("file"), h("span", { class: "grow" }, h("b", {}, name), h("span", { class: "muted small" }, fmtSize(f.size))), icon("eye"));
        return h(src ? "a" : "div", { class: "sent-file", href: src, download: src ? name : null },
          icon("file"), h("span", { class: "grow" }, h("b", {}, name), h("span", { class: "muted small" }, fmtSize(f.size))),
          src ? icon("download") : null);
      });
      return h("div", { class: "sent" }, it.caption ? h("div", { class: "muted small" }, it.caption) : null, ...kids);
    }
    case "bashout": {
      const out = [it.stdout, it.stderr].filter(Boolean).join("\n");
      const lines = out ? out.split("\n").length : 0;
      return h("details", { class: "tool bashout", open: lines > 0 && lines <= 12 },
        h("summary", {}, it.stderr && !it.stdout ? "✗ " : "$ ", "シェルの出力", lines ? `（${lines} 行）` : "（出力なし）"),
        out ? h("pre", { class: it.stderr && !it.stdout ? "err" : "" }, out.length > 6000 ? out.slice(0, 6000) + "\n…" : out) : null);
    }
    case "thinking":
      return h("details", { class: "thinking" }, h("summary", {}, "思考"), h("div", {}, it.text));
    case "note":
      return h("div", { class: "note" }, it.text);
    case "tool": {
      const state = it.result == null ? "…" : it.error ? "✗" : "✓";
      const body = [];
      if (it.name === "Edit" && it.input.old_string != null) {
        body.push(h("pre", { class: "diff" }, ...String(it.input.old_string).split("\n").map((l) => h("div", { class: "del" }, "- " + l)),
          ...String(it.input.new_string ?? "").split("\n").map((l) => h("div", { class: "add" }, "+ " + l))));
      } else if (it.name === "TodoWrite" && Array.isArray(it.input.todos)) {
        body.push(h("div", {}, it.input.todos.map((t) => h("div", {}, (t.status === "completed" ? "☑ " : t.status === "in_progress" ? "▶ " : "☐ ") + (t.content || "")))));
      } else {
        body.push(h("pre", {}, JSON.stringify(it.input, null, 2).slice(0, 4000)));
      }
      if (it.result != null) body.push(h("pre", { class: it.error ? "err" : "" }, it.result.length > 6000 ? it.result.slice(0, 6000) + "\n…" : it.result));
      const agent = (it.name === "Agent" || it.name === "Task") && ctx?.agentFor?.(it.id);
      if (agent) body.unshift(h("button", { type: "button", class: "btn small", onclick: (e) => { e.preventDefault(); ctx.openAgent(agent); } }, "サブエージェントの記録を見る"));
      // A document Claude wrote (Markdown / HTML report): preview it.
      const fp = it.input?.file_path;
      const doc = /^(Write|Edit|MultiEdit)$/.test(it.name) && fp && previewKind(fp) && ctx?.fileURL?.(fp);
      return h("details", { class: "tool" + (it.error ? " error" : "") },
        h("summary", {}, h("span", { class: "tstate" }, state), h("b", {}, it.name), " ", h("span", { class: "targ" }, toolArg(it.name, it.input)),
          doc ? h("button", { type: "button", class: "btn small preview-btn", onclick: (e) => { e.preventDefault(); e.stopPropagation(); previewFile(doc, fp); } }, icon("eye"), "プレビュー") : null),
        ...body);
    }
  }
  return null;
}

// ---- TUI prompt detection (menus / permission dialogs on the screen) ----

// screenAsk parses Claude Code's AskUserQuestion dialog off the screen.
// The question isn't in the transcript until it's answered, so the screen
// is the only source while it's pending. Layout (one question at a time):
//   ←  ☐ 果物  ☐ 色  ✔ Submit  →      (tab bar; a lone "☐ 季節" for one question)
//   好きな果物はどれですか？
//   ❯ 1. りんご                       (multi-select: "1. [✔] 赤")
//        description
//     4. Type something.
//   ─────
//     5. Chat about this
//   Enter to select · …
function screenAsk(rows) {
  const footer = rows.findIndex((r) => /Enter to select/.test(r));
  if (footer < 0 || !rows.some((r) => /Chat about this/.test(r))) return null;
  let tabRow = -1;
  for (let i = footer; i >= 0; i--) if (/[☐☒]\s*\S/.test(rows[i])) { tabRow = i; break; }
  if (tabRow < 0) return null;
  const tabs = [...rows[tabRow].matchAll(/([☐☒])\s+([^☐☒✔→←]+?)(?=\s{2,}|\s*[☐☒✔→]|$)/g)].map((m) => ({ done: m[1] === "☒", name: m[2].trim() }));
  const opt = /^\s*(❯)?\s*(\d{1,2})\.\s+(?:\[([ ✔✓x])\]\s+)?(.*?)\s*$/;
  const options = [];
  const question = [];
  let multi = false;
  for (let i = tabRow + 1; i < footer; i++) {
    const r = rows[i];
    if (/[─━]{3}/.test(r)) continue;
    const m = r.match(opt);
    if (m) {
      if (/^Chat about this/.test(m[4])) continue;
      if (m[3] !== undefined) multi = true;
      options.push({ n: m[2], label: m[4], checked: m[3] !== undefined && m[3] !== " ", cursor: !!m[1], desc: [] });
    } else if (options.length) {
      const t = r.trim();
      if (t && t !== "Submit") options[options.length - 1].desc.push(t);
    } else if (r.trim()) {
      question.push(r.trim());
    }
  }
  if (!options.length) return null;
  const last = options[options.length - 1];
  const other = /^Type something/.test(last.label) || (last.cursor && !last.desc.length) ? last : null;
  return { kind: "ask", tabs, question: question.join(" "), options, multi, other, key: rows.slice(tabRow, footer).join("\n") };
}

function screenPrompt(rows) {
  const ask = screenAsk(rows);
  if (ask) return ask;
  const clean = (r) => r.replace(/^\s*[│|]/, " ").replace(/[│|]\s*$/, "");
  const lines = rows.map(clean);
  const opt = /^\s*(?:❯\s*)?(\d{1,2})[.)]\s+(.+?)\s*$/;
  // Only the bottom-most "❯" row counts: higher ones are past prompts in
  // the scrollback, and an empty one is claude's input box (no dialog).
  let sel = -1;
  for (let i = lines.length - 1; i >= 0; i--) if (/^\s*❯/.test(lines[i])) { sel = i; break; }
  if (sel < 0 || !/^\s*❯\s*\S/.test(lines[sel])) return null;
  let start = sel, end = sel, options = [];
  if (opt.test(lines[sel])) {
    // Numbered menu (permission dialogs, plan approval, AskUserQuestion…):
    // a digit selects and confirms.
    // Options may carry indented description lines (AskUserQuestion);
    // skip those, stop at anything else (a rule, the footer hint…).
    const numCol = lines[sel].search(/\d/);
    const desc = (l) => !opt.test(l) && l.trim() !== "" && l.search(/\S/) > numCol && !/[─━]{3}/.test(l);
    for (let i = sel - 1; i >= 0 && (opt.test(lines[i]) || desc(lines[i])); i--) if (opt.test(lines[i])) start = i;
    for (let i = sel + 1; i < lines.length && (opt.test(lines[i]) || desc(lines[i])); i++) if (opt.test(lines[i])) end = i;
    for (let i = start; i <= end; i++) {
      const m = lines[i].match(opt);
      if (m) options.push({ label: `${m[1]}. ${m[2]}`, keys: m[1] });
    }
  } else {
    // Unnumbered list (e.g. the theme picker): siblings sit in the column
    // after the cursor glyph. A lone "❯" line is the input prompt, not a menu.
    const col = lines[sel].indexOf("❯");
    const sib = (l) => l.length > col + 2 && l.slice(0, col + 1).trim() === "" && l.slice(col + 1).trim() !== "";
    while (start > 0 && sib(lines[start - 1])) start--;
    while (end + 1 < lines.length && sib(lines[end + 1])) end++;
    if (end - start < 1) return null;
    options = [{ label: "↑", keys: "\x1b[A" }, { label: "↓", keys: "\x1b[B" }, { label: "決定 (Enter)", keys: "\r" }];
  }
  let top = Math.max(0, start - 12);
  for (let i = start - 1; i >= top; i--) if (/^\s*[╭┌]/.test(rows[i])) { top = i; break; }
  const bottom = Math.min(rows.length - 1, end + 3);
  const context = rows.slice(top, bottom + 1).join("\n").replace(/\s+$/, "");
  const title = /Ready to submit your answers|Review your answers/.test(context) ? "回答の確認"
    : /Do you want to (proceed|make this edit|create|allow)|Allow .* to/i.test(context) ? "許可の確認"
    : /Would you like to proceed/i.test(context) ? "プランの確認"
    : "ターミナルで選択が必要です";
  return { options, context, title, key: context };
}

// prepareImage keeps small PNG/JPEG/GIF/WebP as-is and re-encodes
// anything big or exotic (HEIC from a phone camera, 12 MP photos) to a
// JPEG no larger than 2048 px — plenty for Claude, cheap over the tunnel.
async function prepareImage(file) {
  const ok = ["image/png", "image/jpeg", "image/gif", "image/webp"].includes(file.type);
  if (ok && file.size <= 3 * 1024 * 1024) return file;
  const bmp = await createImageBitmap(file);
  const scale = Math.min(1, 2048 / Math.max(bmp.width, bmp.height));
  const c = document.createElement("canvas");
  c.width = Math.round(bmp.width * scale);
  c.height = Math.round(bmp.height * scale);
  c.getContext("2d").drawImage(bmp, 0, 0, c.width, c.height);
  bmp.close?.();
  return await new Promise((res, rej) => c.toBlob((b) => (b ? res(b) : rej(new Error("encode failed"))), "image/jpeg", 0.85));
}

// askScreenCard renders the on-screen AskUserQuestion as a form. Every
// click sends the key a person would press, and the card re-renders from
// the next screen read (a single-select pick advances to the next question
// by itself; multi-select toggles, then "次へ" moves on).
function askScreenCard(a, keys, d, ptyKey) {
  const otherInput = h("input", { placeholder: "その他（自由入力）", class: "ask-other grow" });
  const sendOther = async () => {
    const t = otherInput.value.trim();
    if (!t) return;
    await keys(a.other.n);
    await d.post(`/pty/${encodeURIComponent(ptyKey)}/input`, { text: t, paste: true });
    await new Promise((r) => setTimeout(r, 300));
    await keys("\r");
  };
  otherInput.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.isComposing && e.keyCode !== 229) { e.preventDefault(); sendOther(); } });
  const choices = a.options.filter((o) => o !== a.other);
  return h("div", { class: "ask" },
    h("div", { class: "row" },
      h("span", { class: "ask-head grow" }, "Claude からの質問"),
      h("button", { class: "btn small", title: "質問をキャンセル (Esc)", onclick: () => keys("\x1b") }, "キャンセル")),
    a.tabs.length > 1 ? h("div", { class: "ask-tabs" }, a.tabs.map((t) => h("span", { class: "chip" + (t.done ? " idle" : "") }, (t.done ? "✓ " : "") + t.name))) : null,
    h("div", { class: "ask-text" }, a.question, a.multi && !/複数/.test(a.question) ? h("span", { class: "muted small" }, "（複数選択可）") : null),
    h("div", { class: "ask-opts" }, choices.map((o) => h("button", {
      type: "button", class: "ask-opt" + (o.checked ? " on" : ""), onclick: () => keys(o.n),
    }, h("span", { class: "ask-mark" }, a.multi ? (o.checked ? "☑" : "☐") : o.n + "."),
      h("span", {}, h("b", {}, o.label), o.desc.length && o.desc.join(" ") !== o.label ? h("div", { class: "muted small" }, o.desc.join(" ")) : null)))),
    a.other && !a.multi ? h("div", { class: "row" }, otherInput, h("button", { type: "button", class: "btn", onclick: sendOther }, "送信")) : null,
    a.multi ? h("div", { class: "row" }, h("button", { type: "button", class: "btn primary", onclick: () => keys("\x1b[C") }, "次へ →")) : null);
}

// sessionMenu: the TUI's per-session keys (t rename, T group, p project,
// P project color, f pin, x archive, ctrl+t title) as one dialog.
function sessionMenu(id, s, groups, refresh) {
  const sid = s.session_id;
  const d = dev(id);
  const post = async (path, json, done) => {
    try { await d.post(`/api/sessions/${encodeURIComponent(sid)}${path}`, json); toast(done); refresh(); }
    catch (e) { toast(e.message); }
  };
  dialog((body, close) => {
    const title = h("input", { class: "grow", value: s.custom_title || "", placeholder: s.gen_title || s.title || "タイトル" });
    const group = h("input", { class: "grow", value: s.user_group || "", placeholder: s.repo || base(s.cwd) || "グループ", list: "group-names" });
    const projColor = projectColorOf(s.project, s.project_color);
    const postProjectColor = async (color, done) => {
      close();
      try { await d.post("/api/projects/color", { project: s.project, color }); toast(done); refresh(); } catch (e) { toast(e.message); }
    };
    body.append(
      h("h1", {}, (s.num ? `#${s.num} ` : "") + sessionTitle(s)),
      h("datalist", { id: "group-names" }, groups.map((g) => h("option", { value: g }))),
      h("label", {}, "タイトル（空にすると自動のものに戻す）"),
      h("div", { class: "row" }, title, h("button", { type: "button", class: "btn", onclick: () => { close(); post("/title", { title: title.value.trim() }, "タイトルを保存しました"); } }, "保存")),
      h("label", {}, "色（一覧・グリッドの縦線。TUI の c / C と共通）"),
      h("div", { class: "swatches" },
        SESSION_PALETTE.map((c) => h("button", {
          type: "button", class: "swatch" + (sessionColorOf(s) === c ? " on" : ""), style: `--c:${c}`, title: c,
          onclick: () => { close(); post("/color", { color: c }, "色を変更しました"); },
        })),
        h("button", { type: "button", class: "btn small", title: "いま動いているセッションの色と被らない（似ていない）色を引き直す", onclick: () => {
          close();
          post("/color", { color: "random" }, "色を引き直しました");
        } }, icon("shuffle"), "引き直す"),
        h("button", { type: "button", class: "btn small", disabled: !s.color, onclick: () => { close(); post("/color", { color: "" }, "色を自動に戻しました"); } }, "自動に戻す")),
      h("label", {}, "プロジェクト（複数のセッションをまとめて一覧の上に表示）"),
      h("div", { class: "row" },
        s.project ? h("span", { class: "proj-chip static", style: `--proj:${projColor};--ink:${inkOn(projColor)}` }, s.project) : h("span", { class: "muted small grow" }, "なし"),
        h("span", { class: "grow" }),
        h("button", { type: "button", class: "btn", onclick: () => {
          close();
          projectPicker(id, { current: s.project || "" }, (v) => post("/project", { project: v }, v ? `プロジェクト「${v}」に入れました` : "プロジェクトから外しました"));
        } }, s.project ? "変更…" : "プロジェクトに入れる…")),
      s.project ? h("label", {}, `プロジェクト「${s.project}」の色（TUI の P と共通）`) : null,
      s.project ? h("div", { class: "swatches" },
        SESSION_PALETTE.map((c) => h("button", {
          type: "button", class: "swatch" + (projColor === c ? " on" : ""), style: `--c:${c}`, title: c,
          onclick: () => postProjectColor(c, "プロジェクトの色を変更しました"),
        })),
        h("button", { type: "button", class: "btn small", title: "ほかのプロジェクトと被らない色を引き直す", onclick: () => postProjectColor("random", "プロジェクトの色を引き直しました") }, icon("shuffle"), "引き直す")) : null,
      h("label", {}, "グループ（空にすると repo 名に戻す）"),
      h("div", { class: "row" }, group, h("button", { type: "button", class: "btn", onclick: () => { close(); post("/group", { group: group.value.trim() }, "グループを保存しました"); } }, "保存")),
      h("div", { class: "menu-grid" },
        h("button", { type: "button", class: "btn", onclick: () => { close(); post("/favorite", { favorite: !s.favorite }, s.favorite ? "ピン留めを外しました" : "ピン留めしました"); } }, icon("pin"), s.favorite ? "ピン留めを外す" : "ピン留め"),
        h("button", { type: "button", class: "btn", onclick: () => { close(); post("/later", { later: !s.later }, s.later ? "「あとで見る」から外しました" : "「あとで見る」に入れました"); } }, icon("clock"), s.later ? "あとで見るを外す" : "あとで見る"),
        h("button", { type: "button", class: "btn", onclick: () => { close(); post("/archive", { archived: !s.archived }, s.archived ? "アーカイブを解除しました" : "アーカイブしました"); } }, s.archived ? "アーカイブを解除" : "アーカイブ"),
        h("button", { type: "button", class: "btn", onclick: async () => {
          close();
          try { await d.post("/api/titles", { sessionIds: [sid] }); toast("タイトルを生成しています（claude -p）"); refresh(); } catch (e) { toast(e.message); }
        } }, "タイトルを自動生成")),
      h("div", { class: "actions" }, h("button", { class: "btn primary" }, "閉じる")));
  });
}

// ---- grid view ----

// The grid shows an operator-chosen list of "device:session" keys, kept
// per browser. null = never chosen: the grid seeds it with what's running.
// Session lists mark entries onto it directly.
// The grid's sessions are the devices' session rows (grid / grid_at): one
// list shared by every browser and the TUI's grid (V). A change shows at
// once through a local overlay until the next poll reports it.
const gridList = {
  overlay: new Map(), // "device:session" → { on, at }
  is(key, s) { const o = this.overlay.get(key); return o ? o.on : !!s?.grid; },
  at(key, s) { const o = this.overlay.get(key); return o?.on ? o.at : (s?.grid_at ? Date.parse(s.grid_at) : 0); },
  put(key, on) {
    const i = key.indexOf(":"), d = key.slice(0, i), sid = key.slice(i + 1);
    this.overlay.set(key, { on, at: Date.now() });
    return dev(d).post(`/api/sessions/${encodeURIComponent(sid)}/grid`, { grid: on })
      .catch((e) => { this.overlay.delete(key); toast("グリッドを更新できませんでした: " + e.message, 4000); });
  },
  // settle drops overlay entries the devices now report.
  settle(all) {
    for (const c of all) {
      const k = c.device_id + ":" + c.session.session_id, o = this.overlay.get(k);
      if (o && o.on === !!c.session.grid) this.overlay.delete(k);
    }
  },
  // takeLegacy hands over (once) the per-browser list kept before the grid
  // moved onto the devices.
  takeLegacy() {
    try {
      const v = JSON.parse(localStorage.getItem("ccdash.gridItems"));
      localStorage.removeItem("ccdash.gridItems");
      return Array.isArray(v) ? v : [];
    } catch { return []; }
  },
};
//
// Every running session across devices on one screen: a tile each with its
// state, why it wants you, the latest exchange (live), and a one-line reply.

// gridTile is a chatView in compact mode (see chatView): the tile and the
// chat page are one implementation.
function gridTile(c) {
  const v = chatView(c.device_id, { sid: c.session.session_id, compact: true, deviceName: c.device_name });
  return { el: v.el, update() {}, refresh: async () => {}, scrollLog: v.scrollBottom, destroy: v.destroy };
}

// toolLabel: "Bash npm test" — a tool call in one short line.
function toolLabel(it) {
  const i = it.input || {};
  const arg = i.command || i.description || i.file_path || i.pattern || i.url || i.query || "";
  return (it.name + " " + String(arg).split("\n")[0]).trim().slice(0, 80);
}

function gridState(s) {
  if (waitDeclared(s) && (s.attention === "needs_you" || !WAIT_LABEL_NEEDS[s.wait])) return WAIT_LABEL[s.wait];
  return s.attention === "needs_you" ? "要対応" : s.status === "active" ? "作業中" : s.attention === "done" ? "未確認"
    : s.status === "idle" ? "待機中" : "停止";
}

// layoutGrid picks the column count that makes tiles largest for this many
// tiles in this much space, then stretches them to fill it. Tiles shrink as
// the count grows; below the minimum size the grid scrolls instead.
function layoutGrid(grid, n) {
  if (!n) return;
  for (const t of grid.children) t.style.gridColumn = "";
  if (grid.classList.contains("free")) { grid.style.gridTemplateColumns = ""; grid.style.gridAutoRows = ""; return; }
  if (grid.classList.contains("has-max")) {
    grid.style.gridTemplateColumns = "minmax(0, 1fr)";
    grid.style.gridAutoRows = `${Math.max(200, grid.clientHeight)}px`;
    return;
  }
  const gap = 10, minW = 300, minH = 200;
  const W = grid.clientWidth, H = grid.clientHeight;
  if (W < 700) { // phone: one column of comfortable tiles
    grid.style.gridTemplateColumns = "1fr";
    grid.style.gridAutoRows = "320px";
    return;
  }
  let best = { cols: 1, area: -1, h: minH };
  for (let cols = 1; cols <= n; cols++) {
    const rows = Math.ceil(n / cols);
    const w = (W - gap * (cols - 1)) / cols;
    if (w < minW && cols > 1) break;
    const h = Math.max(minH, (H - gap * (rows - 1)) / rows);
    // Usable area of one tile (a sliver narrower than 0.6:1 or wider than
    // 2.5:1 doesn't count beyond that shape), discounted for empty cells.
    // A layout that has to scroll only gets credit for what's on screen.
    const visible = Math.min(1, H / (rows * h + gap * (rows - 1)));
    const area = Math.min(w, h * 2.5) * Math.min(h, w / 0.6) * Math.sqrt(n / (cols * rows)) * visible;
    if (area > best.area) best = { cols, area, h };
  }
  // A part-filled last row stretches across: with C columns and r tiles
  // left over, the grid has C·r tracks — full rows' tiles span r, the last
  // row's span C — so 5 tiles in 3 columns end with two halves, not a gap.
  const C = best.cols, rem = n % C;
  if (rem && n > C) {
    grid.style.gridTemplateColumns = `repeat(${C * rem}, minmax(0, 1fr))`;
    [...grid.children].forEach((t, i) => { t.style.gridColumn = `span ${i >= n - rem ? C : rem}`; });
  } else {
    grid.style.gridTemplateColumns = `repeat(${C}, minmax(0, 1fr))`;
  }
  grid.style.gridAutoRows = `${Math.floor(best.h)}px`;
}

function gridPage() {
  const mount = mainMount;
  const tiles = new Map();
  crumbs({ text: "グリッド" });
  // Which sessions are on the grid is the operator's choice: × takes a tile
  // off, ＋ places any session that isn't on it yet. Kept per browser as
  // "device:session" keys in grid_at order, kept on the devices' session
  // rows (gridList) so the TUI's grid shows the same. With none chosen the
  // grid is automatic: whatever wants you or is working (same rule as the
  // TUI's).
  let items = [], auto = false, legacyDone = false;
  let all = []; // every session the hub knows, from the last poll
  const keyOf = (c) => c.device_id + ":" + c.session.session_id;
  const busy = (s) => !!s.attention || s.status === "active" || s.unseen || s.pending_count > 0;
  const computeItems = () => {
    const marked = all.filter((c) => gridList.is(keyOf(c), c.session))
      .sort((a, b) => gridList.at(keyOf(a), a.session) - gridList.at(keyOf(b), b.session) || (a.session.num || 0) - (b.session.num || 0));
    auto = !marked.length;
    items = (auto ? all.filter((c) => busy(c.session)) : marked).map(keyOf);
  };
  // pinAuto turns the automatic set into chosen tiles (minus except) — what
  // closing or adding a tile means while the grid is automatic.
  const pinAuto = (except) => { if (auto) items.filter((k) => k !== except).forEach((k) => gridList.put(k, true)); };
  const grid = h("div", { class: "grid" });
  const relayout = () => { layoutGrid(grid, grid.childElementCount); if (free) placeFree(); };
  // Free placement: tiles become windows you drag by the header and resize
  // from the corner. Rects are kept as fractions of the grid area (so they
  // follow window resizes) in localStorage; a tile without one starts where
  // the auto layout would put it.
  const store = { get() { try { return JSON.parse(localStorage.getItem("ccdash.gridRects") || "{}"); } catch { return {}; } },
    set(v) { try { localStorage.setItem("ccdash.gridRects", JSON.stringify(v)); } catch {} } };
  let free = false;
  try { free = localStorage.getItem("ccdash.gridMode") === "free"; } catch {}
  let rects = store.get(), zTop = 10;
  const autoRect = (i, n) => {
    // Where the auto layout would put tile i, as fractions.
    const W = grid.clientWidth || 1, H = grid.clientHeight || 1;
    const cols = Math.max(1, Math.round(Math.sqrt(n * (W / H) / 1.4))) , rows = Math.ceil(n / cols);
    const c = i % cols, r = Math.floor(i / cols);
    return { x: c / cols, y: r / rows, w: 1 / cols, h: 1 / rows };
  };
  const placeFree = () => {
    const W = grid.clientWidth, H = grid.clientHeight, n = tiles.size;
    let i = 0;
    for (const [k, t] of tiles) {
      const r = rects[k] || autoRect(i, n);
      Object.assign(t.el.style, { left: r.x * W + "px", top: r.y * H + "px", width: Math.max(220, r.w * W - 8) + "px", height: Math.max(160, r.h * H - 8) + "px" });
      i++;
    }
  };
  const setMode = (m) => {
    free = m;
    try { localStorage.setItem("ccdash.gridMode", m ? "free" : "auto"); } catch {}
    grid.classList.toggle("free", m);
    for (const [, t] of tiles) if (!m) Object.assign(t.el.style, { left: "", top: "", width: "", height: "", zIndex: "" });
    modeAuto.classList.toggle("on", !m);
    modeFree.classList.toggle("on", m);
    resetBtn.hidden = !m;
    relayout();
  };
  const modeAuto = h("button", { class: "btn small", onclick: () => setMode(false) }, "整列");
  const modeFree = h("button", { class: "btn small", onclick: () => setMode(true) }, "自由配置");
  const resetBtn = h("button", { class: "btn small", title: "全部を整列の位置に戻す", onclick: () => { rects = {}; store.set(rects); relayout(); } }, "並べ直す");
  const keyOfEl = (el) => [...tiles].find(([, t]) => t.el === el)?.[0];
  const saveRect = (el) => {
    const k = keyOfEl(el);
    if (!k) return;
    const W = grid.clientWidth, H = grid.clientHeight;
    rects[k] = { x: el.offsetLeft / W, y: el.offsetTop / H, w: (el.offsetWidth + 8) / W, h: (el.offsetHeight + 8) / H };
    store.set(rects);
  };
  grid.addEventListener("pointerdown", (e) => {
    if (!free) return;
    const el = e.target.closest(".tile");
    if (!el || el.classList.contains("max")) return;
    el.style.zIndex = String(++zTop);
    const resizing = e.target.closest(".tile-resize");
    const dragging = !resizing && e.target.closest(".tile-head") && !e.target.closest("a, button, input");
    if (!resizing && !dragging) return;
    e.preventDefault();
    const sx = e.clientX, sy = e.clientY;
    const ox = el.offsetLeft, oy = el.offsetTop, ow = el.offsetWidth, oh = el.offsetHeight;
    const W = grid.clientWidth, H = grid.scrollHeight;
    grid.classList.add("moving");
    const move = (ev) => {
      const dx = ev.clientX - sx, dy = ev.clientY - sy;
      if (resizing) {
        el.style.width = Math.min(W - ox, Math.max(220, ow + dx)) + "px";
        el.style.height = Math.max(160, oh + dy) + "px";
      } else {
        el.style.left = Math.min(Math.max(0, ox + dx), Math.max(0, W - ow)) + "px";
        el.style.top = Math.min(Math.max(0, oy + dy), Math.max(0, H - 40)) + "px";
      }
    };
    const up = () => {
      grid.classList.remove("moving");
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      saveRect(el);
    };
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
  });
  // Double-clicking a tile's title maximizes it over the grid; again (or
  // Esc) restores. The choice survives the 4 s refresh.
  let maxKey = null;
  const applyMax = () => {
    let found = false;
    for (const [k, t] of tiles) {
      const on = k === maxKey;
      t.el.classList.toggle("max", on);
      found ||= on;
    }
    if (!found) maxKey = null;
    grid.classList.toggle("has-max", !!maxKey);
    relayout();
    if (maxKey) tiles.get(maxKey)?.scrollLog?.();
  };
  grid.addEventListener("tile-close", (e) => {
    const k = keyOfEl(e.target);
    if (!k) return;
    if (auto) pinAuto(k); else gridList.put(k, false);
    items = items.filter((x) => x !== k);
    delete rects[k];
    store.set(rects);
    if (maxKey === k) maxKey = null;
    tiles.get(k)?.el.remove();
    tiles.get(k)?.destroy();
    tiles.delete(k);
    render();
  });
  const pick = () => dialog((body, close) => {
    const placed = new Set(items);
    const rest = all.filter((c) => !placed.has(keyOf(c)));
    const q = h("input", { placeholder: "絞り込み（タイトル・端末・#番号）" });
    const list = h("div", { class: "list pick-list" });
    const draw = () => {
      const f = q.value.trim().toLowerCase();
      const rows = rest.filter((c) => !f || `${c.device_name} #${c.session.num} ${sessionTitle(c.session)} ${c.session.cwd || ""}`.toLowerCase().includes(f));
      list.replaceChildren(...(rows.length ? rows.map((c) => h("button", { class: "pick-item", type: "button", onclick: () => {
        pinAuto();
        gridList.put(keyOf(c), true);
        items = [...items, keyOf(c)];
        auto = false;
        close();
        render();
      } },
        h("div", { class: "row" }, h("span", { class: "chip dev" }, c.device_name), h("span", { class: "chip" }, gridState(c.session)),
          h("span", { class: "grow" }), h("span", { class: "muted small" }, rel(c.session.last_seen))),
        h("div", { class: titleClass("card-title", c.session) }, (c.session.num ? `#${c.session.num} ` : "") + sessionTitle(c.session))))
        : [h("div", { class: "muted small" }, rest.length ? "一致するセッションはありません" : "追加できるセッションはありません（すべて配置済み）")]));
    };
    q.addEventListener("input", draw);
    body.append(h("h1", {}, "グリッドに追加"), q, list,
      h("div", { class: "row" }, h("span", { class: "grow" }), h("button", { class: "btn", type: "button", onclick: close }, "閉じる")));
    draw();
    setTimeout(() => autoFocus(q), 0);
  });
  grid.addEventListener("tile-max", (e) => {
    const k = [...tiles].find(([, t]) => t.el === e.target)?.[0];
    if (e.detail?.on && maxKey === k) return; // already the big one
    maxKey = e.detail?.on || maxKey !== k ? k : null;
    applyMax();
  });
  const onKey = (e) => { if (e.key === "Escape" && maxKey) { maxKey = null; applyMax(); } };
  document.addEventListener("keydown", onKey);
  cleanup.push(() => document.removeEventListener("keydown", onKey));
  const ro = new ResizeObserver(relayout);
  ro.observe(grid);
  cleanup.push(() => ro.disconnect());
  cleanup.push(() => tiles.forEach((t) => t.destroy()));
  const count = h("span", { class: "muted small" });
  const addBtn = h("button", { class: "btn small primary", title: "セッションをグリッドに置く", onclick: () => pick() }, icon("plus"), "追加");
  const empty = h("div", { class: "empty", hidden: true }, h("p", {}, "グリッドにセッションがありません"),
    h("button", { class: "btn primary", onclick: () => pick() }, icon("plus"), "セッションを追加"));
  mount.append(h("div", { class: "grid-page" },
    h("div", { class: "row grid-top" }, h("h1", { class: "grow" }, "グリッド"), count, addBtn,
      h("div", { class: "seg" }, modeAuto, modeFree), resetBtn), empty, grid));
  setMode(free);
  // render places a tile for each listed key the hub knows, in list order
  // (a session on an offline device comes back with the device).
  const render = () => {
    const byKey = new Map(all.map((c) => [keyOf(c), c]));
    const shown = [];
    for (const k of items) {
      const c = byKey.get(k);
      if (!c) continue;
      let t = tiles.get(k);
      if (!t) { t = gridTile(c); tiles.set(k, t); }
      t.update(c);
      // Keep list order, but only move a tile that is out of place: moving
      // a node blurs the reply box you are typing in.
      const at = grid.children[shown.length];
      if (at !== t.el) grid.insertBefore(t.el, at || null);
      shown.push(c);
    }
    for (const [k, t] of tiles) if (!byKey.has(k) || !items.includes(k)) { t.el.remove(); t.destroy(); tiles.delete(k); }
    empty.hidden = tiles.size > 0;
    applyMax();
    const needs = shown.filter((c) => c.session.attention === "needs_you").length;
    const working = shown.filter((c) => c.session.status === "active").length;
    count.textContent = (auto && shown.length ? "自動（要対応・作業中・未確認） · " : "") + `${shown.length} 件 · 要対応 ${needs} · 作業中 ${working}`;
  };
  const tick = async () => {
    all = await api("/api/active?all=1");
    gridList.settle(all);
    if (!legacyDone) {
      // A list this browser kept before the grid moved onto the devices:
      // put it there once, in order.
      legacyDone = true;
      const known = new Set(all.map(keyOf));
      for (const k of gridList.takeLegacy()) if (known.has(k)) await gridList.put(k, true);
    }
    computeItems();
    render();
    await Promise.all([...tiles.values()].map((t) => t.refresh().catch(() => {})));
  };
  every(4000, tick);
}

// ---- usage (API-price estimate) ----

function fmtTok(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(n);
}
const fmtUSD = (v) => "$" + (v >= 100 ? v.toFixed(0) : v.toFixed(2));
const shortModel = (m) => (m || "").replace(/^claude-/, "");
// usageLine: "opus-5-5 · 文脈 85.2k · $1.23" (a Brief or a SessionUsage).
const usageLine = (u) => [shortModel(u.model), u.context ? "文脈 " + fmtTok(u.context) : "",
  fmtUSD(u.cost ?? u.total?.cost ?? 0)].filter(Boolean).join(" · ");
// modelName: "claude-opus-5-5" → "Opus 5.5" ("-20251001" date suffixes dropped).
function modelName(m) {
  const parts = (m || "").replace(/^claude-/, "").replace(/-\d{8}$/, "").split("-");
  const fam = parts.filter((p) => !/^\d+$/.test(p)).join(" ");
  const ver = parts.filter((p) => /^\d+$/.test(p)).join(".");
  return [fam.charAt(0).toUpperCase() + fam.slice(1), ver].filter(Boolean).join(" ");
}
// contextWindow: the transcript doesn't say which window a model ran with,
// so 200k unless the prompt already outgrew it (then the 1M window).
const contextWindow = (ctx) => (ctx > 200000 ? 1000000 : 200000);

// ctxPart: a context fill bar with "pct% used/size".
function ctxPart(pct, used, size) {
  pct = Math.max(0, Math.min(100, Math.round(pct)));
  return h("span", { class: "st-ctx", title: "文脈（直近の応答時点）" },
    h("span", { class: "st-bar" + (pct >= 80 ? " hot" : pct >= 50 ? " warm" : "") }, h("span", { style: `width:${pct}%` })),
    ` ${pct}%` + (used ? ` ${fmtTok(used)}/${fmtTok(size)}` : ""));
}
const fmtDur = (ms) => {
  const m = Math.floor(ms / 60000);
  return m >= 60 ? `${Math.floor(m / 60)}h${m % 60}m` : `${m}m`;
};

// liveStatusParts: the chat's status line from Claude Code's statusLine
// JSON (see https://code.claude.com/docs/en/statusline).
function liveStatusParts(st) {
  const out = [h("span", { class: "st-model" }, "◆ " + (st.model?.display_name || modelName(st.model?.id)))];
  const cw = st.context_window;
  if (cw?.context_window_size) {
    const u = cw.current_usage;
    const used = u ? (u.input_tokens || 0) + (u.cache_creation_input_tokens || 0) + (u.cache_read_input_tokens || 0) : cw.total_input_tokens || 0;
    const pct = cw.used_percentage ?? (used / cw.context_window_size) * 100;
    out.push(ctxPart(pct, used, cw.context_window_size));
  }
  if (st.cost?.total_cost_usd != null) out.push(h("span", { class: "st-cost", title: "このセッションの概算コスト（Claude Code の推定）" }, fmtUSD(st.cost.total_cost_usd)));
  const add = st.cost?.total_lines_added || 0, del = st.cost?.total_lines_removed || 0;
  if (add || del) out.push(h("span", { class: "st-lines" }, h("span", { class: "add" }, `+${add}`), " ", h("span", { class: "del" }, `−${del}`)));
  if (st.cost?.total_duration_ms) out.push(h("span", { class: "st-dur" }, "⏱ " + fmtDur(st.cost.total_duration_ms)));
  const rl = st.rate_limits?.five_hour;
  if (rl?.used_percentage != null) out.push(h("span", { class: "st-rate", title: "5 時間枠の使用率" }, `5h ${Math.round(rl.used_percentage)}%`));
  return out;
}

// statusParts: the chat's status line for a SessionUsage.
function statusParts(u) {
  const out = [h("span", { class: "st-model" }, "◆ " + modelName(u.model))];
  if (u.context) {
    const win = contextWindow(u.context);
    out.push(ctxPart((u.context / win) * 100, u.context, win));
  }
  out.push(h("span", { class: "st-cost" }, fmtUSD(u.total?.cost ?? 0)));
  return out;
}
const tokTotal = (t) => (t.input || 0) + (t.output || 0) + (t.cache_write || 0) + (t.cache_read || 0);

function usageDialog(u, title) {
  dialog((body) => {
    const rows = Object.entries(u.by_model || {}).sort((a, b) => b[1].cost - a[1].cost);
    body.append(h("h1", {}, title),
      h("p", { class: "muted small" }, "API の定価で換算した概算です（サブスクリプションの実際の請求額ではありません）。"),
      u.model ? h("p", { class: "small" }, `現在のモデル: ${u.model}` + (u.context ? ` · 文脈 ${fmtTok(u.context)} トークン（直近の応答時点）` : "")) : null,
      u.total ? h("p", { class: "small" }, `合計 ${fmtTok(tokTotal(u.total))} トークン · ${u.total.messages} 応答 · ${fmtUSD(u.total.cost)}`) : null,
      h("div", { class: "md-table" }, h("table", {},
        h("thead", {}, h("tr", {}, ["モデル", "入力", "出力", "キャッシュ書込", "キャッシュ読込", "概算"].map((x) => h("th", {}, x)))),
        h("tbody", {}, rows.map(([m, t]) => h("tr", {}, [m, fmtTok(t.input), fmtTok(t.output), fmtTok(t.cache_write), fmtTok(t.cache_read), fmtUSD(t.cost)].map((x) => h("td", {}, x))))))),
      u.subagents?.messages ? h("p", { class: "small" }, `うちサブエージェント: ${fmtUSD(u.subagents.cost)}（${fmtTok(tokTotal(u.subagents))} トークン）`) : null,
      h("div", { class: "actions" }, h("button", { class: "btn primary" }, "閉じる")));
  });
}

// ---- composer helpers ----

// Claude Code built-ins worth having one tap away (the device's own skills
// and commands are appended from /hub/skills).
const BUILTIN_SLASH = [
  { name: "compact", description: "会話を要約して文脈を圧縮" },
  { name: "clear", description: "会話をクリア" },
  { name: "model", description: "モデルを切り替え" },
  { name: "context", description: "文脈の使用状況" },
  { name: "cost", description: "このセッションのコスト" },
  { name: "review", description: "変更のレビュー" },
  { name: "init", description: "CLAUDE.md を作成" },
  { name: "memory", description: "メモリを編集" },
];

function editQuick(paint) {
  dialog(async (body, close) => {
    const cmds = await api("/api/quick").catch(() => []);
    const ta = h("textarea", { rows: 8, class: "grow" });
    ta.value = cmds.join("\n");
    body.append(h("h1", {}, "クイックコマンド"),
      h("p", { class: "muted small" }, "1 行に 1 つ。チャットの入力欄の上にボタンとして並び、タップで送信します（全端末・全ブラウザで共通）。"),
      ta,
      h("div", { class: "actions" },
        h("button", { class: "btn", value: "cancel" }, "キャンセル"),
        h("button", { type: "button", class: "btn primary", onclick: async () => {
          try { paint(await api("/api/quick", { method: "PUT", json: ta.value.split("\n") })); close(); }
          catch (e) { toast(e.message); }
        } }, "保存")));
  });
}

let recog = null;
function speechSupported() { return !!(window.SpeechRecognition || window.webkitSpeechRecognition); }
function stopDictation() { try { recog?.stop(); } catch {} recog = null; }
function toggleDictation(btn, input, autosize) {
  if (recog) { stopDictation(); btn.classList.remove("on"); return; }
  const R = window.SpeechRecognition || window.webkitSpeechRecognition;
  recog = new R();
  recog.lang = "ja-JP";
  recog.interimResults = true;
  recog.continuous = true;
  const base = input.value ? input.value.replace(/\s*$/, " ") : "";
  let finalText = "";
  recog.onresult = (e) => {
    let interim = "";
    for (let i = e.resultIndex; i < e.results.length; i++) {
      if (e.results[i].isFinal) finalText += e.results[i][0].transcript;
      else interim += e.results[i][0].transcript;
    }
    input.value = base + finalText + interim;
    autosize();
  };
  recog.onend = () => { btn.classList.remove("on"); recog = null; };
  recog.onerror = (e) => { if (e.error !== "aborted") toast("音声入力: " + e.error); };
  btn.classList.add("on");
  recog.start();
}

// ---- diff review ----
//
// Annotate the session's working-tree diff line by line, then send every
// note to Claude as ONE prompt (batched feedback keeps the revision
// coherent). Notes wait in localStorage until sent.

function parseDiff(text) {
  const files = [];
  let f = null, oldNo = 0, newNo = 0;
  for (const line of text.split("\n")) {
    if (line.startsWith("diff --git ")) {
      const m = line.match(/^diff --git a\/(.+?) b\/(.+)$/);
      f = { path: m ? m[2] : line.slice(11), old: m ? m[1] : "", lines: [], added: 0, deleted: 0, binary: false };
      files.push(f);
      continue;
    }
    if (!f) continue;
    if (line.startsWith("+++ ")) { const p = line.slice(4).replace(/^b\//, ""); if (p !== "/dev/null") f.path = p; continue; }
    if (line.startsWith("--- ") || line.startsWith("index ") || line.startsWith("new file") || line.startsWith("deleted file") || line.startsWith("similarity") || line.startsWith("rename ") || line.startsWith("old mode") || line.startsWith("new mode")) continue;
    if (line.startsWith("Binary files")) { f.binary = true; continue; }
    const hunk = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$/);
    if (hunk) { oldNo = +hunk[1]; newNo = +hunk[2]; f.lines.push({ kind: "hunk", text: line }); continue; }
    if (line.startsWith("+")) { f.lines.push({ kind: "add", newNo: newNo++, text: line.slice(1) }); f.added++; }
    else if (line.startsWith("-")) { f.lines.push({ kind: "del", oldNo: oldNo++, text: line.slice(1) }); f.deleted++; }
    else if (line.startsWith(" ")) f.lines.push({ kind: "ctx", oldNo: oldNo++, newNo: newNo++, text: line.slice(1) });
  }
  return files;
}

const reviewKey = (id, sid) => `ccdash.review.${id}.${sid}`;
function loadNotes(id, sid) { try { return JSON.parse(localStorage.getItem(reviewKey(id, sid)) || "[]"); } catch { return []; } }
function saveNotes(id, sid, notes) { try { localStorage.setItem(reviewKey(id, sid), JSON.stringify(notes)); } catch {} }

function reviewPrompt(notes, general) {
  const out = ["差分レビューのコメントです。それぞれ対応してください。", ""];
  notes.forEach((n, i) => {
    const where = n.line ? `${n.path}:${n.line}` : n.path;
    const code = n.code ? `（${n.side === "del" ? "-" : n.side === "add" ? "+" : " "} \`${n.code.trim().slice(0, 120)}\`）` : "";
    out.push(`${i + 1}. ${where}${code}`);
    for (const l of n.text.split("\n")) out.push("   " + l);
  });
  if (general.trim()) out.push("", "全体について:", general.trim());
  return out.join("\n");
}

// sendToSession types text into the session's claude, resuming it with the
// text as the first prompt when it isn't running under ccdash.
// resumeSession starts `claude --resume` for a stopped session in a hosted
// PTY, as sending a message would, but without a prompt.
async function resumeSession(id, sid) {
  const d = dev(id);
  const ptys = (await d.get("/pty/").catch(() => [])) || [];
  if (ptys.some((p) => p.key === sid && p.alive)) return false;
  const sessions = (await d.get("/api/sessions").catch(() => [])) || [];
  const s = sessions.find((x) => x.session_id === sid);
  if (s && (s.status === "active" || s.status === "idle") &&
    !confirm("このセッションは ccdash 管理外のターミナルで実行中のようです。ここで別インスタンスとして再開しますか？")) throw new Error("キャンセルしました");
  await d.post("/pty/start", { sessionId: sid, resumeId: sid, cwd: s?.cwd || "", cols: 120, rows: 40 });
  return true;
}

async function sendToSession(id, sid, text) {
  const d = dev(id);
  const ptys = (await d.get("/pty/").catch(() => [])) || [];
  if (ptys.some((p) => p.key === sid && p.alive)) {
    await d.post(`/pty/${encodeURIComponent(sid)}/input`, { text, submit: true });
    return;
  }
  const sessions = (await d.get("/api/sessions").catch(() => [])) || [];
  const s = sessions.find((x) => x.session_id === sid);
  if (s && (s.status === "active" || s.status === "idle") &&
    !confirm("このセッションは ccdash 管理外のターミナルで実行中のようです。ここで別インスタンスとして再開して送信しますか？")) throw new Error("キャンセルしました");
  await d.post("/pty/start", { sessionId: sid, resumeId: sid, cwd: s?.cwd || "", cols: 120, rows: 40, prompt: text.startsWith("-") ? " " + text : text });
}

async function diffPage(id, sid) {
  const mount = mainMount;
  const name = await deviceName(id);
  const d = dev(id);
  const chatHref = `#/d/${id}/s/${encodeURIComponent(sid)}`;
  crumbs({ text: "端末", href: "#/" }, { text: name, href: `#/d/${id}` }, { text: "差分", href: chatHref });
  let notes = loadNotes(id, sid);
  const info = h("div", { class: "muted small" }, "読み込み中…");
  const filesEl = h("div", { class: "diff-files" });
  const general = h("textarea", { rows: 2, placeholder: "全体へのコメント（任意）" });
  const sendBtn = h("button", { class: "btn primary" }, "Claude に送る");
  const countEl = h("span", { class: "grow muted small" });
  const paintCount = () => {
    countEl.textContent = notes.length ? `コメント ${notes.length} 件` : "行をタップしてコメント";
    sendBtn.disabled = !notes.length && !general.value.trim();
  };
  general.addEventListener("input", paintCount);
  sendBtn.addEventListener("click", async () => {
    sendBtn.disabled = true;
    try {
      await sendToSession(id, sid, reviewPrompt(notes, general.value));
      notes = [];
      saveNotes(id, sid, notes);
      toast("レビューを送りました");
      location.hash = chatHref;
    } catch (e) { toast(e.message); paintCount(); }
  });
  mount.append(h("div", { class: "chat-wrap" },
    h("div", { class: "term-bar" }, h("a", { class: "btn small icon", href: chatHref, "aria-label": "戻る", title: "戻る" }, icon("back")), h("span", { class: "grow title" }, "差分レビュー"),
      h("button", { class: "btn small", onclick: () => route() }, "更新")),
    h("div", { class: "chat" }, h("div", { class: "page diff-page" }, info, filesEl)),
    h("div", { class: "chat-bottom" }, h("div", { class: "composer col" }, general, h("div", { class: "row" }, countEl, sendBtn)))));
  paintCount();

  let st, diffText;
  try {
    [st, diffText] = await Promise.all([
      d.get(`/hub/git/status?session=${encodeURIComponent(sid)}`),
      d.get(`/hub/git/diff?session=${encodeURIComponent(sid)}`),
    ]);
  } catch (e) { info.textContent = e.message; return; }
  if (!st.repo) { info.textContent = "このセッションのディレクトリは git リポジトリではありません"; return; }
  info.replaceChildren(
    h("b", {}, st.branch || "?"),
    st.upstream ? ` → ${st.upstream}` : "",
    st.ahead ? ` · ↑${st.ahead}` : "", st.behind ? ` · ↓${st.behind}` : "",
    st.head ? ` · 最新 ${st.head.hash} ${st.head.subject}（${st.head.when}）` : "",
    diffText.truncated ? " · 差分が大きいため途中まで" : "");
  const files = parseDiff(diffText.diff || "");
  if (!files.length) { filesEl.append(h("div", { class: "empty" }, "変更はありません")); return; }

  const noteFor = (path, ln) => notes.find((n) => n.path === path && n.key === ln);
  const renderFile = (f) => {
    const body = h("div", { class: "diff-body" });
    const det = h("details", { class: "diff-file", open: files.length <= 3 },
      h("summary", {}, h("span", { class: "mono grow" }, f.path),
        h("span", { class: "add" }, `+${f.added}`), " ", h("span", { class: "del" }, `-${f.deleted}`),
        h("span", { class: "note-count" }, "")),
      body);
    const paintCountChip = () => {
      const n = notes.filter((x) => x.path === f.path).length;
      det.querySelector(".note-count").textContent = n ? ` 💬${n}` : "";
    };
    const draw = () => {
      if (f.binary) { body.replaceChildren(h("div", { class: "muted small" }, "バイナリファイル")); return; }
      body.replaceChildren(...f.lines.map((l, idx) => {
        if (l.kind === "hunk") return h("div", { class: "dl hunk" }, l.text);
        const key = `${l.kind}:${l.newNo ?? ""}:${l.oldNo ?? ""}:${idx}`;
        const row = h("div", { class: "dl " + l.kind },
          h("span", { class: "ln" }, l.oldNo ?? ""), h("span", { class: "ln" }, l.newNo ?? ""),
          h("span", { class: "code" }, (l.kind === "add" ? "+" : l.kind === "del" ? "-" : " ") + l.text));
        const wrap = h("div", {}, row);
        const existing = noteFor(f.path, key);
        const editor = (n) => {
          const ta = h("textarea", { rows: 2, placeholder: "この行へのコメント" });
          ta.value = n?.text || "";
          const box = h("div", { class: "note-editor" }, ta, h("div", { class: "row" },
            n ? h("button", { type: "button", class: "btn small danger", onclick: () => { notes = notes.filter((x) => x !== n); saveNotes(id, sid, notes); paintCount(); paintCountChip(); draw(); } }, "削除") : null,
            h("span", { class: "grow" }),
            h("button", { type: "button", class: "btn small", onclick: () => draw() }, "閉じる"),
            h("button", { type: "button", class: "btn small primary", onclick: () => {
              const text = ta.value.trim();
              if (!text) return;
              if (n) n.text = text;
              else notes.push({ path: f.path, key, line: l.newNo ?? l.oldNo, side: l.kind, code: l.text, text });
              saveNotes(id, sid, notes); paintCount(); paintCountChip(); draw();
            } }, "保存")));
          wrap.append(box);
          setTimeout(() => ta.focus(), 0);
        };
        if (existing) wrap.append(h("div", { class: "note", onclick: () => { wrap.lastChild.remove(); editor(existing); } }, "💬 " + existing.text));
        row.addEventListener("click", () => { if (!wrap.querySelector(".note-editor")) editor(existing); });
        return wrap;
      }));
    };
    det.addEventListener("toggle", () => { if (det.open && !body.childElementCount) draw(); });
    if (det.open) draw();
    paintCountChip();
    return det;
  };
  filesEl.replaceChildren(...files.map(renderFile));
}

// ---- subagents ----

function elapsed(from, to) {
  const s = Math.max(0, Math.round(((to ? new Date(to) : new Date()) - new Date(from)) / 1000));
  if (s < 60) return `${s}秒`;
  if (s < 3600) return `${Math.floor(s / 60)}分`;
  return `${Math.floor(s / 3600)}時間${Math.floor((s % 3600) / 60)}分`;
}

const AGENT_STATE = { running: "実行中", done: "完了", stale: "応答なし" };

function agentRow(a, open) {
  return h("div", { class: "agent " + a.status, onclick: () => open(a) },
    h("span", { class: "agent-dot" }),
    h("div", { class: "grow" },
      h("div", { class: "row" },
        h("b", { class: "grow agent-desc" }, a.description || a.agentId),
        h("span", { class: "chip" }, a.agentType || "agent"),
        a.background ? h("span", { class: "chip" }, "bg") : null),
      h("div", { class: "sub" }, `${AGENT_STATE[a.status] || a.status} · ${a.status === "running" ? elapsed(a.startedAt) : elapsed(a.startedAt, a.updatedAt)} · ツール ${a.tools}${a.activity ? " · " + a.activity : ""}`)));
}

// openAgent shows one subagent's own transcript (it lives in a separate
// file; entries are isSidechain) and, once it handed back, its report.
function openAgent(id, sid, a) {
  dialog(async (body, close) => {
    const log = h("div", { class: "chat-log agent-log" }, h("div", { class: "muted" }, "読み込み中…"));
    const reload = async () => {
      try {
        const t = await dev(id).get(`/hub/subagents/${encodeURIComponent(sid)}/${encodeURIComponent(a.agentId)}?bytes=524288`);
        const items = parseTranscript(decodeEntries(t.data), { sidechain: true });
        log.replaceChildren(...(items.length ? items.map((it) => renderItem(it)).filter(Boolean) : [h("div", { class: "muted" }, "まだ記録がありません")]));
        log.scrollTop = log.scrollHeight;
      } catch (e) { log.replaceChildren(h("div", { class: "muted" }, e.message)); }
    };
    body.append(
      h("div", { class: "row" }, h("h1", { class: "grow" }, a.description || a.agentId), h("span", { class: "chip" }, a.agentType || "agent")),
      h("div", { class: "muted small" }, `${AGENT_STATE[a.status] || a.status} · 開始 ${a.startedAt ? new Date(a.startedAt).toLocaleTimeString() : "?"} · ツール ${a.tools}`),
      a.report ? h("details", { class: "summary-card", open: true }, h("summary", {}, "報告"), h("div", {}, a.report)) : null,
      log,
      h("div", { class: "actions" },
        h("button", { type: "button", class: "btn", onclick: reload }, "更新"),
        h("button", { class: "btn primary" }, "閉じる")));
    reload();
  });
}

// ---- the chat view ----

// chatPage mounts a full-screen chatView for one session (or fresh spawn).
async function chatPage(id, { sid, key }) {
  const v = chatView(id, { sid, key });
  mainMount.append(v.el);
  cleanup.push(v.destroy);
}

// sharedGet memoizes a device GET for a moment, so several views of one
// device (the grid's tiles, the chat) polling together cost one request.
const sharedGets = new Map(); // "id path" → { at, p }
function sharedGet(id, path, ttl = 1200) {
  const k = id + " " + path, hit = sharedGets.get(k);
  if (hit && Date.now() - hit.at < ttl) return hit.p;
  const p = dev(id).get(path);
  sharedGets.set(k, { at: Date.now(), p });
  p.catch(() => sharedGets.delete(k));
  return p;
}

// chatView is the one session UI: the chat page shows it full screen, and
// each grid tile is the same view in compact mode (opts.compact) — so the
// grid can do everything the chat can (choice / permission cards, questions,
// approvals, subagents, images, voice, / completion, quick commands, !
// commands, stop) instead of keeping a second, thinner implementation.
// Returns { el, destroy, scrollBottom }; destroy stops its polling.
function chatView(id, { sid, key, compact = false, deviceName: devName = "" }) {
  const bucket = [];
  const d = dev(id);
  let name = devName;
  let session = null, ptyKey = key || null, hosted = false, info = null;
  let lastStat = "", items = [], pendingSends = [];
  let lookingSince = 0;
  // History window: the poll reads only the last TAIL bytes and splices
  // them onto what's loaded (by entry uuid), so "older history" loaded
  // once stays without re-downloading megabytes on every update.
  const TAIL = compact ? 98304 : 393216; // a tile shows less; "older history" still loads more
  let entries = [], hasOlder = false, loadedBytes = 0, loadingOlder = false;
  const tailURL = (bytes) => `/api/sessions/${encodeURIComponent(sid)}/transcript?mode=tail&bytes=${bytes}`;
  const splice = (fresh) => {
    const firstID = fresh.find((e) => e.uuid)?.uuid;
    const at = firstID ? entries.findIndex((e) => e.uuid === firstID) : -1;
    if (entries.length && at >= 0) {
      entries = entries.slice(0, at).concat(fresh);
      return true;
    }
    return false;
  };

  const titleEl = h("span", { class: "grow title sess-title" }, "読み込み中…");
  const statusEl = h("span", { class: "chip" });
  const freshEl = h("span", { hidden: true });
  const waitEl = h("span", { hidden: true });
  const termBtn = h("a", { class: "btn small", href: "#" }, "ターミナル");
  const endBtn = h("button", { class: "btn small danger", hidden: true, title: "claude を終了してアーカイブする", onclick: async () => {
    if (!confirm(END_CONFIRM)) return;
    try { toast(await endAndArchive(d, ptyKey, sid)); } catch (e) { toast(e.message); }
  } }, "終了");
  const restartBtn = h("button", { class: "btn small", hidden: true, title: "claude を終了して --resume で起動し直す", onclick: async () => {
    if (!confirm("このセッションの claude を再起動しますか？（終了して claude --resume で起動し直します。応答中の内容は中断されます）")) return;
    restartBtn.disabled = true;
    try { await d.post(`/pty/${encodeURIComponent(ptyKey)}/restart`, {}); toast("再起動しました"); }
    catch (e) { toast(e.message); }
    finally { restartBtn.disabled = false; setTimeout(poll, 800); }
  } }, "再起動");
  let allGroups = [];
  const diffBtn = h("a", { class: "btn small", hidden: true, title: "変更の差分を見てコメントする" }, "差分");
  let lastUsage = null;
  const menuBtn = h("button", { class: "btn small", title: "セッションの操作", onclick: () => session ? sessionMenu(id, session, allGroups, poll) : toast("まだセッションが登録されていません"), "aria-label": "セッションの操作" }, icon("more"));
  const chatHref = () => sid ? `#/d/${id}/s/${encodeURIComponent(sid)}` : `#/d/${id}/p/${encodeURIComponent(ptyKey)}`;
  const stopTop = h("button", { class: "btn small" + (compact ? " icon" : ""), type: "button", title: "Esc を送って作業を中断", "aria-label": "中断", hidden: true, onclick: () => keys("\x1b") }, icon("stop"), compact ? null : "中断");
  // Tile: maximise / restore (double-click the title does the same).
  const maxBtn = compact ? h("button", { class: "btn small icon tile-maxbtn", type: "button", title: "大きく表示 / 戻す", "aria-label": "大きく表示 / 戻す",
    onclick: () => root.dispatchEvent(new CustomEvent("tile-max", { bubbles: true })) }, icon("expand")) : null;
  let root;
  const bar = compact
    ? h("div", { class: "term-bar tile-head" },
      h("span", { class: "chip dev" }, devName), titleEl, freshEl, waitEl, statusEl, stopTop,
      h("a", { class: "btn small icon", href: "#", title: "チャット画面で開く", "aria-label": "チャット画面で開く", onclick: (e) => { e.preventDefault(); location.hash = chatHref(); } }, icon("open")),
      maxBtn, menuBtn,
      h("button", { class: "btn small icon tile-close", type: "button", title: "グリッドから外す", "aria-label": "グリッドから外す", onclick: () => root.dispatchEvent(new CustomEvent("tile-close", { bubbles: true })) }, icon("x")))
    : h("div", { class: "term-bar" },
      h("a", { class: "btn small icon", href: `#/d/${id}`, "aria-label": "戻る", title: "戻る" }, icon("back")), titleEl, freshEl, waitEl, statusEl, diffBtn, termBtn, restartBtn, endBtn, menuBtn);
  if (compact) titleEl.addEventListener("dblclick", () => root.dispatchEvent(new CustomEvent("tile-max", { bubbles: true })));
  const log = h("div", { class: "chat-log" }, h("div", { class: "empty" }, "読み込み中…"));
  const scroller = h("div", { class: "chat" }, log);
  const approvalsEl = h("div", { class: "chat-dock" });
  // Subagents: a bar under the header when the session has any; the list
  // opens below it.
  let agents = [], agentsOpen = false, agentTick = 0;
  const agentList = h("div", { class: "agent-list", hidden: true });
  const agentBar = h("button", { class: "agent-bar", hidden: true, onclick: () => { agentsOpen = !agentsOpen; paintAgents(); } });
  const openThisAgent = (a) => openAgent(id, sid, a);
  const paintAgents = () => {
    const running = agents.filter((a) => a.status === "running").length;
    agentBar.hidden = !agents.length;
    agentBar.classList.toggle("busy", running > 0);
    agentBar.replaceChildren(h("span", { class: "agent-dot" + (running ? " spin" : "") }),
      running ? `サブエージェント ${running} 件実行中` : `サブエージェント ${agents.length} 件`,
      h("span", { class: "muted small" }, running && agents.length > running ? `（全 ${agents.length} 件）` : ""),
      h("span", { class: "grow" }), icon(agentsOpen ? "up" : "down"));
    agentList.hidden = !agentsOpen || !agents.length;
    if (!agentList.hidden) agentList.replaceChildren(...agents.map((a) => agentRow(a, openThisAgent)));
  };
  const promptEl = h("div", { class: "chat-dock" });
  const input = h("textarea", { rows: 1, placeholder: "メッセージ", enterkeyhint: "send", title: "Enter で送信 / Shift+Enter で改行" });
  const sendBtn = h("button", { class: "btn primary" }, "送信");
  const stopBtn = h("button", { class: "btn", hidden: true, title: "Esc を送って作業を中断", onclick: () => keys("\x1b") }, "中断");
  // Image attachments: picked (camera on phones), pasted or dropped; shown
  // as thumbnails until sent.
  let attachments = []; // { blob, url }
  const thumbs = h("div", { class: "thumbs" });
  const fileInput = h("input", { type: "file", accept: "image/*", multiple: true, hidden: true });
  const attachBtn = h("button", { type: "button", class: "btn attach", title: "画像を添付", "aria-label": "画像を添付", onclick: () => fileInput.click() }, icon("image"));
  const renderThumbs = () => {
    thumbs.replaceChildren(...attachments.map((a, i) => h("div", { class: "thumb" },
      h("img", { src: a.url, alt: "" }),
      h("button", { type: "button", title: "外す", onclick: () => { URL.revokeObjectURL(a.url); attachments.splice(i, 1); renderThumbs(); }, "aria-label": "外す" }, icon("x")))));
  };
  const addFiles = async (files) => {
    for (const f of files) {
      if (!f.type.startsWith("image/")) continue;
      try {
        const blob = await prepareImage(f);
        attachments.push({ blob, url: URL.createObjectURL(blob) });
      } catch (e) { toast("画像を読み込めませんでした: " + e.message); }
    }
    renderThumbs();
  };
  fileInput.addEventListener("change", () => { addFiles([...fileInput.files]); fileInput.value = ""; });
  input.addEventListener("paste", (e) => {
    const files = [...(e.clipboardData?.files || [])].filter((f) => f.type.startsWith("image/"));
    if (files.length) { e.preventDefault(); addFiles(files); }
  });
  bucket.push(() => attachments.forEach((a) => URL.revokeObjectURL(a.url)));
  // Voice input (Web Speech API): dictation is appended to the message.
  const micBtn = speechSupported() ? h("button", { type: "button", class: "btn attach mic", title: "音声入力", "aria-label": "音声入力" }, icon("mic")) : null;
  micBtn?.addEventListener("click", () => toggleDictation(micBtn, input, autosize));
  bucket.push(() => stopDictation());
  // "/" completion: skills and slash commands of this device.
  const slashBox = h("div", { class: "slash-box", hidden: true });
  let slashList = null;
  const showSlash = async () => {
    const v = input.value;
    if (!v.startsWith("/") || /\s/.test(v)) { slashBox.hidden = true; return; }
    if (!slashList) {
      const skills = (await d.get(`/hub/skills?dir=${encodeURIComponent(session?.cwd || "")}`).catch(() => [])) || [];
      slashList = [...BUILTIN_SLASH, ...skills.map((k) => ({ name: k.name, description: k.description }))];
    }
    const q = v.slice(1).toLowerCase();
    const hits = slashList.filter((k) => k.name.toLowerCase().includes(q) || (k.description || "").toLowerCase().includes(q)).slice(0, 30);
    slashBox.hidden = !hits.length;
    slashBox.replaceChildren(...hits.map((k) => h("div", { onclick: () => { input.value = "/" + k.name + " "; slashBox.hidden = true; autosize(); input.focus(); } },
      h("b", {}, "/" + k.name), k.description ? h("span", { class: "muted small" }, " " + k.description) : null)));
  };
  input.addEventListener("input", showSlash);
  // Quick commands: one tap sends a saved prompt.
  const quickRow = h("div", { class: "quick-row" });
  const sendQuick = (text) => { input.value = text; autosize(); composer.requestSubmit(); };
  const paintQuick = (cmds) => quickRow.replaceChildren(
    ...cmds.map((c) => h("button", { type: "button", class: "chip quick", title: c, onclick: () => sendQuick(c) }, c.length > 18 ? c.slice(0, 18) + "…" : c)),
    h("button", { type: "button", class: "chip quick edit", title: "クイックコマンドを編集", "aria-label": "クイックコマンドを編集", onclick: () => editQuick(paintQuick) }, icon("edit")));
  api("/api/quick").then(paintQuick).catch(() => {});
  const composer = h("form", { class: "composer" }, attachBtn, micBtn, h("div", { class: "grow col" }, thumbs, slashBox, input), fileInput, h("div", { class: "row" }, stopBtn, sendBtn));
  for (const ev of ["dragover", "drop"]) {
    scroller.addEventListener(ev, (e) => {
      if (![...(e.dataTransfer?.types || [])].includes("Files")) return;
      e.preventDefault();
      if (ev === "drop") addFiles([...e.dataTransfer.files]);
    });
  }
  // Right of the shortcuts: the status Claude Code itself reports (relayed
  // by ccdash's statusLine hook: model, context window, cost, lines, time,
  // rate limits), or — when none arrived lately — model · context · cost
  // from the transcript. Tap for the token / cost breakdown.
  const statusLine = h("button", { type: "button", class: "chat-status", hidden: true, title: "トークンと API 換算コストの内訳", onclick: () => lastUsage && usageDialog(lastUsage, "このセッションの使用量") });
  let liveSt = null, statusSig = "";
  const paintStatus = () => {
    const fresh = liveSt && Date.now() - new Date(liveSt.at) < 15 * 60000 ? liveSt.data : null;
    const sig = fresh ? "s:" + JSON.stringify([fresh.model, fresh.context_window, fresh.cost, fresh.rate_limits])
      : lastUsage ? "u:" + JSON.stringify([lastUsage.model, lastUsage.context, lastUsage.total?.cost]) : "";
    if (sig === statusSig) return;
    statusSig = sig;
    statusLine.hidden = !sig;
    statusLine.replaceChildren(...(fresh ? liveStatusParts(fresh) : lastUsage ? statusParts(lastUsage) : []));
  };
  root = h("div", { class: compact ? "tile chat-wrap compact" : "chat-wrap" }, bar, agentBar, agentList, scroller,
    h("div", { class: "chat-bottom" }, approvalsEl, promptEl, h("div", { class: "quick-bar" }, quickRow, statusLine), composer),
    compact ? h("div", { class: "tile-resize", title: "ドラッグで大きさを変更" }) : null);

  const autosize = () => { input.style.height = "auto"; input.style.height = Math.min(input.scrollHeight, 200) + "px"; };
  input.addEventListener("input", autosize);
  // The draft follows the session: a fresh spawn's PTY key until the
  // session ID is known (keepDraft moves it over then).
  let draftRef = sid || ptyKey;
  const keepDraft = () => {
    const ref = sid || ptyKey;
    if (ref !== draftRef) { saveDraft(id, draftRef, ""); draftRef = ref; }
    saveDraft(id, draftRef, input.value);
  };
  input.value = loadDraft(id, draftRef);
  if (input.value) requestAnimationFrame(autosize);
  input.addEventListener("input", keepDraft);
  bucket.push(keepDraft); // also catches text set by the quick buttons / slash picker
  // On a touch screen, typing into a tile maximises it: the on-screen
  // keyboard would otherwise cover most of a small tile.
  // engaged / lookingSince gate MarkSeen (see poll).
  if (compact) {
    root.addEventListener("pointerdown", () => { engagedTile = root; });
    root.addEventListener("focusin", () => { engagedTile = root; });
  }
  if (compact) input.addEventListener("focus", () => {
    if (matchMedia("(pointer: coarse)").matches) root.dispatchEvent(new CustomEvent("tile-max", { bubbles: true, detail: { on: true } }));
  });
  input.addEventListener("keydown", (e) => {
    const mobile = matchMedia("(pointer: coarse)").matches;
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229 && !mobile) {
      e.preventDefault();
      composer.requestSubmit();
    }
  });

  const atBottom = () => scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80;
  // Drives Claude Code's AskUserQuestion dialog from the portal's choices,
  // mirroring the keys a person would press: a digit picks a single-select
  // option (and advances), toggles a multi-select one (→ advances); "Type
  // something" is the option after the last, then the text and Enter. With
  // several questions a review step follows; its "1" submits.
  const askCtx = {
    askCards: new Map(),
    agentFor: (toolUseId) => agents.find((a) => a.toolUseId === toolUseId),
    openAgent: (a) => openAgent(id, sid, a),
    hosted: () => hosted,
    fileURL: (path) => sid ? `/api/d/${id}/hub/file?session=${encodeURIComponent(sid)}&path=${encodeURIComponent(path)}` : null,
    runBang: (cmd) => {
      if (!hosted || !ptyKey) return toast("このセッションは ccdash 上で動いていないため実行できません", 4000);
      if (promptEl.childElementCount) return toast("Claude が選択を待っています。先に上のカードで回答してください", 4000);
      confirmBang(cmd, async (c) => {
        // "!" typed into claude's empty input switches it to bash mode;
        // the command follows as plain text, then Enter.
        await d.post(`/pty/${encodeURIComponent(ptyKey)}/input`, { keys: "!" });
        await new Promise((r) => setTimeout(r, 250));
        await d.post(`/pty/${encodeURIComponent(ptyKey)}/input`, { text: c, submit: true });
        pendingSends.push({ text: "! " + c, match: c, at: Date.now() });
        render(true);
        setTimeout(poll, 600);
      });
    },
    answer: async (qs, state) => {
      if (!hosted) throw new Error("ccdash 上で動いていないため回答できません");
      const pause = (ms) => new Promise((r) => setTimeout(r, ms));
      const send = async (body) => { await d.post(`/pty/${encodeURIComponent(ptyKey)}/input`, body); await pause(300); };
      for (let qi = 0; qi < qs.length; qi++) {
        const q = qs[qi], st = state[qi], n = (q.options || []).length;
        if (!q.multiSelect) {
          if (st.other.trim()) {
            await send({ keys: String(n + 1) });
            await send({ text: st.other.trim(), paste: true });
            await send({ keys: "\r" });
          } else {
            await send({ keys: String([...st.picks][0] + 1) });
          }
        } else {
          for (const i of [...st.picks].sort()) await send({ keys: String(i + 1) });
          await send({ keys: "\x1b[C" });
        }
      }
      await pause(700);
      const scr = await d.get(`/pty/${encodeURIComponent(ptyKey)}/text`).catch(() => null);
      if (scr && scr.rows.some((r) => /Submit answers/.test(r))) await send({ keys: "1" });
      setTimeout(poll, 800);
    },
  };

  async function loadOlder() {
    if (loadingOlder || !sid) return;
    loadingOlder = true;
    render(false);
    try {
      const stat = await d.get(`/api/sessions/${encodeURIComponent(sid)}/transcript?mode=stat`);
      const want = loadedBytes * 4;
      const full = want >= stat.size;
      const t = await d.get(full ? `/api/sessions/${encodeURIComponent(sid)}/transcript?mode=full` : tailURL(want));
      const fromBottom = scroller.scrollHeight - scroller.scrollTop;
      entries = decodeEntries(t.data);
      loadedBytes = full ? stat.size : want;
      hasOlder = !full;
      items = parseTranscript(entries);
      loadingOlder = false;
      render(false);
      scroller.scrollTop = scroller.scrollHeight - fromBottom;
    } catch (e) {
      loadingOlder = false;
      toast(e.message);
      render(false);
    }
  }

  const render = (stick) => {
    if (deferWhileSelecting(log, () => render(atBottom()))) return;
    const nodes = items.map((it) => renderItem(it, askCtx)).filter(Boolean);
    if (hasOlder && nodes.length) {
      nodes.unshift(h("button", { class: "btn older", disabled: loadingOlder, onclick: loadOlder },
        loadingOlder ? "読み込み中…" : "↑ 古い履歴を読み込む"));
    }
    pendingSends = pendingSends.filter((p) => Date.now() - p.at < 120000);
    for (const p of pendingSends) nodes.push(h("div", { class: "bubble user pending" }, p.text));
    if (!nodes.length) nodes.push(h("div", { class: "empty" }, ptyKey && !sid ? "claude を起動しました。メッセージを送って始めましょう。" : "まだメッセージがありません。"));
    if (session?.status === "active") {
      // Working: the dots, and the tool running right now (a long tool call
      // otherwise looks like nothing is happening).
      const last = items.filter((x) => x.kind === "tool").pop();
      nodes.push(h("div", { class: "working-line" }, h("div", { class: "typing" }, h("span"), h("span"), h("span")),
        last && last.result == null ? h("span", { class: "muted small mono" }, "実行中: " + toolLabel(last)) : null));
    }
    log.replaceChildren(...nodes);
    if (stick) scroller.scrollTop = scroller.scrollHeight;
  };

  async function keys(seq) {
    if (!ptyKey || !hosted) return;
    haptic();
    try { await d.post(`/pty/${encodeURIComponent(ptyKey)}/input`, { keys: seq }); } catch (e) { toast(e.message); }
    setTimeout(poll, 350);
  }

  composer.addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = input.value.replace(/\s+$/, "");
    if (!text && !attachments.length) return;
    if (hosted && promptEl.childElementCount) {
      // Keystrokes would land in the dialog (or linger in claude's input
      // and merge into the next message) — answer it first.
      toast("Claude が選択を待っています。先に上のカードで回答するか、キャンセルしてください", 4000);
      promptEl.scrollIntoView({ block: "nearest" });
      return;
    }
    sendBtn.disabled = true;
    haptic();
    try {
      if (attachments.length && !info?.attachEnabled) throw new Error("この端末は attach が OFF のため画像を送れません");
      const paths = [];
      for (const a of attachments) {
        const r = await fetch(`/api/d/${id}/hub/upload`, { method: "POST", credentials: "same-origin", headers: { "Content-Type": a.blob.type }, body: a.blob });
        if (!r.ok) throw new Error("画像のアップロードに失敗: " + ((await r.text()).trim() || r.status));
        paths.push((await r.json()).path);
      }
      if (hosted) {
        // Each path as its own paste: Claude Code turns a pasted image
        // path into an [Image #N] attachment, like drag-and-drop.
        for (const p of paths) {
          await d.post(`/pty/${encodeURIComponent(ptyKey)}/input`, { text: p, paste: true });
          await new Promise((res) => setTimeout(res, 350));
        }
        await d.post(`/pty/${encodeURIComponent(ptyKey)}/input`, { text: text ? (paths.length ? " " : "") + text : "", submit: true });
      } else {
        if (!info?.attachEnabled) throw new Error("この端末は attach が OFF のため送信できません");
        if (session && (session.status === "active" || session.status === "idle") &&
          !confirm("このセッションは ccdash 管理外のターミナルで実行中のようです。ここで別インスタンスとして再開して送信しますか？")) return;
        const prompt = [...paths, text].filter(Boolean).join(" ");
        const r = await d.post("/pty/start", {
          sessionId: sid, resumeId: sid, cwd: session?.cwd || "", cols: 120, rows: 40,
          prompt: prompt.startsWith("-") ? " " + prompt : prompt,
        });
        ptyKey = r.ptyKey;
        hosted = true;
      }
      pendingSends.push({ text: [attachments.length ? `🖼 ×${attachments.length}` : "", text].filter(Boolean).join(" "), match: text.trim(), at: Date.now() });
      attachments.forEach((a) => URL.revokeObjectURL(a.url));
      attachments = [];
      renderThumbs();
      input.value = "";
      keepDraft(); // sent: drop the draft
      autosize();
      render(true);
      setTimeout(poll, 600);
    } catch (err) { toast(err.message); }
    finally { sendBtn.disabled = false; }
  });

  async function poll() {
    const [sessions, ptys, aps] = await Promise.all([
      sharedGet(id, "/api/sessions").then((x) => x || []),
      sharedGet(id, "/pty/").then((x) => x || []).catch(() => []),
      sharedGet(id, "/api/approvals").then((x) => x || []).catch(() => []),
    ]);
    if (!name) name = await deviceName(id);
    info = info || (await deviceInfo(id));
    allGroups = uniqueGroups(sessions, true).filter(Boolean);
    if (!sid && ptyKey) {
      const a = aliasOf(ptys, ptyKey);
      if (a) { sid = a; if (!compact) history.replaceState(null, "", `#/d/${id}/s/${encodeURIComponent(sid)}`); }
    }
    if (sid) {
      if (ptys.some((p) => p.key === sid && p.alive)) ptyKey = sid;
      session = sessions.find((s) => s.session_id === sid) ||
        (await d.get("/api/sessions?archived=1").catch(() => []))?.find((s) => s.session_id === sid) || session;
    }
    hosted = !!ptyKey && ptys.some((p) => p.key === ptyKey && p.alive);
    // Open, visible and looked at for SEEN_DWELL_MS: the operator has seen
    // it (clears "done" and the NEW mark; while it works, each poll keeps
    // it seen). A grid tile counts only once engaged (focused, clicked,
    // maximised) — merely being on screen among many tiles isn't reading.
    const looking = !!sid && !document.hidden && document.hasFocus() && (!compact || engagedTile === root);
    if (!looking) lookingSince = 0;
    else if (!lookingSince) lookingSince = Date.now();
    if ((session?.attention === "done" || session?.unseen) && looking && Date.now() - lookingSince >= SEEN_DWELL_MS) {
      d.post(`/api/sessions/${encodeURIComponent(sid)}/seen`, {}).catch(() => {});
      session.attention = "";
      session.unseen = false;
    }
    freshEl.replaceChildren();
    const fc = freshChip(session);
    if (fc) { freshEl.className = fc.className; freshEl.title = fc.title; freshEl.textContent = fc.textContent; }
    freshEl.hidden = !fc;
    const wc = waitChip(session);
    if (wc) { waitEl.className = wc.className; waitEl.title = wc.title; waitEl.textContent = wc.textContent; }
    waitEl.hidden = !wc;
    if (compact) root.classList.toggle("unseen", !!session?.unseen);

    const title = session ? (session.num ? `#${session.num} ` : "") + sessionTitle(session) : sid ? sid : "新規セッション";
    titleEl.textContent = title;
    titleEl.classList.toggle("titling", session?.title_status === "running");
    titleEl.style.setProperty("--sess", session ? sessionColorOf(session) : sessionColor(sid));
    if (!compact) {
      crumbs({ text: "端末", href: "#/" }, { text: name, href: `#/d/${id}` }, { text: title });
      // Full screen (the phone app's usual view): the light runs round the
      // header while Claude is mid-turn, like a tile's frame.
      setBeam(bar, session?.status === "active");
    } else {
      root.style.setProperty("--sess", session ? sessionColorOf(session) : sessionColor(sid || ptyKey || ""));
      root.classList.toggle("needs", session?.attention === "needs_you");
      setBeam(root, session?.status === "active");
    }
    // A ccdash-hosted claude is alive even when discovery still files the
    // session as recent/stopped (no hook fired since it was resumed).
    let st = session?.status || (hosted ? "idle" : "");
    if (hosted && (st === "recent" || st === "stopped")) st = "idle";
    statusEl.textContent = { active: "作業中", idle: "待機中", recent: "停止", stopped: "停止" }[st] || st;
    statusEl.className = "chip " + st;
    statusEl.hidden = !st;
    termBtn.hidden = !hosted;
    termBtn.href = sid ? `#/d/${id}/s/${encodeURIComponent(sid)}/term` : `#/d/${id}/p/${encodeURIComponent(ptyKey)}/term`;
    endBtn.hidden = !hosted;
    restartBtn.hidden = !hosted || !sid; // nothing to resume before the first prompt
    stopBtn.hidden = !(hosted && session?.status === "active");
    stopTop.hidden = stopBtn.hidden;
    if (compact) stopBtn.hidden = true; // the tile's header has 中断
    input.placeholder = hosted ? "メッセージ" : compact ? "送信で再開" : "送信するとこのセッションを再開します";

    // approvals for this session
    renderApprovals(approvalsEl, id, sid ? aps.filter((a) => a.session_id === sid) : [], sessions);
    approvalsEl.querySelector("h2")?.remove();

    // transcript
    if (sid) {
      const stat = await d.get(`/api/sessions/${encodeURIComponent(sid)}/transcript?mode=stat`).catch(() => null);
      const sig = stat ? `${stat.mtime}/${stat.size}` : "";
      if (sig && sig !== lastStat) {
        lastStat = sig;
        const stick = atBottom() || !items.length;
        const t = await d.get(tailURL(TAIL));
        const fresh = decodeEntries(t.data);
        if (!splice(fresh)) {
          // First load, or more than TAIL bytes arrived since the last
          // poll: start the window over from the tail.
          entries = fresh;
          loadedBytes = TAIL;
          hasOlder = stat.size > TAIL;
        }
        items = parseTranscript(entries);
        const users = items.filter((x) => x.kind === "user").map((x) => x.text);
        // Drop a placeholder once its text shows up in any user turn (it may
        // be merged with other text, e.g. after an image or a queued line),
        // and after 2 minutes regardless.
        pendingSends = pendingSends.filter((p) => Date.now() - p.at < 120000 && !(p.match && users.some((u) => u.includes(p.match))));
        render(stick);
      } else {
        render(atBottom());
      }
    } else {
      render(atBottom());
    }

    // Claude Code's own status (every other poll; served from memory)
    if (sid && agentTick % 2 === 0) {
      liveSt = await d.get(`/api/sessions/${encodeURIComponent(sid)}/statusline`).catch(() => null);
    }

    // usage (every seventh poll)
    if (sid && agentTick % 7 === 0) {
      const u = await d.get(`/api/sessions/${encodeURIComponent(sid)}/usage`).catch(() => null);
      if (u?.total?.messages) { lastUsage = u; paintStatus(); }
    }

    // git changes (every fifth poll)
    if (sid && agentTick % 5 === 0) {
      const g = await d.get(`/hub/git/status?session=${encodeURIComponent(sid)}`).catch(() => null);
      const n = g?.files?.length || 0;
      diffBtn.hidden = !g?.repo;
      diffBtn.textContent = n ? `差分 ${n}` : "差分";
      diffBtn.href = `#/d/${id}/s/${encodeURIComponent(sid)}/diff`;
    }

    // subagents (every third poll: it reads a directory of transcripts)
    if (sid && agentTick++ % 3 === 0) {
      const list = await d.get(`/hub/subagents?session=${encodeURIComponent(sid)}`).catch(() => null);
      if (Array.isArray(list)) { agents = list; paintAgents(); }
    }

    // TUI prompt on the screen
    if (hosted) {
      const scr = await d.get(`/pty/${encodeURIComponent(ptyKey)}/text`).catch(() => null);
      const askPending = items.some((x) => x.kind === "tool" && x.name === "AskUserQuestion" && x.result == null);
      const pr = scr && !askPending && screenPrompt(scr.rows);
      if (!pr) promptEl.replaceChildren();
      else if (promptEl.dataset.key !== pr.key) {
        promptEl.dataset.key = pr.key;
        promptEl.replaceChildren(pr.kind === "ask" ? askScreenCard(pr, keys, d, ptyKey) : h("div", { class: "approval" },
          h("div", { class: "row" }, h("b", { class: "grow" }, pr.title), h("button", { class: "btn small", onclick: () => keys("\x1b") }, "Esc")),
          (() => { const pre = h("pre", { class: "mono screen" }, pr.context); requestAnimationFrame(() => (pre.scrollTop = pre.scrollHeight)); return pre; })(),
          h("div", { class: "opts" }, pr.options.map((o) => h("button", { class: "btn", onclick: () => keys(o.keys) }, o.label)))));
      }
      if (!pr) delete promptEl.dataset.key;
    } else {
      promptEl.replaceChildren();
    }
    paintStatus();
  }

  every(compact ? 3000 : 1500, poll, bucket);
  if (!compact) autoFocus(input);
  return {
    el: root,
    destroy: () => bucket.splice(0).forEach((fn) => { try { fn(); } catch {} }),
    scrollBottom: () => requestAnimationFrame(() => (scroller.scrollTop = scroller.scrollHeight)),
  };
}



// ---------- terminal ----------

const KEYS = [
  ["Esc", "\x1b"], ["Tab", "\t"], ["⇧Tab", "\x1b[Z"], ["↑", "\x1b[A"], ["↓", "\x1b[B"],
  ["←", "\x1b[D"], ["→", "\x1b[C"], ["^C", "\x03"], ["Enter", "\r"],
];

const END_CONFIRM = "このセッションの claude を終了してアーカイブしますか？（後から再開・アーカイブ解除できます）";

// endAndArchive stops a hosted claude and archives its session. A fresh
// pid-<N> spawn is archived under the session id it was aliased to; one that
// never registered a session has nothing to archive.
async function endAndArchive(d, key, sid) {
  if (!sid) sid = key.startsWith("pid-") ? aliasOf((await d.get("/pty/").catch(() => [])) || [], key) : key;
  await d.del(`/pty/${encodeURIComponent(key)}`);
  if (!sid) return "終了しました";
  await d.post(`/api/sessions/${encodeURIComponent(sid)}/archive`, { archived: true });
  return "終了してアーカイブしました";
}

function terminal(id, key, title, chatHref) {
  const host = mainMount;
  const mount = h("div", { id: "term" });
  const sendInput = h("input", { placeholder: "テキストを送信（日本語入力向け・Enter で送信+改行）", enterkeyhint: "send", autocomplete: "off" });
  let ws = null, term = null, fit = null, closedByUs = false;

  const send = (data) => {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(enc.encode(data));
  };
  const bar = h("div", { class: "term-bar" },
    h("a", { class: "btn small icon", href: chatHref, "aria-label": "戻る", title: "戻る" }, icon("back")),
    h("span", { class: "grow title" }, title),
    h("span", { class: "muted small hide-sm" }, key),
    h("a", { class: "btn small", href: chatHref, title: "ターミナルの接続を切ってチャット表示に戻ります。claude は端末上で動き続けます" }, "チャット"),
    h("button", { class: "btn small danger", title: "claude を終了してアーカイブする", onclick: async () => {
      if (!confirm(END_CONFIRM)) return;
      closedByUs = true;
      try { toast(await endAndArchive(dev(id), key)); location.hash = `#/d/${id}`; } catch (e) { toast(e.message); }
    } }, "終了"));
  const keys = h("div", { class: "keys" }, KEYS.map(([label, seq]) =>
    h("button", { class: "btn", onclick: () => { send(seq); term?.focus(); } }, label)));
  const sendbar = h("form", { class: "sendbar", onsubmit: (e) => {
    e.preventDefault();
    if (!sendInput.value) { send("\r"); return; }
    send(sendInput.value);
    // Give the TUI a beat to take the text before the submitting Enter.
    setTimeout(() => send("\r"), 30);
    sendInput.value = "";
  } }, sendInput, h("button", { class: "btn" }, "送信"));
  const wrap = h("div", { class: "term-wrap" }, bar, mount, sendbar, keys);
  host.append(wrap);

  term = new Terminal({
    fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono").trim() || "monospace",
    fontSize: window.innerWidth < 640 ? 12 : 14,
    cursorBlink: true,
    scrollback: 5000,
    theme: { background: "#111418" },
    allowProposedApi: true,
  });
  fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  try { term.loadAddon(new WebLinksAddon.WebLinksAddon()); } catch {}
  term.open(mount);
  fit.fit();
  autoFocus(term);

  const connect = () => {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(`${proto}//${location.host}/api/d/${id}/ws/pty/${encodeURIComponent(key)}?cols=${term.cols}&rows=${term.rows}`);
    ws.binaryType = "arraybuffer";
    let opened = false;
    ws.onopen = () => { opened = true; };
    ws.onmessage = (e) => term.write(new Uint8Array(e.data));
    ws.onclose = async () => {
      if (closedByUs) return;
      let why = "切断されました。";
      if (!opened) {
        // The upgrade was refused; ask the device why.
        try {
          const ptys = await dev(id).get("/pty/");
          why = ptys.some((p) => p.key === key && p.alive)
            ? "接続できませんでした。別のクライアント（端末の ccdash の全画面 attach や別タブ）が接続中の可能性があります。"
            : "セッションが見つかりません（終了した可能性があります）。";
        } catch (e) { why = "接続できませんでした: " + e.message; }
      }
      term.write(`\r\n\x1b[33m[ccdash] ${why}\x1b[0m\r\n`);
      bar.querySelector(".reconnect")?.remove();
      bar.insertBefore(h("button", { class: "btn small primary reconnect", onclick: (ev) => { ev.target.remove(); connect(); } }, "再接続"), bar.children[3]);
    };
  };
  term.onData(send);
  term.onBinary((d) => { if (ws?.readyState === WebSocket.OPEN) ws.send(Uint8Array.from(d, (c) => c.charCodeAt(0))); });
  term.onResize(({ cols, rows }) => {
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols, rows }));
  });
  const ro = new ResizeObserver(() => { try { fit.fit(); } catch {} });
  ro.observe(mount);
  connect();

  cleanup.push(() => {
    closedByUs = true;
    ro.disconnect();
    try { ws?.close(); } catch {}
    try { term.dispose(); } catch {}
  });
}

// ---------- notifications (Web Push) ----------

function b64urlToBytes(s) {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

async function pushSubscription() {
  const reg = await navigator.serviceWorker?.ready;
  return reg ? reg.pushManager.getSubscription() : null;
}

// ---- look: theme + accent (per user, /api/prefs; theme.js applies) ----

const ACCENTS = [
  ["#e8590c", "オレンジ"], ["#d97706", "アンバー"], ["#e03131", "レッド"], ["#d6336c", "ピンク"],
  ["#7048e8", "バイオレット"], ["#4263eb", "インディゴ"], ["#1c7ed6", "ブルー"], ["#0c8599", "シアン"],
  ["#099268", "ティール"], ["#2f9e44", "グリーン"], ["#495057", "グラファイト"],
];
let look = {};
try { look = JSON.parse(localStorage.getItem("ccdash.look") || "{}"); } catch {}
function setLook(p, save = true) {
  look = { theme: p.theme || "", accent: p.accent || "" };
  try { localStorage.setItem("ccdash.look", JSON.stringify(look)); } catch {}
  window.applyLook?.(look);
  if (save) api("/api/prefs", { method: "PUT", json: look }).catch((e) => toast("設定を保存できませんでした: " + e.message));
}
// loadLook picks up the hub's copy (another device may have changed it).
function loadLook() {
  api("/api/prefs").then((p) => { if (p && (p.theme !== look.theme || p.accent !== look.accent)) setLook(p, false); }).catch(() => {});
}
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => window.applyLook?.(look));

function lookDialog() {
  dialog((body, close) => {
    const seg = h("div", { class: "seg wide" });
    const paintSeg = () => seg.replaceChildren(...[["", "自動"], ["light", "ライト"], ["dark", "ダーク"]].map(([v, label]) =>
      h("button", { type: "button", class: "btn" + ((look.theme || "") === v ? " on" : ""), onclick: () => { setLook({ ...look, theme: v }); paintSeg(); } }, label)));
    const sw = h("div", { class: "swatches" });
    const custom = h("input", { type: "color", class: "swatch-custom", title: "好きな色", value: look.accent || "#e8590c" });
    custom.addEventListener("change", () => { setLook({ ...look, accent: custom.value }); paintSw(); });
    const paintSw = () => sw.replaceChildren(
      ...ACCENTS.map(([c, name]) => h("button", { type: "button", class: "swatch" + ((look.accent || "#e8590c") === c ? " on" : ""), style: `--c:${c}`, title: name, "aria-label": name,
        onclick: () => { setLook({ ...look, accent: c === "#e8590c" ? "" : c }); paintSw(); } })),
      custom);
    paintSeg(); paintSw();
    const beamOn = () => { try { return localStorage.getItem("ccdash.beam") !== "off"; } catch { return true; } };
    const beam = h("input", { type: "checkbox", checked: beamOn() });
    beam.addEventListener("change", () => {
      try { localStorage.setItem("ccdash.beam", beam.checked ? "on" : "off"); } catch {}
      document.documentElement.classList.toggle("no-beam", !beam.checked);
      refreshBeams();
    });
    body.append(h("h1", {}, "表示"),
      h("label", {}, "テーマ"), seg,
      h("label", {}, "アクセントカラー"), sw,
      h("label", { class: "check" }, beam, "作業中のセッションの枠をアニメーションする"),
      h("p", { class: "muted small" }, "この設定はアカウントごとに保存され、ブラウザとアプリで共通です（アニメーションのみこの端末だけ）。"),
      h("div", { class: "actions" }, h("button", { class: "btn primary" }, "閉じる")));
  });
}

function lookButton() {
  try { document.documentElement.classList.toggle("no-beam", localStorage.getItem("ccdash.beam") === "off"); } catch {}
  return h("button", { class: "btn small icon", title: "表示設定", "aria-label": "表示設定", onclick: lookDialog }, icon("settings"));
}

function notifyButton() {
  if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) return null;
  const btn = h("button", { class: "btn small icon bell", title: "通知" }, icon("bell-off"));
  const paint = async () => {
    const on = Notification.permission === "granted" && !!(await pushSubscription().catch(() => null));
    btn.replaceChildren(icon(on ? "bell" : "bell-off"));
    btn.title = on ? "通知 ON（タップで設定）" : "通知 OFF（タップで ON）";
    btn.dataset.on = on ? "1" : "";
  };
  btn.addEventListener("click", async () => {
    try {
      if (btn.dataset.on) {
        dialog((body, close) => body.append(
          h("h1", {}, "通知"),
          h("p", {}, "承認待ち・Claude からの質問や確認・作業の完了を、このブラウザに通知します。"),
          h("div", { class: "actions" },
            h("button", { type: "button", class: "btn", onclick: () => { close(); pushDiagnose(); } }, "テスト通知"),
            h("button", { type: "button", class: "btn danger", onclick: async () => {
              const sub = await pushSubscription();
              if (sub) { await api("/api/push/unsubscribe", { method: "POST", json: { endpoint: sub.endpoint } }); await sub.unsubscribe(); }
              close(); paint(); toast("通知を OFF にしました");
            } }, "OFF にする"),
            h("button", { class: "btn primary" }, "閉じる"))));
        return;
      }
      if ((await Notification.requestPermission()) !== "granted") { pushDiagnose(); return; }
      const { publicKey } = await api("/api/push/key");
      const reg = await navigator.serviceWorker.ready;
      const sub = (await reg.pushManager.getSubscription()) ||
        await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(publicKey) });
      await api("/api/push/subscribe", { method: "POST", json: sub.toJSON() });
      toast("通知を ON にしました");
      paint();
    } catch (e) { paint(); pushDiagnose(); }
  });
  paint();
  return btn;
}

// pushDiagnose is the notification test: it walks the chain one link at
// a time — permission, the browser showing a notification by itself, this
// browser's subscription on the hub, hub → push service (the service's
// answer), push service → this browser (the service worker reports back)
// — so a failure names the link that broke.
function pushDiagnose() {
  dialog((body, close) => {
    const rows = h("div", { class: "diag" });
    const step = (title) => {
      const mark = h("span", { class: "diag-mark" }, icon("clock"));
      const note = h("div", { class: "muted small" });
      const row = h("div", { class: "diag-row" }, mark, h("div", { class: "grow" }, h("div", {}, title), note));
      rows.append(row);
      const set = (state, text) => {
        row.dataset.state = state;
        mark.replaceChildren(icon(state === "ok" ? "check" : state === "bad" ? "x" : "clock"));
        note.textContent = text || "";
      };
      return { ok: (t) => set("ok", t), bad: (t) => set("bad", t), wait: (t) => set("wait", t) };
    };
    // Dummy notifications: a real-looking one through the real path, now
    // or later (switch to another app meanwhile). Each has its own tag, so
    // none silently replaces an earlier one in the notification center.
    const dummyNote = h("div", { class: "muted small" });
    const dummy = (delay) => async () => {
      try {
        const sub = await pushSubscription();
        if (!sub) { dummyNote.textContent = "このブラウザはまだ購読していません（上のテストを先に）"; return; }
        const res = await api("/api/push/test", { method: "POST", json: { endpoint: sub.endpoint, dummy: true, delay } });
        if (delay) dummyNote.textContent = `${delay} 秒後に送ります。Chrome 以外のアプリに切り替えて待ってください。`;
        else dummyNote.textContent = res.ok ? `${res.service} が受け付けました。画面右上に出るはずです。` : "送れませんでした: " + res.error;
      } catch (e) { dummyNote.textContent = "送れませんでした: " + e.message; }
    };
    const dummyBox = h("div", { class: "diag-dummy" },
      h("h2", {}, "ダミー通知を送る"),
      h("p", { class: "muted small" }, "本物の「要対応」通知と同じ経路・同じ形で送ります。"),
      h("div", { class: "row" },
        h("button", { type: "button", class: "btn", onclick: dummy(0) }, "今すぐ"),
        h("button", { type: "button", class: "btn", onclick: dummy(10) }, "10 秒後"),
        h("button", { type: "button", class: "btn", onclick: dummy(30) }, "30 秒後")),
      dummyNote,
      h("details", { class: "small" },
        h("summary", {}, "テストは通るのに画面に出ないとき（macOS）"),
        h("ul", {},
          h("li", {}, "システム設定 → 通知 → Google Chrome:「通知を許可」を ON、通知のスタイルを「バナー」か「通知パネル」に（「なし」だと通知センターにしか入りません）"),
          h("li", {}, "集中モード（おやすみモード）が ON だと出ません。コントロールセンターで確認"),
          h("li", {}, "画面右上の時計をクリックして通知センターを開き、ccdash の通知が溜まっていないか確認"),
          h("li", {}, "Chrome の設定 → プライバシーとセキュリティ → サイトの設定 → 通知 で、このサイト（127.0.0.1）が「許可」か"))));
    body.append(h("h1", {}, "通知のテスト"), rows, dummyBox,
      h("div", { class: "actions" }, h("button", { class: "btn primary" }, "閉じる")));
    run().catch((e) => toast(e.message, 5000));

    async function run() {
      const perm = step("ブラウザの通知許可");
      if (!("serviceWorker" in navigator) || !("PushManager" in window)) { perm.bad("このブラウザは Web Push に対応していません"); return; }
      if (Notification.permission === "default") await Notification.requestPermission();
      if (Notification.permission !== "granted") {
        perm.bad("このサイトの通知がブロックされています。アドレスバー左のアイコン → 通知 を「許可」にして、もう一度試してください");
        return;
      }
      perm.ok("許可されています");

      const reg = await navigator.serviceWorker.ready;
      const local = step("ブラウザだけで通知を表示");
      try {
        await reg.showNotification("ccdash（ブラウザ内テスト）", { body: "ハブを通さずに表示しています", tag: "test-local", icon: "icon-192.png" });
        local.ok("ブラウザが OS に表示を頼みました（画面に出たかはブラウザには分かりません。出ていなければ下の「画面に出ないとき」を）");
      } catch (e) { local.bad("表示できませんでした: " + e.message); }

      const subStep = step("このブラウザの購読");
      let sub = await reg.pushManager.getSubscription();
      try {
        if (!sub) {
          const { publicKey } = await api("/api/push/key");
          sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(publicKey) });
        }
        // Register again: harmless, and repairs a hub that lost it.
        await api("/api/push/subscribe", { method: "POST", json: sub.toJSON() });
        subStep.ok("登録済み（" + new URL(sub.endpoint).host + "）");
      } catch (e) {
        subStep.bad("購読できませんでした: " + e.message + "（ブラウザがプッシュサービスに接続できていない可能性があります）");
        return;
      }

      const send = step("ハブ → プッシュサービス");
      const recv = step("プッシュサービス → このブラウザ");
      recv.wait("待っています…");
      const got = new Promise((resolve) => {
        const on = (e) => {
          if (e.data?.type !== "push-received") return;
          navigator.serviceWorker.removeEventListener("message", on);
          resolve(e.data);
        };
        navigator.serviceWorker.addEventListener("message", on);
        setTimeout(() => { navigator.serviceWorker.removeEventListener("message", on); resolve(null); }, 20000);
      });
      let res;
      try { res = await api("/api/push/test", { method: "POST", json: { endpoint: sub.endpoint } }); } catch (e) {
        send.bad(e.message);
        recv.bad("");
        return;
      }
      if (!res.ok) {
        send.bad(res.status
          ? `${res.service} が拒否しました: ${res.error}。🔔 を OFF にしてから ON にし直すと直ることがあります`
          : `${res.service} に届きませんでした: ${res.error}。プロキシのある環境では ccdash（collector / ccdash web）に HTTPS_PROXY を渡してください（launchd なら plist の EnvironmentVariables）`);
        recv.bad("");
        return;
      }
      send.ok(`${res.service} が受け付けました（HTTP ${res.status}）`);
      const r = await got;
      if (!r) recv.bad("20 秒待っても届きません。ブラウザがプッシュサービスに接続できていない可能性があります（ネットワークで遮断されている、ブラウザが省電力で止まっている など）");
      else if (!r.shown) recv.bad("届きましたが表示できませんでした: " + r.error);
      else recv.ok("届きました。ブラウザは OS に表示を頼んでいます（画面に出ていなければ OS 側の設定です）");
    }
  });
}

// updateBadge mirrors the board onto the installed app's icon badge, the
// tab title ("(3) ccdash hub") and a header chip: how many sessions need
// the operator (+ unread finishes). The hub's push carries the same count
// for the service worker (notify.go badgeCount).
let BASE_TITLE = document.title;
function updateBadge(b) {
  const needs = (b?.needs_you || []).length, done = (b?.done || []).length;
  try {
    if (needs + done > 0) navigator.setAppBadge?.(needs + done)?.catch?.(() => {});
    else navigator.clearAppBadge?.()?.catch?.(() => {});
  } catch {}
  document.title = needs + done > 0 ? `(${needs + done}) ${BASE_TITLE}` : BASE_TITLE;
  const el = $("#att");
  if (!el) return;
  el.hidden = needs + done === 0;
  el.textContent = needs ? `要対応 ${needs}` : `未確認 ${done}`;
  el.classList.toggle("needs", needs > 0);
}

// ---------- tasks (docs/decisions/0006-tasks.md) ----------

// The to-do list each device's collector keeps. Claude edits it through
// ccdash mcp; here the operator ticks things off, picks the day and reads
// the progress. Buckets and their headings come from the device, so the
// date rules live in Go only. Inside a device the list is that device's;
// on the home page and the grid it is every online device's, merged.
const TASK_STATUS = { todo: "未着手", doing: "進行中", waiting: "待ち", done: "完了", cancelled: "中止" };
const TASK_PLAN = [["today", "今日"], ["tomorrow", "明日"], ["week", "今週"], ["nextweek", "来週"], ["none", "未定"]];
const taskFinished = (t) => t.status === "done" || t.status === "cancelled";
const shortDay = (d) => { const m = /^\d{4}-(\d\d)-(\d\d)$/.exec(d || ""); return m ? `${+m[1]}/${+m[2]}` : d; };

// taskScope: the device the current page is about, "" for all of them.
function taskScope() {
  const p = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  return p[0] === "d" && p[1] ? p[1] : p[0] === "tasks" && p[1] ? p[1] : "";
}

// loadTaskLists fetches the lists ({dev, list}) of one device or of every
// online one; a device whose collector has no task routes is left out.
async function loadTaskLists(only) {
  let devs;
  if (only) devs = [{ id: only, name: deviceCache.get(only)?.name || "" }];
  else {
    const ds = await api("/api/devices");
    ds.forEach((d) => deviceCache.set(d.id, d));
    devs = ds.filter((d) => d.online);
  }
  const got = await Promise.all(devs.map((d) => dev(d.id).get("/api/tasks").then((list) => ({ dev: d, list })).catch(() => null)));
  return got.filter(Boolean);
}

// mergeTasks lays several devices' lists out by bucket. One device keeps
// its own order (manual order first); across devices a bucket is ordered
// by priority, then due date.
function mergeTasks(lists, buckets) {
  const out = [];
  const keys = lists[0]?.list.buckets.map((b) => b.key) || [];
  for (const k of keys) {
    if (buckets && !buckets.includes(k)) continue;
    if (k === "finished") continue;
    const heading = lists[0].list.buckets.find((b) => b.key === k)?.heading || k;
    let rows = [];
    for (const { dev: d, list } of lists) for (const t of list.tasks || []) if (t.bucket === k) rows.push({ dev: d, t, today: list.today });
    if (lists.length > 1) rows.sort((a, b) => (a.t.priority || 4) - (b.t.priority || 4) || (a.t.due || "9999").localeCompare(b.t.due || "9999"));
    if (rows.length) out.push({ key: k, heading, rows });
  }
  return out;
}

async function editTask(d, t, patch, done) {
  try {
    await dev(d.id).post(`/api/tasks/${t.id}`, patch);
    haptic();
    if (done) toast(done);
  } catch (e) { toast(e.message); }
}

// taskNewSession opens the new-session sheet in the task's project
// directory with "#N について、" as the first message.
async function taskNewSession(d, t) {
  let cwd = "";
  if (t.project) {
    try {
      const ss = await dev(d.id).get("/api/sessions?archived=0");
      cwd = ss.filter((s) => s.project === t.project && s.cwd).sort((a, b) => (b.last_seen || "").localeCompare(a.last_seen || ""))[0]?.cwd || "";
    } catch {}
  }
  newSession(d.id, { cwd, project: t.project || "", prompt: `#${t.id} について、` });
}

function taskRow({ dev: d, t, today }, ctx) {
  const key = d.id + ":" + t.id;
  const open = ctx.open.has(key);
  const fin = taskFinished(t);
  const meta = [];
  if (t.status === "doing" || t.status === "waiting") meta.push(h("span", { class: "tstat " + t.status }, TASK_STATUS[t.status]));
  if (!fin && t.plan && t.plan < today) meta.push(h("span", { class: "tcarried" }, shortDay(t.plan) + " から持ち越し"));
  else if (!fin && t.plan && ["week", "nextweek", "later"].includes(t.bucket)) meta.push(h("span", {}, shortDay(t.plan)));
  if (!fin && t.due) meta.push(h("span", { class: t.due <= today ? "tdue over" : "tdue" }, "期限 " + shortDay(t.due)));
  if (t.project) meta.push(h("span", { class: "tproj" }, h("i", { style: `background:${projectColorOf(t.project)}` }), t.project));
  if (ctx.multi) meta.push(h("span", { class: "chip" }, d.name || d.id));
  const check = h("button", { class: "task-check" + (fin ? " on" : ""), title: fin ? "未着手に戻す" : "完了にする", "aria-label": fin ? "未着手に戻す" : "完了にする",
    onclick: async (e) => { e.stopPropagation(); await editTask(d, t, { status: fin ? "todo" : "done" }); ctx.refresh(); } }, fin ? icon("check") : null);
  const head = h("div", { class: "task-main", role: "button", tabindex: "0",
    onclick: () => { open ? ctx.open.delete(key) : ctx.open.add(key); ctx.paint(); },
    onkeydown: (e) => { if (e.key === "Enter") e.currentTarget.click(); } },
    h("div", { class: "task-title" + (fin ? " fin" : "") + (t.priority === 1 && !fin ? " p1" : "") }, h("span", { class: "num mono" }, "#" + t.id), " ", t.title),
    meta.length ? h("div", { class: "task-meta" }, ...meta) : null);
  return h("div", { class: "task" + (open ? " open" : "") }, check, head, open ? taskDetail(d, t, ctx) : null);
}

function taskDetail(d, t, ctx) {
  const act = (patch, msg) => async () => { await editTask(d, t, patch, msg); ctx.refresh(); };
  const plans = h("div", { class: "seg" }, ...TASK_PLAN.map(([v, label]) => {
    const on = v === "none" ? !t.plan : t.bucket === v || (v === "today" && t.bucket === "today");
    return h("button", { class: "btn small" + (on ? " on" : ""), onclick: act({ plan: v }, `#${t.id} のやる日: ${label}`) }, label);
  }));
  const statuses = h("div", { class: "seg" }, ...["todo", "doing", "waiting", "cancelled"].map((s) =>
    h("button", { class: "btn small" + (t.status === s ? " on" : ""), onclick: act({ status: s }) }, TASK_STATUS[s])));
  const links = (t.links || []).map((l) => /^https?:\/\//.test(l)
    ? h("a", { href: l, target: "_blank", rel: "noopener noreferrer", class: "tlink" }, l.replace(/^https?:\/\//, ""))
    : h("span", { class: "tlink mono" }, l));
  const tl = [...(t.timeline || [])].reverse().slice(0, ctx.compact ? 5 : 20);
  const fmt = (at) => { const x = new Date(at); return isNaN(x) ? at : `${x.getMonth() + 1}/${x.getDate()} ${String(x.getHours()).padStart(2, "0")}:${String(x.getMinutes()).padStart(2, "0")}`; };
  const SRC = { slack: "Slack", jira: "Jira", github: "GitHub", manual: "手書き", auto: "自動", claude: "Claude" };
  return h("div", { class: "task-detail" },
    h("div", { class: "row wrap" }, h("span", { class: "muted small" }, "やる日"), plans),
    h("div", { class: "row wrap" }, h("span", { class: "muted small" }, "状態"), statuses),
    t.note ? h("div", { class: "task-note" }, t.note) : null,
    links.length ? h("div", { class: "task-links" }, ...links) : null,
    h("div", { class: "row wrap" },
      h("button", { class: "btn small", onclick: () => copy(`#${t.id} について、`) }, "「#" + t.id + " について、」をコピー"),
      h("button", { class: "btn small", onclick: () => taskNewSession(d, t) }, icon("plus"), "このタスクで新規セッション"),
      taskFinished(t) ? h("button", { class: "btn small", onclick: act({ archived: true }, `#${t.id} をアーカイブしました`) }, "アーカイブ") : null),
    h("div", { class: "task-tl" },
      tl.length ? null : h("div", { class: "muted small" }, "進捗はまだありません"),
      ...tl.map((e) => h("div", { class: "tl-row" },
        h("span", { class: "muted small mono" }, fmt(e.at)), h("span", { class: "muted small" }, SRC[e.source] || e.source),
        h("div", { class: "tl-text" }, e.text, e.link ? h("a", { href: e.link, target: "_blank", rel: "noopener noreferrer", class: "small" }, " リンク") : null)))));
}

// taskList renders a list into el and keeps it fresh. opts: only (device
// id, "" = all), buckets (keys to show), compact, bucket (cleanup list).
function taskList(el, opts = {}) {
  const ctx = { open: new Set(), multi: false, compact: !!opts.compact, lists: [] };
  ctx.paint = () => {
    if (deferWhileSelecting(el, ctx.paint)) return;
    const groups = mergeTasks(ctx.lists, opts.buckets);
    ctx.multi = ctx.lists.length > 1;
    if (!ctx.lists.length) { el.replaceChildren(h("div", { class: "muted small task-empty" }, "タスクを返す端末がありません（ccdash を更新すると使えます）")); return; }
    if (!groups.length) { el.replaceChildren(h("div", { class: "muted small task-empty" }, opts.buckets ? "今日・明日のタスクはありません" : "タスクはありません。Claude に「タスクにして」と頼むと増えます")); return; }
    el.replaceChildren(...groups.map((g) => h("section", { class: "task-group" },
      h("h3", {}, g.heading, h("span", { class: "count" }, g.rows.length)),
      ...g.rows.map((r) => taskRow(r, ctx)))));
    opts.onCount?.(groups.reduce((n, g) => n + g.rows.filter((r) => !taskFinished(r.t)).length, 0));
  };
  ctx.refresh = async () => {
    try { ctx.lists = await loadTaskLists(opts.only ?? taskScope()); } catch (e) { if (e.status !== 401) console.warn(e); return; }
    ctx.paint();
  };
  el.replaceChildren(h("div", { class: "muted small task-empty" }, "読み込み中…"));
  every(5000, ctx.refresh, opts.bucket || cleanup);
  return ctx;
}

// The side pane (wide screens): docked right like the preview sidebar, on
// every page including the grid; the header button opens / closes it and
// the choice is kept per browser.
let taskPane = null;
const tasksOpenKey = "ccdash.tasksOpen";
function setTaskPane(on) {
  try { localStorage.setItem(tasksOpenKey, on ? "1" : "0"); } catch {}
  $("#taskstoggle")?.classList.toggle("on", on);
  if (!on) {
    taskPane?.stop.forEach((f) => f());
    taskPane?.el.remove();
    taskPane = null;
    document.body.classList.remove("tasks-open");
    requestAnimationFrame(() => window.dispatchEvent(new Event("resize")));
    return;
  }
  if (taskPane) return taskPane.list.refresh();
  const body = h("div", { class: "task-body" });
  const title = h("span", { class: "grow" }, "タスク");
  const scope = h("span", { class: "muted small" });
  const el = h("aside", { id: "taskpane", "aria-label": "タスク" },
    h("div", { class: "task-bar" }, title, scope,
      h("button", { class: "btn small icon", title: "更新", "aria-label": "更新", onclick: () => taskPane?.list.refresh() }, icon("restart")),
      h("button", { class: "btn small icon", title: "閉じる", "aria-label": "閉じる", onclick: () => setTaskPane(false) }, icon("x"))),
    body);
  document.body.append(el);
  document.body.classList.add("tasks-open");
  const stop = [];
  taskPane = { el, stop, scopeEl: scope, titleEl: title, only: null, list: null };
  taskPane.list = taskList(body, { only: null, bucket: stop, onCount: (n) => { title.textContent = `タスク ${n}`; } });
  paintTaskScope();
  requestAnimationFrame(() => window.dispatchEvent(new Event("resize")));
}

// paintTaskScope follows the route: a device's own list inside it, all of
// them elsewhere.
function paintTaskScope() {
  if (!taskPane) return;
  const only = taskScope();
  taskPane.scopeEl.textContent = only ? (deviceCache.get(only)?.name || "") : (me?.local ? "" : "すべての端末");
  if (taskPane.only !== only) { taskPane.only = only; taskPane.list.refresh(); }
}

function installTaskPane() {
  const btn = $("#taskstoggle");
  if (!btn) return;
  btn.prepend(icon("tasks"));
  btn.addEventListener("click", () => {
    if (!wideQuery.matches) { location.hash = "#/tasks"; return; }
    setTaskPane(!taskPane);
  });
  let on = true;
  try { on = localStorage.getItem(tasksOpenKey) !== "0"; } catch {}
  if (on && wideQuery.matches) setTaskPane(true);
  wideQuery.addEventListener("change", () => { if (!wideQuery.matches) setTaskPane(false); else if (localStorage.getItem(tasksOpenKey) !== "0") setTaskPane(true); });
}

// tasksPage is the full-page list (phones: the タスク tab).
function tasksPage(only) {
  crumbs({ text: "端末", href: "#/" }, { text: "タスク" });
  const list = h("div", { class: "task-body" });
  view.append(h("div", { class: "page" }, h("h1", {}, "タスク"), list));
  taskList(list, { only: only || "" });
}

// The session list folds away on wide screens (the header's sidebar
// button), leaving the session and the tasks.
const listFoldKey = "ccdash.listFolded";
function paintListFold() {
  let folded = false;
  try { folded = localStorage.getItem(listFoldKey) === "1"; } catch {}
  document.querySelector(".split")?.classList.toggle("folded", folded);
  const b = $("#listtoggle");
  if (b) { b.hidden = !split; b.classList.toggle("on", !folded); b.title = folded ? "セッション一覧を出す" : "セッション一覧をたたむ"; }
}
function installListFold() {
  const b = $("#listtoggle");
  if (!b) return;
  b.append(icon("sidebar"));
  b.addEventListener("click", () => {
    let folded = false;
    try { folded = localStorage.getItem(listFoldKey) === "1"; localStorage.setItem(listFoldKey, folded ? "0" : "1"); } catch {}
    paintListFold();
    requestAnimationFrame(() => window.dispatchEvent(new Event("resize")));
  });
}

// ---------- boot ----------

(async function boot() {
  try {
    const next = sessionStorage.getItem("ccdash.next");
    if (next) { sessionStorage.removeItem("ccdash.next"); if (!location.hash || location.hash === "#/") location.hash = next; }
  } catch {}
  try {
    me = await api("/api/me");
    if (me.local) {
      // The local portal (ccdash web): this machine only — no devices to
      // add, no logout (ADR 0005).
      document.body.classList.add("local");
      BASE_TITLE = document.title = "ccdash web";
      const b = document.querySelector(".brand span");
      if (b) b.textContent = "web";
    }
    loadLook();
    $("#me").replaceChildren(h("span", {}, lookButton(), notifyButton(), h("span", { class: "email" }, me.email), me.noAuth ? null : h("a", { href: "/auth/logout" }, "ログアウト")));
  } catch {}
  // Reload into a new hub build: a single-page app otherwise keeps running
  // the JS it loaded, however long the tab stays open. Wait while the user
  // is mid-message so nothing typed is lost.
  const checkUpdate = async () => {
    try {
      const m = await api("/api/me");
      if (!me?.assets || !m.assets || m.assets === me.assets) return;
      const busy = [...document.querySelectorAll("textarea, input")].some((x) => x.value) || document.querySelector(".thumb");
      if (!busy) location.reload();
      else toast("ポータルが更新されました。入力を送ったら再読み込みしてください", 6000);
    } catch {}
  };
  setInterval(checkUpdate, 30000);
  // Keep the badge / header chip current on every page.
  const badgeTick = () => api("/api/board").then(updateBadge).catch(() => {});
  badgeTick();
  setInterval(badgeTick, 15000);
  // Installable as an app (PWA); the worker caches nothing.
  if ("serviceWorker" in navigator) navigator.serviceWorker.register("sw.js").catch(() => {});
  // A tapped notification asks an open window to show its session.
  navigator.serviceWorker?.addEventListener("message", (e) => {
    if (e.data?.type === "open" && e.data.url) location.href = e.data.url;
  });
  document.addEventListener("visibilitychange", () => { if (!document.hidden) { checkUpdate(); badgeTick(); } });
  window.addEventListener("hashchange", () => { noteHash(); navigate(); });
  for (const sp of document.querySelectorAll("[data-icon]")) sp.replaceWith(icon(sp.dataset.icon));
  installSheet();
  $("#navback").append(icon("chevron"));
  $("#tabbar").addEventListener("click", () => haptic());
  document.addEventListener("click", followPortalLink);
  installSwipeBack();
  installTaskPane();
  installListFold();
  wideQuery.addEventListener("change", () => { closeSplit(); route(); });
  route();
})();
