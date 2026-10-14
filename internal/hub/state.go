package hub

import "context"

// stateStore is where the hub keeps what must survive a restart: the device
// registry, the kv rows (cookie / VAPID / cluster keys, quick links, prefs)
// and the push subscriptions. Two backends: the hub.sqlite file (store, the
// default and the local portal's) and a Kubernetes Secret (k8sStore,
// --state-secret), which lets several hub replicas share one state without
// a volume (docs/decisions/0008-hub-ha.md). Missing rows are sql.ErrNoRows
// in both, so the handlers don't care which one runs.
type stateStore interface {
	Close() error

	// secret returns the value of k, creating it with gen on first use —
	// atomically, so replicas racing on an empty store agree on one value.
	secret(ctx context.Context, k string, gen func() string) (string, error)
	putKV(ctx context.Context, k, v string) error

	createDevice(ctx context.Context, name string) (Device, string, error)
	rotateToken(ctx context.Context, id string) (string, error)
	deleteDevice(ctx context.Context, id string) error
	renameDevice(ctx context.Context, id, name string) error
	listDevices(ctx context.Context) ([]Device, error)
	deviceByToken(ctx context.Context, tok string) (Device, error)
	touchDevice(ctx context.Context, id, hostname, version string) error
	setPeerAccess(ctx context.Context, id, access string) error
	setLetter(ctx context.Context, id, letter string) error
	ensureLocalDevice(ctx context.Context, name string) (Device, error)

	addSub(ctx context.Context, email string, sub pushSub) error
	removeSub(ctx context.Context, endpoint string) error
	subByEndpoint(ctx context.Context, endpoint string) (pushSub, error)
	listSubs(ctx context.Context) ([]pushSub, error)
}

var _ stateStore = (*store)(nil)
