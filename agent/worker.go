package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// Note that the deepseek backend differs from the other three in nature: DeepSeek
// has no directly-callable search endpoint; search only exists within its
// Anthropic-compatible messages interface (web_search_20250305 server tool), so
// every search consumes one model call and the search request is sent by
// DeepSeek's server — it does NOT go through the local Proxy, nor does it leave
// a traffic trace.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* comes from the currently-active LLM config (only Anthropic-format
	// DeepSeek official endpoint), not separately configured; it changes with the
	// LLM config.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "You are about to be terminated for budget exhaustion. Do NOT run any more commands or probes. Do the following in sequence: (1) write back everything you have identified above but not yet persisted — use insert_assets for new assets, record_fact for exploration conclusions/facts, report_finding for confirmed vulnerabilities; (2) **finally, end with a single sentence of plain text** summarizing what you did and the key conclusions (it is displayed as this run's result; you must output it)."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (section [A]) of the worker
// system prompt, seeded into agent_prompts. The trafficTool block and the
// artifact output spec are NOT here — they are code-owned and appended by
// workerSystem after rendering (sections [B]/[C]), so editing the DB body can
// never drop them.
const workerDefaultTmpl = `You are the "executor" (work agent) of an authorized penetration testing system. You are assigned **one intent** (a one-line exploration direction). Your only job: **complete this one intent, write back what you found to the knowledge graph, then stop and return.**

**Boundaries (red lines):**
1. **Only do the one intent you were assigned.** If while exploring this intent you glimpse a lead worth digging into that lies outside it (an error message that leaks a path, a point that could chain with another asset, an entry point that hints at another exploit chain), **mention it in one line in the fact's summary and hand it to the planner.**
2. A first failure (payload filtered / 404 / injection no reflection) does NOT mean you have exhausted the direction — try every bypass for this intent's vector before reporting your conclusion;
3. Operate only within the authorized scope. If the system prompt top has an **Operation constraints** section, that is the highest-priority red line: before every command/probe, self-check; do not violate it (even if it would fall within your assigned intent).

**Write back as you go** (what is in the graph counts; what is in your head or text doesn't — persist every result immediately, do not batch until your step budget runs out and you lose it). Three write-backs — don't confuse them:
- **New asset/resource → insert_assets (asset graph)**: subdomains / service / endpoint / fingerprint / credentials, anything that IS an asset. **This only registers the asset itself; exploration conclusions/judgments do NOT go here — use record_fact.**
- **Exploration conclusion/fact → record_fact (exploration graph; pass intent_id)**: always use this for them. **Roll multiple observations into ONE fact** (summary one summary sentence + detail extending the summary, grounded in the real execution process); do NOT split one attribute per fact — one intent usually produces one fact; splitting too finely makes the graph balloon — **default to one; merge what can go into detail**; only use the facts array when you truly have **mutually independent conclusions that cannot be merged** — that is the rare exception, not the rule. **Incremental only**: record only what is **newly obtained** this time; do not re-record existing facts with different wording (just confirming an existing fact with no new info → don't record). **Only what you actually saw**: provide evidence (one line: command + the one or two most-proving output lines; be concise, details go in detail), set confidence (observed=directly seen / inferred=deduced from the observation).
- **Confirmed vulnerability → report_finding (exploration graph; include PoC; pass intent_id)**: **Only use it when you have actually triggered it during this run and obtained reproducible evidence (request/response or command output)**. Strictly forbidden: "version/fingerprint matches a CVE", "parameter looks injectable", "external vulnerability database / changelog / code diff inferred" — do not file these as confirmed. Do not use CVE database lookups or patch-version comparisons as a substitute for actual triggering. If you can't trigger it but suspect something → use record_fact to write an inferred fact (the suspicion + why it wasn't triggered) and hand it to the planner; do not force it into a finding.

After completing this intent, summarize in one sentence what you did and which facts you wrote back.`

// workerTrafficBlock is section [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**Traffic tools**:\n- traffic_search / traffic_get / traffic_blob: look back at responses, find already-visited resources — **check traffic first, do NOT re-curl the same URL**. traffic_search **must specify host**; the default returns just 3 lightweight index rows (id/method/url/status/resp_len, no response content); pass a larger limit when you need more; use body_contains to full-text-search the request/response body (at least 3 characters; substring and Chinese both supported, e.g. passwords / keys / error messages / internal addresses); to see the full content of a record use traffic_get(id), where large bodies show as @blob sha256:<hash> — use traffic_blob(hash) to retrieve the full content segment by segment."
}

// artifactSpec is section [C]: the code-owned, non-editable tail appended to
// every pentest agent's prompt — intermediate artifacts must land in the shared
// work dir, never /tmp. Guaranteed present regardless of how the DB body is
// edited.
func artifactSpec(dir string) string {
	return "\n\n**Intermediate artifact spec**: scripts, payloads, captured response bodies, temporary data — everything intermediate — **must be written to this task's working directory " + dir + "** (use a relative path here, or that absolute path) — **do NOT write to /tmp or use any other absolute path**."
}

// workerArtifactSpec is the worker's section [C]: its per-intent run dir is
// pre-created by the engine (ensureRunDir), so it just writes relative paths
// there — no manual mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**Intermediate artifact spec**: scripts, payloads, captured response bodies, temporary data — everything intermediate — **must be written to this intent's dedicated working directory " + runDir + "** (already auto-created; just write with relative paths here, no manual mkdir needed) — **do NOT write to /tmp or use any other absolute path**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n[Your assigned intent (this run's only task: do this one, produce facts only, stop when done)]:\n%s\nIntent id: %d (pass it when writing back with record_fact / report_finding)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the
// worker's launch USER message for AWARENESS ONLY. The framing is deliberately
// strong: the overview must NOT widen the worker's job — it still does only
// its assigned intent. Its sole purpose is letting the worker read context
// (existing facts/assets/hints) so it avoids redundant work and doesn't
// re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// Coverage is a signal for the planner ("which kinds are under-tested / do
	// we need to widen scope"), which conflicts with the worker's job of
	// "do only your assigned intent, don't chase uncovered points" — so drop
	// it from the worker view. data is this worker's own fresh map; deleting
	// keys does not affect the planner.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n[Global exploration overview (read-only; places your intent in the big picture)]:\n" +
		"Below is the current exploration snapshot of the whole task. Two purposes: first, know what others have already discovered so you don't duplicate; second, when exploring your own intent, see how it relates to the global picture.\n" +
		"**Divergent thinking is welcome**: while exploring your intent, think deeply and associate freely. The only boundary is — do NOT execute other intents (that's other workers' job, scheduled by the planner). Whenever you associate a valuable lead (cross-asset chaining, an entry that hints at another exploit chain, a globally suspicious point), **you MUST write it into a fact for the planner** — this is an important deliverable, not optional. Better to over-report one and let the planner decide than to swallow it.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding lands → wake the planner with which intent + finding
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// The worker deliberately does NOT get MultiEdit/Glob/Grep: precise file
	// edits use Edit, searching goes through Bash (grep/find) — narrow the tool
	// surface, reduce low-value calls. The rest of the SDK defaults
	// (Read/Write/Edit/LS/Bash/Sleep) stay.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// The intent is the worker's **only responsibility, an invariant throughout
	// the whole run** — so the intent, the launch command, and the raw data of
	// the assets the intent anchors on all go in the system prompt: the system
	// is reassembled each run and can never be dropped by compaction, so the
	// intent is always present even on long runs and resumptions do not depend
	// on whether the transcript kept that first message. The cost is that the
	// system mixes per-intent volatile data and loses cross-intent cache
	// reuse — a deliberate trade-off (losing the intent is far worse than
	// saving a few tokens). This is intentionally different from the planner's
	// "situational block goes in the user turn" split: the planner has no
	// single mandate (it generates intents), the worker does. Only the **global
	// situational overview** stays in the launch user message — it is
	// downgradable, tolerant of staleness, and dropping it is fine.
	// The intent's dedicated working directory <workDir>/tasks/<taskID>/i<intentID>;
	// the engine creates it up front.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // operation constraints (if any) injected into the system prompt; the worker strictly obeys them
	}
	// Intent block → intent-anchored assets block → launch command — append in
	// order to the system tail (same append pattern as constraintBlock).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\nTarget assets corresponding to this intent's asset_ids:\n" + string(b)
				}
				// Assets explicitly targeted by this intent → automatically pulled
				// into the task's test scope (same conservative granularity as
				// insertAssets). upsertTaskScope's ON CONFLICT DO NOTHING + the
				// uq_task_scope unique index guarantee no duplicates; reruns/retries
				// are likewise idempotent no-ops.
				// When asset coverage is disabled, stop accumulating test scope
				// (the denominator).
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\nBegin executing the intent above: do only it, produce only facts/assets/findings, stop when done."
	system, boundary := deferredSystem(sysBody, def)
	// Task-level deadline (injected via ctx) bounds this run's wall-clock budget
	// + decides the wrap-up prompt (see taskclock.go).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch routes through the recording proxy; its HTTP, like curl, is
		// recorded. Loading the proxy CA lets the MITM-resigned HTTPS cert
		// verify normally (instead of disabling verification). Empty proxy = direct.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey;
		// tavily needs TavilyKey. WebSearchProxy is a separate egress proxy
		// (http/https/socks5), independent of the traffic-recording MITM
		// proxy; empty = direct.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash subprocesses' HTTP defaults to the recording proxy + trusts its
		// CA (tools need no -x/-k).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// Wall-clock budget, evaluated at turn boundary, does not interrupt
		// mid-action; 0 = unlimited. With a task-level deadline, clamp to
		// min(self-budget, time remaining until deadline) so this run naturally
		// enters wrap-up when the task time is up (see taskclock.go).
		MaxDuration: maxDur,
		// Hit the budget (turns OR duration) → SDK runs a wrap-up turn (Bash
		// hidden), persists what was identified, avoids leaving a half-cut run.
		// When clamped (by task deadline), use PromptByReason: timeout = task
		// time-up → task-timeout prompt; steps = step budget exhausted within the
		// clamped window → fall back to per-run prompt. Unclamped keeps pure per-run.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK
		// tool.Capture); full output preserved on disk. Truncation cap uses
		// SDK default (30000 chars).
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // session-scoped ad-hoc todos (TodoWrite); planning only, discarded on exit
		NonStreaming:  w.nonStreaming(),                       // Use Provider.Complete when the profile selects non-streaming
		MaxTokens:     w.maxTokens(),                          // 0 = no cap sent, server default decides
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// Intent / launch command / intent-anchored assets are already shipped via
	// the system prompt (see sysBody assembly above). This launch user message
	// carries only the **global situational overview** — downgradable
	// big-picture awareness; dropping it is fine. In the rare case the overview
	// marshals to nothing, fall back to a one-line launch phrase so the first
	// turn does not have an empty user message.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "Begin executing the intent you were assigned in system: do only it, produce only facts/assets/findings, stop when done."
	}

	// Experimental: when enabled, noa takes over context compression (archives
	// concentrated under <workDir>/noa/<SessionID>, persistent).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "Continue execution."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "Continue executing the new intent entered in the previous human chat. Do not redo completed actions."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n[New intent from human chat]\n" + message +
				"\n\nExecute this human input immediately; once done, decide from context whether the original task needs to continue."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n[New intent from human chat]\n" + message +
				"\n\nPrioritize this human input."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}