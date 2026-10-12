// ccdash hub service worker: makes the portal installable and shows the
// hub's push notifications. Everything is live (sessions, terminals,
// tunnels to devices), so nothing is cached and no request is intercepted:
// the browser fetches as if there were no worker at all.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

// Web Push from the hub: approvals, questions on a claude's screen, turns
// that finished. Payload: {title, body, url, tag, badge}.
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
    try {
      await self.registration.showNotification(n.title || "ccdash", {
        body: n.body || "",
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
