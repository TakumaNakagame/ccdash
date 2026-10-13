package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
)

func taskTestModel() *model {
	m := newModel(context.Background(), nil, RemoteInfo{})
	m.width, m.height = 200, 40
	m.settings.LayoutMode = "horizontal"
	m.settings.TasksPane = true
	m.settings.PaneTasksPct = 25
	m.settings.PaneListPct = 30
	m.sessions = []mdl.Session{{SessionID: "s1", Num: 1, Project: "web"}, {SessionID: "s2", Num: 2}}
	m.allSessions = m.sessions
	m.tasks = mdl.TaskList{Today: "2026-10-13",
		Buckets: []mdl.TaskBucket{{Key: "today", Heading: "今日（10/13 火）"}, {Key: "none", Heading: "未定"}},
		Tasks: []mdl.Task{
			{ID: 65, Title: "確認して返事する", Status: "waiting", Priority: 1, Plan: "2026-10-06", Due: "2026-10-06", Bucket: "today", Project: "web"},
			{ID: 2, Title: "dry-run", Status: "todo", Priority: 2, Plan: "2026-10-13", Bucket: "today"},
			{ID: 9, Title: "いつか", Status: "todo", Priority: 4, Bucket: "none"},
		}}
	m.tasksLoaded = true
	return m
}

// TestTasksPaneLayout: list · work · tasks columns agree between the
// renderer, the live pane geometry and mouse zoning; folding the list
// gives the work pane its width.
func TestTasksPaneLayout(t *testing.T) {
	m := taskTestModel()
	tw := m.tasksPaneW()
	if tw != 50 || m.workWidth() != 147 {
		t.Fatalf("tasks %d, work %d", tw, m.workWidth())
	}
	left := m.leftPaneWidth() // 30% of the work width
	g, ok := m.rightPaneGeom()
	if !ok || g.x != left+3 || g.x+g.w != m.workWidth() {
		t.Fatalf("right pane %+v, left %d", g, left)
	}
	top, _ := m.bodyLayout()
	if m.mouseInRightPane(tea.Mouse{X: m.workWidth() + 3, Y: top}) || !m.mouseInTasksPane(tea.Mouse{X: m.workWidth() + 3, Y: top}) {
		t.Fatal("mouse zoning: the task pane")
	}
	_, h := m.bodyLayout()
	for i, line := range strings.Split(m.renderSessionsBody(h), "\n") {
		if w := lipgloss.Width(line); w != m.width {
			t.Fatalf("body line %d is %d wide, want %d", i, w, m.width)
		}
	}

	m.settings.ListCollapsed = true
	if g, _ := m.rightPaneGeom(); g.x != collapsedListW+3 {
		t.Fatalf("collapsed: right pane x = %d", g.x)
	}
	// Too narrow to share: the pane steps aside.
	m.width = 70
	if m.tasksPaneW() != 0 || m.workWidth() != 70 {
		t.Fatalf("narrow: tasks %d", m.tasksPaneW())
	}
}

func TestTasksPaneKeys(t *testing.T) {
	m := taskTestModel()
	m.setTasksFocus(true)
	if m.selectedTaskID() != 65 {
		t.Fatalf("first selection = %d", m.selectedTaskID())
	}
	m.moveTask(1)
	if m.selectedTaskID() != 2 {
		t.Fatalf("down = %d", m.selectedTaskID())
	}
	m.moveTask(1) // over the "未定" heading
	if m.selectedTaskID() != 9 {
		t.Fatalf("down past a heading = %d", m.selectedTaskID())
	}
	m.moveTask(1) // stays on the last one
	if m.selectedTaskID() != 9 {
		t.Fatalf("past the end = %d", m.selectedTaskID())
	}
	m.tasksProjectOnly = true // s1 is selected: project web
	m.clampTaskSel()
	if rows := m.taskRows(); len(rows) != 2 || m.selectedTaskID() != 65 {
		t.Fatalf("project filter: %d rows, sel %d", len(rows), m.selectedTaskID())
	}
	// Focusing the live pane takes the keyboard from the tasks.
	m.setLiveFocus(true)
	if m.tasksFocus {
		t.Fatal("live focus left the task pane focused")
	}
	// The pane renders exactly w × h, with the overdue mark.
	out := m.renderTasksPane(50, 10)
	lines := strings.Split(out, "\n")
	if len(lines) != 10 || !strings.Contains(out, "#65") || !strings.Contains(out, "⚠10/6") {
		t.Fatalf("pane:\n%s", out)
	}
	for _, l := range lines {
		if lipgloss.Width(l) != 50 {
			t.Fatalf("line width %d: %q", lipgloss.Width(l), l)
		}
	}
}
