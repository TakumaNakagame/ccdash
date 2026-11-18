package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TakumaNakagame/ccdash/internal/model"
	"github.com/TakumaNakagame/ccdash/internal/store"
)

// roleCmd gives a session an orchestrator role, takes it away, or hands it
// over (docs/decisions/0009-orchestrator.md §7, §10) — the CLI twin of the
// portal's session menu. Only people do this: ccdash mcp has no such tool.
func roleCmd(rf *remoteFlags) *cobra.Command {
	var from, pmodel, peffort string
	c := &cobra.Command{
		Use:   "role [<session> <orchestrator|project-orchestrator|none>]",
		Short: "Make a session an orchestrator (or stop it being one); with no arguments, list the orchestrators",
		Long: `An orchestrator session takes your requests, hands the work to other sessions
(MCP start_session), watches them and reports — it does not do the work. ccdash
starts and resumes such a session with:

  - the role's rules in its MCP instructions (plus 'ccdash brief orchestrator'),
  - the tools in the 'Orchestrator: tools taken away' setting removed
    (claude --disallowedTools),
  - the model / effort of the 'Orchestrator: model' / 'effort' settings.

  ccdash role                                   # who is an orchestrator
  ccdash role 231 orchestrator                  # the global orchestrator
  ccdash role 240 project-orchestrator          # of the project session 240 is in (one per project)
  ccdash role 231 none                          # an ordinary session again
  ccdash role 250 --from 231                    # 250 takes over 231's role and its children
  ccdash role --project web --model haiku --effort low   # what web's orchestrator runs with ("" = the settings)

A session that is running under ccdash picks the change up at its next restart
(the portal's session menu → restart, or stop and resume it).`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeFn, err := openStore(rf)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := cmd.Context()
			project, _ := cmd.Flags().GetString("project")
			if project != "" {
				if len(args) > 0 {
					return fmt.Errorf("--project sets a project's orchestrator model / effort; it takes no session")
				}
				if err := st.SetProjectOrchestrator(ctx, strings.TrimSpace(project), strings.TrimSpace(pmodel), strings.TrimSpace(peffort)); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "%s orchestrator: model %s, effort %s\n", project, orSetting(pmodel), orSetting(peffort))
				return nil
			}
			if len(args) == 0 {
				return listRoles(ctx, st)
			}
			role := ""
			switch {
			case len(args) == 2 && args[1] == "none":
			case len(args) == 2:
				role = args[1]
			case from == "":
				return fmt.Errorf("say the role: orchestrator, project-orchestrator or none (or --from <session> to take one over)")
			}
			x, err := findSessionArg(ctx, st, args[0])
			if err != nil {
				return err
			}
			fromID := ""
			if from != "" {
				f, err := findSessionArg(ctx, st, from)
				if err != nil {
					return err
				}
				fromID = f.SessionID
			}
			ch, err := st.SetRole(ctx, x.SessionID, role, fromID)
			if err != nil {
				return err
			}
			what := ch.Role
			if what == "" {
				what = "no role"
			}
			if ch.Project != "" {
				what += " of " + ch.Project
			}
			fmt.Fprintf(os.Stderr, "%s: %s\n", x.Ref(), what)
			if ch.Moved > 0 {
				fmt.Fprintf(os.Stderr, "%d children moved over\n", ch.Moved)
			}
			if len(ch.Replaced) > 0 {
				fmt.Fprintf(os.Stderr, "no longer an orchestrator: %s\n", strings.Join(ch.Replaced, ", "))
			}
			if ch.RestartNeeded || ch.RestartFrom {
				fmt.Fprintln(os.Stderr, "running under ccdash: the change applies from the next restart of the session (portal: session menu → restart)")
			}
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "take the role (and the children) over from this session")
	c.Flags().String("project", "", "set what this project's orchestrator runs with (--model / --effort)")
	c.Flags().StringVar(&pmodel, "model", "", `with --project: the model ("" = the 'Orchestrator: model' setting)`)
	c.Flags().StringVar(&peffort, "effort", "", `with --project: the effort ("" = the 'Orchestrator: effort' setting)`)
	return c
}

func orSetting(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(the setting)"
	}
	return v
}

func listRoles(ctx context.Context, st store.Store) error {
	n := 0
	for _, archived := range []bool{false, true} {
		ss, err := st.ListSessions(ctx, archived)
		if err != nil {
			return err
		}
		for _, x := range ss {
			if !model.IsOrchestrator(x.Role) {
				continue
			}
			n++
			what := x.Role
			if x.Project != "" && x.Role == model.RoleProjectOrchestrator {
				what += " of " + x.Project
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", x.Ref(), what, x.Status, x.DisplayTitle())
		}
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "no orchestrator: `ccdash role <session> orchestrator` makes one")
	}
	return nil
}

// findSessionArg resolves "#N", N, a ref like "A231" or a session id among
// this collector's sessions, archived ones included.
func findSessionArg(ctx context.Context, st store.Store, ref string) (model.Session, error) {
	ref = strings.TrimSpace(ref)
	for _, archived := range []bool{false, true} {
		ss, err := st.ListSessions(ctx, archived)
		if err != nil {
			return model.Session{}, err
		}
		for _, x := range ss {
			if x.SessionID == ref || model.MatchesRef(ref, x.HostLetter, x.Num) {
				return x, nil
			}
		}
	}
	return model.Session{}, fmt.Errorf("no session %q (ccdash sessions lists them)", ref)
}
