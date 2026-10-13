package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/takumanakagame/ccmanage/internal/db"
	"github.com/takumanakagame/ccmanage/internal/model"
	"github.com/takumanakagame/ccmanage/internal/tasks"
)

// The to-do list's routes (docs/decisions/0006-tasks.md). Tasks are named
// in the path by id or key ({id}). sync and import take whole files, so
// they read a larger body than decodeJSONBody allows, and stay off the hub
// allowlist.

const taskBulkBody = 8 << 20

func (s *Server) registerTaskRoutes(wrap func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/tasks", wrap(s.handleAPITasks))
	s.mux.HandleFunc("POST /api/tasks", wrap(s.handleAPITaskAdd))
	s.mux.HandleFunc("GET /api/tasks/context", wrap(s.handleAPITaskContext))
	s.mux.HandleFunc("POST /api/tasks/order", wrap(s.handleAPITaskOrder))
	s.mux.HandleFunc("POST /api/tasks/sync", wrap(s.handleAPITaskSync))
	s.mux.HandleFunc("POST /api/tasks/import", wrap(s.handleAPITaskImport))
	s.mux.HandleFunc("GET /api/tasks/{id}", wrap(s.handleAPITask))
	s.mux.HandleFunc("POST /api/tasks/{id}", wrap(s.handleAPITaskUpdate))
	s.mux.HandleFunc("POST /api/tasks/{id}/notes", wrap(s.handleAPITaskNote))
}

func writeTaskErr(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrTaskNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Everything else from the task layer is a rule the request broke
	// (bad date, archiving an open task, duplicate key …).
	http.Error(w, err.Error(), http.StatusBadRequest)
}

func (s *Server) handleAPITasks(w http.ResponseWriter, r *http.Request) {
	l, err := s.db.TaskList(r.Context(), r.URL.Query().Get("archived") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, l)
}

func (s *Server) handleAPITask(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	t.Bucket = tasks.BucketOf(t, time.Now())
	writeOK(w, t)
}

func (s *Server) handleAPITaskAdd(w http.ResponseWriter, r *http.Request) {
	var in tasks.Input
	if err := decodeJSONBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Source == "" {
		in.Source = tasks.SourceManual
	}
	t, err := s.db.AddTask(r.Context(), in)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeOK(w, t)
}

func (s *Server) handleAPITaskUpdate(w http.ResponseWriter, r *http.Request) {
	var p tasks.Patch
	if err := decodeJSONBody(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if p.Source == "" {
		p.Source = tasks.SourceManual
	}
	t, err := s.db.UpdateTask(r.Context(), r.PathValue("id"), p)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeOK(w, t)
}

func (s *Server) handleAPITaskNote(w http.ResponseWriter, r *http.Request) {
	var n model.TaskNote
	if err := decodeJSONBody(r, &n); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if n.Source == "" {
		n.Source = tasks.SourceManual
	}
	t, err := s.db.AddTaskNote(r.Context(), r.PathValue("id"), n)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeOK(w, t)
}

func (s *Server) handleAPITaskOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.OrderTasks(r.Context(), body.IDs); err != nil {
		writeTaskErr(w, err)
		return
	}
	writeOK(w, nil)
}

func (s *Server) handleAPITaskContext(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.TaskContext(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, c)
}

func decodeBulk(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, taskBulkBody)).Decode(v); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

func (s *Server) handleAPITaskSync(w http.ResponseWriter, r *http.Request) {
	var p tasks.Proposal
	if err := decodeBulk(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.db.SyncTasks(r.Context(), p)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeOK(w, res)
}

func (s *Server) handleAPITaskImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File     tasks.LegacyFile  `json:"file"`
		Map      map[string]string `json:"map"`
		Unmapped string            `json:"unmapped"`
		DryRun   bool              `json:"dry_run"`
	}
	if err := decodeBulk(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	plan, err := s.db.ImportTasks(r.Context(), body.File, tasks.ImportOptions{Map: body.Map, Unmapped: body.Unmapped}, body.DryRun)
	if err != nil {
		// The dry-run table is useful even when the import is refused.
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "plan": plan})
		return
	}
	writeOK(w, plan)
}
