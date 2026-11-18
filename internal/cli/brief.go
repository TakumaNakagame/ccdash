package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TakumaNakagame/ccdash/internal/settings"
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// briefCmd edits the operator's notes for start_session briefs and each
// project's working directory (docs/decisions/0009-orchestrator.md §5).
// The TUI settings page holds one-line values, so the multi-line notes
// live here (and in the portal).
func briefCmd(rf *remoteFlags) *cobra.Command {
	c := &cobra.Command{
		Use:   "brief",
		Short: "Show / edit the notes ccdash appends to every MCP start_session prompt, and each project's working directory",
		Long: `An orchestrating session hands work to new sessions with the MCP tool
start_session. ccdash appends your standing notes to each of those prompts, so
the orchestrator doesn't retype them:

  ## 共通の注意              the common note of this machine      (ccdash brief common)
  ## このプロジェクトの注意  the note of the session's project    (ccdash brief project <name>)

A project can also have a working directory; start_session with just the project
then starts there.

  ccdash brief                                   # what is set
  ccdash brief common                            # print the common note
  ccdash brief common --edit                     # edit it in $EDITOR
  ccdash brief common --set-file notes.md        # replace it from a file (- = stdin)
  ccdash brief common --clear
  ccdash brief project web                       # print the project's directory and note
  ccdash brief project web --dir ~/src/web       # set its working directory ("" clears)
  ccdash brief project web --edit | --set-file f | --clear
  ccdash brief orchestrator                      # print your own rules for orchestrators
  ccdash brief orchestrator --edit | --set-file f | --clear

Only people write these: no MCP tool can, because the notes become input to every
session an orchestrator starts. Sessions started with 'n', the portal or
handoff-mcp get nothing appended.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := cmd.Context()
			common, err := st.BriefCommon(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("common note: %s\n", noteSize(common))
			ps, err := st.ListProjects(ctx)
			if err != nil {
				return err
			}
			for _, p := range ps {
				dir := p.Dir
				if dir == "" {
					dir = "(no directory)"
				}
				fmt.Printf("%s\t%s\tnote: %s\n", p.Name, dir, noteSize(p.Brief))
			}
			return nil
		},
	}
	c.AddCommand(briefCommonCmd(rf), briefProjectCmd(rf), briefOrchestratorCmd(rf))
	return c
}

func noteSize(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return fmt.Sprintf("%d lines", strings.Count(strings.TrimSpace(s), "\n")+1)
}

// noteFlags are the three ways to replace a note.
type noteFlags struct {
	edit, clear bool
	file        string
}

func (f *noteFlags) add(c *cobra.Command) {
	c.Flags().BoolVar(&f.edit, "edit", false, "edit the note in $EDITOR")
	c.Flags().StringVar(&f.file, "set-file", "", "replace the note with this file's content (- = stdin)")
	c.Flags().BoolVar(&f.clear, "clear", false, "remove the note")
}

func (f *noteFlags) writes() bool { return f.edit || f.clear || f.file != "" }

// next returns the note's new text from the flags, given the current one.
func (f *noteFlags) next(cur string) (string, error) {
	switch {
	case f.clear:
		return "", nil
	case f.file == "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	case f.file != "":
		b, err := os.ReadFile(f.file)
		return string(b), err
	}
	return editText(cur)
}

// editText opens text in $VISUAL / $EDITOR (vi when neither is set) and
// returns what was saved.
func editText(text string) (string, error) {
	f, err := os.CreateTemp("", "ccdash-brief-*.md")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// The editor setting may carry arguments ("code -w").
	c := exec.Command("sh", "-c", editor+` "$1"`, "sh", f.Name())
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", editor, err)
	}
	b, err := os.ReadFile(f.Name())
	return string(b), err
}

func briefCommonCmd(rf *remoteFlags) *cobra.Command {
	var nf noteFlags
	c := &cobra.Command{
		Use:   "common",
		Short: "Print or replace the note appended to every start_session prompt on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := cmd.Context()
			cur, err := st.BriefCommon(ctx)
			if err != nil {
				return err
			}
			if nf.writes() {
				text, err := nf.next(cur)
				if err != nil {
					return err
				}
				if err := st.SetBriefCommon(ctx, text); err != nil {
					return err
				}
				if cur, err = st.BriefCommon(ctx); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "common note: %s\n", noteSize(cur))
				return nil
			}
			printNote(cur)
			return nil
		},
	}
	nf.add(c)
	return c
}

// briefOrchestratorCmd edits the orchestrator_prompt setting: the
// operator's own rules, appended to the role rules in the instructions
// every orchestrator session gets from ccdash mcp at each start and resume
// (docs/decisions/0009-orchestrator.md §11). Several lines don't fit the
// settings page, so it lives here.
func briefOrchestratorCmd(rf *remoteFlags) *cobra.Command {
	var nf noteFlags
	c := &cobra.Command{
		Use:   "orchestrator",
		Short: "Print or replace your own rules for orchestrator sessions (the orchestrator_prompt setting)",
		Long: fmt.Sprintf(`A session you made an orchestrator (the portal's session menu, or 'ccdash role')
gets ccdash's rules for that role in its MCP instructions every time it starts or
resumes. This text is added after them — your own way of working, e.g.

  新しい仕事は既存のプロジェクトに入れる。無ければプロジェクトを作ってよい。
  依頼文には受け入れの条件を必ず書く。

At most %d characters: Claude Code shows a session only the first 4,096
characters of the instructions, and ccdash's own rules come first. A running
orchestrator reads a change at its next restart. Can only be changed on this
machine.`, settings.OrchestratorPromptMax),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := cmd.Context()
			cfg, err := settings.Load(ctx, st)
			if err != nil {
				return err
			}
			if !nf.writes() {
				printNote(cfg.OrchestratorPrompt)
				return nil
			}
			text, err := nf.next(cfg.OrchestratorPrompt)
			if err != nil {
				return err
			}
			if cfg, err = settings.Set(ctx, st, cfg, settings.KeyOrchestratorPrompt, text); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "orchestrator rules: %s (%d of %d characters)\n", noteSize(cfg.OrchestratorPrompt), len([]rune(cfg.OrchestratorPrompt)), settings.OrchestratorPromptMax)
			return nil
		},
	}
	nf.add(c)
	return c
}

func printNote(s string) {
	if s != "" {
		fmt.Println(s)
	}
}

func briefProjectCmd(rf *remoteFlags) *cobra.Command {
	var nf noteFlags
	var dir string
	c := &cobra.Command{
		Use:   "project <name>",
		Short: "Print or replace one project's note and working directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := cmd.Context()
			name := strings.TrimSpace(args[0])
			curDir, curNote, known, err := projectNote(ctx, st, name)
			if err != nil {
				return err
			}
			setDir := cmd.Flags().Changed("dir")
			if !setDir && !nf.writes() {
				if !known {
					return fmt.Errorf("no project %q (ccdash brief lists them; --dir or --edit creates it)", name)
				}
				if curDir == "" {
					curDir = "(none)"
				}
				fmt.Printf("directory: %s\n", curDir)
				printNote(curNote)
				return nil
			}
			if setDir {
				if dir != "" {
					dir = absDir(dir)
				}
				got, err := st.SetProjectDir(ctx, name, dir)
				if err != nil {
					return err
				}
				if got == "" {
					got = "(none)"
				}
				fmt.Fprintf(os.Stderr, "%s directory: %s\n", name, got)
			}
			if nf.writes() {
				text, err := nf.next(curNote)
				if err != nil {
					return err
				}
				if err := st.SetProjectBrief(ctx, name, text); err != nil {
					return err
				}
				if _, curNote, _, err = projectNote(ctx, st, name); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "%s note: %s\n", name, noteSize(curNote))
			}
			return nil
		},
	}
	nf.add(c)
	c.Flags().StringVar(&dir, "dir", "", `the project's working directory on this machine (an existing folder; "" clears)`)
	return c
}

func projectNote(ctx context.Context, st store.Store, name string) (dir, note string, known bool, err error) {
	ps, err := st.ListProjects(ctx)
	if err != nil {
		return "", "", false, err
	}
	for _, p := range ps {
		if p.Name == name {
			return p.Dir, p.Brief, true, nil
		}
	}
	return "", "", false, nil
}

// absDir makes a directory argument absolute: ~/ is the home directory,
// anything relative is taken from where the command runs.
func absDir(d string) string {
	if d == "~" || strings.HasPrefix(d, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(d, "~"))
		}
	}
	if p, err := filepath.Abs(d); err == nil {
		return p
	}
	return d
}
