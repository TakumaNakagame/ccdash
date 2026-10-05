// ccdash hub service worker — only what installing the portal as an app
// needs. Everything is live (sessions, terminals, tunnels to devices), so
// nothing is cached and no request is intercepted: the browser fetches as
// if there were no worker at all.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));
