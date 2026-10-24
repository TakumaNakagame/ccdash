package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"github.com/TakumaNakagame/ccdash/internal/i18n"
	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/settings"
	"github.com/TakumaNakagame/ccdash/internal/store"
	"github.com/TakumaNakagame/ccdash/internal/tasks"
)

// tasksCmd is the to-do list's CLI (docs/decisions/0006-tasks.md), over
// store.Store so -r works too.
func tasksCmd(rf *remoteFlags) *cobra.Command {
	c := &cobra.Command{
		Use:   "tasks",
		Short: "The to-do list: show, edit, sync and import tasks",
		Long: `The to-do list ccdash keeps per machine. Tasks are named by id (11 or #11) or by key
(a Jira key, a PR URL …). Claude edits it through ccdash mcp; automated runs pipe a
proposal into 'ccdash tasks sync'.`,
	}
	with := func(fn func(ctx context.Context, st store.Store, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			if cfg, err := settings.Load(cmd.Context(), st); err == nil {
				i18n.Set(cfg.Language)
			}
			return fn(cmd.Context(), st, args)
		}
	}

	var all bool
	var project string
	list := &cobra.Command{
		Use:   "list",
		Short: "List open tasks by plan (--all: finished and archived too)",
		Args:  cobra.NoArgs,
		RunE: with(func(ctx context.Context, st store.Store, _ []string) error {
			l, err := st.ListTasks(ctx, all)
			if err != nil {
				return err
			}
			printTaskList(os.Stdout, l, func(t model.Task) bool {
				return (all || !t.Finished() || t.Bucket == tasks.BucketToday) && (project == "" || t.Project == project)
			})
			return nil
		}),
	}
	list.Flags().BoolVar(&all, "all", false, "include finished and archived tasks")
	list.Flags().StringVar(&project, "project", "", "only this project's tasks")

	show := &cobra.Command{
		Use:   "show <id|key>",
		Short: "Show a task with its links and whole timeline",
		Args:  cobra.ExactArgs(1),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			t, err := st.GetTask(ctx, args[0])
			if err != nil {
				return err
			}
			printTask(os.Stdout, t)
			return nil
		}),
	}

	find := &cobra.Command{
		Use:   "find <word>",
		Short: "Search titles, keys, projects, notes and timelines (archived too)",
		Args:  cobra.ExactArgs(1),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			l, err := st.ListTasks(ctx, true)
			if err != nil {
				return err
			}
			q := strings.ToLower(args[0])
			for _, t := range l.Tasks {
				if strings.Contains(taskHaystack(t), q) {
					fmt.Println(taskLine(t))
				}
			}
			return nil
		}),
	}

	var in tasks.Input
	add := &cobra.Command{
		Use:   "add <title>",
		Short: "Add a task",
		Args:  cobra.ExactArgs(1),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			in.Title = args[0]
			in.Source = tasks.SourceClaude
			t, err := st.AddTask(ctx, in)
			if err != nil {
				return err
			}
			fmt.Print(i18n.Tf("#%d を追加しました: %s\n", t.ID, t.Title))
			return nil
		}),
	}
	add.Flags().StringVar(&in.Key, "key", "", "key (Jira key, PR URL, misc:…)")
	add.Flags().IntVar(&in.Priority, "priority", 3, "1 (now) – 4 (someday)")
	add.Flags().StringVar(&in.Plan, "plan", "", "today / tomorrow / week / nextweek / YYYY-MM-DD")
	add.Flags().StringVar(&in.Due, "due", "", "deadline (YYYY-MM-DD)")
	add.Flags().StringVar(&in.Project, "project", "", "ccdash project (created on first use)")
	add.Flags().StringVar(&in.Note, "note", "", "note")
	add.Flags().StringSliceVar(&in.Links, "link", nil, "link (repeatable)")

	var link string
	note := &cobra.Command{
		Use:   "note <id|key> <text>",
		Short: "Append progress to a task's timeline",
		Args:  cobra.ExactArgs(2),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			t, err := st.AddTaskNote(ctx, args[0], model.TaskNote{Source: tasks.SourceClaude, Text: args[1], Link: link})
			if err != nil {
				return err
			}
			fmt.Print(i18n.Tf("#%d に進捗を追記しました\n", t.ID))
			return nil
		}),
	}
	note.Flags().StringVar(&link, "link", "", "a link for the entry")

	var why string
	status := &cobra.Command{
		Use:   "status <id|key> <todo|doing|waiting|done|cancelled>",
		Short: "Change a task's status (logged in its timeline)",
		Args:  cobra.ExactArgs(2),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			s := args[1]
			before, err := st.GetTask(ctx, args[0])
			if err != nil {
				return err
			}
			t, err := st.UpdateTask(ctx, args[0], tasks.Patch{Status: &s, Why: why, Source: tasks.SourceClaude})
			if err != nil {
				return err
			}
			fmt.Printf("#%d: %s → %s\n", t.ID, i18n.T(tasks.StatusLabel[before.Status]), i18n.T(tasks.StatusLabel[t.Status]))
			return nil
		}),
	}
	status.Flags().StringVar(&why, "why", "", "the reason, for the timeline")

	plan := &cobra.Command{
		Use:   "plan <id|key> <today|tomorrow|week|nextweek|YYYY-MM-DD|none>",
		Short: "Set the day to work on a task (week = this Friday, nextweek = next Monday)",
		Args:  cobra.ExactArgs(2),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			p := args[1]
			t, err := st.UpdateTask(ctx, args[0], tasks.Patch{Plan: &p, Why: why, Source: tasks.SourceClaude})
			if err != nil {
				return err
			}
			fmt.Print(i18n.Tf("#%d のやる日: %s\n", t.ID, orNone(t.Plan)))
			return nil
		}),
	}
	plan.Flags().StringVar(&why, "why", "", "the reason, for the timeline")

	linkCmd := &cobra.Command{
		Use:   "link <id|key> <url or path>",
		Short: "Add a link (a URL, a working directory …) to a task",
		Args:  cobra.ExactArgs(2),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			t, err := st.UpdateTask(ctx, args[0], tasks.Patch{AddLinks: []string{args[1]}, Source: tasks.SourceClaude})
			if err != nil {
				return err
			}
			fmt.Print(i18n.Tf("#%d にリンクを足しました\n", t.ID))
			return nil
		}),
	}

	var undo bool
	archive := &cobra.Command{
		Use:   "archive <id|key>",
		Short: "Archive a done or cancelled task (--undo brings it back)",
		Args:  cobra.ExactArgs(1),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			v := !undo
			t, err := st.UpdateTask(ctx, args[0], tasks.Patch{Archived: &v, Source: tasks.SourceClaude})
			if err != nil {
				return err
			}
			if v {
				fmt.Print(i18n.Tf("#%d をアーカイブしました\n", t.ID))
			} else {
				fmt.Print(i18n.Tf("#%d をアーカイブから戻しました\n", t.ID))
			}
			return nil
		}),
	}
	archive.Flags().BoolVar(&undo, "undo", false, "unarchive")

	var syncDry bool
	sync := &cobra.Command{
		Use:   "sync < proposal.json",
		Short: "Merge an automated run's proposal from stdin (the person's edits win)",
		Long: `Reads {"synced_at", "tasks": [{"key", "title", "priority", "plan", "due", "project", "note",
"links", "done", "status", "timeline": [{"at", "source", "text", "link", "ref"}],
"replan": {"plan", "why"}}]} from stdin and merges it:
tasks match by key; title, priority and note are never overwritten; a plan set by hand
today doesn't move; a status other than done changes only with new timeline entries; a
finished task is never reopened; a new task's project is kept only if it exists.`,
		Args: cobra.NoArgs,
		RunE: with(func(ctx context.Context, st store.Store, _ []string) error {
			var p tasks.Proposal
			if err := decodeJSON(os.Stdin, &p); err != nil {
				return fmt.Errorf("proposal: %w", err)
			}
			p.DryRun = p.DryRun || syncDry
			res, err := st.SyncTasks(ctx, p)
			if err != nil {
				return err
			}
			fmt.Print(res.String())
			return nil
		}),
	}

	sync.Flags().BoolVar(&syncDry, "dry-run", false, "show what would change, save nothing (synced_at not moved)")

	contextCmd := &cobra.Command{
		Use:   "context",
		Short: "Print the open tasks with their Slack threads, Jira keys and PRs (JSON)",
		Args:  cobra.NoArgs,
		RunE: with(func(ctx context.Context, st store.Store, _ []string) error {
			c, err := st.TaskContext(ctx)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", " ")
			return enc.Encode(c)
		}),
	}

	md := &cobra.Command{
		Use:   "md",
		Short: "Print the open tasks as Markdown, grouped by plan",
		Args:  cobra.NoArgs,
		RunE: with(func(ctx context.Context, st store.Store, _ []string) error {
			l, err := st.ListTasks(ctx, false)
			if err != nil {
				return err
			}
			fmt.Print(tasks.Markdown(l.Tasks, time.Now()))
			return nil
		}),
	}

	var out string
	export := &cobra.Command{
		Use:   "export",
		Short: "Write every task (archived too) as a tasks.json that import reads",
		Args:  cobra.NoArgs,
		RunE: with(func(ctx context.Context, st store.Store, _ []string) error {
			l, err := st.ListTasks(ctx, true)
			if err != nil {
				return err
			}
			// last_timeline_sync isn't carried: a run reading the exported
			// file starts from its own 24-hour fallback.
			f := tasks.LegacyFile{Updated: l.Today, Tasks: l.Tasks, Rev: l.Rev}
			for i := range f.Tasks {
				f.Tasks[i].Bucket = ""
				f.NextID = max(f.NextID, f.Tasks[i].ID+1)
			}
			b, err := json.MarshalIndent(f, "", "  ")
			if err != nil {
				return err
			}
			b = append(b, '\n')
			if out == "" || out == "-" {
				_, err = os.Stdout.Write(b)
				return err
			}
			return os.WriteFile(out, b, 0o600)
		}),
	}
	export.Flags().StringVarP(&out, "output", "o", "", "file to write (default stdout)")

	var mapFile, unmapped string
	var dry bool
	importCmd := &cobra.Command{
		Use:   "import <tasks.json>",
		Short: "Load a tasks.json into an empty list (ids, keys and timelines kept)",
		Long: `Loads a tasks.json — the file 'ccdash tasks export' writes, e.g. to move the list to
another machine — into this machine's list, which must be empty. The file's project names are matched to
ccdash projects: --map takes a JSON object {"old name": "ccdash project", "old": ""} ("" = none);
names neither mapped nor already a project stop the import unless --unmapped create|drop.
Finished tasks from before today come in archived. Run with --dry-run first.`,
		Args: cobra.ExactArgs(1),
		RunE: with(func(ctx context.Context, st store.Store, args []string) error {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			f, err := tasks.ParseLegacy(b)
			if err != nil {
				return err
			}
			opt := tasks.ImportOptions{Unmapped: unmapped}
			if mapFile != "" {
				mb, err := os.ReadFile(mapFile)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(mb, &opt.Map); err != nil {
					return fmt.Errorf("%s: %w", mapFile, err)
				}
			}
			if unmapped != "" && unmapped != "create" && unmapped != "drop" {
				return fmt.Errorf("--unmapped is create or drop")
			}
			plan, err := st.ImportTasks(ctx, f, opt, dry)
			printImportPlan(os.Stdout, f, plan, dry && err == nil)
			if err != nil {
				return err
			}
			if !dry {
				fmt.Print(i18n.Tf("%d 件を取り込みました（うちアーカイブ %d 件）。次の id は %d\n", plan.Tasks, plan.Archived, plan.NextID))
			}
			return nil
		}),
	}
	importCmd.Flags().StringVar(&mapFile, "map", "", "JSON file mapping old project names to ccdash projects")
	importCmd.Flags().StringVar(&unmapped, "unmapped", "", "create | drop: what to do with projects not in --map")
	importCmd.Flags().BoolVar(&dry, "dry-run", false, "only show what would happen")

	c.AddCommand(list, show, find, add, note, status, plan, linkCmd, archive, sync, contextCmd, md, export, importCmd)
	return c
}

func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return i18n.T("未定")
	}
	return s
}

func taskLine(t model.Task) string {
	s := tasks.Line(t)
	if t.Archived {
		s += i18n.T(" (アーカイブ)")
	}
	return s
}

func taskHaystack(t model.Task) string {
	parts := []string{t.Key, t.Title, t.Project, t.Note}
	for _, e := range t.Timeline {
		parts = append(parts, e.Text)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func printTaskList(w io.Writer, l model.TaskList, keep func(model.Task) bool) {
	headings := map[string]string{}
	for _, b := range l.Buckets {
		headings[b.Key] = i18n.Heading(b.Heading)
	}
	cur := ""
	for _, t := range l.Tasks {
		if !keep(t) {
			continue
		}
		if t.Bucket != cur {
			if cur != "" {
				fmt.Fprintln(w)
			}
			cur = t.Bucket
			fmt.Fprintf(w, "## %s\n", headings[cur])
		}
		fmt.Fprintln(w, taskLine(t))
	}
	if cur == "" {
		fmt.Fprintln(w, "(no tasks)")
	}
}

func printTask(w io.Writer, t model.Task) {
	fmt.Fprintf(w, "# #%d %s\n\n", t.ID, t.Title)
	fmt.Fprintf(w, "- key: %s\n", orDash(t.Key))
	fmt.Fprint(w, i18n.Tf("- 状態: %s / 優先度: %s / やる日: %s", i18n.T(tasks.StatusLabel[t.Status]), i18n.T(tasks.PriorityLabel[t.Priority]), orNone(t.Plan)))
	if t.Due != "" {
		fmt.Fprint(w, i18n.Tf(" / 期限: %s", t.Due))
	}
	fmt.Fprintln(w)
	if t.Project != "" {
		fmt.Fprint(w, i18n.Tf("- プロジェクト: %s\n", t.Project))
	}
	if t.Archived {
		fmt.Fprintln(w, i18n.T("- アーカイブ済み"))
	}
	if t.Note != "" {
		fmt.Fprintf(w, "\n## %s\n\n%s\n", i18n.T("メモ"), t.Note)
	}
	if len(t.Links) > 0 {
		fmt.Fprintf(w, "\n## %s\n\n", i18n.T("リンク"))
		for _, l := range t.Links {
			fmt.Fprintf(w, "- %s\n", l)
		}
	}
	if len(t.Sessions) > 0 {
		fmt.Fprintf(w, "\n## %s\n\n", i18n.T("セッション"))
		for _, s := range t.Sessions {
			fmt.Fprintf(w, "- %s\n", s)
		}
	}
	fmt.Fprintf(w, "\n## %s\n\n", i18n.T("進捗（古い順）"))
	if len(t.Timeline) == 0 {
		fmt.Fprintln(w, "- "+i18n.T("（まだ無い）"))
	}
	src := map[string]string{"slack": "Slack", "jira": "Jira", "github": "GitHub", "manual": i18n.T("手書き"), "auto": i18n.T("自動"), "claude": "Claude"}
	for _, e := range t.Timeline {
		at := strings.Replace(e.At, "T", " ", 1)
		if len(at) > 16 {
			at = at[:16]
		}
		label := src[e.Source]
		if label == "" {
			label = e.Source
		}
		link := ""
		if e.Link != "" {
			link = " <" + e.Link + ">"
		}
		fmt.Fprintf(w, "- %s [%s] %s%s\n", at, label, i18n.Timeline(e.Text), link)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printImportPlan(w io.Writer, f tasks.LegacyFile, p tasks.ImportPlan, dry bool) {
	count := map[string]int{}
	for _, t := range f.Tasks {
		count[strings.TrimSpace(t.Project)]++
	}
	fmt.Fprintln(w, i18n.T("プロジェクトの対応:"))
	olds := make([]string, 0, len(count))
	for old := range count {
		olds = append(olds, old)
	}
	sort.Strings(olds)
	for _, old := range olds {
		n := count[old]
		if old == "" {
			continue
		}
		to, ok := p.Projects[old]
		col := runewidth.FillRight(old, 30)
		switch {
		case !ok:
			fmt.Fprint(w, i18n.Tf("  %s → （未決定）  %d 件\n", col, n))
		case to == "":
			fmt.Fprint(w, i18n.Tf("  %s → （なし）  %d 件\n", col, n))
		default:
			mark := ""
			for _, c := range p.Create {
				if c == to {
					mark = i18n.T("（新規作成）")
				}
			}
			fmt.Fprint(w, i18n.Tf("  %s → %s%s  %d 件\n", col, to, mark, n))
		}
	}
	if dry {
		fmt.Fprint(w, i18n.Tf("取り込むタスク %d 件（うちアーカイブ %d 件）、次の id %d、前回の同期 %s\n", p.Tasks, p.Archived, p.NextID, orDash(p.LastSync)))
	}
}
