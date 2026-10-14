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
	AbortPausedByUser = cause("paused_by_user", "User paused the task",
		"The user paused the task through the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; the running intent returns to frontier(open) and, once the task resumes, is re-claimed and executed again from the start")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestrator Agent paused the task",
		"The orchestrator Agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; the running intent returns to frontier(open) and is re-executed once the task resumes")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier cancelled this task's running Planner, Workers, and main Agent, and this run's results will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task's paused state",
		"On startup the backend restored the task's paused state from the state persisted in the database. This run was cancelled; normally no Agent is running during the recovery phase")
	AbortGoalMet = cause("goal_met", "Planner determined the task goal was met",
		"The planner determined the task goal was met and set the task to done, then cancelled the Workers still running; those intents are marked stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Timeout wind-down drain exhausted",
		"After the task reached its timeout it waited for the running Workers to wind down gracefully, but the 90-second drain grace period still was not enough, so a hard cancel was issued; intents are marked exhausted, and facts and assets already written during the wind-down are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent",
		"The planner called kill_work to actively terminate this intent, usually because it had drifted off course or no longer offered value; the intent is marked stopped and is not re-claimed automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused a running Worker. This call was cancelled and the intent moved to paused; all intents, facts, findings, and activity records already registered are kept, and the intent is re-executed from the start once resumed")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this Worker intent",
		"The user deleted a running Worker. This call was cancelled; after the Worker exits the write region, the server handles the intent according to the deletion mode the user chose — a soft delete only marks it as deleted and keeps all its output, while a hard delete cascades to remove the intent and the downstream nodes supported by it alone")
	AbortWorkFinished = cause("work_finished", "Worker finished; context released",
		"The Worker finished normally and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, it means the cancel and the teardown events raced")
	AbortPausedRaceGuard = cause("paused_race_guard", "New run refused while the task is paused",
		"While the task is paused the engine refuses to issue a new execution context, guarding against a race between claim and pause that would let a Worker keep starting; a claimed intent returns to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this conversation turn",
		"The user clicked stop, actively aborting this turn's main Agent or conversation Agent run. Activity records already produced are kept, and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task paused; main conversation aborted",
		"When the user paused the task, the running main Agent conversation was cancelled along with it. Activity records already produced are kept; this turn's message is not replayed automatically once the task resumes")
	AbortChatTurnFinished = cause("chat_turn_finished", "Conversation turn finished normally",
		"This conversation turn finished normally and the server is releasing the turn's context resources. This is not a run interruption; if it appears in an interruption message, it means the cancel and the teardown events raced")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running Agents are cancelled; after restart, leftover running intents are reset to open and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "The per-run hard-timeout backstop fired",
		"A single run exceeded its soft wall-clock budget plus the extra grace period, meaning a model request or some tool went a long time without returning and the normal turn-boundary wind-down could not run. Focus on the last unreturned tool call before the interruption")
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
		return "deadline_exceeded", "Upstream context reached its deadline",
			"The upstream context reached its deadline, but whoever set it did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceller attached no named cause",
			"The upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; please register a cause in agent/cancelcause.go and wire it into that cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
