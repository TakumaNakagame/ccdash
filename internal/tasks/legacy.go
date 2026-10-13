package tasks

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// LegacyFile is work-status's tasks.json: what `ccdash tasks import`
// reads and `ccdash tasks export` writes.
type LegacyFile struct {
	Updated          string       `json:"updated,omitempty"`
	Tasks            []model.Task `json:"tasks"`
	Rev              int64        `json:"rev,omitempty"`
	NextID           int64        `json:"next_id,omitempty"`
	LastTimelineSync string       `json:"last_timeline_sync,omitempty"`
}

// ParseLegacy reads a tasks.json.
func ParseLegacy(b []byte) (LegacyFile, error) {
	var f LegacyFile
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("tasks.json: %w", err)
	}
	for i, t := range f.Tasks {
		if t.ID <= 0 || strings.TrimSpace(t.Title) == "" {
			return f, fmt.Errorf("tasks.json: task %d has no id or title", i)
		}
	}
	return f, nil
}

// ImportOptions maps the file's free-text projects onto ccdash projects.
type ImportOptions struct {
	// Map is old project → ccdash project ("" = no project).
	Map map[string]string
	// Unmapped decides old projects that are neither in Map nor an
	// existing project: "" refuses the import, "create" makes them
	// projects, "drop" leaves those tasks without one.
	Unmapped string
}

// ImportPlan is the dry run: what each old project becomes, and how many
// tasks come in archived.
type ImportPlan struct {
	Projects  map[string]string `json:"projects"` // old → new ("" = none)
	Create    []string          `json:"create"`   // projects the import creates
	Unmapped  []string          `json:"unmapped"` // old values with no decision
	Tasks     int               `json:"tasks"`
	Archived  int               `json:"archived"`
	NextID    int64             `json:"next_id"`
	LastSync  string            `json:"last_sync,omitempty"`
	Converted []model.Task      `json:"-"`
}

// PlanImport converts the file's tasks: projects mapped, finished tasks
// from before today archived, timeline refs filled in. exists reports
// whether a ccdash project of that name is there already.
func PlanImport(f LegacyFile, opt ImportOptions, exists func(string) bool, now time.Time) (ImportPlan, error) {
	today := DateString(Day(now))
	p := ImportPlan{Projects: map[string]string{}, LastSync: f.LastTimelineSync}
	create := map[string]bool{}
	seen := map[string]bool{}
	var maxID int64
	for _, t := range f.Tasks {
		old := strings.TrimSpace(t.Project)
		if old == "" || seen[old] {
			continue
		}
		seen[old] = true
		nv, mapped := opt.Map[old]
		switch {
		case mapped:
		case exists(old):
			nv = old
		case opt.Unmapped == "create":
			nv = old
		case opt.Unmapped == "drop":
			nv = ""
		default:
			p.Unmapped = append(p.Unmapped, old)
			continue
		}
		nv = strings.TrimSpace(nv)
		p.Projects[old] = nv
		if nv != "" && !exists(nv) {
			create[nv] = true
		}
	}
	for c := range create {
		p.Create = append(p.Create, c)
	}
	sort.Strings(p.Create)
	sort.Strings(p.Unmapped)
	for _, t := range f.Tasks {
		t.Project = p.Projects[strings.TrimSpace(t.Project)]
		if t.Priority < 1 || t.Priority > 4 {
			t.Priority = 4
		}
		if !ValidStatus(t.Status) {
			t.Status = model.TaskTodo
		}
		if t.Links == nil {
			t.Links = []string{}
		}
		if t.Finished() && t.DoneOn < today {
			t.Archived = true
			p.Archived++
		}
		tl := t.Timeline
		t.Timeline = []model.TaskNote{}
		for _, n := range tl {
			AddNote(&t, n)
		}
		t.Bucket = ""
		maxID = max(maxID, t.ID)
		p.Converted = append(p.Converted, t)
	}
	p.Tasks = len(p.Converted)
	p.NextID = max(f.NextID, maxID+1)
	if len(p.Unmapped) > 0 {
		return p, fmt.Errorf("no decision for projects %q: map them (--map) or pass --unmapped create|drop", p.Unmapped)
	}
	return p, nil
}

// Markdown renders the list the way work-status's TASKS.md did.
func Markdown(all []model.Task, now time.Time) string {
	ts := make([]model.Task, 0, len(all))
	for _, t := range all {
		if !t.Archived {
			ts = append(ts, t)
		}
	}
	Sort(ts, now)
	var b strings.Builder
	fmt.Fprintf(&b, "# タスクリスト\n\n最終更新: %s（ccdash tasks md）\n\n", DateString(Day(now)))
	cur := ""
	for _, t := range ts {
		if t.Finished() {
			continue
		}
		if t.Bucket != cur {
			if cur != "" {
				b.WriteString("\n")
			}
			cur = t.Bucket
			fmt.Fprintf(&b, "## %s\n\n", Heading(cur, now))
		}
		extra := []string{fmt.Sprintf("P%d", priorityKey(t))}
		if t.Status != model.TaskTodo {
			extra = append(extra, StatusLabel[t.Status])
		}
		if CarriedOver(t, now) {
			extra = append(extra, mdDay(t.Plan)+" から持ち越し")
		} else if t.Bucket == BucketWeek || t.Bucket == BucketNextWeek || t.Bucket == BucketLater {
			extra = append(extra, mdDay(t.Plan)+" にやる")
		}
		if t.Due != "" {
			extra = append(extra, "期限 "+t.Due)
		}
		if t.Project != "" {
			extra = append(extra, t.Project)
		}
		fmt.Fprintf(&b, "- [ ] #%d %s（%s）\n", t.ID, t.Title, strings.Join(extra, " / "))
		if n := len(t.Timeline); n > 0 {
			e := t.Timeline[n-1]
			fmt.Fprintf(&b, "  - 最新 %s: %s\n", strings.Replace(clip(e.At, 16), "T", " ", 1)[min(5, len(clip(e.At, 16))):], e.Text)
		}
	}
	if cur != "" {
		b.WriteString("\n")
	}
	first := true
	for _, t := range ts {
		if !t.Finished() {
			continue
		}
		if first {
			b.WriteString("## 完了\n\n")
			first = false
		}
		mark := "x"
		if t.Status == model.TaskCancelled {
			mark = "-"
		}
		fmt.Fprintf(&b, "- [%s] #%d %s\n", mark, t.ID, t.Title)
	}
	return b.String()
}

func mdDay(d string) string {
	if len(d) < 10 {
		return d
	}
	return strings.Replace(d[5:], "-", "/", 1)
}
