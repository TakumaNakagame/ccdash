// ccdash hub service worker: makes the portal installable and shows the
// hub's push notifications. Everything is live (sessions, terminals,
// tunnels to devices), so nothing is cached and no request is intercepted:
// the browser fetches as if there were no worker at all.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

// Web Push from the hub: approvals, questions on a claude's screen, turns
// that finished. Payload: {title, body, url, tag}.
self.addEventListener("push", (event) => {
  let n = {};
  try { n = event.data ? event.data.json() : {}; } catch { n = { title: "ccdash", body: event.data?.text() || "" }; }
  event.waitUntil(self.registration.showNotification(n.title || "ccdash", {
    body: n.body || "",
    tag: n.tag || undefined,
    renotify: !!n.tag,
    icon: "icon-192.png",
    badge: "icon-192.png",
    data: { url: n.url || "/" },
  }));
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
