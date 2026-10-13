package tui

// The task pane (docs/decisions/0006-tasks.md): the to-do list as a third
// column right of the work pane — or of the grid — shown with w, focused
// with W. The session list can fold to a strip of numbers (b) to make
// room. Every column position comes from workWidth / leftPaneWidth /
// tasksPaneW so the renderer, mouse zoning and the live pane's cursor agree
// to the cell.

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	mdl "github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/settings"
	"github.com/takumanakagame/ccmanage/internal/tasks"
)

const (
	taskSepW       = 3 // " │ " between the work area and the task pane
	collapsedListW = 8 // the folded session list: bar, #N, status
	taskMinW       = 28
	taskEvery      = 3 // refresh ticks between task reloads
)

var (
	taskHeadStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))
	taskBucketStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250"))
	taskDueStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	taskP1Style      = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	taskDoneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Strikethrough(true)
	taskCarriedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// tasksPaneW is the task pane's width, 0 when it isn't beside the work
// area: hidden, the vertical layout (where a focused pane takes the list's
// place instead), or a terminal too narrow to share.
func (m *model) tasksPaneW() int {
	if !m.settings.TasksPane || m.pane != paneSessions || m.width == 0 {
		return 0
	}
	if !m.gridOpen && m.useVerticalLayout() {
		return 0
	}
	w := max(m.width*m.settings.Ratio().Tasks/100, taskMinW)
	if m.width-w-taskSepW < 50 {
		return 0
	}
	return w
}

// workWidth is what the list + work pane (or the grid) get.
func (m *model) workWidth() int {
	if w := m.tasksPaneW(); w > 0 {
		return m.width - w - taskSepW
	}
	return m.width
}

// tasksInListSlot: in the vertical layout the focused task pane takes the
// session list's place.
func (m *model) tasksInListSlot() bool {
	return m.tasksFocus && !m.gridOpen && m.useVerticalLayout()
}

// withTasksPane puts the task pane right of a body rendered workWidth wide.
func (m *model) withTasksPane(body string, height int) string {
	tw := m.tasksPaneW()
	if tw == 0 {
		return body
	}
	sepLine := " " + subtitleStyle.Render("│") + " "
	sep := strings.TrimSuffix(strings.Repeat(sepLine+"\n", height), "\n")
	body = lipgloss.NewStyle().Width(m.workWidth()).MaxWidth(m.workWidth()).Render(body)
	return lipgloss.JoinHorizontal(lipgloss.Top, body, sep, m.renderTasksPane(tw, height))
}

// mouseInTasksPane reports a position over the task pane.
func (m *model) mouseInTasksPane(mm tea.Mouse) bool {
	if m.pane != paneSessions || m.modalOpen() {
		return false
	}
	bodyTop, bodyHeight := m.bodyLayout()
	if mm.Y < bodyTop || mm.Y >= bodyTop+bodyHeight {
		return false
	}
	if m.tasksInListSlot() {
		listH, _ := m.verticalSplit(bodyHeight)
		return mm.Y < bodyTop+listH
	}
	return m.tasksPaneW() > 0 && mm.X >= m.workWidth()+taskSepW
}

// renderCollapsedList is the folded session list: one line per session —
// color bar, #N, status — the selected one highlighted.
func (m *model) renderCollapsedList(width, height int) string {
	m.listLineSess = nil
	if height <= 0 {
		return ""
	}
	n := len(m.sessions)
	top := 0
	if m.selSess >= height {
		top = m.selSess - height + 1
	}
	lines := make([]string, 0, height)
	for i := top; i < n && len(lines) < height; i++ {
		s := m.sessions[i]
		bar := " "
		if c := s.Color(); c != "" {
			bar = lipgloss.NewStyle().Background(lipgloss.Color(c)).Render(" ")
		}
		dot := renderStatusDot(s.Status, m.animTick)
		if s.Attention == mdl.AttentionNeedsYou || s.PendingCount > 0 {
			dot = pendingStyle.Render("?")
		}
		ref := ansi.Truncate(s.Ref(), width-4, "")
		row := padRight(" "+ref+" ", width-2)
		if i == m.selSess {
			row = selectedRow.Render(row)
		}
		lines = append(lines, bar+row+dot)
		m.listLineSess = append(m.listLineSess, i)
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return strings.Join(lines, "\n")
}

// --- data -----------------------------------------------------------------

type tasksMsg struct {
	list mdl.TaskList
	err  error
}

type taskEditedMsg struct {
	flash string
	err   error
}

// tasksWanted: the list is on screen (or about to be).
func (m *model) tasksWanted() bool {
	return m.settings.TasksPane || m.tasksFocus
}

func (m *model) loadTasksCmd() tea.Cmd {
	st, ctx := m.store, m.ctx
	return func() tea.Msg {
		l, err := st.ListTasks(ctx, false)
		return tasksMsg{list: l, err: err}
	}
}

func (m *model) applyTasks(msg tasksMsg) {
	if msg.err != nil {
		m.tasksErr = msg.err
		return
	}
	m.tasksErr = nil
	keep := m.selectedTaskID()
	m.tasks = msg.list
	m.tasksLoaded = true
	if keep != 0 {
		for i, r := range m.taskRows() {
			if r.task >= 0 && m.tasks.Tasks[r.task].ID == keep {
				m.taskSel = i
				return
			}
		}
	}
	m.clampTaskSel()
}

// taskRow is one line of the pane: a bucket heading (task = -1) or a task
// (index into m.tasks.Tasks).
type taskRow struct {
	heading string
	task    int
}

// taskRows lists what the pane shows: open tasks plus the ones finished
// today, under their bucket headings; with tasksProjectOnly, only the
// selected session's project.
func (m *model) taskRows() []taskRow {
	headings := map[string]string{}
	for _, b := range m.tasks.Buckets {
		headings[b.Key] = b.Heading
	}
	project := ""
	if m.tasksProjectOnly {
		project = m.currentProject()
	}
	var rows []taskRow
	cur := ""
	for i, t := range m.tasks.Tasks {
		if t.Bucket == tasks.BucketFinished {
			continue
		}
		if m.tasksProjectOnly && t.Project != project {
			continue
		}
		if t.Bucket != cur {
			cur = t.Bucket
			rows = append(rows, taskRow{heading: headings[cur], task: -1})
		}
		rows = append(rows, taskRow{task: i})
	}
	return rows
}

func (m *model) currentProject() string {
	if m.selSess >= 0 && m.selSess < len(m.sessions) {
		return m.sessions[m.selSess].Project
	}
	return ""
}

func (m *model) selectedTask() (mdl.Task, bool) {
	rows := m.taskRows()
	if m.taskSel < 0 || m.taskSel >= len(rows) || rows[m.taskSel].task < 0 {
		return mdl.Task{}, false
	}
	return m.tasks.Tasks[rows[m.taskSel].task], true
}

func (m *model) selectedTaskID() int64 {
	t, ok := m.selectedTask()
	if !ok {
		return 0
	}
	return t.ID
}

// clampTaskSel keeps the selection on a task line.
func (m *model) clampTaskSel() {
	rows := m.taskRows()
	if len(rows) == 0 {
		m.taskSel = 0
		return
	}
	m.taskSel = clamp(m.taskSel, 0, len(rows)-1)
	if rows[m.taskSel].task >= 0 {
		return
	}
	for i := m.taskSel; i < len(rows); i++ {
		if rows[i].task >= 0 {
			m.taskSel = i
			return
		}
	}
	for i := m.taskSel; i >= 0; i-- {
		if rows[i].task >= 0 {
			m.taskSel = i
			return
		}
	}
}

// moveTask moves the selection by delta task lines, skipping headings.
func (m *model) moveTask(delta int) {
	rows := m.taskRows()
	i := m.taskSel
	for step := 0; step < abs(delta); step++ {
		j := i
		for {
			j += sign(delta)
			if j < 0 || j >= len(rows) {
				j = i
				break
			}
			if rows[j].task >= 0 {
				break
			}
		}
		i = j
	}
	m.taskSel = i
	m.taskDetailScroll = 0
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int) int {
	if v < 0 {
		return -1
	}
	return 1
}

// --- keys -----------------------------------------------------------------

// setTasksFocus moves the keyboard to the task pane (on) or back.
func (m *model) setTasksFocus(on bool) tea.Cmd {
	m.tasksFocus = on
	m.taskDetail = false
	if !on {
		return nil
	}
	m.setLiveFocus(false)
	m.gridTyping = false
	m.clampTaskSel()
	return m.loadTasksCmd()
}

// toggleTasksPane is w: show / hide the pane (persisted).
func (m *model) toggleTasksPane() tea.Cmd {
	next, err := settings.Set(m.ctx, m.store, m.settings, "tasks_pane", !m.settings.TasksPane)
	if err != nil {
		m.err = err
		return nil
	}
	m.settings = next
	if !m.settings.TasksPane {
		m.tasksFocus = false
		return nil
	}
	if m.tasksPaneW() == 0 && !m.useVerticalLayout() {
		m.flash = "task pane: the terminal is too narrow"
	}
	return m.loadTasksCmd()
}

// toggleListCollapsed is b (persisted).
func (m *model) toggleListCollapsed() {
	next, err := settings.Set(m.ctx, m.store, m.settings, "list_collapsed", !m.settings.ListCollapsed)
	if err != nil {
		m.err = err
		return
	}
	m.settings = next
}

// taskPassKeys reach the dashboard even while the task pane has focus.
var taskPassKeys = map[string]bool{"q": true, "ctrl+c": true, "?": true, "w": true, "b": true, "V": true, ",": true, "<": true, ">": true}

// handleTasksKey runs while the task pane has focus. ok is false for keys
// the dashboard should take.
func (m *model) handleTasksKey(msg tea.KeyPressMsg) (cmd tea.Cmd, ok bool) {
	k := msg.String()
	if m.taskDetail {
		switch k {
		case "esc", "enter", "backspace":
			m.taskDetail = false
		case "j", "down":
			m.taskDetailScroll++
		case "k", "up":
			m.taskDetailScroll = max(0, m.taskDetailScroll-1)
		case "y":
			return m.copyTaskRef(), true
		default:
			if taskPassKeys[k] {
				return nil, false
			}
		}
		return nil, true
	}
	t, has := m.selectedTask()
	switch k {
	case "esc", "W", "tab":
		return m.setTasksFocus(false), true
	case "j", "down":
		m.moveTask(1)
	case "k", "up":
		m.moveTask(-1)
	case "g", "home":
		m.taskSel = 0
		m.clampTaskSel()
	case "G", "end":
		m.taskSel = len(m.taskRows()) - 1
		m.clampTaskSel()
	case "enter":
		if has {
			m.taskDetail = true
			m.taskDetailScroll = 0
		}
	case "r":
		return m.loadTasksCmd(), true
	case "f":
		m.tasksProjectOnly = !m.tasksProjectOnly
		m.taskSel = 0
		m.clampTaskSel()
		if m.tasksProjectOnly {
			m.flash = "tasks: this session's project only"
		} else {
			m.flash = "tasks: all"
		}
	case "space":
		if has {
			st := mdl.TaskDone
			if t.Finished() {
				st = mdl.TaskTodo
			}
			return m.editTask(t, tasks.Patch{Status: &st}, fmt.Sprintf("#%d → %s", t.ID, tasks.StatusLabel[st])), true
		}
	case "s":
		if has && !t.Finished() {
			st := map[string]string{mdl.TaskTodo: mdl.TaskDoing, mdl.TaskDoing: mdl.TaskWaiting, mdl.TaskWaiting: mdl.TaskTodo}[t.Status]
			return m.editTask(t, tasks.Patch{Status: &st}, fmt.Sprintf("#%d → %s", t.ID, tasks.StatusLabel[st])), true
		}
	case "1", "2", "3", "4", "0":
		if has {
			plan := map[string]string{"1": "today", "2": "tomorrow", "3": "week", "4": "nextweek", "0": "none"}[k]
			return m.editTask(t, tasks.Patch{Plan: &plan}, fmt.Sprintf("#%d のやる日: %s", t.ID, taskPlanLabel[plan])), true
		}
	case "[", "]":
		if has {
			return m.reorderTask(t, map[string]int{"[": -1, "]": 1}[k]), true
		}
	case "x":
		if has {
			v := !t.Archived
			if v && !t.Finished() {
				m.flash = fmt.Sprintf("#%d: only a done / cancelled task can be archived", t.ID)
				return nil, true
			}
			return m.editTask(t, tasks.Patch{Archived: &v}, fmt.Sprintf("#%d archived", t.ID)), true
		}
	case "c":
		if has && !t.Finished() {
			st := mdl.TaskCancelled
			return m.editTask(t, tasks.Patch{Status: &st}, fmt.Sprintf("#%d → 中止", t.ID)), true
		}
	case "y":
		return m.copyTaskRef(), true
	case "n":
		if has {
			return m.newSessionForTask(t), true
		}
	default:
		if taskPassKeys[k] {
			return nil, false
		}
	}
	return nil, true
}

var taskPlanLabel = map[string]string{"today": "今日", "tomorrow": "明日", "week": "今週", "nextweek": "来週", "none": "未定"}

func (m *model) editTask(t mdl.Task, p tasks.Patch, done string) tea.Cmd {
	p.Source = tasks.SourceManual
	st, ctx := m.store, m.ctx
	id := strconv.FormatInt(t.ID, 10)
	return tea.Sequence(func() tea.Msg {
		_, err := st.UpdateTask(ctx, id, p)
		return taskEditedMsg{flash: done, err: err}
	}, m.loadTasksCmd())
}

// reorderTask swaps t with its neighbour in its bucket and saves the
// bucket's order.
func (m *model) reorderTask(t mdl.Task, delta int) tea.Cmd {
	var ids []int64
	for _, x := range m.tasks.Tasks {
		if x.Bucket == t.Bucket && !(m.tasksProjectOnly && x.Project != m.currentProject()) {
			ids = append(ids, x.ID)
		}
	}
	i := slices.Index(ids, t.ID)
	j := i + delta
	if i < 0 || j < 0 || j >= len(ids) {
		return nil
	}
	ids[i], ids[j] = ids[j], ids[i]
	st, ctx := m.store, m.ctx
	return tea.Sequence(func() tea.Msg {
		return taskEditedMsg{err: st.OrderTasks(ctx, ids)}
	}, m.loadTasksCmd())
}

func (m *model) copyTaskRef() tea.Cmd {
	t, ok := m.selectedTask()
	if !ok {
		return nil
	}
	text := fmt.Sprintf("#%d について、", t.ID)
	m.flash = "copied: " + text
	return copyToClipboard(text)
}

// newSessionForTask is n in the pane: the directory picker, pre-filled
// with the task's project directory, and "#N について、" as the first
// prompt.
func (m *model) newSessionForTask(t mdl.Task) tea.Cmd {
	if !m.settings.AttachEnabled {
		m.flash = "attach is OFF (settings ',')"
		return nil
	}
	m.tasksFocus = false
	m.spawnPrompt = fmt.Sprintf("#%d について、", t.ID)
	m.spawnProject = t.Project
	m.openDirPicker()
	return nil
}

// clickTasks selects the task under the pointer and focuses the pane.
func (m *model) clickTasks(mm tea.Mouse) tea.Cmd {
	bodyTop, _ := m.bodyLayout()
	line := mm.Y - bodyTop - 1 // the pane's header line
	cmd := tea.Cmd(nil)
	if !m.tasksFocus {
		cmd = m.setTasksFocus(true)
	}
	if m.taskDetail || line < 0 {
		return cmd
	}
	rows := m.taskRows()
	if line < len(m.taskLineRow) {
		if i := m.taskLineRow[line]; i >= 0 && i < len(rows) && rows[i].task >= 0 {
			m.taskSel = i
			m.taskDetailScroll = 0
		}
	}
	return cmd
}

// wheelTasks scrolls the pane (the detail view, or the selection).
func (m *model) wheelTasks(up bool) {
	if m.taskDetail {
		if up {
			m.taskDetailScroll = max(0, m.taskDetailScroll-3)
		} else {
			m.taskDetailScroll += 3
		}
		return
	}
	if up {
		m.moveTask(-1)
	} else {
		m.moveTask(1)
	}
}

// --- render ---------------------------------------------------------------

// renderTasksPane is the pane, exactly w × h.
func (m *model) renderTasksPane(w, h int) string {
	if h <= 0 || w <= 0 {
		return ""
	}
	open := 0
	for _, t := range m.tasks.Tasks {
		if !t.Finished() {
			open++
		}
	}
	title := fmt.Sprintf(" タスク %d", open)
	if m.tasksProjectOnly {
		p := m.currentProject()
		if p == "" {
			p = "プロジェクトなし"
		}
		title += " · " + p
	}
	hint := ""
	if m.tasksFocus {
		hint = "enter 詳細 esc 戻る "
	} else {
		hint = "W "
	}
	head := padRight(title, w-runeW(hint)) + hint
	head = taskHeadStyle.Render(ansi.Truncate(head, w, ""))

	var body []string
	switch {
	case m.tasksErr != nil:
		body = []string{subtitleStyle.Render(ansi.Truncate(" "+m.tasksErr.Error(), w, "…"))}
	case !m.tasksLoaded:
		body = []string{subtitleStyle.Render(" loading…")}
	case m.taskDetail:
		body = m.taskDetailLines(w, h-1)
	default:
		body = m.taskListLines(w, h-1)
	}
	lines := append([]string{head}, body...)
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = padRight(ansi.Truncate(lines[i], w, ""), w)
	}
	return strings.Join(lines[:h], "\n")
}

func runeW(s string) int { return lipgloss.Width(s) }

// taskListLines lays the list out two lines per task — what it is (status,
// priority, title) above when and where (id, due, plan, project, sessions)
// — under bucket headings with their counts. taskScroll counts lines;
// taskLineRow maps each drawn line back to its row for clicks.
func (m *model) taskListLines(w, h int) []string {
	rows := m.taskRows()
	m.taskLineRow = nil
	if len(rows) == 0 {
		return []string{subtitleStyle.Render(" タスクはありません")}
	}
	today := m.tasks.Today
	counts := map[string]int{}
	for _, r := range rows {
		if r.task >= 0 {
			counts[m.tasks.Tasks[r.task].Bucket]++
		}
	}
	var lines []string
	var owner []int
	selTop, selBottom := 0, 0
	stripe := 0 // tasks drawn in the current bucket, for the alternating band
	for i, r := range rows {
		if r.task < 0 {
			if len(lines) > 0 {
				lines, owner = append(lines, ""), append(owner, -1)
			}
			stripe = 0
			bucket := ""
			if i+1 < len(rows) && rows[i+1].task >= 0 {
				bucket = m.tasks.Tasks[rows[i+1].task].Bucket
			}
			head := taskBucketStyle.Render(" "+r.heading) + subtitleStyle.Render(fmt.Sprintf("  %d", counts[bucket]))
			lines, owner = append(lines, head), append(owner, i)
			continue
		}
		t := m.tasks.Tasks[r.task]
		if stripe > 0 && m.settings.TaskSpacing {
			lines, owner = append(lines, ""), append(owner, -1)
		}
		var band lipgloss.TerminalColor
		if stripe%2 == 1 {
			band = m.taskBand()
		}
		stripe++
		l1, l2 := m.taskRowLines(t, today, w, band)
		if i == m.taskSel && m.tasksFocus {
			selTop = len(lines)
			l1 = taskSelBar + selectedRow.Render(padRight(ansi.Truncate(ansi.Strip(l1), w-1, ""), w-1))
			l2 = taskSelBar + selectedRow.Render(padRight(ansi.Truncate(ansi.Strip(l2), w-1, ""), w-1))
			selBottom = len(lines) + 1
		}
		lines, owner = append(lines, l1, l2), append(owner, i, i)
	}
	// Keep the selected task (both lines, and its heading when it is the
	// bucket's first) in view.
	if m.tasksFocus {
		top := selTop
		if selTop >= 2 && owner[selTop-1] >= 0 && rows[owner[selTop-1]].task < 0 {
			top = selTop - 1
		}
		if top < m.taskScroll {
			m.taskScroll = top
		}
		if selBottom >= m.taskScroll+h {
			m.taskScroll = selBottom - h + 1
		}
	}
	m.taskScroll = clamp(m.taskScroll, 0, max(0, len(lines)-h))
	end := min(len(lines), m.taskScroll+h)
	m.taskLineRow = owner[m.taskScroll:end]
	return lines[m.taskScroll:end]
}

var taskSelBar = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("▌")

// taskStatusStyle colours the status word: doing = working green, waiting
// = attention yellow, finished = dim.
var taskStatusStyle = map[string]lipgloss.Style{
	mdl.TaskTodo:      subtitleStyle,
	mdl.TaskDoing:     statusActive.Bold(true),
	mdl.TaskWaiting:   pendingRowStyle.Bold(true),
	mdl.TaskDone:      statusIdle,
	mdl.TaskCancelled: subtitleStyle,
}

var taskPriorityStyle = map[int]lipgloss.Style{
	1: taskP1Style,
	2: lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
	3: lipgloss.NewStyle().Foreground(lipgloss.Color("250")),
	4: subtitleStyle,
}

// taskBand is the faint background every other task gets, so each
// two-line task reads as one unit: a step off the terminal's own
// background, darker on light terminals, lighter on dark ones — and quieter
// than the selection (selectedRow).
func (m *model) taskBand() lipgloss.TerminalColor {
	if m.lightBG {
		return lipgloss.Color("254")
	}
	return lipgloss.Color("234")
}

// taskRowLines is one task as two lines. The first says what it is; the
// second, dimmed and starting under the title, says when and where — so it
// never reads as the next task's first line. Only an overdue date keeps
// its colour. band (nil = none) is painted behind both lines, edge to edge.
//
//	P1 待ち   定点観測テンプレに入ったインフラ管理組合…
//	          #65  ⚠期限 10/6  10/6〜持ち越し  Alert D4U  #302
func (m *model) taskRowLines(t mdl.Task, today string, w int, band lipgloss.TerminalColor) (string, string) {
	on := func(st lipgloss.Style) lipgloss.Style {
		if band != nil {
			return st.Background(band)
		}
		return st
	}
	plain := on(lipgloss.NewStyle())
	dim := on(subtitleStyle)
	fin := t.Finished()
	pri := t.Priority
	if pri < 1 || pri > 4 {
		pri = 4
	}
	prio := on(taskPriorityStyle[pri]).Render(fmt.Sprintf("P%d", pri))
	if fin {
		prio = dim.Render(fmt.Sprintf("P%d", pri))
	}
	label := tasks.StatusLabel[t.Status]
	if t.Status == mdl.TaskDone {
		label = "✓完了"
	}
	status := on(taskStatusStyle[t.Status]).Render(padRight(label, 6))
	prefix := plain.Render(" ") + prio + plain.Render(" ") + status + plain.Render(" ")
	indent := lipgloss.Width(prefix)
	title := ansi.Truncate(t.Title, max(4, w-indent), "…")
	switch {
	case fin:
		title = on(taskDoneStyle).Render(title)
	case pri == 1:
		title = on(lipgloss.NewStyle().Bold(true)).Render(title)
	default:
		title = plain.Render(title)
	}
	line1 := prefix + title

	var meta []string
	meta = append(meta, fmt.Sprintf("#%d", t.ID))
	overdue := ""
	if t.Due != "" && !fin {
		switch {
		case t.Due < today:
			overdue = "⚠期限 " + shortDay(t.Due)
		case t.Due == today:
			meta = append(meta, "期限 今日")
		default:
			meta = append(meta, "期限 "+shortDay(t.Due))
		}
	}
	var after []string
	if !fin && t.Plan != "" && t.Plan < today {
		after = append(after, shortDay(t.Plan)+"〜持ち越し")
	} else if !fin && t.Plan != "" && t.Bucket != tasks.BucketToday && t.Bucket != tasks.BucketTomorrow {
		after = append(after, "予定 "+shortDay(t.Plan))
	}
	if t.Project != "" {
		after = append(after, t.Project)
	}
	for _, sid := range t.Sessions {
		after = append(after, m.sessionRefOf(sid))
	}
	line2 := dim.Render(strings.Repeat(" ", indent) + strings.Join(meta, "  "))
	if overdue != "" {
		line2 += dim.Render("  ") + on(taskDueStyle.Bold(true)).Render(overdue)
	}
	if len(after) > 0 {
		line2 += dim.Render("  " + strings.Join(after, "  "))
	}
	line2 = ansi.Truncate(line2, w, "…")
	if band != nil {
		line1 = ansi.Truncate(line1, w, "")
		line1 += plain.Render(strings.Repeat(" ", max(0, w-lipgloss.Width(line1))))
		line2 += plain.Render(strings.Repeat(" ", max(0, w-lipgloss.Width(line2))))
	}
	return line1, line2
}

// shortDay is "10/6" for 2026-10-06.
func shortDay(d string) string {
	t, err := time.ParseInLocation("2006-01-02", d, time.Local)
	if err != nil {
		return d
	}
	return fmt.Sprintf("%d/%d", int(t.Month()), t.Day())
}

// taskDetailLines is the selected task in full, newest progress first.
func (m *model) taskDetailLines(w, h int) []string {
	t, ok := m.selectedTask()
	if !ok {
		m.taskDetail = false
		return nil
	}
	wrap := func(s string, indent int) []string {
		if s == "" {
			return nil
		}
		pad := strings.Repeat(" ", indent)
		var out []string
		for _, l := range strings.Split(lipgloss.NewStyle().Width(max(10, w-indent-1)).Render(s), "\n") {
			out = append(out, pad+strings.TrimRight(l, " "))
		}
		return out
	}
	var out []string
	out = append(out, wrap(lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("#%d %s", t.ID, t.Title)), 1)...)
	meta := []string{tasks.StatusLabel[t.Status], fmt.Sprintf("P%d", t.Priority)}
	if t.Plan != "" {
		meta = append(meta, "やる日 "+shortDay(t.Plan))
	} else {
		meta = append(meta, "やる日 未定")
	}
	if t.Due != "" {
		meta = append(meta, "期限 "+shortDay(t.Due))
	}
	out = append(out, wrap(subtitleStyle.Render(strings.Join(meta, " · ")), 1)...)
	if t.Project != "" {
		out = append(out, wrap(subtitleStyle.Render("プロジェクト: "+t.Project), 1)...)
	}
	if t.Key != "" {
		out = append(out, wrap(subtitleStyle.Render("key: "+t.Key), 1)...)
	}
	if t.Note != "" {
		out = append(out, "")
		out = append(out, wrap(t.Note, 1)...)
	}
	if len(t.Links) > 0 {
		out = append(out, "", taskBucketStyle.Render(" リンク"))
		for _, l := range t.Links {
			out = append(out, " "+ansi.Truncate(l, w-2, "…"))
		}
	}
	if len(t.Sessions) > 0 {
		var refs []string
		for _, sid := range t.Sessions {
			refs = append(refs, m.sessionRefOf(sid))
		}
		out = append(out, "", subtitleStyle.Render(" セッション: "+strings.Join(refs, " ")))
	}
	out = append(out, "", taskBucketStyle.Render(" 進捗（新しい順）"))
	if len(t.Timeline) == 0 {
		out = append(out, subtitleStyle.Render(" （まだ無い）"))
	}
	for i := len(t.Timeline) - 1; i >= 0; i-- {
		e := t.Timeline[i]
		at := e.At
		if ts, err := time.Parse(time.RFC3339, e.At); err == nil {
			at = ts.Local().Format("1/2 15:04")
		}
		out = append(out, wrap(subtitleStyle.Render(at)+" "+e.Text, 1)...)
	}
	m.taskDetailScroll = clamp(m.taskDetailScroll, 0, max(0, len(out)-h))
	return out[m.taskDetailScroll:]
}

func (m *model) sessionRefOf(sid string) string {
	for _, s := range m.allSessions {
		if s.SessionID == sid {
			if r := s.Ref(); r != "" {
				return r
			}
		}
	}
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}
