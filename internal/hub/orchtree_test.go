package hub

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// orchTreeRunner feeds testdata/orchtree.json to the block of app.js
// between the ">>> orchtree" and "<<< orchtree" markers and prints what
// differs.
const orchTreeRunner = `
const fs = require("fs");
const src = fs.readFileSync(0, "utf8");
const fx = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const api = new Function(src + "; return { listSort, listOrder, laterFirst, newFirst, projectsFirst, byActivity, childGroup, childShown, treeIndex, subtreeIds, inboxGroups };")();
let bad = 0, n = 0;
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const check = (why, got, want) => { n++; if (!same(got, want)) { bad++; console.log(why + "\n  got  " + JSON.stringify(got) + "\n  want " + JSON.stringify(want)); } };
const line = (e) => e.s.session_id + "|" + e.bucket + "|" + e.depth + "|" + (e.via ? e.via.session_id : "");
for (const c of fx.order) {
  const opt = { ...c.opt, collapsed: new Set(c.opt.collapsed || []) };
  const rows = api.listSort(c.rows);
  const got = api.listOrder(rows, opt);
  check(c.why, got.map(line), c.want);
  if (!c.legacy) continue;
  // What the list did before orchestrators: the same rows, the same bands.
  const old = c.opt.flat ? api.byActivity(rows) : c.opt.lift ? api.newFirst(api.laterFirst(api.projectsFirst(rows))) : api.laterFirst(api.projectsFirst(rows));
  const band = (s) => c.opt.flat ? "" : c.opt.lift && s.unseen ? "new" : s.later ? "later" : s.project ? "p:" + s.project : "";
  check(c.why + " (as before the tree)", got.map(line), old.map((s) => s.session_id + "|" + band(s) + "|0|"));
}
for (const c of fx.groups) check(c.why, api.childGroup(c.row, Date.parse(c.shown)), c.want);
for (const c of fx.shown) check(c.why, api.childShown(c.parent, Date.parse(c.now)), Date.parse(c.want));
for (const c of fx.counts) check(c.why, Object.fromEntries([...api.treeIndex(c.rows).counts].map(([k, v]) => [k, [v.n, v.needs]])), c.want);
for (const c of fx.subtree) check(c.why, [...api.subtreeIds(c.rows, c.root)], c.want);
for (const c of fx.inbox) check(c.why, api.inboxGroups(c.rows).map((g) => [g.orch ? g.orch.session_id : "", g.project, g.rows.map((s) => s.session_id)]), c.want);
// O(n): a thousand rows, one long chain of parents among them.
const big = [];
for (let i = 0; i < 1000; i++) big.push({ session_id: "s" + i, num: i, first_seen: "2026-11-01T00:00:00Z", last_seen: "2026-11-02T00:00:00Z", parent: i ? "s" + (i - 1) : "", role: i ? "" : "orchestrator", project: i % 3 ? "" : "p" + (i % 7) });
const t0 = Date.now();
const laid = api.listOrder(api.listSort(big), {});
check("every row is laid out once", [laid.length, new Set(laid.map((e) => e.s.session_id)).size], [1000, 1000]);
check("the chain is counted", api.treeIndex(big).counts.get("s0").n, 999);
if (Date.now() - t0 > 2000) { bad++; console.log("1000 rows took " + (Date.now() - t0) + " ms"); }
console.log(n + " checks");
process.exit(bad ? 1 : 0);
`

// TestOrchTree: the session list's order with orchestrators (who is fixed
// where, who hangs under whom), the grouping of an orchestrator's children
// and of the sessions that wait on the operator — the rules live in app.js,
// so they run under node. The legacy cases also pin that a list without
// any role or parent is laid out exactly as before. The Android app and
// the TUI follow the same cases.
func TestOrchTree(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is needed in CI to test the portal's list order")
		}
		t.Skip("node not installed")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)// >>> orchtree\n(.*?)// <<< orchtree`).FindSubmatch(app)
	if m == nil {
		t.Fatal("app.js has no >>> orchtree … <<< orchtree block")
	}
	cmd := exec.Command(node, "-e", orchTreeRunner, "testdata/orchtree.json")
	cmd.Stdin = bytes.NewReader(m[1])
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", bytes.TrimSpace(out))
}
