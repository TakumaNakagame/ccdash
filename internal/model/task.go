package model

import "time"

// Task statuses. done and cancelled are the finished ones: only a finished
// task can be archived, and an automated sync never reopens one.
const (
	TaskTodo      = "todo"
	TaskDoing     = "doing"
	TaskWaiting   = "waiting"
	TaskDone      = "done"
	TaskCancelled = "cancelled"
)

// Task is one entry of the to-do list (docs/decisions/0006-tasks.md). The
// JSON names follow the tasks.json the list was migrated from, so export
// writes that file back out unchanged.
type Task struct {
	ID       int64  `json:"id"`
	Key      string `json:"key,omitempty"`
	Title    string `json:"title"`
	Priority int    `json:"priority"`
	Status   string `json:"status"`
	// Plan is the day to work on it (YYYY-MM-DD, "" = unplanned); PlanBy
	// is "manual" or "auto" and PlanOn the day it was set — a plan the
	// person set today is not moved by an automated sync.
	Plan      string     `json:"plan"`
	PlanBy    string     `json:"plan_by,omitempty"`
	PlanOn    string     `json:"plan_on,omitempty"`
	Due       string     `json:"due"`
	Project   string     `json:"project"`
	Note      string     `json:"note"`
	Links     []string   `json:"links"`
	Source    string     `json:"source,omitempty"`
	CreatedOn string     `json:"created_on,omitempty"`
	DoneOn    string     `json:"done_on,omitempty"`
	Order     *float64   `json:"order,omitempty"`
	Archived  bool       `json:"archived,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitzero"`
	Timeline  []TaskNote `json:"timeline"`
	// Sessions are the ids of the Claude sessions linked to the task.
	Sessions []string `json:"sessions,omitempty"`

	// Bucket is filled by the API (tasks.BucketOf): where the list shows
	// the task today.
	Bucket string `json:"bucket,omitempty"`
}

// Finished reports done or cancelled.
func (t Task) Finished() bool { return t.Status == TaskDone || t.Status == TaskCancelled }

// TaskNote is one timeline entry. Ref dedups: the same Slack message
// (slack:<channel>:<ts>) is never appended twice to a task.
type TaskNote struct {
	At     string `json:"at"`
	Source string `json:"source"`
	Text   string `json:"text"`
	Link   string `json:"link"`
	Ref    string `json:"ref"`
}

// TaskBucket is one section of the list, in display order.
type TaskBucket struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Heading string `json:"heading"`
}

// TaskList is what GET /api/tasks returns: the tasks with their bucket,
// the sections to show them in, and the change counter clients poll.
type TaskList struct {
	Rev     int64        `json:"rev"`
	Today   string       `json:"today"`
	Buckets []TaskBucket `json:"buckets"`
	Tasks   []Task       `json:"tasks"`
}
