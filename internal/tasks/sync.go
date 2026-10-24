package tasks

import (
	"fmt"
	"strings"
	"time"

	"github.com/TakumaNakagame/ccdash/internal/i18n"
	"github.com/TakumaNakagame/ccdash/internal/model"
)

// Proposal is what an automated run (e.g. a scheduled claude -p that reads
// Slack / Jira / GitHub) pipes into `ccdash tasks sync` or hands to the
// sync_tasks MCP tool.
type Proposal struct {
	SyncedAt string         `json:"synced_at,omitempty"`
	Tasks    []ProposalTask `json:"tasks"`
	// DryRun reports what the merge would do and saves nothing.
	DryRun bool `json:"dry_run,omitempty"`
}

// ProposalTask is one task as the run saw it, matched by Key.
type ProposalTask struct {
	Key      string           `json:"key"`
	Title    string           `json:"title"`
	Priority int              `json:"priority,omitempty"`
	Plan     string           `json:"plan,omitempty"`
	Due      string           `json:"due,omitempty"`
	Project  string           `json:"project,omitempty"`
	Note     string           `json:"note,omitempty"`
	Links    []string         `json:"links,omitempty"`
	Done     bool             `json:"done,omitempty"`
	Status   string           `json:"status,omitempty"`
	Timeline []model.TaskNote `json:"timeline,omitempty"`
	Replan   *ProposalReplan  `json:"replan,omitempty"`
}

// ProposalReplan moves an existing task's plan, with the reason.
type ProposalReplan struct {
	Plan string `json:"plan"`
	Why  string `json:"why"`
}

// SyncResult reports what a merge did, per kind, as task titles.
type SyncResult struct {
	Added    []string `json:"added"`
	Status   []string `json:"status"`
	Replan   []string `json:"replan"`
	Notes    []string `json:"notes"`
	Updated  []string `json:"updated"`
	Skipped  []string `json:"skipped"`
	Warnings []string `json:"warnings,omitempty"`
	DryRun   bool     `json:"dry_run,omitempty"`
}

// Touched reports whether the merge changed any task.
func (r SyncResult) Touched() bool {
	return len(r.Added)+len(r.Status)+len(r.Replan)+len(r.Notes)+len(r.Updated) > 0
}

// String is the summary `ccdash tasks sync` prints (the old script's).
func (r SyncResult) String() string {
	var b strings.Builder
	mode := ""
	if r.DryRun {
		mode = i18n.T("（dry-run）")
	}
	groups := []struct {
		label string
		items []string
	}{{i18n.T("追加"), r.Added}, {i18n.T("状態変更"), r.Status}, {i18n.T("やる日変更"), r.Replan}, {i18n.T("進捗追記"), r.Notes}, {i18n.T("期限・リンク更新"), r.Updated}}
	parts := make([]string, len(groups))
	for i, g := range groups {
		parts[i] = fmt.Sprintf("%s %d", g.label, len(g.items))
	}
	b.WriteString(i18n.T("タスクリスト") + mode + ": " + strings.Join(parts, " / ") + "\n")
	for _, g := range groups {
		for _, it := range g.items {
			fmt.Fprintf(&b, "- %s: %s\n", g.label, it)
		}
	}
	for _, it := range r.Skipped {
		fmt.Fprint(&b, i18n.Tf("- やる日変更を見送り: %s\n", it))
	}
	for _, it := range r.Warnings {
		fmt.Fprint(&b, i18n.Tf("- 注意: %s\n", it))
	}
	return b.String()
}

// Merge applies p to all (the whole list, archived included) the way the
// person's edits survive it:
//
//   - tasks are matched by key; a proposal without key or title is ignored;
//   - an existing task gets its due date updated, links added, new
//     timeline entries appended — title, priority and note stay as they are;
//   - its plan moves only through replan (with a reason), and never when
//     the person set the plan today;
//   - its status changes to done whenever the run says so, to anything
//     else only when the same run brought new timeline entries; a finished
//     task (done / cancelled) is never reopened;
//   - a new task is added unless it is already done; its project is kept
//     only if knownProject says the project exists;
//   - tasks the proposal doesn't mention are left alone.
//
// It returns the tasks to save (new ones have ID 0) and the summary.
func Merge(all []*model.Task, p Proposal, knownProject func(string) bool, now time.Time) (changed []*model.Task, res SyncResult) {
	byKey := map[string]*model.Task{}
	for _, t := range all {
		if t.Key != "" {
			byKey[t.Key] = t
		}
	}
	mark := map[*model.Task]bool{}
	touch := func(t *model.Task) {
		if !mark[t] {
			mark[t] = true
			changed = append(changed, t)
		}
	}
	for _, pt := range p.Tasks {
		key := strings.TrimSpace(pt.Key)
		if key == "" || strings.TrimSpace(pt.Title) == "" {
			continue
		}
		status := wantedStatus(pt)
		cur := byKey[key]
		if cur == nil {
			if status == model.TaskDone || status == model.TaskCancelled {
				continue
			}
			project := strings.TrimSpace(pt.Project)
			if project != "" && (knownProject == nil || !knownProject(project)) {
				res.Warnings = append(res.Warnings, i18n.Tf("%s: プロジェクト「%s」は無いので付けなかった", pt.Title, project))
				project = ""
			}
			t, err := New(Input{
				Key: key, Title: pt.Title, Priority: pt.Priority, Status: status,
				Due: pt.Due, Project: project, Note: pt.Note, Links: pt.Links, Source: SourceAuto,
			}, now)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", pt.Title, err))
				continue
			}
			if plan, err := ParsePlan(pt.Plan, now); err == nil && plan != "" {
				t.Plan, t.PlanBy, t.PlanOn = plan, "auto", DateString(Day(now))
			}
			for _, n := range pt.Timeline {
				AddNote(&t, n)
			}
			nt := &t
			byKey[key] = nt
			touch(nt)
			res.Added = append(res.Added, t.Title)
			continue
		}

		added := 0
		for _, n := range pt.Timeline {
			if AddNote(cur, n) {
				added++
			}
		}
		if added > 0 {
			touch(cur)
			res.Notes = append(res.Notes, i18n.Tf("%s（%d 件）", cur.Title, added))
		}
		if status != "" && status != cur.Status && !cur.Finished() && (status == model.TaskDone || added > 0) {
			before := cur.Status
			SetStatus(cur, status, "", SourceAuto, now)
			touch(cur)
			res.Status = append(res.Status, i18n.Tf("%s（%s → %s）", cur.Title, i18n.T(StatusLabel[before]), i18n.T(StatusLabel[status])))
		}
		if rp := pt.Replan; rp != nil && !cur.Finished() {
			plan, err := ParsePlan(rp.Plan, now)
			before := cur.Plan
			switch {
			case err != nil:
				res.Skipped = append(res.Skipped, i18n.Tf("%s（日付が不正: %s）", cur.Title, rp.Plan))
			case plan == cur.Plan:
			default:
				if _, err := Replan(cur, plan, strings.TrimSpace(rp.Why), true, SourceAuto, now); err != nil {
					res.Skipped = append(res.Skipped, i18n.Tf("%s（%s）", cur.Title, skipReason(err)))
				} else {
					touch(cur)
					res.Replan = append(res.Replan, fmt.Sprintf("%s（%s → %s）", cur.Title, orUnset(before), orUnset(plan)))
				}
			}
		}
		diff := false
		if d := strings.TrimSpace(pt.Due); d != "" && d != cur.Due && ValidDate(d) {
			cur.Due = d
			diff = true
		}
		if links := dedup(cur.Links, pt.Links); len(links) != len(cur.Links) {
			cur.Links = links
			diff = true
		}
		if diff {
			touch(cur)
			res.Updated = append(res.Updated, cur.Title)
		}
	}
	return changed, res
}

func skipReason(err error) string {
	switch err.Error() {
	case "no reason given":
		return i18n.T("理由が無い")
	case "the plan was set by hand today":
		return i18n.T("今日手で決めたやる日")
	}
	return err.Error()
}

func wantedStatus(p ProposalTask) string {
	if p.Done {
		return model.TaskDone
	}
	if ValidStatus(p.Status) {
		return p.Status
	}
	return ""
}
