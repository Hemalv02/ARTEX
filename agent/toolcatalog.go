package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns "built-in tools" from pure code into an enumerable catalog that the DB can override:
//   - BuiltinToolSeeds(): expands the built-in tool sets of the three executing agents into seed records (key +
//     description + parameter schema + default-bound agent), for the server to idempotently seed into the tools table at boot.
//   - ToolResolve hook: at runtime, for each tools row in the DB, applies "filter by agent +
//     override description/schema + inject parameter defaults" to the assembled tools. key/handler still live in the code layer; the DB only changes "prose and defaults".
// The handler (Call behavior) always comes from code — the DB can't change it, only the description the model sees and the default inputs.

// ToolSeed is a seedable snapshot of one built-in tool: key is CoreTool.Name() (hard-bound to the
// handler, read-only in the UI), Desc/Schema come from the tool definition in code, and Agents is which agents the code gives it by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // top-level description (overridable in the UI)
	Schema map[string]any // parameter JSON-Schema (structure read-only; description/default editable in the UI)
	Agents []string       // default-bound agent keys (worker/planner/mainagent)
}

// builtinToolsByAgent uses a "read-only empty shell" ToolSet (nil stores) to construct each executing agent's
// domain tool set. Tool constructors only stuff a closure into the Spec and don't dereference the store at construction time, so nil is safe —
// these tools are only used here to read Name()/Description()/InputSchema(), never Call.
//
// Deliberately [excludes] the SDK's generic tools actool.DefaultTools() (Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash): every agent has them fixed, there's no "bound to whom" tradeoff, and their descriptions are mostly in Prompt()
// (this table only overrides Description(), which would cause misleading half-coverage). Not seeded → no DB row → ToolResolve
// passes them through as-is, no override, behavior unchanged. Only artex's own domain tools enter the table and are manageable.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (the goal decomposer) binds set_goals + set_constraints by default: it relies on them to write the
		// decomposed goals and extracted operational constraints into the DB. Shares the same managed tools as mainagent; the web UI can edit the description/schema and toggle per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto binds finding-reporting + asset-management tools by default; other domain tools can be toggled on in the UI as needed.
		// New DBs are written from this seed; old DBs are migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (the standalone pentest agent) binds by default: list assets / insert assets / report findings / list findings / list companies.
		// New DBs are written from this seed; old DBs are migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools enter the catalog as usual (visible in the web UI, manually toggleable per agent), but
// [bind to no agent] by default — ToolResolve drops an empty-bound tool for every agent, so it must be explicitly opted in.
// The reason they still remain in some agent's base tool set (e.g. goal_met in PlannerTools) is twofold: one, so the seed can
// construct it to get its desc/schema; two, so that after a user manually binds it back, base has it at runtime and ToolResolve keeps it.
//
// goal_met: bypasses proving each goal one by one and declares [the entire task complete] globally — high-weight with a risk of misjudgment, and redundant with
// "prove_goal marks the last goal → auto-finalize", so it's given to no agent by default; bind it manually when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds dedupes and merges each agent's built-in tool sets into a seed list: a tool with the same name (e.g. list_assets,
// which multiple agents have) merges into one entry with Agents as the union; tools in defaultUnbound are forced to an empty binding.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // enters the catalog, bindable manually, but given to no agent by default (store [] rather than null, consistent with other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// default inputs get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
