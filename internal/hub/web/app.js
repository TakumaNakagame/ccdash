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

async function api(path, opts = {}) {
  const init = { credentials: "same-origin", ...opts, headers: { ...(opts.headers || {}) } };
  if (opts.json !== undefined) {
    init.body = JSON.stringify(opts.json);
    init.headers["Content-Type"] = "application/json";
    delete init.json;
  }
  const r = await fetch(path, init);
  if (r.status === 401) {
    try { sessionStorage.setItem("ccdash.next", location.hash); } catch {}
    location.href = "/auth/login?next=/";
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

function every(ms, fn) {
  let stopped = false, timer;
  const loop = async () => {
    try { await fn(); } catch (e) { if (e.status !== 401) console.warn(e); }
    if (!stopped) timer = setTimeout(loop, ms);
  };
  loop();
  cleanup.push(() => { stopped = true; clearTimeout(timer); });
}

function crumbs(...parts) {
  const nav = $("#crumbs");
  nav.replaceChildren();
  parts.forEach((p, i) => {
    if (i) nav.append(" / ");
    nav.append(p.href ? h("a", { href: p.href }, p.text) : h("span", {}, p.text));
  });
}

function dialog(build) {
  const dlg = $("#dlg"), body = $("#dlg-body");
  body.replaceChildren();
  build(body, () => dlg.close());
  dlg.showModal();
  return dlg;
}

async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast("コピーしました"); }
  catch { toast("コピーできませんでした（手動で選択してください）"); }
}

const deviceCache = new Map(); // id → device row, for crumbs

// ---------- router ----------

function route() {
  cleanup.forEach((f) => { try { f(); } catch {} });
  cleanup = [];
  view.replaceChildren();
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  if (parts[0] === "d" && parts[1]) {
    if (parts[2] === "s" && parts[3] && parts[4] === "diff") return diffPage(parts[1], parts[3]);
    if (parts[2] === "s" && parts[3]) return sessionPage(parts[1], parts[3], parts[4]);
    if (parts[2] === "p" && parts[3]) return spawnPage(parts[1], parts[3], parts[4]);
    return devicePage(parts[1]);
  }
  return devicesPage();
}

// ---------- devices ----------

// The board: every connected device's sessions that need the operator,
// are working, or finished unread — the first thing to look at.
const BOARD_COLS = [["needs_you", "要対応"], ["working", "作業中"], ["done", "未確認"]];

function boardCard(c) {
  const s = c.session;
  const reason = s.attention === "needs_you" ? (s.attention_reason || "要対応") : s.attention === "done" ? "完了" : "作業中";
  return h("a", { class: "card " + (s.attention || "working"), href: `#/d/${c.device_id}/s/${encodeURIComponent(s.session_id)}` },
    h("div", { class: "row" }, h("span", { class: "chip" }, c.device_name), h("span", { class: "grow" }), h("span", { class: "muted small" }, rel(s.last_seen))),
    h("div", { class: "card-title" }, (s.num ? `#${s.num} ` : "") + sessionTitle(s)),
    h("div", { class: "card-reason" }, reason));
}

function devicesPage() {
  crumbs({ text: "端末" });
  const list = h("div", { class: "list" }, h("div", { class: "empty" }, "読み込み中…"));
  const board = h("div", { class: "board" });
  view.append(h("div", { class: "page" },
    board,
    h("div", { class: "row" },
      h("h1", { class: "grow" }, "端末"),
      h("button", { class: "btn primary", onclick: addDevice }, "＋ 端末を追加")),
    h("h2", {}, "登録済み"),
    list,
    h("p", { class: "muted small" },
      "端末側で ", h("span", { class: "mono" }, "ccdash hub join"), " を実行すると、その端末の collector がこのハブへ接続します。端末にポートを開ける必要はありません。")));

  every(4000, async () => {
    const b = await api("/api/board");
    updateBadge(b);
    const cols = BOARD_COLS.map(([k, label]) => {
      const cards = b[k] || [];
      return h("div", { class: "board-col " + k },
        h("div", { class: "board-head" }, label, h("span", { class: "count" }, cards.length)),
        ...(cards.length ? cards.slice(0, 30).map(boardCard) : [h("div", { class: "muted small board-empty" }, "なし")]));
    });
    board.replaceChildren(...cols);
  });
  every(5000, async () => {
    const ds = await api("/api/devices");
    ds.forEach((d) => deviceCache.set(d.id, d));
    list.replaceChildren(...(ds.length ? ds.map(deviceRow) : [h("div", { class: "empty" }, "まだ端末がありません。「端末を追加」から登録してください。")]));
  });
}

function deviceRow(d) {
  const sub = d.online
    ? `${d.hostname || ""} · ccdash ${d.version || "?"} · ${rel(d.since)}から接続`
    : `オフライン${d.last_seen ? " · 最終接続 " + rel(d.last_seen) : " · 未接続"}`;
  return h("div", { class: "item", onclick: () => d.online ? (location.hash = `#/d/${d.id}`) : toast("この端末はオフラインです") },
    h("span", { class: "dot" + (d.online ? " on" : "") }),
    h("div", { class: "grow" }, h("div", { class: "title" }, d.name), h("div", { class: "sub" }, sub)),
    h("button", { class: "btn small", onclick: (e) => { e.stopPropagation(); deviceMenu(d); } }, "管理"));
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

function sessionTitle(s) {
  return s.custom_title || s.gen_title || s.title || "(無題)";
}

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

// dateBucket follows the TUI's date grouping (favorites pinned on top).
function dateBucket(s) {
  if (s.favorite) return "★ お気に入り";
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

const tabKey = (id) => `ccdash.tab.${id}`;
function loadTab(id) { try { return localStorage.getItem(tabKey(id)) || ""; } catch { return ""; } }
function saveTab(id, g) { try { localStorage.setItem(tabKey(id), g); } catch {} }

async function devicePage(id) {
  const name = await deviceName(id);
  crumbs({ text: "端末", href: "#/" }, { text: name });
  const approvals = h("div");
  const spawns = h("div");
  const tabs = h("div", { class: "tabs" });
  const list = h("div", { class: "list" }, h("div", { class: "empty" }, "読み込み中…"));
  const more = h("div");
  const filter = h("input", { placeholder: "絞り込み（タイトル・パス・#番号）", class: "grow" });
  let showArchived = false;
  const archBtn = h("button", { class: "btn small", onclick: () => { showArchived = !showArchived; archBtn.classList.toggle("on", showArchived); archBtn.textContent = showArchived ? "アーカイブ表示中" : "アーカイブ"; refreshNow(); } }, "アーカイブ");
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
      days.push([key.slice(5), u.by_day?.[key]?.cost || 0]);
    }
    const max = Math.max(...days.map((x) => x[1]), 0.01);
    usageCard.hidden = !u.range?.messages;
    usageCard.replaceChildren(
      h("div", { class: "row" },
        h("div", { class: "grow" }, h("div", { class: "muted small" }, "今日（API 換算）"), h("b", { class: "big" }, fmtUSD(u.today?.cost || 0))),
        h("div", {}, h("div", { class: "muted small" }, "7 日間"), h("b", {}, fmtUSD(u.range?.cost || 0)), h("span", { class: "muted small" }, ` · ${fmtTok(tokTotal(u.range || {}))} tok`))),
      h("div", { class: "bars", title: "日別" }, days.map(([d, v]) => h("div", { class: "bar", title: `${d} ${fmtUSD(v)}` },
        h("span", { style: `height:${Math.max(2, Math.round((v / max) * 40))}px` }), h("small", {}, d.slice(3))))));
  };
  loadUsage();
  const usageTimer = setInterval(loadUsage, 60000);
  cleanup.push(() => clearInterval(usageTimer));
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
  view.append(h("div", { class: "page" },
    h("div", { class: "row" },
      h("h1", { class: "grow" }, name),
      h("button", { class: "btn primary", onclick: () => newSession(id) }, "＋ 新規セッション")),
    status, usageCard, approvals, spawns,
    tabs,
    h("div", { class: "row", style: "margin-bottom:8px" }, filter, archBtn),
    list, more, settingsBox));

  let last = null, tab = loadTab(id), limit = 100;
  const render = () => {
    if (!last) return;
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
    tabs.querySelector(".tab.on")?.scrollIntoView({ block: "nearest", inline: "nearest" });

    const q = filter.value.trim().toLowerCase();
    const rows = sessions
      .filter((s) => !tab || groupOf(s) === tab)
      .filter((s) => !q || `${sessionTitle(s)} ${s.cwd} #${s.num} ${groupOf(s)}`.toLowerCase().includes(q))
      .sort((a, b) => (!!b.favorite - !!a.favorite) || (new Date(b.last_seen) - new Date(a.last_seen)));
    const out = [];
    let bucket = null;
    for (const s of rows.slice(0, limit)) {
      const b = dateBucket(s);
      if (b !== bucket) {
        bucket = b;
        out.push(h("div", { class: "bucket" }, b));
      }
      out.push(sessionRow(id, s, live.has(s.session_id), info, !tab));
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
    spawns.replaceChildren(...(fresh.length ? [h("h2", {}, "起動中（未登録）"), h("div", { class: "list" },
      fresh.map((k) => h("div", { class: "item", onclick: () => (location.hash = `#/d/${id}/p/${encodeURIComponent(k)}`) },
        h("span", { class: "chip live" }, "live"), h("div", { class: "grow title mono" }, k))))] : []));
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
    status.textContent = `${info.hostname} · ccdash ${info.version}` +
      (info.attachEnabled ? "" : " · attach OFF（閲覧のみ）") + (info.approveEnabled ? "" : " · 承認 OFF");
    renderApprovals(approvals, id, aps, sessions);
    render();
  };
  every(3000, tick);
}

function sessionRow(id, s, live, info, showGroup) {
  const chips = [];
  if (live) chips.push(h("span", { class: "chip live" }, "live"));
  if (s.attention === "needs_you") chips.push(h("span", { class: "chip pend", title: s.attention_reason || "" }, "要対応"));
  else if (s.attention === "done") chips.push(h("span", { class: "chip done" }, "未確認"));
  if (s.account && s.account !== "default") chips.unshift(h("span", { class: "chip" }, s.account));
  chips.push(h("span", { class: "chip " + s.status }, s.status));
  const sub = [s.attention === "needs_you" && s.attention_reason ? "? " + s.attention_reason : null,
    showGroup ? groupOf(s) : null, shortPath(s.cwd, info?.home), s.branch, rel(s.last_seen)].filter(Boolean).join(" · ");
  return h("div", { class: "item", onclick: () => (location.hash = `#/d/${id}/s/${encodeURIComponent(s.session_id)}`) },
    h("span", { class: "num" }, s.num ? `#${s.num}` : ""),
    h("div", { class: "grow" },
      h("div", { class: "title" }, (s.favorite ? "★ " : "") + sessionTitle(s)),
      h("div", { class: "sub" }, sub)),
    ...chips);
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
    const prompt = h("textarea", { rows: 2, class: "grow", placeholder: "最初の指示（任意）。/ でスキル・コマンドを選べます", value: preset.prompt || "" });
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
        const r = await dev(id).post("/pty/start", { cwd: path.value, cols: 120, rows: 36, prompt: first, permissionMode: mode.value });
        close();
        location.hash = `#/d/${id}/p/${encodeURIComponent(r.ptyKey)}`;
      } catch (err) { toast(err.message); }
    };
    body.append(h("h1", {}, "新規セッション"),
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
    .replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>')
    .replace(/(^|[\s（])(https?:\/\/[^\s<)]+)/g, '$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>');
}

function md(text) {
  const out = [];
  const lines = text.split("\n");
  let i = 0, list = null;
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
    const c = e.message.content;
    const parts = typeof c === "string" ? [{ type: "text", text: c }] : Array.isArray(c) ? c : [];
    for (const p of parts) {
      if (p.type === "text" && p.text?.trim()) {
        let t = p.text.trim();
        if (e.type === "user") {
          if ((e.origin?.kind ?? "human") !== "human") continue;
          if (e.isMeta || /^<(local-command|system-reminder)/.test(t)) continue;
          const cmd = t.match(/<command-name>([^<]*)<\/command-name>/);
          if (cmd) {
            const args = t.match(/<command-args>([\s\S]*?)<\/command-args>/)?.[1]?.trim();
            items.push({ kind: "user", text: cmd[1] + (args ? " " + args : "") });
            continue;
          }
          if (/^\[Request interrupted/.test(t)) { items.push({ kind: "note", text: "中断しました" }); continue; }
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

function renderItem(it, ctx) {
  if (it.kind === "tool" && it.name === "AskUserQuestion" && ctx) return askCard(it, ctx);
  switch (it.kind) {
    case "user":
      return h("div", { class: "bubble user" + (it.queued ? " queued" : ""), title: it.queued ? "作業中に送信" : null }, it.text);
    case "image":
      return h("a", { class: "chat-img " + it.role, href: it.src, target: "_blank", rel: "noopener" }, h("img", { src: it.src, alt: "画像", loading: "lazy" }));
    case "assistant": {
      const el = h("div", { class: "bubble assistant" });
      el.innerHTML = md(it.text);
      return el;
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
      return h("details", { class: "tool" + (it.error ? " error" : "") },
        h("summary", {}, h("span", { class: "tstate" }, state), h("b", {}, it.name), " ", h("span", { class: "targ" }, toolArg(it.name, it.input))),
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

// sessionMenu: the TUI's per-session keys (t rename, T group, f favorite,
// x archive, s summarize, ctrl+t title) as one dialog.
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
    body.append(
      h("h1", {}, (s.num ? `#${s.num} ` : "") + sessionTitle(s)),
      h("datalist", { id: "group-names" }, groups.map((g) => h("option", { value: g }))),
      h("label", {}, "タイトル（空にすると自動のものに戻す）"),
      h("div", { class: "row" }, title, h("button", { type: "button", class: "btn", onclick: () => { close(); post("/title", { title: title.value.trim() }, "タイトルを保存しました"); } }, "保存")),
      h("label", {}, "グループ（空にすると repo 名に戻す）"),
      h("div", { class: "row" }, group, h("button", { type: "button", class: "btn", onclick: () => { close(); post("/group", { group: group.value.trim() }, "グループを保存しました"); } }, "保存")),
      h("div", { class: "menu-grid" },
        h("button", { type: "button", class: "btn", onclick: () => { close(); post("/favorite", { favorite: !s.favorite }, s.favorite ? "お気に入りを外しました" : "お気に入りにしました"); } }, s.favorite ? "☆ お気に入りを外す" : "★ お気に入り"),
        h("button", { type: "button", class: "btn", onclick: () => { close(); post("/archive", { archived: !s.archived }, s.archived ? "アーカイブを解除しました" : "アーカイブしました"); } }, s.archived ? "アーカイブを解除" : "アーカイブ"),
        h("button", { type: "button", class: "btn", onclick: () => { close(); post("/summarize", {}, "要約を作成しています（claude -p）"); } }, "要約を作成"),
        h("button", { type: "button", class: "btn", onclick: async () => {
          close();
          try { await d.post("/api/titles", { sessionIds: [sid] }); toast("タイトルを生成しています（claude -p）"); refresh(); } catch (e) { toast(e.message); }
        } }, "タイトルを自動生成")),
      h("div", { class: "actions" }, h("button", { class: "btn primary" }, "閉じる")));
  });
}

// ---- usage (API-price estimate) ----

function fmtTok(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(n);
}
const fmtUSD = (v) => "$" + (v >= 100 ? v.toFixed(0) : v.toFixed(2));
const tokTotal = (t) => (t.input || 0) + (t.output || 0) + (t.cache_write || 0) + (t.cache_read || 0);

function usageDialog(u, title) {
  dialog((body) => {
    const rows = Object.entries(u.by_model || {}).sort((a, b) => b[1].cost - a[1].cost);
    body.append(h("h1", {}, title),
      h("p", { class: "muted small" }, "API の定価で換算した概算です（サブスクリプションの実際の請求額ではありません）。"),
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
  view.append(h("div", { class: "chat-wrap" },
    h("div", { class: "term-bar" }, h("a", { class: "btn small", href: chatHref }, "←"), h("span", { class: "grow title" }, "差分レビュー"),
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

// ---- the page ----

async function chatPage(id, { sid, key }) {
  const name = await deviceName(id);
  const d = dev(id);
  let session = null, ptyKey = key || null, hosted = false, info = null;
  let lastStat = "", items = [], pendingSends = [], summaryOpen = false;
  // History window: the poll reads only the last TAIL bytes and splices
  // them onto what's loaded (by entry uuid), so "older history" loaded
  // once stays without re-downloading megabytes on every update.
  const TAIL = 393216;
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

  const titleEl = h("span", { class: "grow title" }, "読み込み中…");
  const statusEl = h("span", { class: "chip" });
  const termBtn = h("a", { class: "btn small", href: "#" }, "ターミナル");
  const endBtn = h("button", { class: "btn small danger", hidden: true, onclick: async () => {
    if (!confirm("このセッションの claude を終了しますか？（後から再開できます）")) return;
    try { await d.del(`/pty/${encodeURIComponent(ptyKey)}`); toast("終了しました"); } catch (e) { toast(e.message); }
  } }, "終了");
  let allGroups = [];
  const diffBtn = h("a", { class: "btn small", hidden: true, title: "変更の差分を見てコメントする" }, "差分");
  let lastUsage = null;
  const costBtn = h("button", { class: "btn small cost", hidden: true, title: "このセッションのトークンと API 換算コスト", onclick: () => lastUsage && usageDialog(lastUsage, "このセッションの使用量") }, "");
  const menuBtn = h("button", { class: "btn small", title: "セッションの操作", onclick: () => session ? sessionMenu(id, session, allGroups, poll) : toast("まだセッションが登録されていません") }, "⋯");
  const bar = h("div", { class: "term-bar" },
    h("a", { class: "btn small", href: `#/d/${id}` }, "←"), titleEl, statusEl, costBtn, diffBtn, termBtn, endBtn, menuBtn);
  const log = h("div", { class: "chat-log" });
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
      h("span", { class: "grow" }), agentsOpen ? "▲" : "▼");
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
  const attachBtn = h("button", { type: "button", class: "btn attach", title: "画像を添付", onclick: () => fileInput.click() }, "＋");
  const renderThumbs = () => {
    thumbs.replaceChildren(...attachments.map((a, i) => h("div", { class: "thumb" },
      h("img", { src: a.url, alt: "" }),
      h("button", { type: "button", title: "外す", onclick: () => { URL.revokeObjectURL(a.url); attachments.splice(i, 1); renderThumbs(); } }, "×"))));
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
  cleanup.push(() => attachments.forEach((a) => URL.revokeObjectURL(a.url)));
  // Voice input (Web Speech API): dictation is appended to the message.
  const micBtn = speechSupported() ? h("button", { type: "button", class: "btn attach mic", title: "音声入力" }, "🎤") : null;
  micBtn?.addEventListener("click", () => toggleDictation(micBtn, input, autosize));
  cleanup.push(() => stopDictation());
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
    h("button", { type: "button", class: "chip quick edit", title: "クイックコマンドを編集", onclick: () => editQuick(paintQuick) }, "✎"));
  api("/api/quick").then(paintQuick).catch(() => {});
  const composer = h("form", { class: "composer" }, attachBtn, micBtn, h("div", { class: "grow col" }, thumbs, slashBox, input), fileInput, h("div", { class: "row" }, stopBtn, sendBtn));
  for (const ev of ["dragover", "drop"]) {
    scroller.addEventListener(ev, (e) => {
      if (![...(e.dataTransfer?.types || [])].includes("Files")) return;
      e.preventDefault();
      if (ev === "drop") addFiles([...e.dataTransfer.files]);
    });
  }
  view.append(h("div", { class: "chat-wrap" }, bar, agentBar, agentList, scroller, h("div", { class: "chat-bottom" }, approvalsEl, promptEl, quickRow, composer)));

  const autosize = () => { input.style.height = "auto"; input.style.height = Math.min(input.scrollHeight, 200) + "px"; };
  input.addEventListener("input", autosize);
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
    const nodes = items.map((it) => renderItem(it, askCtx)).filter(Boolean);
    if (session?.summary_status === "running") nodes.unshift(h("div", { class: "note" }, "要約を作成中…"));
    else if (session?.summary) {
      const det = h("details", { class: "summary-card", open: summaryOpen }, h("summary", {}, "要約"), h("div", {}, session.summary));
      det.addEventListener("toggle", () => (summaryOpen = det.open));
      nodes.unshift(det);
    }
    if (hasOlder && nodes.length) {
      nodes.unshift(h("button", { class: "btn older", disabled: loadingOlder, onclick: loadOlder },
        loadingOlder ? "読み込み中…" : "↑ 古い履歴を読み込む"));
    }
    pendingSends = pendingSends.filter((p) => Date.now() - p.at < 120000);
    for (const p of pendingSends) nodes.push(h("div", { class: "bubble user pending" }, p.text));
    if (!nodes.length) nodes.push(h("div", { class: "empty" }, ptyKey && !sid ? "claude を起動しました。メッセージを送って始めましょう。" : "まだメッセージがありません。"));
    if (session?.status === "active") nodes.push(h("div", { class: "typing" }, h("span"), h("span"), h("span")));
    log.replaceChildren(...nodes);
    if (stick) scroller.scrollTop = scroller.scrollHeight;
  };

  async function keys(seq) {
    if (!ptyKey || !hosted) return;
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
      autosize();
      render(true);
      setTimeout(poll, 600);
    } catch (err) { toast(err.message); }
    finally { sendBtn.disabled = false; }
  });

  async function poll() {
    const [sessions, ptys, aps] = await Promise.all([
      d.get("/api/sessions").then((x) => x || []),
      d.get("/pty/").then((x) => x || []).catch(() => []),
      d.get("/api/approvals").then((x) => x || []).catch(() => []),
    ]);
    info = info || (await deviceInfo(id));
    allGroups = uniqueGroups(sessions, true).filter(Boolean);
    if (!sid && ptyKey) {
      const a = aliasOf(ptys, ptyKey);
      if (a) { sid = a; history.replaceState(null, "", `#/d/${id}/s/${encodeURIComponent(sid)}`); }
    }
    if (sid) {
      if (ptys.some((p) => p.key === sid && p.alive)) ptyKey = sid;
      session = sessions.find((s) => s.session_id === sid) ||
        (await d.get("/api/sessions?archived=1").catch(() => []))?.find((s) => s.session_id === sid) || session;
    }
    hosted = !!ptyKey && ptys.some((p) => p.key === ptyKey && p.alive);
    // Open and visible: the operator is looking at it.
    if (session?.attention === "done" && sid && !document.hidden) {
      d.post(`/api/sessions/${encodeURIComponent(sid)}/seen`, {}).catch(() => {});
      session.attention = "";
    }

    const title = session ? (session.num ? `#${session.num} ` : "") + sessionTitle(session) : sid ? sid : "新規セッション";
    titleEl.textContent = title;
    crumbs({ text: "端末", href: "#/" }, { text: name, href: `#/d/${id}` }, { text: title });
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
    stopBtn.hidden = !(hosted && session?.status === "active");
    input.placeholder = hosted ? "メッセージ" : "送信するとこのセッションを再開します";

    // approvals for this session
    renderApprovals(approvalsEl, id, sid ? aps.filter((a) => a.session_id === sid) : [], sessions);
    approvalsEl.querySelector("h2")?.remove();

    // usage (every seventh poll)
    if (sid && agentTick % 7 === 0) {
      const u = await d.get(`/api/sessions/${encodeURIComponent(sid)}/usage`).catch(() => null);
      if (u?.total?.messages) {
        lastUsage = u;
        costBtn.hidden = false;
        costBtn.textContent = fmtUSD(u.total.cost);
      }
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
  }

  every(1500, poll);
  input.focus();
}

// ---------- terminal ----------

const KEYS = [
  ["Esc", "\x1b"], ["Tab", "\t"], ["⇧Tab", "\x1b[Z"], ["↑", "\x1b[A"], ["↓", "\x1b[B"],
  ["←", "\x1b[D"], ["→", "\x1b[C"], ["^C", "\x03"], ["Enter", "\r"],
];

function terminal(id, key, title, chatHref) {
  const mount = h("div", { id: "term" });
  const sendInput = h("input", { placeholder: "テキストを送信（日本語入力向け・Enter で送信+改行）", enterkeyhint: "send", autocomplete: "off" });
  let ws = null, term = null, fit = null, closedByUs = false;

  const send = (data) => {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(enc.encode(data));
  };
  const bar = h("div", { class: "term-bar" },
    h("a", { class: "btn small", href: chatHref }, "←"),
    h("span", { class: "grow title" }, title),
    h("span", { class: "muted small hide-sm" }, key),
    h("a", { class: "btn small", href: chatHref, title: "ターミナルの接続を切ってチャット表示に戻ります。claude は端末上で動き続けます" }, "チャット"),
    h("button", { class: "btn small danger", onclick: async () => {
      if (!confirm("このセッションの claude を終了しますか？（後から再開できます）")) return;
      try { closedByUs = true; await dev(id).del(`/pty/${encodeURIComponent(key)}`); location.hash = `#/d/${id}`; } catch (e) { toast(e.message); }
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
  view.append(wrap);

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
  term.focus();

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

function notifyButton() {
  if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) return null;
  const btn = h("button", { class: "btn small bell", title: "通知" }, "🔕");
  const paint = async () => {
    const on = Notification.permission === "granted" && !!(await pushSubscription().catch(() => null));
    btn.textContent = on ? "🔔" : "🔕";
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
            h("button", { type: "button", class: "btn", onclick: async () => { await api("/api/push/test", { method: "POST" }); toast("テスト通知を送りました"); } }, "テスト通知"),
            h("button", { type: "button", class: "btn danger", onclick: async () => {
              const sub = await pushSubscription();
              if (sub) { await api("/api/push/unsubscribe", { method: "POST", json: { endpoint: sub.endpoint } }); await sub.unsubscribe(); }
              close(); paint(); toast("通知を OFF にしました");
            } }, "OFF にする"),
            h("button", { class: "btn primary" }, "閉じる"))));
        return;
      }
      if ((await Notification.requestPermission()) !== "granted") { toast("通知が許可されませんでした（ブラウザの設定を確認してください）", 5000); return; }
      const { publicKey } = await api("/api/push/key");
      const reg = await navigator.serviceWorker.ready;
      const sub = (await reg.pushManager.getSubscription()) ||
        await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(publicKey) });
      await api("/api/push/subscribe", { method: "POST", json: sub.toJSON() });
      toast("通知を ON にしました");
      paint();
    } catch (e) { toast("通知を設定できませんでした: " + e.message, 5000); }
  });
  paint();
  return btn;
}

// updateBadge mirrors the board onto the installed app's icon badge and a
// header chip: how many sessions need the operator (+ unread finishes).
function updateBadge(b) {
  const needs = (b?.needs_you || []).length, done = (b?.done || []).length;
  try {
    if (needs + done > 0) navigator.setAppBadge?.(needs + done);
    else navigator.clearAppBadge?.();
  } catch {}
  const el = $("#att");
  if (!el) return;
  el.hidden = needs + done === 0;
  el.textContent = needs ? `要対応 ${needs}` : `未確認 ${done}`;
  el.classList.toggle("needs", needs > 0);
}

// ---------- boot ----------

(async function boot() {
  try {
    const next = sessionStorage.getItem("ccdash.next");
    if (next) { sessionStorage.removeItem("ccdash.next"); if (!location.hash || location.hash === "#/") location.hash = next; }
  } catch {}
  try {
    me = await api("/api/me");
    $("#me").replaceChildren(h("span", {}, notifyButton(), h("span", { class: "email" }, me.email), me.noAuth ? null : h("a", { href: "/auth/logout" }, "ログアウト")));
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
  document.addEventListener("visibilitychange", () => { if (!document.hidden) checkUpdate(); });
  window.addEventListener("hashchange", route);
  route();
})();
