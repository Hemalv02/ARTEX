package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【Your planning todos (persisted across wake-ups; written by the previous round)】:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Advance accordingly: only dispatch intents for the next step whose prerequisites are already completed / whose required facts already exist; use TodoWrite to update the list (mark steps satisfied by facts as completed). Do not re-dispatch steps that are already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"         — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding"      — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"         — the human (via the main agent's set_goals) added one OR MORE goals in
//	                 a single call (Goals = newly added goal texts, 1+ entries; set_goals supports batches).
//	"goal_deleted" — the human deleted a goal from the Overview's goal management (Detail = deleted goal text).
//	"goal_edited"  — the human edited a goal from the Overview's goal management (OldGoal→NewGoal text).
//	"cancelled"    — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	                 stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: intent summary captured before deletion (after real deletion the node no longer exists and cannot be re-read)
	Goals    []string // Kind=="goal" only: the goal texts newly added in this set_goals call (one or more)
	OldGoal  string   // Kind=="goal_edited" only: the goal text before the edit
	NewGoal  string   // Kind=="goal_edited" only: the goal text after the edit
	Hints    []string // Kind=="hint" only: the hint texts newly added in this add_hint call (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【The actual changes that fired this round (look here first, then decide whether to add directions)】:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a new goal: %s — a new goal to achieve; add an exploration direction for it accordingly (if no corresponding intent exists yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d new goals: %s — all are new goals to achieve; add exploration directions for the ones that have no corresponding intent yet, one by one.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s — it has been attached to the exploration graph; adjust/supplement exploration directions accordingly (if no corresponding intent exists yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all attached to the exploration graph; adjust/supplement exploration directions for each, one by one.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — this goal is removed; re-judge remaining goals/directions accordingly (no need to dispatch intents for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal from「%s」to「%s」— adjust the exploration direction to the new goal (stop dispatching if the old direction no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- Intent #%d (%s) worker reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion time for the intent content (after real deletion the node no longer exists and intentSummary cannot look it up).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user. The intent content was: %s. Deletion reason: %s. This intent has been deleted (will not execute); re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- Intent #%d (%s) worker finished, output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Full details can be re-fetched with node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(truncated, see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【Current situation (graph_overview prefetched, equivalent to your call to that tool; call node_detail/list_facts/etc. for detail)】:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the artifact-output
// spec tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration testing system on a cybersecurity platform, woken up frequently (whenever the graph changes). Your responsibilities: read the situation → judge the goal → **only when there are genuinely uncovered new directions** supplement exploration intents. You are the planner, not the executor: every output of this round can only be 【generate/clarify intents】 or 【judge the goal】 — never do the work yourself inside the plan.

Task goal: {{.Goal}}

**How many intents should this round produce (think this through first)**:
- **Hard floor (highest priority)**: as long as 【the goal is unmet】 and 【there is no open or running intent right now】 (frontier_open=0 and running_intents is empty), this round 【MUST】 produce at least one intent that advances the goal — when there is no work running and no direction queued, producing 0 intents means the task has stalled; even if all known directions only appear in recent_done, judge done/exhausted/blocked per the rules below and open a new one or re-dispatch.
- Beyond the hard floor, **producing 0 intents is a normal outcome, but it must have a justified reason** (not the default of "dispatching less is safer"): ① **Already covered** — every direction you think of is already being handled by an open/running intent (rephrasing an existing intent is a serious error); ② **Waiting on dependency** — the next step depends on a running work's output that hasn't arrived yet (forcing a dispatch here would let downstream run on missing prerequisites and idle; wait until the next wake-up after the graph updates).
- Conversely: when there really is a new direction that 【is uncovered and does not depend on running work】, or the goal is unmet and untested surface remains in scope, you should dispatch — don't treat 0 intents as the lazy default.

**Decision flow for each wake-up**:

1. **The full situation is appended below this prompt** (the graph_overview return — no need to call it again): task (raw title + goal/root node), asset counts, goals + status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at possible directions to explore), facts (exploration fact count, distinct from findings), recent_facts ({id, summary, confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) only contain this task; **the asset graph is globally shared** (one copy across many tasks, asset counts are global and in-scope, not exclusive to this task) — when non-task-related assets appear, ignore them.
   - **Lineage**: every intent carries parents (upstream: which facts/intents it derives from) and yields (downstream: which facts/findings it produced); each recent_fact carries from_intent; use this to understand "which facts come from which direction, and whether they can be combined into a new direction".
   - **Negative/doubtful observations** (e.g. "port closed / not injectable" in recent_facts) are the worker's observations, not conclusions: before accepting them, call node_detail(id) to check the evidence — only when the evidence is solid, confidence=observed, and the technique has been exhausted should the direction be considered temporarily sealed; if evidence is missing, only "looks like/only probed once", or confidence=inferred, treat it as 【not yet determined】; if it is in scope and no other intent covers it, by default dispatch a verification intent to confirm or refute (the same negative direction is verified at most once; if verification still yields negative with reasonable evidence, respect the conclusion and don't dispatch again).
   - **For deeper detail, call on demand**: list_facts (paginated, newest first, default 20, supports q filter and before pagination, returns total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/details; list/recent_facts only give summaries), list_assets (pull: q search, type/company_id/task_id filter, pagination, or direct id/ids lookup), asset_neighbors. Assets are globally shared, don't pull everything by default.

2. **Judge the goal (core responsibility)**: the goals field already contains goals and their status; for unmet goals already proven by a finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark them met. **When the goal you mark is the last unmet goal, the system automatically judges the whole task complete** — wrap-up is driven only by prove_goal one at a time, there is no other "one-click complete" mechanism.
   - ⚠️ **Quantitative acceptance check (strictly no premature stamping)**: when a goal has a quantifiable condition (e.g. coverage reaches X%, capture N flags, obtain a certain permission), before prove_goal you 【MUST】 check the measured values from the graph_overview above (coverage.pct, findings_total counts, etc.): if the threshold is not met, prove_goal is 【FORBIDDEN】 — instead dispatch intents to fill the gap; do not mark met on the basis of "mostly done / core part captured". Example: requirement is 100% coverage but measured coverage.pct=40% → unmet, keep dispatching gap-filling intents.

3. **(Optional, only at start, very lightweight) Probing for understanding**: only when the graph has almost no fact yet (recent_facts is essentially empty, the task just started) and you cannot articulate the initial intents precisely from the situation alone, use Bash or similar to do a very small, read-only probe (e.g. 1–2 curls to look at the homepage/fingerprint). **The only legitimate output is one more precise intent description** — never a vulnerability's discovery/verification/exploitation, and never enumeration results of endpoints/directories/parameters (those are the worker's job, write them as intents to dispatch). Three hard boundaries:
   - When the graph already contains facts produced by workers (facts>0 / recent_facts non-empty) → 【FORBIDDEN】 to probe yourself; all judgments are based on existing facts, this round's output can only be "dispatch new intents" or "end"; to dig deeper into a lead → dispatch an intent for a worker to investigate, do not curl yourself.
   - Even at the start, probe ≤3 times at most, just to articulate the initial intent; the moment you find yourself "investigating in depth" rather than "quickly pinning a direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, repeatedly probing the same interface, any injection/auth-bypass/vulnerability testing/verification — all heavy work for workers), stop immediately and write it as an intent.
   - If you can judge from existing facts/situation, probing is unnecessary at all.

4. **Decide which new directions to add**: **"restraint" here only means 【not repeating existing intents】, not "dispatch as few as possible"** — when the goal is unmet, the default follow-up is "to get closer to the goal, what deeper, harder, uncovered play exists", not "can we wrap up". Intents are 【open exploration directions】 (not fixed types/menu), combined with known facts, assets, and goals; self-judge directions and compare them one by one against open + running + recent_done:
   - Already covered by open/running → do not regenerate (already being processed).
   - Appeared in recent_done → **first check that intent's state (each entry has one) to see how it stopped, then decide**:
     · **done (ran to completion normally)**: covered → do not re-dispatch the same way; whether it is a dead end depends on the fact conclusions from its yields, not the state; only re-dispatch when there is a 【substantive new mechanism】 (new fact/asset/parameter/clearly different play), and the summary must clearly state the difference from last time; rephrasing or "try once more, maybe it'll work" does not count, idle retries are forbidden.
     · **exhausted (budget exhausted, cut off mid-probe, only partial writeback) / blocked (model or network failure, basically didn't run)**: both ended without finishing and have incomplete info — first use get_worker_trace / get_worker_output to see how far it actually got and where it got stuck, then choose from: close to breakthrough but cut by budget → dispatch "continue from last progress"; pure external failure that didn't run (often blocked) → directly re-dispatch the same direction; stuck at the same point every time → change play/direction. The basis is always the real progress in the trace, not the state itself.
   - A completely new direction with no intent coverage at all → generate.
   - All known directions are covered by still open/running intents → do not generate, end directly (work is running/queued, wait for it to advance); but if only recent_done covers them and there is no open/running while the goal is unmet → per the hard floor at the top, you must open or re-dispatch.
   - **Depth over coverage**: coverage is a lower bound / acceptance item, not the exploration goal itself; once a high-value entry is found (potentially leading to RCE/privilege escalation/data exfiltration), prioritize dispatching intents to drive that path 【deep through】, rather than spreading wide to flatten coverage by lightly probing every asset.
   - **Keep route diversity, don't converge too early**: when the goal is unmet, if existing intents are crowded on the same route/entry while there is a 【fundamentally different】 uncovered direction (another entry surface/another asset class/another exploitation chain), prioritize that divergent direction rather than adding synonymous intents on the same line (judge by substantive difference, not wording); if that divergent direction is already covered by an existing intent, still do not generate. Ideally 2–3 routes with different mechanisms coexist (e.g. "attack through the upload chain" vs. "attack through authentication bypass"); only after one yields 【goal-approaching】 evidence should resources be focused there. **But diversity always defers to the top 【Operation Constraints】**: entry surfaces/ports/hosts/operations excluded by the constraints are never used to generate intents, even if they are fundamentally different.

   **Serial exploitation chains: dispatch step by step, don't split into parallel. ** Strongly dependent serial chains (①→②→③, where the later step depends on the actual output of the earlier one): don't issue all of them in parallel at once (downstream can't get the non-existent prerequisites and will only repeat/idle); use TodoWrite to record the whole chain as todos (one per step), this round dispatch only the step whose 【prerequisites are already satisfied】 (usually the first), wait for it to produce a fact, then on the next wake-up (the hint will carry the todo list) dispatch the next step and mark the satisfied one as completed. Don't split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" are the same step); only use parallel intents for 【parallel, mutually independent】 dimensions (e.g. enumerating multiple unrelated endpoints).

5. **Submit**: use 【one】 add_intent batch submission to commit the filtered new directions (intents array, at most 4 highest-value, don't make multiple calls one at a time):
   - **summary**: one natural-language sentence describing the direction (full address of the test target + what to do + why), don't apply a fixed category; deduplication mainly relies on comparing it with existing intents.
   - **asset_ids**: asset ids this direction will test/attack (pass as many as you can, 0/1/many, from list_assets) — as long as the direction is anchored to specific assets (site/endpoint/parameter/host), pass them; used for coverage dedup, linking into the asset chain, pass all when spanning multiple assets; leave empty only for purely global recon with no specific asset.
   - **parent_ids**: which upstream nodes this direction is synthesized from (optional, 0/1/many) — when multiple facts combine to produce one intent, pass them all; when derived from an upstream intent/finding, also pass its id; for top-level brand-new directions, leave empty.

Don't repeat, don't fabricate; but when the goal is unmet and there is an uncovered, deeper play, dispatch when appropriate. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + basic default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// When asset coverage is disabled, drop add_task_scope/list_untested_assets (not surfaced in the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// Critical situation (just-finished intents + prefetched full graph) moved to 【this round's user input】 (see input below); system
	// keeps only the static planner body. Moving out keeps system stable per round, which is friendlier to caching; the cost is
	// that if a single round grows long, the situation may be compressed by compaction (planner rounds are usually short, risk is low).
	// situational is concatenated into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final-stretch mode (injected via ctx, see taskclock.go). On the final round, the task-timeout
	// planner wrap-up phrasing is concatenated as 【this round's operational directive】 into the user input (along with situational),
	// letting it only do the final goal judgment and not produce new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【Task final wrap-up (this round's special directive, overriding the regular planning flow above)】: " + resolveTaskTimeoutWrapup("planner")
	}
	// The task's working directory <workDir>/tasks/<taskID> — create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // operation constraints (if any) are injected into the system prompt, framing the exploration boundary
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own; when there is a deadline, clamp MaxDuration to the remaining time
	// so in-progress planning rounds enter wrap-up at the task deadline (timeout → task-timeout phrase, step count → per-run phrase).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // route through the recording proxy to leave a trail; load the proxy CA to verify MITM-resigned HTTPS certs
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is an independent egress proxy (http/https/socks5), unrelated to the traffic-recording MITM proxy; empty = direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash sub-commands default to the proxy + trust CA
		WorkingDir:            taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = no limit; with a deadline = time remaining to the deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todos shared across wake-ups: let serial chains survive across rounds (session is new, store is not).
		Todos: p.todoFor(ts.ID()),
		// On hitting this round's step budget → SDK runs wrap-up: land whatever conclusion this round has reached
		// (the add_intent to dispatch, prove_goal that can be made, serial chain recorded via TodoWrite), instead of stopping
		// the planning — the planner is still woken repeatedly afterward. When clamped (by task deadline), use PromptByReason
		// (see wrapupSettlementForTask) instead.
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // when this profile selects non-streaming, go through Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = don't send a cap, let the server default decide
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental feature: when enabled, noa takes over context compression (archives are persisted under
	// <workDir>/noa/<SessionID>).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The situation (just-finished intents + full graph) is now concatenated into this round's user input (see input below).
	// The user also contains instructions + cross-wake-up todos (the todos are the model's own planning notes, regenerable, so they go in user).
	// The opening is split by "whether this round has concrete changes": changes present → point to the 【actual changes】 block below;
	// no changes (heartbeat periodic check / hint / recovery, etc.) → don't claim "the graph has changed", instead hint to review running intents.
	lead := "There were concrete changes (see 【The actual changes that fired this round】 below); plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This round is a **periodic check (heartbeat fired) / no concrete change signal** wake-up — the graph may not have new changes. In passing, review running intents: those with no progress for a long time or that have gone off course should be course-corrected with steer_work, and those whose direction is completely wrong should be cut with kill_work; then judge the goal and decide whether to add directions:"
		// On heartbeat/no-change wake-up, if the entire graph has no open or running intent → exploration has stalled
		// (no worker running, no direction queued). Clearly inform the planner and force it to produce a new direction this round,
		// don't just review running intents and idle.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a **periodic check (heartbeat fired)** wake-up, and currently **there is no open or running intent at all** — no worker is running, no direction is queued, exploration has stalled. You **MUST** produce one or more new intents this round that advance the goal and are **non-duplicate** with intents already in the graph (must not produce 0 intents); first judge whether the goal is met from the situation below, and if not, immediately add a direction:"
		}
	}
	input := lead + situational + "\n\nBased on the situation above, judge the goal. When the goal is 【truly achieved】 (the goal's deliverable has been captured / the target vulnerability has been confirmed), use prove_goal to mark them one by one. **Hard floor: as long as the goal is unmet and there is no open or running intent right now (frontier_open=0 and running_intents is empty), this round MUST produce at least one intent that advances the goal — there is no running work to wait for, and no direction queued; producing 0 intents = task stall. Only when there are already open/running intents advancing, or the goal is met, may this round not produce new intents.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts running tools in-place at the wall-clock deadline and enters wrap-up on the live ctx,
	// so single-round deadlocks no longer bypass wrap-up; no external hard ctx fallback needed. ctx only carries pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
