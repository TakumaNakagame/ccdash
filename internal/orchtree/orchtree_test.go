package orchtree

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// fixture is internal/hub/testdata/orchtree.json: the cases the portal's
// orchtree block passes under node (hub.TestOrchTree).
type fixture struct {
	Order []struct {
		Why string `json:"why"`
		Opt struct {
			Flat      bool     `json:"flat"`
			Lift      bool     `json:"lift"`
			Collapsed []string `json:"collapsed"`
		} `json:"opt"`
		Rows   []model.Session `json:"rows"`
		Legacy bool            `json:"legacy"`
		Want   []string        `json:"want"`
	} `json:"order"`
	Groups []struct {
		Why   string        `json:"why"`
		Row   model.Session `json:"row"`
		Shown time.Time     `json:"shown"`
		Want  string        `json:"want"`
	} `json:"groups"`
	Shown []struct {
		Why    string        `json:"why"`
		Parent model.Session `json:"parent"`
		Now    time.Time     `json:"now"`
		Want   time.Time     `json:"want"`
	} `json:"shown"`
	Counts []struct {
		Why  string            `json:"why"`
		Rows []model.Session   `json:"rows"`
		Want map[string][2]int `json:"want"`
	} `json:"counts"`
	Subtree []struct {
		Why  string          `json:"why"`
		Rows []model.Session `json:"rows"`
		Root string          `json:"root"`
		Want []string        `json:"want"`
	} `json:"subtree"`
	Inbox []struct {
		Why  string          `json:"why"`
		Rows []model.Session `json:"rows"`
		Want [][]any         `json:"want"`
	} `json:"inbox"`
}

// fixtureKeys are the sections TestFixture runs; a section added to the
// file without a Go case here fails the test instead of being skipped.
var fixtureKeys = []string{"about", "order", "groups", "shown", "counts", "subtree", "inbox"}

// fixtureGroup maps the Go group names (my_sessions' keys) to the
// fixture's, which are the portal's.
var fixtureGroup = map[string]string{GroupNeeds: "needs", GroupWorking: "working", GroupReported: "reported", GroupFinished: "finished"}

func line(e Entry) string {
	return fmt.Sprintf("%s|%s|%d|%s", e.S.SessionID, e.Bucket, e.Depth, e.Via)
}

// newFirst is the portal's old 新着を上に (before the tree): unread rows in
// a block at the top, last moved first.
func newFirst(rows []model.Session) []model.Session {
	var fresh, rest []model.Session
	for _, s := range rows {
		if s.Unseen {
			fresh = append(fresh, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.SliceStable(fresh, func(i, j int) bool { return ActivityAt(fresh[i]).After(ActivityAt(fresh[j])) })
	return append(fresh, rest...)
}

// TestFixture runs every case of the shared fixture through the Go twin.
func TestFixture(t *testing.T) {
	raw, err := os.ReadFile("../hub/testdata/orchtree.json")
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, k := range fixtureKeys {
		known[k] = true
	}
	for k := range keys {
		if !known[k] {
			t.Errorf("fixture section %q has no Go case", k)
		}
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	n := 0
	check := func(why string, got, want any) {
		t.Helper()
		n++
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s\n  got  %v\n  want %v", why, got, want)
		}
	}
	lines := func(es []Entry) []string {
		out := make([]string, 0, len(es))
		for _, e := range es {
			out = append(out, line(e))
		}
		return out
	}
	for _, c := range fx.Order {
		opt := Options{Flat: c.Opt.Flat, Lift: c.Opt.Lift, Collapsed: map[string]bool{}}
		for _, id := range c.Opt.Collapsed {
			opt.Collapsed[id] = true
		}
		rows := ListSort(c.Rows)
		got := lines(ListOrder(rows, opt))
		check(c.Why, got, c.Want)
		if !c.Legacy {
			continue
		}
		// What the list did before orchestrators: the same rows, the
		// same bands.
		var old []model.Session
		switch {
		case opt.Flat:
			old = ByActivity(rows)
		case opt.Lift:
			old = newFirst(LaterFirst(ProjectsFirst(rows)))
		default:
			old = LaterFirst(ProjectsFirst(rows))
		}
		var before []string
		for _, s := range old {
			band := ""
			switch {
			case opt.Flat:
			case opt.Lift && s.Unseen:
				band = BucketNew
			case s.Later:
				band = BucketLater
			case s.Project != "":
				band = ProjectBucket + s.Project
			}
			before = append(before, s.SessionID+"|"+band+"|0|")
		}
		check(c.Why+" (as before the tree)", got, before)
	}
	for _, c := range fx.Groups {
		check(c.Why, fixtureGroup[ChildGroup(c.Row, c.Shown)], c.Want)
	}
	for _, c := range fx.Shown {
		check(c.Why, ChildShown(c.Parent, c.Now).UTC(), c.Want.UTC())
	}
	for _, c := range fx.Counts {
		got := map[string][2]int{}
		for id, v := range NewIndex(c.Rows).Counts() {
			got[id] = [2]int{v.N, v.Needs}
		}
		check(c.Why, got, c.Want)
	}
	for _, c := range fx.Subtree {
		check(c.Why, NewIndex(c.Rows).Subtree(c.Root), c.Want)
	}
	for _, c := range fx.Inbox {
		var got [][]any
		for _, g := range InboxGroups(c.Rows) {
			orch := ""
			if g.Orch != nil {
				orch = g.Orch.SessionID
			}
			ids := []any{}
			for _, s := range g.Rows {
				ids = append(ids, s.SessionID)
			}
			got = append(got, []any{orch, g.Project, ids})
		}
		check(c.Why, got, c.Want)
	}
	if len(fx.Order) == 0 || len(fx.Groups) == 0 || len(fx.Shown) == 0 || len(fx.Counts) == 0 || len(fx.Subtree) == 0 || len(fx.Inbox) == 0 {
		t.Fatal("a fixture section is empty")
	}
	t.Logf("%d checks", n)
}

// TestBig: a thousand rows, one long chain of parents among them — every
// row laid out once, the chain counted, no deep recursion, O(n).
func TestBig(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var big []model.Session
	for i := 0; i < 1000; i++ {
		s := model.Session{SessionID: fmt.Sprint("s", i), Num: int64(i), FirstSeen: at, LastSeen: at.Add(24 * time.Hour)}
		if i > 0 {
			s.Parent = fmt.Sprint("s", i-1)
		} else {
			s.Role = model.RoleOrchestrator
		}
		if i%3 == 0 {
			s.Project = fmt.Sprint("p", i%7)
		}
		big = append(big, s)
	}
	t0 := time.Now()
	laid := ListOrder(ListSort(big), Options{})
	ids := map[string]bool{}
	for _, e := range laid {
		ids[e.S.SessionID] = true
	}
	if len(laid) != 1000 || len(ids) != 1000 {
		t.Fatalf("laid out %d rows, %d distinct; want 1000", len(laid), len(ids))
	}
	if c := NewIndex(big).Count("s0"); c.N != 999 {
		t.Fatalf("chain counted %d, want 999", c.N)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("1000 rows took %s", d)
	}
}

// TestChildGroups: the four sections, each newest change first.
func TestChildGroups(t *testing.T) {
	shown := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	rows := []model.Session{
		{SessionID: "a", Status: model.StatusActive, LastSeen: shown},
		{SessionID: "b", Status: model.StatusActive, LastSeen: shown.Add(time.Minute)},
		{SessionID: "c", Status: model.StatusIdle, PendingCount: 1},
		{SessionID: "d", Status: model.StatusStopped, LastActivity: shown.Add(-time.Hour)},
		{SessionID: "e", Status: model.StatusStopped, Unseen: true},
	}
	g := ChildGroups(rows, shown)
	ids := func(k string) (out []string) {
		for _, s := range g[k] {
			out = append(out, s.SessionID)
		}
		return
	}
	for k, want := range map[string][]string{GroupNeeds: {"c"}, GroupWorking: {"b", "a"}, GroupReported: {"e"}, GroupFinished: {"d"}} {
		if got := ids(k); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
}
