# 0004 — Hub: read-only bearer tokens for machine clients

- Date: 2026-10-06
- Status: Accepted

## Context

The hub (0003) authenticates people: OIDC (or `tailscale serve`'s login
header) and a signed cookie. A machine client — in the home lab an
in-cluster "alert-triage" bot that wants to see what the Claude sessions
on each machine are doing when an alert fires — cannot complete a browser
login. It only needs to read: the cross-device board, the running
sessions, a device's session list, and the tail of a session's transcript.

Giving it a cookie (minting one out of band) would hand it everything the
portal can do: typing into PTYs, deciding approvals, adding devices.

## Decision

- **Static read tokens, configured at start.** `CCDASH_HUB_READ_TOKENS`
  (comma-separated) and/or `--read-token-file` /
  `CCDASH_HUB_READ_TOKEN_FILE` (one per line, `#` comments). Tokens shorter
  than 32 characters are refused. The hub keeps only their SHA-256 digests
  and compares digests with `subtle.ConstantTimeCompare` against every
  entry. Tokens are never logged; a rejected token logs the client IP and
  path only.
- **`Authorization: Bearer` is the only way in, and only for GET on a
  fixed allowlist** (`internal/hub/readtoken.go`):
  `/api/board`, `/api/active`, `/api/devices`,
  `/api/d/{id}/api/sessions`,
  `/api/d/{id}/api/sessions/{sid}/transcript` (mode `tail` / `stat` only,
  `bytes` capped at 1 MiB). A valid token on any other route or method is
  403; an unknown token is 401 with `WWW-Authenticate: Bearer`.
- **The bearer branch runs only when tokens are configured.** Without
  them, a request carrying `Authorization` goes to the cookie login exactly
  as before. With them, a bearer request is decided by the token alone —
  any cookie on it is dropped, so a bearer header can never widen a cookie
  request (a cookie + bearer write is 403).
- **Cross-origin protection is unchanged.** The cookie path still runs
  `http.CrossOriginProtection`; bearer requests are GETs, which that guard
  passes anyway. Browsers can't attach an `Authorization` header
  cross-origin without a CORS preflight, which the hub never grants.
- **The device proxy keeps stripping credentials.** The bearer header
  never reaches a device; the device's own hub allowlist still applies
  (sessions and transcripts are already on it).
- **`?lines=N` on the collector's transcript tail** keeps only the last N
  JSONL records of the byte tail, so a bot can ask for "the latest few
  messages" without parsing a 256 KiB tail. Older collectors ignore it and
  return the whole tail.

## Alternatives considered

- **Per-token scopes / tokens issued from the portal and stored in
  hub.sqlite.** More flexible, but one read-only bot doesn't need a token
  UI, and env/file configuration fits the Kubernetes Secret the bot and the
  hub already share. Can be added later behind the same `apiAuth` seam.
- **OAuth client credentials against the OIDC provider.** Cloudflare
  Access for SaaS doesn't issue client-credential tokens for this kind of
  app; it would also tie a LAN-internal bot to an external IdP.
- **A separate read-only listener.** Another port to expose and protect for
  the same five routes.

## Consequences

- Rotating a read token means restarting the hub with the new list.
- Transcript tails are the device's raw JSONL (base64 in the `data`
  field). The collector redacts hook payloads, not transcripts, so a bot
  holding a read token can read what the sessions read. Treat the token
  like the portal login, and keep the bot's own outputs (Slack, etc.)
  summarised.
- Adding a route to `readTokenRoutes` widens what every read token can do;
  each addition needs a case in `readtoken_test.go`.
