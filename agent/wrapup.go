package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up / settlement prompt: when an agent is terminated because of
// exhausted steps (MaxTurns) or wall-clock timeout (run_seconds/MaxDuration),
// the SDK's settlement phase injects this prompt so the agent first writes
// back anything it has already identified, then outputs a summary — avoiding
// an unfinished run.
//
// Each agent's wrap-up prompt can be overridden from the admin UI (stored in
// agents.wrapup_prompt); an empty value falls back to the built-in default
// defined here. Only the prompt body is editable; the disabled-tools list and
// the wrap-up turn budget are code-owned policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. The worker reuses the
// historically-hard-coded settleWrapUpPrompt (defined in worker.go); the planner
// and mainagent each have their own version. Unmatched keys (custom agents) use
// the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: the built-in default turn budget for each agent's wrap-up
// phase (overridable from the admin UI when > 0). All set to 10 so the wrap-up
// phase has enough steps to write back. Unmatched keys use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planned-step budget for this round is nearly exhausted — note that this only means **this round** is ending; the system will continue to wake you for further planning as the situation evolves. It is NOT a task termination, and you do not need to wrap up the whole plan here. Land the conclusions you've already reached this round — don't let the round go to waste — but **do not force intents just to wrap up** (0 intents this round is still a perfectly normal outcome): (1) if you have already identified an exploration direction that **should be dispatched right now**, batch-submit it with a single add_intent (don't sit on what you've already decided); (2) for any goal that is already proven by a finding/fact, call prove_goal to mark it met (don't miss it); (3) if you recognize a serial exploit chain that needs to be split into steps, write it down with TodoWrite so the next wake-up can continue dispatching. When done, just end this round; no summary text is needed."

const mainAgentWrapUpDefault = "Your step budget is nearly exhausted and this interaction is about to end. Do not start any new exploration or operations. Please **summarize the current progress, key conclusions, and suggested next steps in a single plain-text sentence** to the user."

const genericWrapUpDefault = "You are about to be terminated because your budget is exhausted. Please first write back any completed results that are not yet persisted, then **summarize in a single plain-text sentence** what you did and the key conclusions you reached (this sentence will be shown as the result of this run)."

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
// This is a **different pair** from the per-run wrap-up prompt: per-run says "your
// budget for THIS run is up"; task-timeout says "the whole task has reached its
// deadline, time to end". The semantics often run in opposite directions (especially
// for the planner: per-run says "don't stop, keep planning", while task-timeout says
// "time's up, stop planning and make a final judgment"). Only configured for
// worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB override for the task-timeout
// wrap-up prompt and turn count (wired to agents.task_timeout_wrapup_prompt /
// _max_turns; worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not your per-run budget — the entire exploration is done). This is the last chance to: (1) write back **everything** you have already identified but not yet persisted — new assets via insert_assets, exploration conclusions/facts via record_fact, and confirmed findings via report_finding; (2) do NOT start any new commands or probes; (3) **finally, summarize the key conclusions for this intent in a single plain-text sentence**."

const plannerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not this round — the whole task is terminating). Based on the current **all** facts and findings, make a final goal judgment: for any goal that is already proven by evidence, call prove_goal to mark it met (don't miss any). **Do NOT generate any new intents** (intents dispatched now will not be executed). Once judgment is complete, wrap up; no summary text is needed."

// TaskTimeoutWrapupDefault returns the built-in task-timeout default wrap-up
// prompt for the given agent (used by the admin UI as the placeholder / restore
// default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // unconfigured (mainagent/chat) returns empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. An empty
// string means this agent has no task-timeout prompt (i.e. not a worker/planner),
// in which case the caller should fall back to the per-run prompt.
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
	return resolveWrapupTurns(agentKey) // default: reuse the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is squeezed by the task deadline: Timeout wind-up
//     = task deadline reached → use the task-timeout prompt; MaxTurns wind-up =
//     steps exhausted inside the squeeze window while the task still has time →
//     fall back to the per-run prompt.
//   - clamped=false → task is still early: both reasons use the per-run prompt
//     (i.e. degraded to wrapupSettlement).
//
// Harness.PromptByReason picks the prompt at wind-up time based on the **actual**
// reason, avoiding the build-time mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // task deadline reached
				harness.ReasonMaxTurns: perRun, // steps exhausted first, task still has time
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
