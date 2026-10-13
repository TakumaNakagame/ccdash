package tasks

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

// Context is what `ccdash tasks context` / the task_context MCP tool hand
// an automated run: the open tasks with their Slack threads, Jira keys and
// PRs pulled out of the links, and where to start looking (since). Same
// shape as the old timeline_context.py.
type Context struct {
	Since string        `json:"since"`
	Now   string        `json:"now"`
	Today string        `json:"today"`
	Tasks []ContextTask `json:"tasks"`
	// Projects are the ccdash projects a new task may name (a sync drops
	// any other project).
	Projects []string `json:"projects"`
}

// ContextTask is one open task in a Context.
type ContextTask struct {
	ID             int64         `json:"id"`
	CreatedOn      string        `json:"created_on"`
	Key            string        `json:"key"`
	Title          string        `json:"title"`
	Status         string        `json:"status"`
	Priority       int           `json:"priority"`
	Plan           string        `json:"plan"`
	Due            string        `json:"due"`
	Project        string        `json:"project"`
	PlanFixedToday bool          `json:"plan_fixed_today"`
	LastActivity   string        `json:"last_activity"`
	SlackThreads   []SlackThread `json:"slack_threads"`
	Jira           []string      `json:"jira"`
	PRs            []string      `json:"prs"`
	KnownRefs      []string      `json:"known_refs"`
}

// SlackThread is a thread a task links to.
type SlackThread struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
}

var (
	slackRE = regexp.MustCompile(`^https://[\w.-]*slack\.com/archives/([A-Z0-9]+)/p(\d{16})(?:\?(\S*))?`)
	jiraRE  = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)
	prRE    = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)
)

func slackThread(link string) (SlackThread, bool) {
	m := slackRE.FindStringSubmatch(link)
	if m == nil {
		return SlackThread{}, false
	}
	th := SlackThread{Channel: m[1], ThreadTS: m[2][:10] + "." + m[2][10:]}
	if q, err := url.ParseQuery(m[3]); err == nil {
		if v := q.Get("cid"); v != "" {
			th.Channel = v
		}
		if v := q.Get("thread_ts"); v != "" {
			th.ThreadTS = v
		}
	}
	return th, true
}

// BuildContext lists the open, unarchived tasks. lastSync is the previous
// run's synced_at; without one the run looks back 24 hours.
func BuildContext(all []model.Task, lastSync string, now time.Time) Context {
	since := lastSync
	if since == "" {
		since = Stamp(now.Add(-24 * time.Hour))
	}
	today := DateString(Day(now))
	out := Context{Since: since, Now: Stamp(now), Today: today, Tasks: []ContextTask{}, Projects: []string{}}
	for _, t := range all {
		if t.Finished() || t.Archived {
			continue
		}
		key := t.Key
		if key == "" {
			key = fmt.Sprintf("id:%d", t.ID)
		}
		ct := ContextTask{
			ID: t.ID, CreatedOn: t.CreatedOn, Key: key, Title: t.Title, Status: t.Status,
			Priority: t.Priority, Plan: t.Plan, Due: t.Due, Project: t.Project,
			PlanFixedToday: t.PlanBy == "manual" && t.PlanOn == today,
			SlackThreads:   []SlackThread{}, KnownRefs: []string{},
		}
		if n := len(t.Timeline); n > 0 {
			ct.LastActivity = clip(t.Timeline[n-1].At, 10)
			for _, e := range t.Timeline[max(0, n-20):] {
				ct.KnownRefs = append(ct.KnownRefs, e.Ref)
			}
		}
		seen := map[SlackThread]bool{}
		for _, l := range t.Links {
			if th, ok := slackThread(l); ok && !seen[th] {
				seen[th] = true
				ct.SlackThreads = append(ct.SlackThreads, th)
			}
		}
		text := strings.Join(append([]string{t.Key, t.Title, t.Project}, t.Links...), " ")
		ct.Jira = uniqSorted(jiraRE.FindAllString(text, -1))
		ct.PRs = uniqSorted(prRE.FindAllString(strings.Join(t.Links, " "), -1))
		out.Tasks = append(out.Tasks, ct)
	}
	return out
}

func uniqSorted(in []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
