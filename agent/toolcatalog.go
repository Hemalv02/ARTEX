package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns the "built-in tools" from pure code into an enumerable, DB-
// overridable catalog:
//
//   - BuiltinToolSeeds(): expands each of the three execution agents' built-in
//     tool sets into seed records (key + description + parameter schema + default
//     agent bindings), for the server to idempotently seed into the tools table
//     on startup.
//   - ToolResolve hook: at runtime, applies the DB tools table's per-agent
//     filtering + description/schema overrides + default value injection to the
//     already-assembled tool set. The key/handler still live in code; the DB can
//     only change "prose and defaults". The handler (Call behavior) always comes
//     from code — the DB cannot change it, only what description/defaults the
//     model sees.

// ToolSeed is a seedable snapshot of a built-in tool: Key equals CoreTool.Name()
// (hard-bound to the handler; UI is read-only); Desc/Schema come from the code-
// side tool definition; Agents is the code default for which agent the tool
// belongs to.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // top-level description (overridable in UI)
	Schema map[string]any // parameter JSON-Schema (structure is read-only; description/default editable in UI)
	Agents []string       // default-bound agent keys (worker/planner/mainagent)
}

// builtinToolsByAgent builds each execution agent's domain tool set from a
// "read-only shell" ToolSet (nil stores). Tool constructors only put closures
// into Spec and don't dereference the store at construction time, so nil is
// safe — these tools are only used here to read Name()/Description()/
// InputSchema() and are never Call()ed here.
//
// Deliberately 【excludes】 the SDK general tools actool.DefaultTools() (Read/
// Write/Edit/MultiEdit/LS/Glob/Grep/Bash): they are fixed per agent, with no
// "who to bind to" tradeoff, and most of their description lives in Prompt()
// (this catalog only covers Description(), which would cause a half-overridden
// misdirection). No seed → no DB row → ToolResolve passes them through
// unchanged, matching prior behavior. Only artex's own domain tools enter
// the catalog.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (goal decomposer) defaults to set_goals + set_constraints: it uses
		// them to write the decomposed goals and extracted operation constraints
		// into the store. It shares the same managed tools with mainagent; the
		// web UI can edit description/schema and toggle bindings per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto defaults to the finding-reporting and asset-management tools; other
		// domain tools can be toggled in the UI as needed. New stores are seeded
		// by this; old stores are migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (standalone pentest agent) defaults to: list assets / insert
		// assets / report finding / list findings / list companies. New stores
		// are seeded by this; old stores are migrated by
		// seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools are still listed in the catalog (visible
// in the web UI, manually togglable per agent), but 【are not bound to any
// agent by default】 — ToolResolve drops tools with empty bindings for every
// agent, so they must be explicitly opted in. The reason they still live in
// some agent's base tool set (e.g. goal_met in PlannerTools): (1) so the seed
// can construct them to read desc/schema, and (2) so a user who manually
// rebinds it has it in the base set at runtime, and ToolResolve can keep it.
//
// goal_met: bypasses per-goal prove_goal and declares the whole task done
// globally. High-weight with misjudgment risk, and overlaps with
// "prove_goal marks the last goal → auto wrap-up", so it is unbound by
// default and must be manually bound when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds deduplicates and merges the per-agent built-in tool sets
// into a seed list: tools with the same name (e.g. list_assets, present in
// multiple agents) collapse into one entry with the union of Agents; tools in
// defaultUnbound are force-bound to empty.
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
			agents = []string{} // Listed in the catalog, manually bindable, but unbound by default (store [] instead of null, for consistency with other tools)
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
// has default parameter values injected. Tools with no matching DB row
// (MCP/skill/host tools like traffic) pass through untouched. nil = tools
// unchanged. Wired in server/assembly.go.
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
// null. Structure (names/types/required) is untouched — only default values
// are merged in.
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
