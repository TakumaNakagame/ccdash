# ccdash-intake: ask Claude from inside your own app

A small button for your own web apps. Pressing it sends the page's URL,
title, selected text, a screenshot, the app's own context and the page's
recent console errors to the ccdash hub, which either **starts a session**
in that app's repository or keeps a **draft** you start with one tap in the
portal (`#/intake`). Which one, and where, is decided by the app's token on
the hub — the app cannot choose a directory or a permission mode.

```
browser (the app's page)
  └─ ccdash-intake.js ── multipart ──▶ the app's backend  POST /api/ccdash-intake
                                         └─ + Authorization: Bearer <app token>
                                            ──▶ hub  POST /api/intake/requests
```

The token stays on the app's server. Keep the hub on a private network
(LAN / VPN); the hub route takes bearer tokens only, no cookies.

## 1. Register the app on the hub

`ccdash hub serve --intake-apps-file apps.json` (or `$CCDASH_HUB_INTAKE_APPS_FILE`):

```json
{
  "apps": [
    {
      "name": "romancecar",
      "token_sha256": "<sha256 hex of the token>",
      "owner": "you@example.com",
      "host": "build-box",
      "cwd": "/home/you/src/romancecar",
      "project": "romancecar",
      "mode": "auto",
      "action": "draft"
    }
  ]
}
```

- `token_sha256`: `printf %s "$TOKEN" | sha256sum` (generate the token with `openssl rand -hex 32`; the file holds only the hash)
- `owner`: the login whose portal shows the drafts and who gets the push
- `host`: device id or name; `cwd` must exist there
- `mode`: permission mode of the session (`""`, `manual`, `acceptEdits`, `auto`, `plan`)
- `action`: `draft` (default; nothing runs until you press start in the portal) or `session` (starts at once — only for apps nobody else can reach)

## 2. Hub API

`POST /api/intake/requests`, `Authorization: Bearer <token>`, `multipart/form-data`:

| field | limit | |
|---|---|---|
| `text` | 4000 chars | required: the request |
| `url`, `title` | 2000 / 300 | the page |
| `sel` | 4000 | selected text |
| `context` | 16 KB | anything the app adds (JSON or text) |
| `errors` | 8 KB | recent console errors |
| `submitter` | 200 | who used the app (informational) |
| `files` | 5 images, 20 MB each | screenshots |

Images, `context` and `errors` are stored on the target machine (its upload
dir, swept after 7 days) right away; the request carries their paths.

Answers (JSON): `201 {"status":"draft","id":"…","url":"<hub>/#/intake?draft=…"}`
or `201 {"status":"started","url":"<hub>/#/d/<device>/p/<pty>"}`;
errors are `{"error":"…"}` with 400 (bad field), 401 (token), 413 (too big),
429 (more than 30 requests an hour per app), 503 (target machine offline).

## 3. The app's relay route

Forward the multipart body as is and return the hub's answer. FastAPI:

```python
@app.post("/api/ccdash-intake")
async def ccdash_intake(request: Request):
    hub, token = os.environ.get("CCDASH_HUB_URL"), os.environ.get("CCDASH_INTAKE_TOKEN")
    if not hub or not token:
        return JSONResponse({"error": "ccdash intake is not configured"}, status_code=503)
    body = await request.body()
    if len(body) > 60 * 1024 * 1024:
        return JSONResponse({"error": "too large"}, status_code=413)
    async with httpx.AsyncClient(timeout=60) as c:
        r = await c.post(hub.rstrip("/") + "/api/intake/requests", content=body,
                         headers={"Authorization": f"Bearer {token}", "Content-Type": request.headers["content-type"]})
    return JSONResponse(r.json() if r.headers.get("content-type", "").startswith("application/json") else {"error": r.text[:300]},
                        status_code=r.status_code)
```

Add a `submitter` field on the way if the app knows who is logged in.

## 4. The page

```html
<script src="/static/ccdash-intake.js" data-endpoint="/api/ccdash-intake" defer></script>
<script>
  // optional: what the app knows that a screenshot doesn't show
  window.ccdashIntake = { context: () => ({ view: location.hash, selectedId: window.app?.selected }) };
</script>
```

Hide the button where it doesn't belong by not loading the script there.
