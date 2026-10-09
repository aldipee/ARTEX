package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// Manual CRUD endpoints for the Overview "Goal management". They write the same goal nodes as the agent-side
// set_goals tool, but the entry point is a human directly adding/editing/deleting in the UI; after add/edit they
// reuse the "revive task" logic (admitTask resume: terminal→running, clear pause, queue if necessary), while delete
// does not revive (per product decision). Every mutation handler goes through beginTaskOperation/decInflight to
// avoid racing with task deletion (consistent with the intent CRUD).

// listGoals returns all goals of this task (text/vulnclass/state split out) for the goal management card to render.
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal manually adds a goal: persist (attached under the task root spawns) → record an "added a goal" trigger to
// wake the planner → revive the task so the planner re-judges whether the goals are met.
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "task is being deleted, cannot add goal")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "goal content cannot be empty")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // record "human added a goal: …" to trigger and wake the planner
	s.reviveTask(t)              // pull a completed/paused task back to running and keep going
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "failed to read after writing goal")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal manually edits a goal's text (and vulnclass): update the DB → record "user changed the goal from old to
// new" to wake the planner → revive the task so the planner adjusts direction based on the new goal.
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "task is being deleted, cannot edit goal")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "goal content cannot be empty")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "goal not found")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // record "user changed the goal from old to new" to trigger and wake the planner
	s.reviveTask(t)                   // same as add: revive the task to re-judge against the new goal
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "failed to read after updating goal")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal manually deletes a goal (hard delete, cascading edge/anchor deletion): delete from DB → record "user
// deleted the goal X" to wake the planner to re-judge the remaining goals. Per product decision, delete does NOT
// revive the task.
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "task is being deleted, cannot delete goal")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "goal not found")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // record "human deleted the goal: …" to trigger and wake the planner (does not revive the task)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
