# Remote mode

By default the collector only binds `127.0.0.1` and the TUI reads its
SQLite DB / transcripts directly off the same disk — the two have to run
on the same host. Remote mode lifts that: run the collector on a dev
server, and point a TUI on any other machine at it over HTTP.

**1. Start a collector that listens beyond loopback**, on the host that
runs `claude` (call it `dev-box`):

```sh
ccdash server --listen 0.0.0.0:9123
# or a specific address if the host has more than one interface:
ccdash server --listen 192.168.1.50:9123
```

`--listen` is opt-in (without it the collector stays `127.0.0.1:9123`; the
older `--addr` flag still works but is deprecated in favor of `--listen`).
Binding non-loopback logs a clear warning, and ccdash refuses to start if
no auth token exists yet — run `ccdash server` (or `ccdash install-hooks`)
locally once first so `$XDG_STATE_HOME/ccdash/token` gets created, *then*
add `--listen`.

When `--listen` names a specific non-loopback IP, the collector **also**
keeps a listener on `127.0.0.1:<port>`: Claude Code's installed hooks are
wired to `127.0.0.1` and always arrive via loopback, and a local TUI on the
same host keeps working too — remote clients are the only ones using the
`--listen` address. (A wildcard bind like `0.0.0.0:9123` already accepts
loopback connections, so it stays a single listener.)

For a persistent setup, run it as a systemd user unit:

```ini
# ~/.config/systemd/user/ccdash.service
[Unit]
Description=ccdash collector

[Service]
ExecStart=%h/.local/bin/ccdash server --listen 0.0.0.0:9123
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now ccdash.service
```

**2. Copy the collector's token** to the machine that will run the TUI —
it's the same shared secret hook events already use, just scp'd over. Copy
it only to your own machines: whoever holds it can drive your sessions.

```sh
scp dev-box:~/.local/state/ccdash/token ~/.config/ccdash/collector.token
```

**3. Configure the remote once**, then use `-r` from anywhere:

```sh
ccdash remote set --url http://192.168.1.50:9123 \
  --token-file ~/.config/ccdash/collector.token \
  --ssh-target dev-box        # optional; defaults to the URL hostname
ccdash remote show            # prints the effective config + sources
ccdash -r                     # open the TUI against the configured remote
```

`ccdash remote set` writes `~/.config/ccdash/config.json`
(`$XDG_CONFIG_HOME/ccdash/config.json`, mode 0600):

```json
{
  "remote": {
    "url": "http://192.168.1.50:9123",
    "token_file": "/home/op/.config/ccdash/collector.token",
    "ssh_target": "dev-box"
  }
}
```

Per-invocation overrides: `--remote-url <url>` (implies `-r`, overrides the
configured url), `--token-file`, `--ssh-target`. Precedence for every
field: explicit flag → config file → (token only) `$CCDASH_TOKEN` env var →
a helpful error. `ccdash -r` without any configuration tells you to run
`ccdash remote set` first. In remote mode ccdash never opens a local DB and
never spawns/pings a local collector — every session list, approval
decision, and transcript read goes over HTTP to the remote origin,
authenticated the same way hook events are (the `X-Ccdash-Token` header).

**Attach and new-session in remote mode** go through `ssh -t` instead of a
local PTY (`internal/attach` only manages a claude process on the machine
running ccdash, which in remote mode isn't `dev-box`): pressing `enter` on
a running session with a tmux pane runs `tmux attach -t <pane>` over ssh;
on a stopped session it runs `cd <cwd> && exec claude --resume <id>`. The
remote command is executed under `bash -lc` — a **login** shell — so PATH
additions from `~/.profile` / `~/.bash_profile` (e.g. `~/.local/bin`, where
claude usually lives) apply even though ssh's remote-command path skips rc
files. `n` (new session) works the same way with an operator-typed
directory — there's no local filesystem to check or tab-complete against,
so the path is passed through as-is and a typo just fails inside the ssh
session. The ssh target defaults to the remote URL's hostname; set
`ssh_target` in the config (or pass `--ssh-target user@host`) when it
differs (e.g. a different SSH alias or username).

`sessions` and `approvals` (the plain-text CLI subcommands) also accept
`-r` / `--remote-url` / `--token-file` / `--ssh-target`; `events` stays
local-only and says so if you pass `-r`.

**Known limitation (v1):** toggling a value on the settings page (`,`) in
remote mode performs a synchronous HTTP write; if the collector just went
unreachable, the UI can stall for up to the 3-second mutation timeout on
that keypress. Reads (session list, transcript tail) are asynchronous and
don't block the UI.
