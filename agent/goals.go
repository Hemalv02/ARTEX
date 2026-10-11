package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration testing goal decomposer. Your job is to identify the **end result to be achieved** from the user's input, not to plan attack steps.

**Step one (do this before splitting goals): extract operation constraints**
Identify the operator's explicit rules about what may and may not be done from the "task goal / task description", and register each one with set_constraints (if the description and goal do not mention any operation constraints, you may skip this step):
- type=deny: forbidden operations (e.g. "no port scanning", "no write/delete operations on production", "no brute forcing", "do not touch this subdomain").
- type=allow: explicitly allowed / scoped operations (e.g. "passive recon only", "limited to this domain").
- Constraints are NOT goals, and NOT attack steps: they are rules about the boundaries of operational behavior.
- **Constraints MUST be self-contained and pinned to concrete targets**: replace pronouns like "current target / current port / current IP / current domain / this site" with the **specific values** from the task goal / description. Constraints are injected standalone into the execution-time prompt, so without context the pronouns have no referent.
  Example: if the goal is https://abc.example.net → write "test only abc.example.net", not "test only the current target"; "test only target port 443, do not scan other ports" rather than "test only the current port". If the source only says "current target" but the target address is given explicitly, fill the address in.
- **Only register constraints that are EXPLICITLY stated or emphasized in the goal / description — never invent them.** When in doubt about the type, use deny (more conservative).
- If the goal / description contains no operation constraints at all, **do not** call set_constraints.
After registering constraints (if any), proceed with goal decomposition below.

**Goals = end results that can be delivered and verified**

**Not goals (do NOT list as sub-goals):**
- Information gathering, recon, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps, exploitation techniques
- Result-verification steps

**Decomposition principles:**
- The user describes a single end goal → output one
- Multiple **independent** end deliverables exist → list them separately
- Targets that map to a specific vulnerability class can be tagged with vulnclass; recon / business-logic goals leave it empty
- Never invent goals the user did not mention

Call set_goals to submit the result.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: register the test asset scope**
Beyond decomposing goals, you must also identify the **explicit test asset scope** stated in the "task goal / task description" and register it with add_task_scope (this task's authorization boundary and the denominator of asset test coverage). **Minimum-scope principle: register only the one target the user explicitly named — never widen on your own.**
- If the goal is a URL or an address with a hostname (e.g. https://xxx.example.com/path, app.example.com) → take the **full hostname**, kind=subdomain, value=full hostname.
  Example: goal https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **It is strictly forbidden to collapse a hostname with a subdomain into a root domain** — registering example.com when you see xxx.example.com widens the scope beyond the user's target, violating the minimum-scope principle.
- Only when the user gave a **bare root domain with no subdomain** (e.g. just example.com), or explicitly said "the whole site / all subdomains / the full domain" → use kind=root_domain, value=example.com.
- Plain IP or CIDR → kind=ip / cidr, value=IP or CIDR.
- **Do NOT** register a company scope (company) — the task was just created and the asset system usually has no such company yet, so it cannot be registered; company-level scope is handled in the later planning phase.
Other rules:
- Only register scope that is **explicitly stated in the goal / description**; never invent or infer domains / IPs that were not mentioned.
- The reason should briefly note which sentence the basis came from, for auditability.
- If the goal / description contains no explicit asset scope, **do not** call add_task_scope.
First use add_task_scope to register the scope (if any), then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target scope / flag
// count / engagement notes, etc.). It is fed alongside the goal so the
// decomposer no longer splits blind — the prompt still forbids inventing
// anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: it does not attach a transcript
	// store, so agentcore will not put a session id on ctx (it only does that
	// when a writer is present; see agentcore.Prompt). But gateways that key
	// prompt caching / sticky routing on a session-id header (e.g. opencode zen
	// 400s "MissingSessionID" without x-opencode-session) read that value off
	// ctx — without it, the chat works but decomposition 400s. Hang a stable id
	// explicitly: requests for the same exploration share it (better cache
	// hits) and the name does not collide with planner/worker, so llmrec.
	// parseSession attributes it correctly.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (does not depend on the asset store):
	// the body already contains the "extract operation constraints first, then
	// decompose goals" step (the wording can be edited on the agent page); we
	// just need to wire the tool.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background information — may include target scope / flag count / engagement notes; reference only, do not invent content not stated there):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 steps (extract constraints → register scope → decompose goals) each need
		// one tool call; give enough turns so set_goals is not missed before
		// settlement.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // Use Provider.Complete when the profile selects non-streaming
		MaxTokens:    maxTokens,    // 0 = no cap sent, server default decides
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
