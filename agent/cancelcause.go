package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "Task paused by user",
		"The user paused the task through the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was cancelled; running intents return to frontier(open) and are reclaimed and rerun from the beginning when the task resumes.")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Task paused by orchestration agent",
		"The orchestration agent paused this task with the pause_task tool. This Planner/Worker run was cancelled; running intents return to frontier(open) and are rerun when the task resumes.")
	AbortTaskDeleted = cause("task_deleted", "Task deleted",
		"The task is being deleted (DELETE /api/tasks/{id}). The deletion barrier cancelled its running Planner, Worker, and main agent; this run's result will not be used.")
	AbortPausedOnReload = cause("paused_on_reload", "Task pause restored by backend",
		"At startup, the backend restored the task's paused state from the database. This run was cancelled; normally, no agent is running during restoration.")
	AbortGoalMet = cause("goal_met", "Planner marked the task goal as met",
		"The Planner marked the task done after deciding its goal was met, then cancelled any remaining Workers; their intents are marked stopped rather than failed.")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task timeout drain period expired",
		"The task timed out and waited for running Workers to settle gracefully, but the 90-second drain period was insufficient, so they were force-cancelled. Their intents are marked exhausted, and facts and assets already written during settlement are retained.")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner stopped this intent",
		"The Planner stopped this intent with kill_work, usually because its direction was off track or no longer useful. It is marked stopped and will not be reclaimed automatically.")
	AbortWorkPausedByUser = cause("work_paused_by_user", "Worker intent paused by user",
		"The user paused a running Worker. This call was cancelled and the intent is now paused; its registered intent, facts, findings, and activity records are retained. It will restart from the beginning when resumed.")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "Worker intent deleted by user",
		"The user deleted a running Worker. This call was cancelled. After the Worker leaves its write section, the server applies the selected deletion mode: a soft delete marks the intent deleted and retains all output; a hard delete cascades through the intent and downstream nodes supported only by it.")
	AbortWorkFinished = cause("work_finished", "Worker finished and released its context",
		"The Worker finished normally, and the engine released its context in detachWork. This is not an interrupted run; if it appears in an interruption message, cancellation and cleanup raced.")
	AbortPausedRaceGuard = cause("paused_race_guard", "New run rejected while task is paused",
		"While the task is paused, the engine refuses to create a new execution context. This prevents a race between claim and pause from starting another Worker; already claimed intents return to the frontier.")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "Current conversation turn stopped by user",
		"The user clicked Stop to interrupt this main-agent or session-agent run. Existing activity records are retained, and the user can send another message.")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task paused and main-agent conversation cancelled",
		"Pausing the task also cancelled its running main-agent conversation. Existing activity records are retained; resuming the task will not replay this message automatically.")
	AbortChatTurnFinished = cause("chat_turn_finished", "Conversation turn finished and released its context",
		"The conversation turn finished normally, and the server is releasing its context. This is not an interrupted run; if it appears in an interruption message, cancellation and cleanup raced.")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running agents are being cancelled; after restart, any intents left running are reset to open and retried.")
	AbortRunHardTimeout = cause("run_hard_timeout", "Per-run hard timeout fallback triggered",
		"The run exceeded its soft wall-clock budget and grace period. A model request or tool likely did not return in time, preventing normal turn-boundary settlement. Check the last tool call that had not returned before the interruption.")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context deadline reached",
			"The upstream context deadline was reached, but the caller did not attach a named cause with WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Cancelling caller attached no named cause",
			"The upstream context was cancelled, but the caller did not attach a named cause with context.WithCancelCause. Register a cause in agent/cancelcause.go and connect it to this cancellation site.", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
