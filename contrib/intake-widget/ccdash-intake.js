// ccdash-intake.js — a "ask Claude about this page" button for your own web
// apps. Copy this file into the app's static files and load it:
//
//   <script src="/static/ccdash-intake.js" data-endpoint="/api/ccdash-intake" defer></script>
//
// The button posts multipart/form-data to data-endpoint, which must be the
// app's OWN backend route: the backend adds `Authorization: Bearer <app token>`
// and forwards the body unchanged to <hub>/api/intake/requests (see README.md).
// The token never reaches the browser.
//
// Fields sent: text, url, title, sel (selected text), context (whatever
// window.ccdashIntake.context() returns, JSON), errors (the page's recent
// console errors), files (screenshots). No dependencies; styles live in a
// shadow root so they can't clash with the app's.
//
// Options (data-* on the script tag): data-endpoint (required),
// data-label (button text, default "依頼"), data-position ("right" | "left").
(() => {
  "use strict";
  const me = document.currentScript;
  const endpoint = me?.dataset.endpoint;
  if (!endpoint || window.__ccdashIntake) return;
  window.__ccdashIntake = true;
  const label = me.dataset.label || "依頼";
  const side = me.dataset.position === "left" ? "left" : "right";

  // Recent errors from load time on: often the most useful part of a report.
  const errors = [];
  const keep = (s) => { errors.push(new Date().toISOString().slice(11, 19) + " " + String(s).slice(0, 500)); if (errors.length > 20) errors.shift(); };
  const origError = console.error;
  console.error = function (...a) { keep(a.map((x) => (x instanceof Error ? x.stack || x.message : typeof x === "string" ? x : safeJSON(x))).join(" ")); return origError.apply(this, a); };
  addEventListener("error", (e) => keep(e.error?.stack || e.message));
  addEventListener("unhandledrejection", (e) => keep("unhandled rejection: " + (e.reason?.stack || e.reason)));
  function safeJSON(x) { try { return JSON.stringify(x); } catch { return String(x); } }

  const host = document.createElement("div");
  host.setAttribute("data-ccdash-intake", "");
  const root = host.attachShadow({ mode: "open" });
  root.innerHTML = `<style>
    :host { all: initial; }
    * { box-sizing: border-box; font-family: system-ui, -apple-system, "Hiragino Sans", "Noto Sans JP", sans-serif; }
    .fab { position: fixed; ${side}: 16px; bottom: calc(16px + env(safe-area-inset-bottom)); z-index: 2147483000;
      border: 1px solid rgb(0 0 0 / .15); border-radius: 999px; padding: 8px 14px; font-size: 13px; font-weight: 600;
      background: #1f2328; color: #fff; cursor: pointer; opacity: .55; box-shadow: 0 2px 8px rgb(0 0 0 / .2); }
    .fab:hover, .fab:focus-visible { opacity: 1; }
    .panel { position: fixed; ${side}: 16px; bottom: calc(60px + env(safe-area-inset-bottom)); z-index: 2147483001;
      width: min(380px, calc(100vw - 32px)); background: #fff; color: #1f2328; border: 1px solid #d0d7de; border-radius: 12px;
      padding: 12px; box-shadow: 0 8px 24px rgb(0 0 0 / .2); display: flex; flex-direction: column; gap: 8px; font-size: 13px; }
    @media (prefers-color-scheme: dark) { .panel { background: #161b22; color: #e6edf3; border-color: #30363d; } textarea { background: #0d1117; color: #e6edf3; border-color: #30363d; } }
    .panel[hidden] { display: none; }
    textarea { width: 100%; min-height: 84px; resize: vertical; border: 1px solid #d0d7de; border-radius: 6px; padding: 6px 8px; font-size: 14px; line-height: 1.5; }
    .muted { color: #6e7781; font-size: 12px; overflow-wrap: anywhere; }
    .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
    button.b { border: 1px solid #d0d7de; background: transparent; color: inherit; border-radius: 6px; padding: 6px 10px; font-size: 13px; cursor: pointer; min-height: 32px; }
    button.p { background: #d9771f; border-color: #d9771f; color: #fff; font-weight: 600; margin-left: auto; }
    button:disabled { opacity: .5; }
    .thumbs { display: flex; gap: 6px; flex-wrap: wrap; }
    .thumbs img { width: 56px; height: 56px; object-fit: cover; border-radius: 6px; border: 1px solid #d0d7de; cursor: pointer; }
    a { color: #d9771f; }
  </style>
  <button class="fab" type="button" title="この画面について Claude に依頼する">${label}</button>
  <div class="panel" hidden role="dialog" aria-label="Claude に依頼">
    <textarea placeholder="この画面で直したいこと・足したいこと"></textarea>
    <div class="muted url"></div>
    <div class="thumbs"></div>
    <div class="row">
      <button class="b shot" type="button">画面を撮る</button>
      <button class="b pick" type="button">画像を選ぶ</button>
      <input type="file" accept="image/*" multiple hidden>
      <button class="b p send" type="button">送る</button>
    </div>
    <div class="muted status" aria-live="polite"></div>
  </div>`;
  const $ = (s) => root.querySelector(s);
  const fab = $(".fab"), panel = $(".panel"), text = $("textarea"), thumbs = $(".thumbs"), status = $(".status");
  const shotBtn = $(".shot"), pickBtn = $(".pick"), fileInput = $("input[type=file]"), sendBtn = $(".send");
  let files = [], selection = "";
  const canShoot = !!navigator.mediaDevices?.getDisplayMedia && !matchMedia("(pointer: coarse)").matches;
  if (!canShoot) shotBtn.hidden = true;

  const paintThumbs = () => {
    thumbs.replaceChildren(...files.map((f, i) => {
      const img = document.createElement("img");
      img.src = URL.createObjectURL(f);
      img.title = "クリックで外す";
      img.onclick = () => { URL.revokeObjectURL(img.src); files.splice(i, 1); paintThumbs(); };
      return img;
    }));
  };
  const addFiles = (list) => { for (const f of list) if (f.type.startsWith("image/") && f.type !== "image/svg+xml" && files.length < 5) files.push(f); paintThumbs(); };

  // The selection is lost once the textarea takes focus: read it on press.
  fab.addEventListener("pointerdown", () => { selection = String(getSelection() || "").slice(0, 4000); });
  fab.addEventListener("click", () => {
    panel.hidden = !panel.hidden;
    if (!panel.hidden) { $(".url").textContent = location.href; status.textContent = ""; text.focus(); }
  });
  pickBtn.addEventListener("click", () => fileInput.click());
  fileInput.addEventListener("change", () => { addFiles([...fileInput.files]); fileInput.value = ""; });
  text.addEventListener("paste", (e) => { const fs = [...(e.clipboardData?.files || [])]; if (fs.length) { e.preventDefault(); addFiles(fs); } });

  // One frame of this tab (Chrome offers it first with preferCurrentTab);
  // the widget hides itself while the frame is taken.
  shotBtn.addEventListener("click", async () => {
    let stream;
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({ video: { displaySurface: "browser" }, audio: false, preferCurrentTab: true, selfBrowserSurface: "include" });
      host.style.visibility = "hidden";
      const v = document.createElement("video");
      v.muted = true; v.srcObject = stream; await v.play();
      await new Promise((r) => setTimeout(r, 300));
      const c = document.createElement("canvas");
      c.width = v.videoWidth; c.height = v.videoHeight;
      c.getContext("2d").drawImage(v, 0, 0);
      const blob = await new Promise((r) => c.toBlob(r, "image/png"));
      if (blob) addFiles([new File([blob], "screen.png", { type: "image/png" })]);
    } catch (e) {
      if (e?.name !== "NotAllowedError") status.textContent = "画面を撮れませんでした: " + (e?.message || e);
    } finally {
      stream?.getTracks().forEach((t) => t.stop());
      host.style.visibility = "";
    }
  });

  sendBtn.addEventListener("click", async () => {
    const body = text.value.trim();
    if (!body) { status.textContent = "依頼を書いてください"; text.focus(); return; }
    const fd = new FormData();
    fd.set("text", body);
    fd.set("url", location.href);
    fd.set("title", document.title || "");
    if (selection) fd.set("sel", selection);
    try {
      const ctx = window.ccdashIntake?.context?.();
      if (ctx != null) fd.set("context", (typeof ctx === "string" ? ctx : JSON.stringify(ctx, null, 2)).slice(0, 16000));
    } catch (e) { keep("ccdashIntake.context(): " + e); }
    if (errors.length) fd.set("errors", errors.join("\n"));
    for (const f of files) fd.append("files", f, f.name || "screen.png");
    sendBtn.disabled = true;
    status.textContent = "送っています…";
    try {
      const r = await fetch(endpoint, { method: "POST", body: fd, credentials: "same-origin" });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(j.error || j.detail || r.status + " " + r.statusText);
      status.replaceChildren(document.createTextNode(j.status === "started" ? "セッションを起動しました " : "下書きにしました（ccdash で起動） "));
      if (j.url) { const a = document.createElement("a"); a.href = j.url; a.target = "_blank"; a.rel = "noopener"; a.textContent = "ccdash で開く"; status.append(a); }
      text.value = ""; files = []; paintThumbs(); selection = "";
    } catch (e) {
      status.textContent = "送れませんでした: " + e.message;
    } finally { sendBtn.disabled = false; }
  });

  const mount = () => document.body.append(host);
  if (document.body) mount(); else addEventListener("DOMContentLoaded", mount);
})();
