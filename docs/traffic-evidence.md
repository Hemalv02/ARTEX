# Multiple Traffic Evidence for Findings

The "Linked Traffic" section of a vulnerability detail page supports cross-page multi-selection, role and notes, ordering, and unbinding. The traffic page also supports multi-selecting records and binding them to an existing vulnerability in a single action. Vulnerability evidence inherited from a task is read-only; modifications must be made in the source task.

The "Agent auto-bind traffic" toggle in System Settings is disabled by default; it is read and modified through the `agent_traffic_binding` key of the `/api/settings` endpoint. Enabling it increases Token consumption from inspecting requests/responses, tool calls, and prompts; the new setting takes effect on the next Agent turn. When disabled, auto-bind parameters and rebinding tools are hidden, the auto-binding guidance is not injected, and any new auto-binding submissions from already-running sessions are rejected. Manual binding, traffic capture, reading of saved evidence, and exports are not affected.

When enabled, the default flow is "finding stored → report Agent auto-triggered → verify and bind traffic → write report against the latest evidence version". The reporter keeps the verification command, key output, existing real-traffic IDs, and their roles in `evidence`. The report Agent combines vulnerability details with execution history, verifies with `traffic_search` / `traffic_get`, calls `bind_finding_traffic`, and then reads the latest `version` before saving the report. When the toggle is off, the report Agent is not given additional raw-traffic search/read tools and does not auto-bind; it can still read manually bound snapshots and produce the report.

Backward compatibility for existing callers: the `traffic_refs` and `evidence_hint_id` parameters of `report_finding` can still be used for explicit immediate binding. Binding is optional: for non-HTTP vulnerabilities such as TCP, or when traffic has not been collected or no exact record can be found, omitting references still allows normal reporting and report writing. Keep command output, logs, and other verifiable evidence, and recommend documenting the reason for not binding without adding new required fields. All submitted IDs must be valid and the body text must be complete. Any single failure rolls back the entire binding operation; an explicit in-report binding failure rolls back the whole report submission. Repeatedly appending the same snapshot does not increase bindings or overwrite notes.

## Agent numbering and report version

`report_finding` gains new optional parameters; array order defines the initial evidence order:

```json
{
  "traffic_refs": [
    {"traffic_id": "real-traffic-id", "role": "baseline", "note": "Normal account request"},
    {"traffic_id": "another-real-traffic-id", "role": "proof", "note": "Reproduction request"}
  ]
}
```

Roles are `baseline` (normal control), `proof` (proof of concept), `verification` (additional verification), and `supporting` (auxiliary evidence, default). First use `traffic_search` / `traffic_get` to verify the real record; domain and time are only used for candidate filtering and do not imply task ownership.

The prompt reference is [CyberStrikeAI's vulnerability reporting tool guide](https://github.com/RuoJi6/CyberStrikeAI/blob/54d56774b8bd285817d16d48b70a4a5e6e0963f7/internal/app/vulnerability_tools.go), combined with this project's optional-binding convention, and does not introduce required-field validation for reasons without traffic. Do not guess IDs, and do not repeat probing merely to add traffic.

The first line of the return is still `finding recorded: <exploration node ID>`; the JSON that follows provides the standalone vulnerability record `finding_id`, the exploration node `finding_node_id`, and a binding summary.

- `get_finding_traffic(finding_id)` uses the **standalone vulnerability record ID** and returns the ordered list and `version`; pass `binding_id`, `side=request|response`, `offset`, and `length` to read in segments, with each segment capped at 8192 bytes.
- The `finding_id` of `update_finding_report` **continues to use the exploration node ID**. The new `evidence_version` carries the actually read version; if evidence changes during generation, writing the old version is rejected and the system must re-read and regenerate.
- When an old report call omits the version, do not claim to overwrite existing traffic evidence. After bindings, notes, roles, or order change, the existing report is flagged for update.

After auto-binding is enabled, `add_hint` / `add_task_hint` support saving `traffic_refs` on each element of either a single hint or a batch `hints` array. When the Planner reports on the user's behalf, it may pass `evidence_hint_id` to clearly select references from the hint belonging to the current task; inherited hints cannot be referenced. The system does not guess bindings from domain, time, or browsing history. Submission failures do not produce partial findings or trigger the report prematurely.

For existing findings that are missing bindings, use `bind_finding_traffic(finding_id, traffic_refs)` to backfill without re-registering. `list_findings` / `list_task_findings` / `node_detail` / `get_task_node_detail` return distinct `finding_id` and `finding_node_id`; the old `id` retains its exploration-node semantics.

At startup, only optional properties are added to the old tool schema; the original default traffic-tool binding extensions are passed to the report Agent; the rebinding tool is given to the report Agent by default. Custom binding lists, prompts, descriptions, and enablement state are preserved. The guidance is injected uniformly after final tool assembly: the reporting role is responsible for handing over existing evidence, and the report Agent is responsible for verifying, binding, and writing the report. When a platform chat lacks task context, it should hand off to the task Agent via a structured prompt rather than reporting directly. Before judging a task as complete, existing evidence should first be handed over; absence of traffic does not require waiting. A failed `report_finding` does not trigger the report Agent.

## API

Base path: `/api/exploration/findings/{finding_id}/traffic`, using the standalone vulnerability ID. Existing authentication applies; `context_task` validates task visibility and inherited read-only access.

| Method / relative path | Request / response |
| --- | --- |
| `GET` | Ordered summary, evidence version, version used by the report |
| `POST` | `{"traffic_refs":[...]}` append the entire batch |
| `PATCH /{binding_id}` | `{"version":1,"role":"proof","note":"notes"}` |
| `DELETE /{binding_id}` | `{"version":1}` |
| `PUT /order` | `{"version":1,"binding_ids":["2","1"]}`, must be the complete list |
| `GET /{binding_id}` | Snapshot metadata and bounded body preview |
| `GET /{binding_id}/body` | `side`, `offset`, `length`; `download=1` downloads the full raw bytes |

Version/order-set conflicts and writes during archiving return `409`; inherited writes return `403`; missing bindings or bindings that do not belong to a vulnerability return `404`; traffic/attachment reads and verification failures return explicit errors.

## Storage and migration

Run an idempotent PostgreSQL migration at startup: add `traffic_evidence_snapshots`, `finding_traffic_bindings`, and `findings.evidence_version` / `report_evidence_version` (default 0). Do not infer backfills from historical text.

Snapshots preserve the original traffic ID, capture time, URL, method, status, request/response headers, body length, and SHA-256. Bodies are stored by hash under `<data>/evidence/blobs/<first-two>/<hash>.bin`, independent of the clearable `data/traffic`, and may be shared by multiple findings. Snapshots do not provide a content-update interface; reads and exports fail on hash mismatch.

Read the full body under the original traffic write lock, including large body blobs and legacy directory records. First persist and verify the file, then write the exploration node, intent relations, findings, snapshots, and bindings in a single PostgreSQL transaction; the planner is notified only after commit. Failures may leave unreferenced files but do not produce partial business records.

PostgreSQL advisory lock `7337741004` coordinates evidence files and SQL references; the task row lock forbids evidence modification once archiving is queued. Restore keeps the evidence lock from body install to metadata commit. Deleting a finding cascades to remove its bindings.

The cleaner runs hourly, reclaiming only unreferenced content that is not part of an in-progress operation, with a minimum delay of 24 hours. Normal traffic cleanup does not touch the evidence directory. Back up PostgreSQL and `data/evidence` together when backing up hot data.

## Export and archiving

Markdown includes the ordered evidence list and version; JSON includes metadata; CSV adds quantity and binding IDs. `md-zip` keeps the vulnerability Markdown and provides:

```text
evidence/<finding_id>/<binding_id>/
  manifest.json
  request.http
  response.http
  request.bin
  response.bin
```

Markdown references message files through relative links. Before sending the download, complete attachment copying, hash verification, compression, disk sync, and CRC read-verification of every ZIP entry; missing or corrupted entries cause the entire download to fail. Complete attachments preserve the binary original bytes.

Archive v3 collects snapshots and bodies by vulnerability-binding relationships, independent of the original traffic or domain. Hot data is only cleaned after package verification completes; shared evidence is preserved. Restore first verifies installed bodies, then recovers metadata and bindings in a transaction, supporting failed retries. v1/v2 remain restorable, and missing new fields are explicitly filled with 0.

## Validation and boundaries

Configure an independent, freshly created PostgreSQL test database for each test package via `ARTEX_PG_DSN` to prevent residual task/model fixtures from triggering background runs. Run the full tests for the relevant package and confirm that no configuration omissions cause skips:

```sh
# Set ARTEX_PG_DSN to the corresponding independent test database before each package run; explicit configuration failures must error.
go test ./<package> -count=1
go test -race -p 1 ./evidence ./db ./agent ./server -run 'TestEvidence|TestFindingTraffic|TestFindingEvidence|TestReportFindingAtomicContract|TestTaskArchive'
```

Front-end validation includes `npx tsc --noEmit`, Biome checks on affected files, Webpack build, and `NEXT_EXPORT=1` static export. Use an independent cache directory to avoid overwriting any running dev server.

Local end-to-end acceptance uses independent ports, controlled HTTP / domain HTTPS targets, and a temporary data directory, covering both binding entry points, cross-page selection, ordering/notes, error messages, inherited read-only, download, and after deleting the original traffic: export, archive, hot-body reclaim, restore, and hash verification.

The first version uses a global evidence coordination lock; during large-volume binding/export or slow attachment downloads, other evidence operations may wait. Evidence must not be fabricated when capture records or full bodies are missing. This feature does not change the capture toggle, nor does it handle certificate issues from HTTPS direct IP access.
