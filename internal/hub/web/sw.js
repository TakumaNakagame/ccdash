// ccdash hub service worker: makes the portal installable, shows the
// hub's push notifications and receives the share sheet (share_target in
// the manifest). Everything is live (sessions, terminals, tunnels to
// devices), so nothing is cached and the one request it intercepts is the
// share POST: every other fetch goes to the network as if there were no
// worker at all.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

// Web Push from the hub: approvals, questions on a claude's screen, turns
// that finished. Payload: {title, body, title_en, body_en, url, tag, badge}.

// lang is the portal's language, which the page stores in Cache Storage
// (the one pref kept there; a worker can't read localStorage).
async function lang() {
  try {
    const r = await (await caches.open("ccdash-prefs")).match("/__lang");
    if (r) return await r.text();
  } catch {}
  return /^ja\b/i.test(self.navigator.language || "") ? "ja" : "en";
}

self.addEventListener("push", (event) => {
  let n = {};
  try { n = event.data ? event.data.json() : {}; } catch { n = { title: "ccdash", body: event.data?.text() || "" }; }
  // The hub sends the app-icon count with each push so the installed app's
  // badge moves even while no portal window is open.
  if (typeof n.badge === "number") {
    try { (n.badge > 0 ? self.navigator.setAppBadge?.(n.badge) : self.navigator.clearAppBadge?.())?.catch?.(() => {}); } catch {}
  }
  event.waitUntil((async () => {
    let shown = true, error = "";
    const en = (await lang()) === "en";
    try {
      await self.registration.showNotification((en && n.title_en) || n.title || "ccdash", {
        body: (en && n.body_en) || n.body || "",
        tag: n.tag || undefined,
        renotify: !!n.tag,
        icon: "icon-192.png",
        badge: "icon-192.png",
        data: { url: n.url || "/" },
      });
    } catch (e) { shown = false; error = String(e); }
    // The portal's notification test waits for this to tell "never
    // arrived" from "arrived but the OS didn't show it".
    if (n.tag === "test") {
      const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const w of wins) w.postMessage({ type: "push-received", tag: n.tag, shown, error });
    }
  })());
});

// Tapping a notification focuses an open portal window (navigating it to
// the session) or opens a new one.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(event.notification.data?.url || "/", self.location.origin).href;
  event.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const w of wins) {
      if (new URL(w.url).origin === self.location.origin) {
        await w.focus();
        return w.navigate(url).catch(() => w.postMessage({ type: "open", url }));
      }
    }
    return self.clients.openWindow(url);
  })());
});

// The share sheet POSTs title / text / url and screenshots to
// /share-target. The files wait in Cache Storage under /__share/<id>/ until
// the quick request page (#/intake) takes them; the rest rides the query.
self.addEventListener("fetch", (event) => {
  const u = new URL(event.request.url);
  if (event.request.method !== "POST" || u.origin !== self.location.origin || u.pathname !== "/share-target") return;
  event.respondWith((async () => {
    const q = new URLSearchParams({ intake: "1" });
    try {
      const form = await event.request.formData();
      for (const k of ["title", "text", "url"]) if (form.get(k)) q.set(k, String(form.get(k)));
      const files = form.getAll("files").filter((f) => f && typeof f !== "string" && f.size);
      if (files.length) {
        const id = Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 8);
        const cache = await caches.open("ccdash-share");
        // Shares nobody opened (the page was closed) go after a day.
        for (const req of await cache.keys()) {
          const t = parseInt(new URL(req.url).pathname.split("/")[2].split("-")[0], 36);
          if (!(t > Date.now() - 86400e3)) await cache.delete(req);
        }
        await Promise.all(files.slice(0, 10).map((f, i) => cache.put(`/__share/${id}/${i}`,
          new Response(f, { headers: { "Content-Type": f.type || "application/octet-stream", "X-Filename": encodeURIComponent(f.name || "") } }))));
        q.set("share", id);
      }
    } catch {}
    return Response.redirect("/?" + q, 303);
  })());
});
