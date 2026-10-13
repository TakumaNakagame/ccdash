// Package tasks holds the to-do list's rules as plain functions over
// model.Task: the plan buckets, the order, edits with their timeline
// entries, and the merge an automated sync goes through
// (docs/decisions/0006-tasks.md). internal/db loads and saves; everything
// that decides something lives here so it can be tested without SQLite.
//
// Dates are the machine's local time. Callers pass "now" in so tests can
// pin it.
package tasks

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/takumanakagame/ccmanage/internal/model"
)

const dateLayout = "2006-01-02"

// Bucket keys in display order. finished holds done / cancelled tasks
// from before today (finished today stays in today).
const (
	BucketToday    = "today"
	BucketTomorrow = "tomorrow"
	BucketWeek     = "week"
	BucketNextWeek = "nextweek"
	BucketLater    = "later"
	BucketNone     = "none"
	BucketFinished = "finished"
)

var bucketOrder = []string{BucketToday, BucketTomorrow, BucketWeek, BucketNextWeek, BucketLater, BucketNone, BucketFinished}

var bucketLabel = map[string]string{
	BucketToday:    "今日",
	BucketTomorrow: "明日",
	BucketWeek:     "今週中",
	BucketNextWeek: "来週",
	BucketLater:    "再来週以降",
	BucketNone:     "未定",
	BucketFinished: "完了",
}

// StatusLabel names a status the way the list shows it.
var StatusLabel = map[string]string{
	model.TaskTodo:      "未着手",
	model.TaskDoing:     "進行中",
	model.TaskWaiting:   "待ち",
	model.TaskDone:      "完了",
	model.TaskCancelled: "中止",
}

// PriorityLabel names priorities 1–4.
var PriorityLabel = map[int]string{1: "P1 今すぐ", 2: "P2 今週", 3: "P3 近いうち", 4: "P4 いつか"}

// ValidStatus reports whether s is one of the five statuses.
func ValidStatus(s string) bool { _, ok := StatusLabel[s]; return ok }

// Day truncates t to its local date.
func Day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// DateString formats a day as YYYY-MM-DD.
func DateString(t time.Time) string { return t.Format(dateLayout) }

// Stamp is the timeline's timestamp format (local, with offset).
func Stamp(t time.Time) string { return t.Format(time.RFC3339) }

func parseDate(s string, loc *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation(dateLayout, s, loc)
	return t, err == nil
}

// NextBusinessDay is "tomorrow": the next weekday (no holidays).
func NextBusinessDay(today time.Time) time.Time {
	d := Day(today).AddDate(0, 0, 1)
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// mondayIndex is Monday = 0 … Sunday = 6, like Python's weekday().
func mondayIndex(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// nextMonday is the Monday after today's week.
func nextMonday(today time.Time) time.Time {
	d := Day(today)
	return d.AddDate(0, 0, 7-mondayIndex(d))
}

// ThisFriday is the "this week" shortcut (the week's close); NextMonday the
// "next week" one (the week's start, so it shows up in today then).
func ThisFriday(today time.Time) time.Time {
	d := Day(today)
	return d.AddDate(0, 0, 4-mondayIndex(d))
}

// NextMonday is the "next week" shortcut.
func NextMonday(today time.Time) time.Time { return nextMonday(today) }

// ParsePlan turns today / tomorrow / week / nextweek / YYYY-MM-DD / none
// (or "") into a plan date ("" = unplanned).
func ParsePlan(v string, now time.Time) (string, error) {
	today := Day(now)
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "", "none":
		return "", nil
	case "today":
		return DateString(today), nil
	case "tomorrow":
		return DateString(NextBusinessDay(today)), nil
	case "week":
		d := ThisFriday(today)
		if d.Before(today) {
			return "", fmt.Errorf("this Friday has passed; use nextweek or a date")
		}
		return DateString(d), nil
	case "nextweek":
		return DateString(nextMonday(today)), nil
	}
	d, ok := parseDate(strings.TrimSpace(v), now.Location())
	if !ok {
		return "", fmt.Errorf("plan must be today / tomorrow / week / nextweek / YYYY-MM-DD / none: %q", v)
	}
	return DateString(d), nil
}

// ValidDate reports whether s is "" or YYYY-MM-DD.
func ValidDate(s string) bool {
	if s == "" {
		return true
	}
	_, ok := parseDate(s, time.Local)
	return ok
}

// BucketOf places a task in the list as of now. A plan in the past stays
// in today ("carried over"); a task finished today stays in today too, so
// the day's work is still visible.
func BucketOf(t model.Task, now time.Time) string {
	today := Day(now)
	if t.Finished() {
		if t.DoneOn == DateString(today) {
			return BucketToday
		}
		return BucketFinished
	}
	if t.Plan == "" {
		return BucketNone
	}
	d, ok := parseDate(t.Plan, now.Location())
	if !ok {
		return BucketNone
	}
	switch nm := nextMonday(today); {
	case !d.After(today):
		return BucketToday
	case !d.After(NextBusinessDay(today)):
		return BucketTomorrow
	case d.Before(nm):
		return BucketWeek
	case d.Before(nm.AddDate(0, 0, 7)):
		return BucketNextWeek
	}
	return BucketLater
}

// CarriedOver reports a plan before today on an unfinished task.
func CarriedOver(t model.Task, now time.Time) bool {
	return !t.Finished() && t.Plan != "" && t.Plan < DateString(Day(now))
}

func shortDate(d time.Time) string {
	return fmt.Sprintf("%d/%d %s", int(d.Month()), d.Day(), []string{"月", "火", "水", "木", "金", "土", "日"}[mondayIndex(d)])
}

// Heading is a bucket's title with its dates, e.g. 今日（10/13 火）.
func Heading(bucket string, now time.Time) string {
	today := Day(now)
	nm := nextMonday(today)
	var span string
	switch bucket {
	case BucketToday:
		span = shortDate(today)
	case BucketTomorrow:
		span = shortDate(NextBusinessDay(today))
	case BucketWeek:
		span = "〜" + shortDate(nm.AddDate(0, 0, -3))
	case BucketNextWeek:
		// Fri–Sun: next Monday is already "tomorrow", so next week starts Tuesday.
		start := nm
		if s := NextBusinessDay(today).AddDate(0, 0, 1); s.After(start) {
			start = s
		}
		span = shortDate(start) + "〜" + shortDate(nm.AddDate(0, 0, 4))
	case BucketLater:
		span = shortDate(nm.AddDate(0, 0, 7)) + "〜"
	}
	if span == "" {
		return bucketLabel[bucket]
	}
	return bucketLabel[bucket] + "（" + span + "）"
}

// Buckets lists every section in display order with its heading.
func Buckets(now time.Time) []model.TaskBucket {
	out := make([]model.TaskBucket, 0, len(bucketOrder))
	for _, b := range bucketOrder {
		out = append(out, model.TaskBucket{Key: b, Label: bucketLabel[b], Heading: Heading(b, now)})
	}
	return out
}

// BucketLabel is a bucket's short name.
func BucketLabel(b string) string { return bucketLabel[b] }

// BucketKeys are the bucket keys in display order.
func BucketKeys() []string { return append([]string(nil), bucketOrder...) }

// Less orders tasks inside a bucket: manual order, then plan date,
// priority, due date, id. Tasks never placed by hand follow the placed
// ones.
func Less(a, b model.Task) bool {
	oa, ob := orderKey(a), orderKey(b)
	if oa != ob {
		return oa < ob
	}
	if pa, pb := dateKey(a.Plan), dateKey(b.Plan); pa != pb {
		return pa < pb
	}
	if a.Finished() && b.Finished() && a.DoneOn != b.DoneOn {
		return a.DoneOn > b.DoneOn // newest finished first
	}
	if pa, pb := priorityKey(a), priorityKey(b); pa != pb {
		return pa < pb
	}
	if da, db := dateKey(a.Due), dateKey(b.Due); da != db {
		return da < db
	}
	return a.ID < b.ID
}

func orderKey(t model.Task) float64 {
	if t.Order == nil {
		return 1e18
	}
	return *t.Order
}

func dateKey(s string) string {
	if s == "" {
		return "9999-99-99"
	}
	return s
}

func priorityKey(t model.Task) int {
	if t.Priority < 1 || t.Priority > 4 {
		return 4
	}
	return t.Priority
}

// Sort fills each task's Bucket and orders the slice by bucket, then Less.
func Sort(ts []model.Task, now time.Time) {
	rank := map[string]int{}
	for i, b := range bucketOrder {
		rank[b] = i
	}
	for i := range ts {
		ts[i].Bucket = BucketOf(ts[i], now)
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ri, rj := rank[ts[i].Bucket], rank[ts[j].Bucket]; ri != rj {
			return ri < rj
		}
		return Less(ts[i], ts[j])
	})
}

// Line is the one-line form the CLI prints: #11  P1 未着手 title …
func Line(t model.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%-4d P%d %-3s %s", t.ID, priorityKey(t), StatusLabel[t.Status], t.Title)
	if t.Plan != "" {
		fmt.Fprintf(&b, " やる日 %s", t.Plan)
	}
	if t.Due != "" {
		fmt.Fprintf(&b, " 期限 %s", t.Due)
	}
	if t.Project != "" {
		fmt.Fprintf(&b, " [%s]", t.Project)
	}
	return b.String()
}

// List sorts ts for display and wraps them with the buckets and rev.
func List(ts []model.Task, rev int64, now time.Time) model.TaskList {
	if ts == nil {
		ts = []model.Task{}
	}
	Sort(ts, now)
	return model.TaskList{Rev: rev, Today: DateString(Day(now)), Buckets: Buckets(now), Tasks: ts}
}
