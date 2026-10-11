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
		"The user paused the task via the task control API (POST /api/tasks/{id}/control, action=pause). The current Planner/Worker run was actively cancelled; in-flight intents return to frontier(open) and will be re-claimed and run from scratch when the task is resumed")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestrator Agent paused the task",
		"The Orchestrator Agent called pause_task to pause this task. The current Planner/Worker run was actively cancelled; in-flight intents return to frontier(open) and re-execute on resume")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier has cancelled the task's running Planner, Worker, and main Agent; the result of this run will not be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task's paused state",
		"On startup the backend restored the task's paused state from the database. This run was cancelled; under normal conditions no Agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "Planner judged the goal achieved",
		"The planner decided the task goal has been achieved and set the task to done, then cancelled any still-running Worker; those intents are marked stopped, not failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task-timeout settle grace exhausted",
		"The task reached its timeout while waiting for a running Worker to settle gracefully, but a 90-second drain grace was still insufficient, so a hard cancel was applied; intents are marked exhausted, and any facts and assets already written during the settle phase are preserved")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent",
		"The planner called kill_work to actively terminate this intent, typically meaning the direction has gone off-track or has no further value; the intent is marked stopped and is not auto-claimed again")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused a running Worker. This call was cancelled and the intent transitioned to paused; already-recorded intents, facts, findings, and activity logs are all preserved, and execution restarts from scratch on resume")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this Worker intent",
		"The user deleted a running Worker. This call was cancelled; after the Worker exits its write region, the server processes the intent per the user's chosen delete mode — soft delete only marks it deleted and preserves all output, while hard delete cascades to remove the intent and any downstream nodes it solely supported")
	AbortWorkFinished = cause("work_finished", "Worker ended normally",
		"The Worker ended normally and the engine released its context resources in detachWork. This is NOT a run interruption; if it appears in an interruption message, a race between cancellation and wind-down events occurred")
	AbortPausedRaceGuard = cause("paused_race_guard", "Refused start while paused",
		"When the task is in the paused state, the engine refused to issue a new execution context to prevent a race between claim and pause that would let a Worker continue to start; already-claimed intents return to frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this conversation turn",
		"The user clicked Stop and actively aborted the current main-Agent or chat-Agent run. Already-generated activity logs are preserved; the next message can be sent normally")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task paused; chat aborted",
		"When the user paused the task, the running main-Agent conversation was also cancelled. Already-generated activity logs are preserved; this turn's message is not auto-replayed on resume")
	AbortChatTurnFinished = cause("chat_turn_finished", "Chat turn finished",
		"The conversation turn ended normally and the server is releasing that turn's context resources. This is NOT a run interruption; if it appears in an interruption message, a race between cancellation and wind-down events occurred")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running Agents are cancelled; leftover running intents on restart are reset to open and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "Run hard-timeout fired",
		"A single run exceeded its soft wall-clock budget plus extra grace, indicating a model request or a tool has been hanging for a long time and the normal turn-boundary wind-down could not run. Focus on the last tool call that had not returned before the interruption")
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
		return "deadline_exceeded", "upstream context reached its deadline",
			"Upstream context reached its deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "canceller did not attach a named cause",
			"Upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; please register the cause in agent/cancelcause.go and wire it at the cancel point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
