package hub

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// toolHeadRunner feeds the cases to the block of app.js between the
// ">>> toolhead" and "<<< toolhead" markers and prints what differs.
const toolHeadRunner = `
const fs = require("fs");
const src = fs.readFileSync(0, "utf8");
const cases = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const api = new Function(src + "; return { toolName, toolHead, toolCommand, toolRest };")();
let bad = 0;
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
for (const c of cases) {
  const got = { ...api.toolHead(c.name, c.input), shown: api.toolName(c.name), command: api.toolCommand(c.input), rest: api.toolRest(c.input) };
  const want = { text: c.text, said: c.said, shown: c.shown, command: c.command, rest: c.rest };
  if (!same(got, want)) { bad++; console.log(c.why + "\n  got  " + JSON.stringify(got) + "\n  want " + JSON.stringify(want)); }
}
for (const inp of [null, undefined, "x", [], 3]) {
  const g = api.toolHead("Bash", inp);
  if (g.text !== "" || g.said !== false) { bad++; console.log("odd input " + JSON.stringify(inp) + " → " + JSON.stringify(g)); }
}
const long = api.toolHead("Grep", { pattern: "a".repeat(500) }).text;
if (long.length !== 200) { bad++; console.log("a headline is capped at 200 characters, got " + long.length); }
console.log(cases.length + " cases");
process.exit(bad ? 1 : 0);
`

// TestToolHead: what a tool call's row says in the chat (the description
// Claude gave, else the argument that names the call) — the rules live in
// app.js, so they run under node. The Android app pins the same cases.
func TestToolHead(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is needed in CI to test the portal's tool headlines")
		}
		t.Skip("node not installed")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)// >>> toolhead\n(.*?)// <<< toolhead`).FindSubmatch(app)
	if m == nil {
		t.Fatal("app.js has no >>> toolhead … <<< toolhead block")
	}
	cmd := exec.Command(node, "-e", toolHeadRunner, "testdata/toolhead.json")
	cmd.Stdin = bytes.NewReader(m[1])
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", bytes.TrimSpace(out))
}
