package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up / settlement prompt: when an agent is terminated due to [step exhaustion (MaxTurns)] or
// [timeout (run_seconds/MaxDuration)], the SDK's settlement phase injects this prompt so the agent
// first persists what it identified but has not written back, then outputs a one-line summary, avoiding an unfinished mess.
//
// Each agent's wrap-up prompt can be overridden in the backend as needed (stored in agents.wrapup_prompt); empty uses the
// built-in default here. Only the [prompt body] is editable; which tools are disabled and how many turns the wrap-up itself gets are code-owned policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. worker reuses the historically hardcoded settleWrapUpPrompt
// (defined in worker.go), planner/mainagent each have their own; an unmatched key (a custom agent) uses the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: the built-in default turn budget for each agent's [own] wrap-up phase (overridable in the backend with >0).
// All get 10 turns, to ensure the wrap-up phase has enough steps to persist. An unmatched key uses genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planning steps for this round are about to run out — note that only [this round] ends; the system will wake you again as the state changes to keep planning, this is not task termination, and you need not wind down the whole plan here. Land the conclusions already worked out this round so the round is not wasted, but also [do not pad intents just to wrap up] (0 intents this round is still a perfectly normal result): (1) if you have judged an exploration direction that [should be dispatched now], submit it in one batched add_intent (do not hold back what you have figured out); (2) for a goal already proven met by some finding/fact, call prove_goal to mark it met (do not miss one); (3) if you identified a serial exploitation chain that needs stepping, note it with TodoWrite so you can continue dispatching on the next wake. When done, just end the round; no summary text needed."

const mainAgentWrapUpDefault = "Your steps are about to run out and this interaction is ending. Do not start any new exploration/operation. In **a single separate plain-text sentence**, summarize the current progress, the key conclusions, and the suggested next step for the user."

const genericWrapUpDefault = "You are about to be terminated due to budget exhaustion. First write back any completed-but-unpersisted results, then in **a single separate plain-text sentence** summarize what you did and the key conclusions you reached (this sentence is shown as this run's result)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up prompt (see docs/task-level-timeout-and-wrapup-design.md) ----------
//
// It is a [separate set] from the per-run wrap-up prompt: per-run means "your budget for this one run is spent"; task timeout means
// "the whole task's time is up and it is about to end". The semantics are often opposite (especially the planner: per-run says "don't stop, keep planning",
// task timeout says "time's up, stop planning and make the final judgment"). Configured only for worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: the DB override for the task-timeout wrap-up prompt and turn count
// (wired to agents.task_timeout_wrapup_prompt / _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not your budget for this run — the entire exploration is up). This is the last chance: (1) persist [all] of what you identified but have not written back — new assets with insert_assets, exploration conclusions/facts with record_fact, confirmed vulns with report_finding; (2) do not start any new command/probe; (3) in **a single separate plain-text sentence at the end**, summarize your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not this round — the entire task is terminating). Based on [all] current facts and findings, make a final goal judgment: for a goal proven met by evidence, call prove_goal to mark it met (do not miss one). **Do not generate any new intents** (dispatching an intent now would not be executed anyway). Once the judgment is done, wind down; no summary text needed."

// TaskTimeoutWrapupDefault returns an agent's built-in default task-timeout wrap-up prompt (for the backend placeholder / restore-default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // returns an empty string for unconfigured ones (mainagent/chat)
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. An empty string means this agent has no task-timeout prompt
// (not worker/planner), in which case the caller should fall back to the per-run prompt.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // defaults to the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is squeezed by the task deadline: wrap-up due to Timeout = the task's time is up → the task-timeout prompt;
//     wrap-up due to MaxTurns = steps ran out first within the clamped window while the task has a few minutes left → fall back to the per-run prompt.
//   - clamped=false → the task is not near its end: both reasons use the per-run prompt (i.e. it degrades to wrapupSettlement).
//
// Handing PromptByReason to the harness lets it pick by the [actual] reason at wrap-up time, with no build-time mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value both reasons take when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // the task's time is up
				harness.ReasonMaxTurns: perRun, // steps ran out first while the task still has time
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
