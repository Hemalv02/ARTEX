package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Finding ID convention**: finding_id is the independent finding-record ID; finding_node_id is the exploration-node ID. The id returned by list_findings / list_task_findings / node_detail / get_task_node_detail is the exploration-node ID — read the independent id from the same response's finding_id. get_finding_traffic / bind_finding_traffic take the independent finding_id. The legacy update_finding_report still takes the finding_node_id in its finding_id parameter. Do not use the number from the first line of report_finding output for evidence tools, and do not guess other numbers when an id error occurs."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nBy default the Reporter Agent verifies and binds traffic before writing the report. The reporter keeps the verification commands, key outputs, any verified real traffic IDs and their purposes in evidence, for the Reporter Agent to corroborate against execution records; no need to fetch extra packets just to bind. Explicit immediate binding remains supported: traffic_refs or evidence_hint_id can submit already-verified references; the latter reads the structured references from the task-specified hint; if any is invalid the entire submission fails. TCP / no-packet cases do not need these optional parameters. The returned finding_id and finding_node_id represent the independent record and the exploration node, respectively."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing over a confirmed finding, keep the verified traffic ID, purpose, description, and order in the corresponding hint's traffic_refs (single one at top level, batched ones inside the matching hints element), and explain in text which vulnerability it proves. The caller may not hand over only text and drop the existing traffic references. Unverified candidates may not be passed as evidence."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic evidence handover (optional)**: automatic binding is performed by the Reporter Agent by default, after the finding is persisted and before the report is written. The reporter should keep the verification commands, key outputs, the verified real traffic IDs and their purposes in evidence, and include intent_id in the task so the Reporter Agent can trace it back; no need to fetch extra packets just to bind. When the Auto / Planner agent reports on behalf of the executor, do not drop the executor's existing references. add_hint / add_task_hint can use traffic_refs for handover; explicit immediate binding via report_finding's traffic_refs / evidence_hint_id is still supported. For TCP or no-packet cases, register normally; do not guess IDs, and do not re-probe just to fill in packets."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen the platform chat has no task context, do not call report_finding directly; instead hand over to the existing task via add_task_hint, let the task Agent register it, and use list_task_findings to verify the result."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore declaring a goal met, finish reporting/handing over all existing evidence for this round. Do not end the task or cancel Workers just because a written finding has been recorded, while evidence handover is still incomplete; a no-packet case does not require waiting or forcing a capture."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**Automatic traffic binding before the report (enabled)**: you are responsible for verifying and binding traffic for the finding triggered this round, then writing the report. First obtain the explicit finding_id and finding_node_id from the report_finding return JSON or from get_task_node_detail / list_task_findings. Read the finding details, the corresponding intent's execution record, and the existing evidence list, preferring the verified real IDs handed over by the reporter. If this verification is HTTP and traffic tools are available, use traffic_search to filter candidates, then use traffic_get to verify one by one that each request/response actually supports the finding; domain and time are only used for filtering and do not prove attribution. Associate the confirmed evidence with bind_finding_traffic(finding_id, traffic_refs) in reproduction order, choosing baseline / proof / verification / supporting and describing the purpose. Only act on this round's finding; do not re-create findings or re-probe the target. After successful binding, re-call get_finding_traffic to get the latest version, read the body you need, then pass the version you actually read as evidence_version to update_finding_report (its finding_id parameter still uses finding_node_id). Existing bindings need not be re-appended. For TCP, uncaptured, unavailable tools, or no exact match, skip automatic binding and write the report normally based on text/command evidence while explaining why; do not guess just to fill the traffic. Binding failure does not claim success; keep the existing evidence and explain in the report why binding was skipped."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references corresponding to specific findings in this hint, kept in order; after handover, report_finding can carry these via evidence_hint_id.", "items": obj(map[string]any{"traffic_id": str("Real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("What conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must be a hint node in this task (inherited hints cannot be used directly for binding)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
