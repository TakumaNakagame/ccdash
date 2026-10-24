# 0008 — Hub high availability: shared state in a Secret, replicas forward to each other

- Date: 2026-10-15
- Status: Accepted

## Context

The hub (0003) has become the main way into ccdash, from a browser and from
the phone. In the operator's setup it ran as one replica on a home Kubernetes
cluster with its state in `hub.sqlite` on a ReadWriteOnce volume. On 2026-10-15 two
node drains moved the Pod. Each move gave about a minute of 502/503, because
the disk had to be detached from one VM and attached to another
(Multi-Attach), plus a few seconds while the devices' tunnels reconnected.

What the hub holds:

- **Persistent:** devices (token hash, peer access), the kv rows (cookie
  signing key, VAPID keys, quick commands, prefs) and push subscriptions. It
  is small and rarely written: on join / rotate / rename / delete, on a push
  (un)subscribe or a prefs edit, and as a last-seen stamp.
- **In memory:** each device's tunnel (`h.conns`). A request for a device
  can only be served through the tunnel that device dialed. The watchers
  (`watchDevice`: push notifications, the board snapshots) run per tunnel.

Options considered:

- **(a) One replica, restarting faster** (PDB, priority, topology, startup).
  None of these removes the volume move, which is most of the minute. It
  shaves only seconds.
- **(b) Active/standby** (a Lease picks the leader, which serves; shared
  DB). It still needs a shared DB, and a failover is lease expiry plus
  readiness, about 5–15 s.
- **(c) Active/active.** It needs shared state, plus a way to reach a
  device's tunnel from any replica: forward between replicas, or have each
  device connect to every replica.

For shared state: the cluster has no HA Postgres (netbox's is a single
StatefulSet on the same kind of RWO volume) and no S3. CNPG would add an
operator and two instances to run and back up, for a few KB of data.
SQLite on NFS cannot have two writers.

## Decision

**(c) with two pieces and no new infrastructure.** Two replicas is the
expected setup.

1. **State in a Kubernetes Secret** (`--state-secret NAME`,
   `internal/hub/k8sstore.go`). The whole state is one JSON document under
   `data["state.json"]`. Each write is a read-modify-write guarded by
   `resourceVersion`, and a 409 re-reads and retries, so replicas racing on
   an empty state agree on one cookie, VAPID or cluster key. Reads come from
   a copy refreshed every 2 s. A device token that isn't found triggers an
   immediate re-read (rate-limited), so a device joined through one replica
   can connect to the other at once. Last-seen stamps are written at most
   every 5 min. When the Secret doesn't exist yet and `<data-dir>/hub.sqlite`
   does, the file is imported once: the move keeps every device's token, the
   cookie key (no re-login) and the VAPID keys (subscriptions stay valid).
   The talking to the API server is hand-rolled (get / create / update of
   one Secret over the service account) rather than client-go, which would
   be a large dependency for three calls. `stateStore`
   (`internal/hub/state.go`) is the seam; SQLite stays the default and the
   local portal's backend. Both return `sql.ErrNoRows` for a missing row.
2. **Replicas forward to each other** (`--cluster-peers`, `cluster.go`).
   - **Tunnels:** each device keeps its one tunnel, to whichever replica the
     Service picked. The devices are unchanged.
   - **Finding the others:** replicas find each other through a headless
     Service (DNS lists only Ready Pods). They talk on a separate cluster
     port (8081, never behind the ingress), and every request is
     HMAC-signed with a `cluster_key` from the shared state (timestamped,
     ±60 s).
   - **Polling:** every 2 s each replica polls the others' `/cluster/state`:
     which tunnels they hold and their watchers' snapshots.
   - **Forwarding:** `h.conn(id)` returns this replica's tunnel, else a
     `deviceConn` whose `open` dials `/cluster/stream/{id}` on the holder.
     That is an HTTP upgrade spliced onto one yamux stream there, so the
     device proxy, the PTY WebSocket and resize work unchanged. A miss
     re-polls once (the device may have just moved).
   - **Push stays single:** only the replica holding the tunnel watches the
     device, so each change is pushed once. The board, `/api/active`, the
     device list, `/s/<id>` and the push badge read local plus peer
     snapshots (`allSnapshots`).
   - **One tunnel per device:** a new tunnel broadcasts
     `/cluster/drop/{id}?since=`, and a peer still holding an older
     (dead) tunnel closes it, so only one watcher survives.
   - **Revocation reaches every replica:** delete / rotate broadcasts
     `drop?reload=1`, which re-reads the state first, so the old token can't
     reconnect there. Any state refresh that shows a device gone or its
     token changed also drops that tunnel (`revalidateConns`).
   - **No lone-replica misconfiguration:** `--cluster-peers` without
     `--state-secret` is refused, because a private hub.sqlite per replica
     would split the device registry.

### Behaviour on a drain or rollout

- **The Pod that is going away:** the preStop sleep takes it out of the
  Service. On SIGTERM it closes its tunnels.
- **Its devices:** they reconnect after the collector's 1 s backoff and land
  on the other replica. Requests for them fail for about 1–3 s: a
  forwarding replica re-polls on a miss, but the device itself is
  reconnecting.
- **Everything else** is unaffected: browsers on either replica, and
  devices on the other replica.
- **There is no volume move.**

### Accepted trade-offs

- **Small staleness.** A replica's copy of the state can be up to 2 s old
  (a rename, a peer-access change). The peer list can also be up to 2 s
  stale; a miss forces a re-poll.
- **No catch-up after a move.** When a device moves, its new watcher starts
  from a baseline, as after any hub restart. A change in the second or two
  of the move may not be pushed.
- **Bigger RBAC.** The hub needs `create` on Secrets in its namespace (RBAC
  can't restrict create by name) and get/update on its own Secret only.
- **The API server becomes a dependency.** It is needed at startup. While
  it is unreachable, reads keep working from the copy, but joins,
  subscriptions and prefs edits fail.

## Consequences

- Kubernetes manifest: `replicas: 2`, a ServiceAccount + Role, a headless
  Service, `POD_IP` from the downward API, a PDB (`maxUnavailable: 1`),
  spread over nodes, and no PVC (after the one-time import).
- A device that connected through an older collector needs nothing new.
- A hub not on Kubernetes keeps working exactly as before (single process,
  hub.sqlite).
