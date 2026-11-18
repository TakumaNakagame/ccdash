package db

import (
	"context"
	"testing"

	"github.com/TakumaNakagame/ccdash/internal/model"
)

// TestRoles: one orchestrator per project, the role follows the project,
// a merge keeps one, and listings carry the role.
func TestRoles(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "k"} {
		if err := d.UpsertSession(ctx, &model.Session{SessionID: id, Cwd: "/tmp", Status: model.StatusIdle}); err != nil {
			t.Fatal(err)
		}
	}
	role := func(id string) string {
		x, _, _ := d.GetSession(ctx, id)
		return x.Role
	}
	orch := func(name string) string {
		ps, _ := d.ListProjects(ctx)
		for _, p := range ps {
			if p.Name == name {
				return p.Orchestrator
			}
		}
		return "?"
	}
	if _, err := d.SetRole(ctx, "a", "boss", ""); err == nil {
		t.Error("an unknown role was accepted")
	}
	if _, err := d.SetRole(ctx, "nobody", model.RoleOrchestrator, ""); err != ErrNoSession {
		t.Errorf("unknown session: %v", err)
	}
	if _, err := d.SetRole(ctx, "a", model.RoleProjectOrchestrator, ""); err == nil {
		t.Error("a project orchestrator without a project was accepted")
	}
	_ = d.SetProject(ctx, "a", "web")
	_ = d.SetProject(ctx, "b", "docs")
	for _, id := range []string{"a", "b"} {
		if _, err := d.SetRole(ctx, id, model.RoleProjectOrchestrator, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.SetRole(ctx, "c", model.RoleOrchestrator, ""); err != nil {
		t.Fatal(err)
	}
	ss, _ := d.ListSessions(ctx, false)
	got := map[string]string{}
	for _, x := range ss {
		got[x.SessionID] = x.Role
	}
	if got["a"] != model.RoleProjectOrchestrator || got["c"] != model.RoleOrchestrator || got["k"] != "" {
		t.Errorf("listed roles = %v", got)
	}
	if r, p, _, _, _ := d.RoleOf(ctx, "a"); r != model.RoleProjectOrchestrator || p != "web" {
		t.Errorf("RoleOf(a) = %q %q", r, p)
	}
	if r, p, _, _, _ := d.RoleOf(ctx, "c"); r != model.RoleOrchestrator || p != "" {
		t.Errorf("RoleOf(c) = %q %q", r, p)
	}
	if id, _ := d.Orchestrator(ctx); id != "c" {
		t.Errorf("Orchestrator = %q", id)
	}

	// Changing a project orchestrator into the global one frees its project.
	if _, err := d.SetRole(ctx, "a", model.RoleOrchestrator, ""); err != nil {
		t.Fatal(err)
	}
	if orch("web") != "" || role("a") != model.RoleOrchestrator {
		t.Errorf("after a became global: web orchestrator %q, a %q", orch("web"), role("a"))
	}
	_, _ = d.SetRole(ctx, "a", model.RoleProjectOrchestrator, "")

	// Merging docs into web: web keeps its own orchestrator, docs's steps
	// down; merging into a project without one carries it over.
	if err := d.RenameProject(ctx, "docs", "web"); err != nil {
		t.Fatal(err)
	}
	if orch("web") != "a" || role("b") != "" || role("a") != model.RoleProjectOrchestrator {
		t.Errorf("after the merge: web orchestrator %q, a %q, b %q", orch("web"), role("a"), role("b"))
	}
	_ = d.SetProject(ctx, "k", "empty")
	_ = d.SetProjectOrchestratorModel(ctx, "web", "haiku", "low")
	if err := d.RenameProject(ctx, "web", "empty"); err != nil {
		t.Fatal(err)
	}
	ps, _ := d.ListProjects(ctx)
	if len(ps) != 1 || ps[0].Orchestrator != "a" || ps[0].OrchestratorModel != "haiku" || ps[0].OrchestratorEffort != "low" || role("a") != model.RoleProjectOrchestrator {
		t.Errorf("after merging into a project without one: %+v, a %q", ps, role("a"))
	}
	// A plain rename keeps it.
	if err := d.RenameProject(ctx, "empty", "site"); err != nil {
		t.Fatal(err)
	}
	if r, p, m, e, _ := d.RoleOf(ctx, "a"); orch("site") != "a" || r != model.RoleProjectOrchestrator || p != "site" || m != "haiku" || e != "low" {
		t.Errorf("after the rename: orchestrator %q, RoleOf = %q %q %q %q", orch("site"), r, p, m, e)
	}
}
