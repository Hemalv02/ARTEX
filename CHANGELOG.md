# Changelog

All notable changes to this project are documented in this file, in the format described by [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.3.15] - 2026-10-07

### Intercept

#### Added

- **New built-in intercept rule "Destructive API paths"**: Previously, the built-in HTTP destructive rule only matched the DELETE **method**, but most apps trigger delete endpoints via GET/POST — calls like `curl 'http://t/api/user/delete?id=1'` would actually delete data. A new `deny` rule now covers `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, `/delta` are not mistakenly blocked). Existing instances pick this up after upgrade; it can be disabled or removed under "System → Command intercept".
- **Model-fallback approval token usage is now shown separately**: Approval calls previously had no independent metering — you could only guess how much they cost. It is now metered on its own. Under the "Model-fallback approval" toggle in "System → Command intercept", a new usage card shows cumulative stats for call count, input, output, cache reads/writes, plus a mini bar chart of daily usage for the last 30 days.

### Vulnerability push

#### Added

- **Vulnerability findings can be pushed to IM platforms**: A new "System → Notifications" page lets you push scan findings to **DingTalk, Feishu, WeCom, generic Webhook, Telegram, Email** — six channels. Any number of instances can be configured per type; each instance independently sets enable/disable, rate limit, and filter rules.
- **Two push modes: real-time and digest**: Real-time sends one message per hit; digest merges them on a global schedule (default 30 minutes) with the new count and severity distribution in the header. To get "critical in real-time, others in digest", create two channels and configure each.
- **Filter rules support four dimensions**: Minimum severity, scoped to a specific task/asset range, vulnerability type keyword include/exclude (exclude wins), and whether to receive disposition state changes (off by default). Messages include a "View details" button, with the target address configured globally via "Callback URL"; left empty removes the button.
- **Delivery history and manual resend**: Lists the status, attempt count, failure reason, and channel of each delivery, filterable by channel and status. Failed entries can be resent with one click (resend resets the retry counter).
- **Channel credentials are shown masked**: Webhook URLs, signing secrets, Bot Tokens, SMTP passwords etc. are only echoed back as masked values with a tail hint. Submitting them unchanged means "do not modify"; clearing the field removes it.

#### Fixed

> This section is the conclusion of a security audit after the initial version landed — every item is reproduced, then fixed, then covered by a regression test.

- **Masking can be bypassed by "change the target URL, keep the credentials" (critical)**: The config merge kept the stored value for any "unmentioned" key, so changing only the URL and leaving credentials untouched let the server send the real credentials to an arbitrary address, completely silently. The target URL is now required to trigger explicit handling of every credential field — provide a new value, or explicitly clear it to mark as no longer needed.
- **Markdown-based channels do not escape untrusted content**: Vulnerability titles and summaries come from model output, asset URLs from scan results; crafted titles render as clickable external links in DingTalk / WeCom / Feishu, and image syntax can be fetched by the client during render, leaking the reader's IP. All such content is now flattened to one line with markdown metacharacters escaped, with escaping applied at each channel's render output rather than the shared title function.
- **SSRF surface on delivery URLs**: Previously, loopback, intranet, and cloud metadata addresses were all deliverable, and the first 200 bytes of failed response bodies went into `last_error` and were echoed in the delivery history — a semi-blind read primitive. Protection is now applied at the **dial stage**, covering DNS rebinding and rejecting cross-host redirects. Loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`; RFC1918 private addresses remain allowed (on-prem SMTP relays are common).
- **Push credentials leaked via error messages**: DingTalk's `access_token`, WeCom's `key`, Telegram's bot token are all in the URL, and on delivery failure the full URL was leaked into `last_error`, delivery history, server logs, and UI toasts in four places. They are now uniformly masked, keeping only `scheme://host` and the underlying reason.
- **Truncated digest batches mark the whole batch as delivered, causing silent loss**: Truncated vulnerabilities are neither in the message nor in the failure list, yet the delivery history shows success. Delivery is now packaged per **whole** batch: only the items actually packed in the message are marked, the rest go back to the queue for the next message without consuming a retry; the message header also states how many more will continue in the next message.
- **Misspelled minimum severity silently disables the filter**: When `min_severity` is misspelled (e.g. `hgih`), the unknown severity sorts as 0, the check degenerates to always-true, and the user thinks they only receive high-severity findings while everything is being dumped into the group. Values are now validated on write and the allowed values are listed.
- **`rate_per_min = 0` (no rate limit) was unreachable**: The write path silently changed an explicit 0 to the default value, so the operator thought rate limiting was disabled while it was still in effect. The default is now applied at the API layer when the field is missing.
- **Digest channels completely bypass the token bucket**: `rate_per_min` previously had no effect on digest mode. The number of items claimed is now bounded by both this round's quota and an in-memory upper bound.
- **Failure disposition was decided by the batch's max attempt count, dragging old deliveries into new ones**: An old delivery that had already retried twice would drag a brand-new delivery in the same batch into failed, so a new vulnerability would be permanently lost without ever using a retry. The decision is now made per item.
- **Deliveries with un-parseable snapshots in digest batches were silently marked successful**: Such items were skipped at the render stage but counted as successful with the whole batch. They are now explicitly marked as failed with a reason.
- **Per-channel per-round delivery count could exceed the lease duration**: In multi-instance deployments, rows with expired leases were re-claimed and resent, doubling the attempt counter. The per-round cap is now derived from "lease / single-attempt timeout", with assertions pinning down the relationship between these constants.
- **Telegram truncation could cut through HTML entities**: Truncation only avoided half-cut tags, not entity fragments like `&amp`, and the parser may reject the **entire** message because of them. Both are now avoided.
- **Email channel treated temporary SMTP failures as permanent**: Greylist `450`-class 4xx are temporary rejections that should be retried later; they were previously treated as fatal, so servers with greylisting would fail every push on the first attempt. Failures are now distinguished by the first digit of the response code: 4xx is retryable, 5xx is permanent.
- **Submitting nested structures actually wrote mask literals to the database**: Object fields like `webhook.headers` can only be masked or submitted as a whole; placing a mask sentinel inside the object would be stored as a real value, silently breaking auth. Such submissions are now explicitly rejected.

#### Design notes

- **Writing a vulnerability is a single blind INSERT**: `notification_events` and the vulnerability are written in the same transaction, so commit guarantees both are consistent. The INSERT deliberately does not read the channel table, does not run the user's filter rules, and is wrapped in a `SAVEPOINT` — otherwise one misconfigured filter could abort the transaction and prevent a high-severity vulnerability from being stored.
- **Delivery uses a lease, not a long transaction**: After claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as a lease; the database lock is not held during delivery. Rows left behind by a crashed process are reclaimed after the lease expires — self-healing and no infinite retry.
- **Rate limiting does not consume the retry budget**: The per-round quota is first computed from the token bucket, and then that many rows are claimed. Reversing the order would mean rate-limited deliveries burn one attempt for nothing, and after three attempts' worth of waiting, fall into failed.

### Test infrastructure

#### Fixed

- **Company ICP ownership test cleanup order caused assets to be permanently retained**: `defer d.Close()` ran first when the function returned, with `t.Cleanup` after, so cleanup statements all ran on already-closed connections and errors were discarded, leaving test assets and the company permanently in the database and causing other tests asserting on asset counts to mysteriously fail. Closing the connection now also goes through `t.Cleanup` and is registered first, and cleanup failures are surfaced.

### Traffic

#### Added

- **Traffic list now supports content search, path/status-code/length filters, and column sorting**: The filter row supports filtering by **response content** (via full-text index), **path**, **status code** (exact or bucket like `2xx`), and **response length range**; the header can be sorted by time, status code, or response length, with sort preferences persisted locally. Two new indexes are added and applied automatically to existing instances after upgrade. The Agent tool query contract is unchanged.
- **New "Clear all" on the traffic list**: Deletes all traffic records at once (ignoring current filters) and cleans up historical host directories that the index no longer records. After clearing, a full compaction is performed with a prompt showing how much space was actually freed. Traffic evidence bound to vulnerabilities lives in a separate evidence store and is not affected.

#### Fixed

- **Deleting traffic did not free disk space**: The index database was created without `auto_vacuum`, so the file never shrinks; the full-text index is also `contentless_delete` — `DELETE` only writes tombstones without reclaiming the original postings, so deleting traffic actually made the index larger (real-world: 6MB body captured → 16MB index, still 16MB after deletion). New index databases now enable `auto_vacuum=incremental`; after each delete, full-text index merge and incremental reclaim are performed in the background in chunks, gradually returning space to the system (same scenario drops to 104KB); reclaim releases the write lock between chunks and does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set at database creation, so **existing instance index databases are still in the old mode** — incremental reclaim is a no-op on them (a hint is printed at startup). For these databases, tombstone growth has been stopped, and the already-occupied volume can be reclaimed once via "Clear all" — that step will switch the database to incremental reclaim mode, after which daily deletes take effect on their own.

### LLM

#### Fixed

- **Custom session header was not effective on the "one-shot LLM call" path**: Goal decomposition (turn 0) and cold-node compaction are one-shot calls that don't attach a transcript store, so the context has no session id and the gateway cannot read this header. For endpoints like opencode zen that return 400 without it, the symptom is "turn 0 fails, subsequent planner turns are completely fine". A stable session id per exploration is now explicitly attached to both paths, and token usage is correctly attributed to the corresponding exploration (previously not recorded at all).

### Tools

#### Fixed

- **Output truncation was not effective for MCP and custom tools, large results flooded the context**: MCP tool returns were not truncated at all; HTTP-style custom tools passed an empty tool context, always using a hardcoded 6000 characters instead of the session-configured limit. Both paths now go through `actool.Capture`: truncated by session `MaxOutputChars` (default 30000); when `ToolOutputDir` is configured, the full output is spilled to disk and the message only keeps the leading fragment plus a file pointer.
- **Upgrade norma to v0.4.3**: The harness layer uniformly runs `CaptureOnce`; no tool (including future ones) will overflow the context by missing truncation.

### Deployment

#### Fixed

- **Activity stream could not connect under reverse proxy deployment**: The SSE address was previously unconditionally assembled as `${hostname}:8787`. In reverse-proxy-to-443 deployments, the page loads from `https://domain/` but SSE is pointed at port 8787, which is not exposed to the public, so the activity stream keeps reconnecting. The behavior is now distinguished by `NODE_ENV`: production defaults to same-origin, and only `next dev` keeps `:8787` to bypass Next.js's SSE buffering; the `NEXT_PUBLIC_SSE_BASE` override is preserved. The README has been supplemented with reverse-proxy deployment instructions.

### Findings

#### Fixed

- **"By asset" view asset list overflowed without a scrollbar**: The outer card had only `max-height` with no definite height, so the scroll viewport's percentage height could not resolve; with many assets, the list either broke out of the card or was clipped and could not be scrolled. The height cap is now applied directly to the native scroll container of the asset tree, adapting to the window height.

### Database

#### Fixed

- **Set a cap on the connection pool**: `database/sql` does not limit the number of connections the application itself can open by default; when there are no idle connections in the pool, it unconditionally opens new ones, only being rejected when it hits PostgreSQL's `max_connections` (default 100). The cap is now 32; excess queries wait for an idle connection, presenting as slowness rather than error under the same load. If `max_connections` is lowered, this must be lowered in sync.
- **Improve connection reuse**: `MaxIdleConns` defaults to 2; connections beyond that are closed after use, requiring TCP and PostgreSQL auth handshake on the next request. The idle cap is now raised to match the total cap, with a 30-minute connection lifetime and 5-minute idle eviction, so the pool can naturally cycle out long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### Added

- **New "Vulnerabilities" column on the task list**: Buckets by severity and displays counts of critical / high / medium / low; non-zero buckets are colored by severity for an at-a-glance view of each task's vulnerability scale and distribution.

#### Changed

- **"Description / Target" column widths narrowed**: Long content is truncated; hover to view the full text, reducing the horizontal space eaten by long text.

### Exploration graph and planning overview

#### Changed

- **Streamlined the planning overview (graph_overview) and set count caps for each list**: Recent facts, completed intents, pending intents, cold-region summaries, and confirmed vulnerability details are now "latest window + count fallback"; unshown ones can be queried on demand. Significantly reduces per-turn LLM context volume and avoids long-task context bloat (the cold region of the associated task overview is also throttled).

#### Fixed

- **Reverse duplicate edges between digest and finding in the exploration graph are eliminated after collapsing cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset intercept

#### Added

- **New global asset intercept rules (blacklist)**: Supports exact and fuzzy matching of domain / IP / URL, and CIDR ranges; provides rule CRUD and enable/disable. Management page is under "System → Asset intercept". By default, fuzzy-intercepts government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites.
- **Asset intercept plugged into the execution chain**: Before issuing intents (`add_intent`) and inserting assets (`insert_assets`), the Agent intercept-checks the target assets — intercepted intents are not issued, intercepted assets are not inserted, and the Agent receives the asset info and the intercept reason.
- **New task-level intercept / allow (whitelist) rules**: Independent of global rules, effective only for this task, evaluated in "intercept first, then allow" order — intercept match means forbidden; no intercept match but task-configured allow rules with no match means "testing not allowed"; no allow rules configured means whitelist is not enabled. Rules can be entered when creating a task, or added/edited/enabled/disabled on the task detail "Overview".
- **Task templates support preset categories and task-level intercept/allow rules**: Templates can save task category and a set of task-level rules; applying a template brings them into the new task form.

### Operation audit

#### Fixed

- **Tightened model adjudicator output protocol to reduce false allow due to truncation**: The verdict comment cap is reduced from 500 Chinese characters to 120, with mandatory "JSON only, no preamble or code block" — preventing verdicts from being truncated by `MaxTokens` and un-parseable, then falling through to the model failure policy (fail-open).

### Task archive

#### Fixed

- **Archive skips symbolic links instead of failing the whole package**: The archive format end-to-end only supports regular files and directories; previously, any symlink in the working directory would fail the whole archive. It now skips the symlink and logs it, archiving the rest normally (does not follow links, does not step outside the directory tree).

### Accounts and compliance

#### Added

- **New "Terms of Use and Disclaimer" dialog before login**: Must be checked to log in.

### License and dependencies

#### Changed

- **Project adopts the AGPL-3.0 open source license**, and the README's license and disclaimer sections are completed.
- **Upgrade norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration chain broadcast board

#### Added

- **New "Exploration chain broadcast board" on the task detail page** (#144): Renders task exploration nodes (start / target / intent / fact / vulnerability / hint / compaction) in a timeline flow view, with type filtering, keyword search, ascending/descending order, pagination, and auto-refresh. When on the "first page with newest on top", it is a live position, refreshing every turn; when off that position, only an unread count is accumulated, without disturbing the current read, with a "N new broadcasts · jump to newest" button. Grouped by day, so long tasks can be recognized as "this is from day X" when paging.
- **Broadcast board shows node id per row** (#145), for cross-referencing the exploration chain graph and locating specific nodes.
- **Broadcast board node details expand to show upstream/downstream and anchored assets** (#147): Expanding a broadcast shows the node's upstream (where it came from) / downstream (what it produced) relations; hovering an associated entry shows the corresponding node's card (type / status / source / time / summary / payload fragment). Also lists the assets the node is anchored to (type label + identifiable text). Related data is delivered along with the broadcast page; expanding does not trigger additional requests.
- **Broadcast board search supports filtering by node id** (#150): The search box adds node id matching in addition to "content / source"; enter a pure number or the "#41" form displayed in the UI to precisely locate the corresponding node.

### Intent management

#### Added

- **Intent deletion supports two modes: soft delete and hard delete** (#149): Pending, running, and paused intents can all be deleted; deletion requires a reason; the mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: The intent is set to "deleted" and the reason recorded in a separate field, preserving the intent node with all its output and lineage.
  - **Hard delete**: Physically removes the intent and the "exclusively supported" descendant nodes (cascading down output / intent chain to leaves), avoiding orphaned data. Token metering for the deleted intent is archived by original date; shared nodes (also referenced by other intents), targets, and task root facts are preserved. The hard delete dialog shows the expected cascade impact count.

  Both modes notify the planner "this intent was deleted by the user + reason" and replan accordingly.

### Operation audit

#### Added

- **Approval records support filtering by status and decision source** (#139): Filter approval records by approval status and decision source to quickly locate target records.

#### Fixed

- **Pending approval requests are loaded independently of history pagination** (#133): Pending requests are no longer affected by the history list pagination; they remain fully visible while browsing history.

### Agent

#### Changed

- **Worker role description changed to generic cybersecurity platform wording** (#138).

#### Fixed

- **Cold node compaction was never triggered**: Compactor was wired to the per-task planner, but cold node compaction (cold-digest) had never run because it was not connected; now restored.

### Chat

#### Added

- **Chat supports @-references and scroll pagination for multiple record types** (#135): Multiple record types can be @-referenced in chat, with candidates loaded via scroll pagination.

#### Fixed

- **Long message bubble constrained within the conversation panel** (#137): Excessively long message bubbles no longer overflow the conversation area.
- **Fix "missing upload file" when uploading files before a new conversation is created**: The Composer's file selection snaps `FileList` to an array before clearing input, then calls back; previously the draft state required asynchronously creating a conversation, and by the time execution resumed the `FileList` live-bound to input had been cleared, causing the upload to lack the file field and the backend to return 400.

### Traffic

#### Fixed

- **Traffic recording proxy now defaults to listening only on 127.0.0.1** (#129, #130): Avoids exposing the open proxy on other network interfaces by default.

### Web

#### Fixed

- **Fix static export task list page losing the global header**.
- **Fill in demo vulnerability detail page associated traffic mocks, fix white screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation audit

#### Added

- **Approval records support source location** (#125): Clicking the "Source" of an approval record jumps back to the tool execution that triggered the approval — opens the original full conversation, auto-paginates to the target position, expands the command and result in place and centers with highlight in the scroll area, with surrounding messages kept readable; auto-correction stops when the user manually scrolls. Spans regular chat, task Worker, planner, and main Agent segmented conversations, all precisely located via persisted `tool_use_id` and task mapping (new in-scope call ID index); missing, duplicate, or ambiguous associations are explicitly prompted rather than jumping to other executions. Clear prompts when the conversation is deleted or task archive/record is missing.
- **Approval records support pagination** (#115).

#### Changed

- **Streamlined model review input** (#124, #125): The model review (LLM adjudicator after rule miss) input is now a versioned JSON of "the current complete tool call + explicitly selected short context + local working directory". The context is only the real user message (current user message for chat / task main Agent); Workers no longer attach intent summaries, and clear background inherited from the parent Agent; planner and auto-triggered conversations do not fabricate user messages. No longer attaches task description, target, operating constraints, global exploration state, historical calls, or Worker full intent — these still follow the original logic for Agent execution and independent session audit, but don't enter the operation audit. All verdicts must output `decision`/`comment` JSON; the explanation must include "actual operation, post-success impact, matched rule"; instructions in parameters or context cannot change the review policy. Saves the actual input snapshot and fingerprint sent to the model; old snapshots are preserved and labeled with version, and historical inputs are not back-filled with current data.

#### Fixed

- **Model verdicts wrapped in code blocks are no longer silently allowed** (#126): If the model's verdict JSON is wrapped in a ``` code block, previous parse failures would silently allow via the model failure policy; code block fences are now stripped first before parsing, and only after parse failure is the configured failure policy applied.

### Agent

#### Added

- **New experimental noa context compression** (norma upgraded to v0.4.0): An experimental feature toggleable in system settings, off by default. When on, noa (model-driven context compression) takes over context compression for main Agent, planner, Worker, and chat, replacing the built-in compression. The original compressed text is persisted in `<workDir>/noa/<sessionID>/` (not scattered in task directories), with globally unique directories by session ID. Failure to engage automatically falls back to the built-in compression without interrupting the actual task. The toggle is read once per run; switching only affects subsequent runs.

#### Fixed

- **Exploration graph tools refuse instead of nil-panicking when there is no task context**: Calls to exploration graph tools in non-task context return a clear error, no longer crashing on empty storage dereference.

### MCP

#### Added

- **Support legacy SSE MCP services** (#117): Compatible with MCP servers providing only legacy SSE transport.

### Traffic

#### Fixed

- **Traffic search is port-aware when matching the host of a record** (#114): `traffic_search` host matching now includes the port, preventing same-host records on different ports from interfering with each other.
- **`traffic_search` description migration failure no longer breaks subsequent reporter migrations**: A single migration failure is isolated and does not affect subsequent migration execution.

### Web

#### Changed

- **Retest vulnerability options on a task get a divider** (#122): Separators are added between retest vulnerability options for visual clarity.

#### Fixed

- **Fix demo task detail page full-page crash**: In mock mode, the task detail "Conversation" tab would fetch `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, so the read fallback returned `[]` based on "path ends in s = collection", causing `data.items` to be `undefined`, and the side-question hook's `merge()` iterating it threw `TypeError: t is not iterable`; this exception occurred in the `setItems` updater, was deferred by React to render, the caller's `catch` couldn't catch it, and the whole page was taken over by the error boundary showing "This page couldn't load". The mock handler now explicitly returns an empty side-question history, and `sideAPI.history` also normalizes the return value (`items` non-array is always fall back to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **Network search adds DeepSeek official source**: Directly reuses the currently active LLM config. It differs in nature from the other three sources — DeepSeek has no directly callable search interface; search only exists inside its Anthropic-compatible interface (`web_search_20250305` server-side tool), executed by the DeepSeek server. Therefore it **only supports the DeepSeek official endpoint + anthropic protocol** (OpenAI protocol endpoints will directly reject server-side tools), each search **additionally consumes one model call**, requests **do not go through the search egress proxy**, and **are not recorded in traffic**. Returned results **only have title and link** (no summary; WebFetch is required to get the body). The settings page describes the above limitations but **does not validate / block**; users confirm on their own. A "Test search" button is provided for a real-run verification.

### Agent

#### Added

- **Workers get cross-work recall capability**: New `search_all_worker_traces` (no need to know intent_id first; globally search hits by keyword across all work's execution in this task) and `get_worker_trace` (after locking onto a work, list its step stream, search by keyword in place, get full content by step_id), used to reuse observations seen by other works but not written to facts, avoiding duplicate work.
- **`node_detail` is given to workers**: Cooperating with the above recall tools, after getting intent_id / node id, workers can directly query the full details of the node.
- **`add_hint` triggered planning turns are explicitly broadcast**: Previously, adding a hint only folded the hint into the situation overview, letting the planner discover it on its own. Now, one `add_hint` records a "user added N strategic hints: …" trigger for the planner (batch counts as one, not flooding). The planner is explicitly told "this turn is triggered by new hints" and directly sees the hint content.

#### Changed

- **Global situation hints are now "loosened"**: Encourages exploration divergence, timely reporting of cross-intent clues, rather than early convergence.
- **Streamlined worker boundary wording**: An initial obstacle does not mean the probe is through; focus on "exhausting the bypass methods within this intent before drawing a conclusion".
- `insert_assets` removes the `related` parameter per asset: Its only role is to decide whether to add this asset to the task's range, but the value is not stored (the same asset being re-registered invalidates it, and the UI doesn't show who was judged irrelevant), effectively just making the model do an extra judgment that doesn't stick.
- **Test scope (`task_scope`) is no longer affected by the asset coverage toggle**: `insert_assets`'s auto-into-range (`source='auto'`) executes regardless of coverage on/off, and `add_task_scope` is always available to planner / task main Agent / goal-decomposition Agent. Scope is the task's authorization boundary and the filter base for asset queries; the coverage toggle only decides whether to use it as the denominator for metrics, not whether to accumulate scope itself. Previously, turning off coverage disabled both the `auto` and `agent` write paths, leaving `task_scope` with only manually-added rows. `list_untested_assets` remains hidden with coverage off (it is purely a coverage view).

#### Fixed

- **Fix assets being mixed across tasks** (#59): `list_assets` previously hardcoded the task id to 0, querying the entire shared asset library, and the model couldn't write range filter conditions, so it took other tasks' assets (especially IPs) as this task's targets, and the testing direction was led astray. It now only returns assets within the test scope (`task_scope`) of 【this task and directly associated tasks】, matching by **ownership** rather than literal value: a root domain in scope means all subdomains / services / endpoints under it; a CIDR means the hosts and services within. Direct id lookup outside scope is also unreachable. Non-task context (Auto / pentest) has no scope to rely on, still falling back to the whole library. Also fixes the ownership blind spot where IP-direct hosts (like `http://1.2.3.4/api`) couldn't be matched by CIDR scope. UI's "Test assets" view (filtered by task producer) is unaffected.
- **Fix worker cross-work recall tools being mistakenly removed**: After `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default tool set, an old migration "converging worker tool surface" at startup unbound them again, so workers couldn't actually get these tools. They have been removed from that unbind list, and an one-time re-bind to worker is done for databases that have run the old migration.
- **Fix `/btw` side-question occasional DB write failure**: The side_question checkpoint strips JSONB-unsupported NUL (`\u0000`) escapes before writing, avoiding write errors on content containing that character.

### Triggers

#### Fixed

- **Merged triggered sessions deduplicate task description/target by task**: On multi-task merge trigger, the same task's description/target is no longer repeatedly concatenated into the message, avoiding long targets stacking and blowing up the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- Task main Agent supports **multiple conversations**: Can create, switch, independently reset context; each conversation can interact.
- Supports persistent `/btw` **side questions**: Does not interrupt the main line, Q&A is persisted, still present after restart.
- `spawn_task` adds `source_task_ids`; new tasks can **read-only inherit** the source task's assets and findings.

#### Changed

- Disable worker cross-engagement memory and converge its default tool surface: Reading context, cross-work review is the planner's job; workers only execute a single intent and write back.
- Streamlined worker default prompts; `get_worker_output` removes `terminated` and `worker_name` fields; streamlined `insert_assets` / `list_assets` descriptions.

#### Fixed

- **Fix work being interrupted early by idle turns**: The model sometimes uses a whole turn to only output thinking, neither giving body nor calling tools. At this point, the harness sees a natural end (`end_turn` and no `tool_use`), directly closes with `completed` + empty summary — an unfinished intent is cut off halfway, manifested as "task ends normally but no text summary". None of the five LLM retry layers can reach it: it is not an error, the same-provider safety window retry entry is "stream failure", the circuit breaker on `err == nil` directly judges success, and the intent rerun only recognizes `model_error`; the SDK's empty response retry also can't reach it, because that layer judges empty by "whether events were yielded", and thinking increments are themselves events. The work's Stop hook now identifies this kind of turn and injects a continue instruction, letting the model continue to the next step with the already-produced thinking — deliberately not choosing "resend as-is", because this kind of idle turn is usually determined by the prompt and context shape, is stable behavior rather than random glitch, resending would only let the model think again.
- **Reuse the empty response retry count from the "Retry and backoff" page on the LLM page** (both answer "the model completed but produced no substantive content", just with different criteria and means), default 2; set to `-1` to disable, returning to the original "idle = wrap up" behavior. It is the **total** upper bound for an intent, not a consecutive count: the harness itself limits consecutive idle turns to only one push, and only when a real tool turn quota has actually occurred does the count refresh, so this number stops "tool → idle → push → tool → idle" loops from exhausting the intent budget. Trigger log: `[work <worker> · #<intent>] idle turn…injected continue instruction (n/N)`. Design see `docs/LLM重试设计.md` §1.1.
- **Fix `/btw` context budget and input layout under long conversations**; non-secure context (non-HTTPS access) degrades to generate side-question request id.

### Traffic

#### Added

- Vulnerabilities support **linking multiple traffic evidence** items, sortable, with notes, distinguished by role (request / response / corroboration).
- The report Agent will **auto-link** relevant traffic evidence before writing the report.
- New Agent **traffic binding toggle**, with traffic evidence handoff between Agents completed.

### Tasks

#### Added

- Supports **conversation-level vulnerability retest**: Retest runs in a separate Agent conversation; list and detail show run status.

### Intercept

#### Added

- Approval records add **detail and execution audit**: Can view tool request context, model/rule initial judgment, execution output, and parameter fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fix test connection not re-sending custom session header, causing opencode zen to return 400.

### Network

#### Added

- MCP HTTP transport supports **skipping TLS certificate verification**, for accessing self-signed services.

### UI

#### Added

- Conversation list is grouped by Agent, with expand/collapse and independent pin; conversation supports filtering by Agent.
- Attack chain graph renders digest (compaction) nodes, and collapses their members.

#### Fixed

- Fix white screen caused by login credentials out of sync.
- Intercept prompt copy now clearly marks "Platform control", avoiding being misread as target-side defense.

### Deployment & updates

#### Added

- **One-click page update**: A new "Version & Update" card on the system config page; the top bar shows a prompt when a new version is available. The flow is: download release package → verify `SHA256SUMS` → smoke test → stage → exit and have the daemon script re-launch to complete the swap, with the page auto-refreshing.
- New daemon startup scripts `start.sh` / `start.bat` as official startup entry points (already included in release packages and Docker images, `install.sh` unchanged), deciding whether to re-launch based on exit code, and responsible for forwarding SIGTERM to artex. Verification and swap logic is in Go; the script stays foolproof.
- **Failure auto-fallback**: Discard on verification or smoke test failure, continue running the current version; if the new version fails to start 3 times in a row, roll back to the previous version. The settings page has a separate manual rollback (note: database schema does not roll back).
- Update only recognizes the GitHub domain and forces HTTPS; the release source is not configurable; one-click update is disabled in development builds. GitHub query results are cached for 30 minutes to avoid the top bar prompt exhausting the API quota.

#### Known limitations

- Under Docker, only the program is swapped, not the image: the toolchain does not upgrade, and rebuilding the container will revert to the version bundled with the image; if needed, still use `docker compose pull artex`.
- `skills/` in the release package is not synced; new built-in skills in the new version do not auto-activate.
- Updating means restarting, which interrupts running tasks.

### Dependencies

#### Changed

- Upgrade norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- LLM page adds "Retry and backoff" tab: the **count** and **interval** of the five retry layers are configurable. A model call failure goes from inner to outer through: connection retry (SDK, connection reset / timeout / 429 / 5xx before stream start), empty response retry (SDK, normal end but no content, openai format only), same-provider safety window retry (replay of break before delivering any output to caller), polling circuit breaker (cooldown skip after consecutive failures hit threshold), intent rerun (Worker closes as model_error then rerun the whole intent) — five layers; outer only after inner exhausted. Each layer has two knobs, with unified semantics: blank = original default count and exponential backoff; fill count = use that count; fill interval = replace exponential backoff with fixed interval; fill -1 = disable this layer. The first three layers are endpoint-scoped and can override the global default field-by-field in each model config (only-interval still inherits the global count); circuit breaker and intent rerun are process-level, only one global copy. Takes effect hot after save, no restart needed. All blank = current behavior, old databases byte-identical after upgrade (new columns default 0, settings key missing = all default). Connection test deliberately does not carry these parameters — it has a 30s hard timeout, layering on user-configured retries would only make usable endpoints time out. Design see `docs/LLM重试设计.md`.
- Each LLM config supports custom **session header** (`session_header_key`): when non-empty, every LLM request carries that HTTP header, with the value being the current run's session id (`conv-<id>` for chat, `exp<x>-worker-i<intent>` for worker, etc.), for gateways doing prompt cache / sticky routing by session-id header. Implemented by reading session id from request context and injecting, no norma change needed, the same shared provider can also send different header values per conversation. Old databases get migration; front-end LLM config dialog adds a new input field.

#### Fixed

- Fix LLM config save reporting `23502` because session header field is empty: the column is `NOT NULL DEFAULT ''`, previously incorrectly applied `NULLIF($n,'')` writing unfilled session header as NULL triggering NOT NULL constraint; now passes parameter directly per empty-string semantics.

### Agent

#### Added

- Wall-clock timeout changed to **on-the-spot wrap-up**: Depends on norma v0.3.6, `MaxDuration` interrupts the running tool at the time and continues on the live ctx for the wrap-up round count (write back recognized content + summary, terminal state is timeout), no longer relying on each worker/planner's `maxDur+90s` external hard ctx killing stuck runs directly to `aborted_tools`. chat uses the same harness and gets the same behavior; mainagent has no `MaxDuration` and is unaffected.
- **Stall fallback**: When planner is woken by heartbeat / no change and there are no open or running intents in the whole graph, the opening is changed to a stall alert, clearly stating no Worker is running, no queued direction, and this round must produce one or more mutually non-duplicate new intents (must not produce 0 intents).

#### Changed

- **Worker prompt rewrite**: intent / startup instruction / anchored asset raw JSON moved into the system prompt, reassembled every turn, never compressed away by compaction, continuation does not depend on transcript first-message retention; startup user message slimmed down to just the global situation overview (degradable, stale-tolerant). The cost is per-intent data mixed in the system, losing cross-intent cache reuse — a deliberate trade for "not dropping intent".
- Streamlined planner / worker default body and fixed several practical issues: planner distinguishes `recent_done` by state (blocked/exhausted first check trace, don't treat as dead-end or blindly rerun), negative conclusions changed to "observation / uncertain, not a conclusion" and read evidence before trusting, target not met with no open/running intents must produce (hard floor), depth over coverage; worker negative conclusions only write "observation + tentative reading", the verdict belongs to planner, cross-intent clues write to fact's summary for the planner, don't chase themselves. Reseed appends new defaults as a new version and switches over; user-customized / old versions are kept in history and can be rolled back.
- **Converge work agent default tool set and asset write-back hints**: Workers only handle single-intent execution and write-back, reading context / cross-work review is the planner's job, so `list_facts` / `node_detail` / `list_companies` and cross-work retrieval (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) are removed from its default tools, leaving only `list_findings` (check for duplicates before reporting vulnerabilities) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; prompts remove stale field descriptions conflicting with `insert_assets` schema (`type=tech` / `on_url` / `props`). Old databases get a one-time migration stripping the corresponding bindings; planner/main same-name bindings are not touched.

### Exploration graph

#### Added

- **Exploration graph cold node compaction (cold-digest)**: Folds old and long-inactive intents / facts into digest nodes for display in `graph_overview`; original nodes are permanently preserved, fully restorable by id (storage lossless, only presentation compressed, collapse reversible). Hot/cold judgement by reverse reachability + any live branch = hot, with R=6 rounds debounce and connected component grouping; background compaction (minor covers uncompacted cold blocks, major source-recompress merge fragments) with liveness check and cooldown mutex, never on the hot path, never overwriting revived nodes. The overview side provides `cold_digests` and `cold_index` indexed by asset; `expand_digest` / `expand_index` is responsible for restoration. Associated / inherited task overview also reuses its own collapsed view; `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are only given to planner and main agent, not worker. All include old database migration.
- `graph_overview` fully outputs `finding_list`: findings are the highest-value product of a task and usually not many per task, so they are now carried in full in the overview (unlike facts which only give the most recent window), planner/worker can see all confirmed vulnerabilities at a glance each turn, no need to call `list_findings`. Each is streamlined to `{id, summary, evidence?, from_intent?, assets?}`, with affected assets given as readable content (url / domain / ip:port) rather than bare id.

#### Changed

- `graph_overview` no longer flattens `hosts`: the coverage block removes the host list (large-scope tasks can carry up to 500 host strings per turn, of limited value to planning decisions), keeping only `host_count`; specific hosts queried on demand via `list_assets`. New top-level `done_intents_total` (total completed intents) is added, parallel to `recent_done_intents` truncated to ≤15, letting planner know whether dedup is truncated.

### Tools

#### Changed

- `add_company_scope` default binding changed from worker to planner: defining enterprise asset scope is the responsibility of planning / main control / Auto; workers only execute exploration. New database seed default bound to mainagent/planner/auto, old database one-time migration.
- planner default binding `list_assets`: now has both `list_assets` (DSL whole-library retrieval) and `list_untested_assets` (untested in range), old database one-time backfill, does not override user unbind.

### Tasks

#### Added

- New "Running Worker" column in the task list: counts intent nodes with `state='running'` under the task, the metric is fully consistent with the "Running Worker" on the task detail page overview; shows 0 when there is no running worker.

### Network

#### Added

- New **global egress proxy** config: all target traffic can egress through a unified global proxy (http/https/socks5, supports `user:pass`). When traffic capture is enabled, it acts as the MITM record proxy's upstream (traffic is recorded then egressed through the proxy, both intercept and pass-through paths go through upstream, no source IP leak); when capture is disabled, it is directly injected into agent's bash environment and WebFetch (`proxyEnv` adds `ALL_PROXY` for socks5). Config is stored in settings KV (no migration), front-end system config page adds a "Global proxy" card, independent of network search proxy and LLM proxy.

### UI

#### Fixed

- Fix semantic errors in intent status labels: `exhausted` "Exhausted" → "Budget exhausted" (actually means step / time budget cut off mid-way with only partial write-back, not that the direction is fully explored), `blocked` "Blocked" → "Execution error" (actually means model / API / network failures retries exhausted, intent basically didn't really explore, not target / WAF blocked); also added previously missing `stopped` "Stopped" (user manually stopped work).

### Dependencies

#### Changed

- Upgrade norma to v0.3.4: MCP tool output adds truncation and disk spill (see `651b961`), later v0.3.6 supports wall-clock on-the-spot wrap-up.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model config adds "Output cap", with the choice of which request field name to use for the cap: the cap limits the maximum number of tokens generated per response, sent with each request; 0 (default) = don't send the field, server-side default decides. It's a separate thing from "context window" — the latter is the model's total capacity, only used locally to compute compression threshold, not appearing in the request. The field name switch is only meaningful for `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which most compatible gateways recognize; OpenAI official reasoning models (o-series / GPT-5) only recognize `max_completion_tokens`, returning `unsupported_parameter` on `max_tokens` — such endpoints need to manually switch to the new field. The other two formats have field names locked (Anthropic is `max_tokens`, Responses API is `max_output_tokens`), so the switch is greyed out for non-openai formats and cleared on save. Also patching in a chain that was never connected: planner / worker / chat / main Agent / goal-decomposition never set output cap, openai family simply doesn't send, Anthropic uses SDK's 8192 fallback — just adding a config item without connecting this line, filling in a number does nothing; now value is parsed per turn from config like "streaming output", takes effect on the next turn after failover. Behavior completely unchanged after old database upgrade (new column default 0 and empty field name).
- Background "Test" now logs each HTTP attempt's status code and the gateway's raw response body in server logs (body clipped to 4K), for diagnosing 401, quota text, empty frames, returning HTML pages, etc., no longer only seeing ok / err from the UI collapse. The log does not depend on the "LLM records" toggle.

#### Changed

- Task LLM config chain can be modified in any state, no longer limited to "running / paused / config chain exhausted": after the task ends (done / failed / timeout), the main Agent conversation still uses this chain; if a model on the chain has issues, you couldn't change it and couldn't continue interacting. Both the back-end HTTP and DB transaction terminal-state blocks are removed, and the front-end dialog opens editing and saving for terminal-state tasks. Saving on a terminal-state task no longer re-opens quota-blocked intents (those intents are moved to open, no worker executes them, and they no longer meet the "rerun intent" criteria); to keep running, use rerun intent / add target, which pulls the task back to running.

### Agent

#### Changed

- "Send a message to a running Worker to redirect" changed to reusing the existing pause / resume + transcript resume mechanism, with the same behavior as main Agent conversation: previously it was a self-built intervention persistence protocol, entangling scheduling barriers, resume flow, and over a dozen active query filters. Now messages are injected via the next turn's input, intents run directly in their dedicated goroutine, not subject to the 3-slot worker pool limit, run as sent; the front-end keeps the message box and restores the "Continue directly" button, with sending now over SSE. No DB schema change. Trade-off: messages are kept only in memory, no crash recovery, and the task may temporarily exceed concurrency by one worker at message-send time (low-frequency scenario, acceptable).

### Tasks

#### Fixed

- **Fix OOM crash when archiving large tasks**: Archive is now streamed-write to snapshot, no longer reading the whole task into memory at once; also fills three recovery gaps on the cold-archive path — modern installs have traffic only in SQLite, previously a crash between PostgreSQL commit and SQLite commit would leave the archived task's traffic stranded in hot storage with no recovery; now whether or not there's a history directory, a stage log is written, and missing `journal.json` is treated as discardable.
- **Fix lag when there are many tasks**: Loading conversations and UI has noticeable delay. Task list, task context, and exploration record queries are optimized, and the front-end dashboard, conversation, and task detail pages correspondingly reduce duplicate requests.

### Assets

#### Changed

- **Remove the "max 256 rules" limit on enterprise asset scope**: Enterprises entering scope one IP / domain at a time easily hit this cap, and could only split into multiple enterprises, while the scope itself is not slower because of more rules. Both front-end and back-end, plus demo mock, remove the cap; single rule still limited to 1024 characters, request body 2 MiB cap remains as a fallback (about 40-50k rules).

### Skills

#### Fixed

- Fix uploading a skill archive reporting "Upload failed: zip: unsupported compression": Go's standard library only includes Store / Deflate decompressors by default; bzip2, Zstandard packages written by compression software at non-default levels cannot be decompressed. These two decompressors are now added (pure Go implementation, no new external dependencies); formats that really cannot be decompressed (Deflate64 / LZMA / XZ / PPMd, etc.) and encrypted archives are now reported with a Chinese message before decompression, directly naming which file uses which compression method and how to repackage, no longer throwing raw English errors at the user.
- Fix skill file name validation judging Chinese file names as illegal: path validation was originally an ASCII whitelist `[A-Za-z0-9-_./]`, and any Chinese-named file in the archive (e.g. `参考/说明.md`) would fail the whole upload with "archive contains illegal path". Now changed to a Unicode blacklist: file names in various languages and spaces are allowed, still rejecting control characters, illegal UTF-8, zero-width and bidirectional control characters (RLO file name spoofing), `\ % # ? * : " < > |` and `..` / absolute paths / empty path segments; directory traversal protection unchanged. Skill names are also relaxed — ASCII still limited to lowercase letters, digits, and hyphens (agentskills.io spec), Chinese and other non-ASCII letters can be used directly as skill names, but spaces, dots, and path separators are not accepted.
- Fix Chinese-named archives made by Windows compression software getting garbled file names or the whole package being rejected after decompression: these zips don't set the UTF-8 flag and write file names in GBK; now GBK is the fallback decode before path validation. `name: "中文技能"` style quoted frontmatter also correctly extracts the skill name.

### UI

#### Added

- Findings page adds "All findings" flat view, set as default, switched with the original "Group by task" view via page header Tab: the group view requires expanding each task one by one to see vulnerabilities, which is a detour when you just want to glance at a list across tasks. The flat view is a cross-task big table (10/20/50/100 per page), with the same behavior as the group view — checkbox export, inline expand for evidence and detailed report, inline change of name/category/severity, change disposition state, drill in, delete. Both views share the stats cards and filter bar, switching doesn't lose filters, current view and filter options are persisted locally; polling only hits the current view, inline changes sync both caches, switching doesn't see stale data.
- Findings page adds "By asset" view: asset tree on the left (company → root domain / IP → subdomain → service → endpoint, company layer only appears when assets actually have ownership), the right side is the findings under the entire subtree of the selected node, sharing the same table and filter conditions as the other two views. The tree only collects assets with findings, ancestor chain patched on demand (when findings only hang on the deepest endpoint, the upper layers can still be assembled), counts on nodes are subtree aggregated and deduplicated by finding; assets that have been deleted or findings not associated with any asset go to the "Unassociated assets" bucket. Unlike the other two views, the asset view doesn't poll — only queries when entering the view, changing filters, in-page adding/removing/editing findings, or clicking the refresh button on the tree; the left tree is a navigation structure, no need to recompute every 5 seconds. Each level on the tree only shows the increment relative to the previous level (subdomains drop the root domain suffix, services show `https :443`, endpoints only show path), full value in hover tooltip and breadcrumb; when nodes are too many, the whole level drops the endpoint/service hierarchy and gives a hint, count still rolls up to the parent.

#### Fixed

- Fix mobile conversation detail squeezing conversation records and being unreadable: collapse the conversation list on narrow screens, giving the width to the records themselves.

### Dependencies

#### Changed

- Upgrade norma v0.3.2 → v0.3.3: Add `Config.MaxTokensField`, letting OpenAI Chat Completions output cap send `max_completion_tokens` instead (reasoning models only recognize this key). The two keys are mutually exclusive, only one is sent; default is still `max_tokens`.
- Upgrade `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security advisories.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fix "non-streaming" toggle reverting to "streaming" after save and reopen (#69): The config list API's DTO missed the `streaming` field, so the response never returns it, and the front-end reading `undefined` always falls back to default streaming; actual write and DB storage had no problem, just can't read back. DTO adds the field back (no `omitempty`, `false` must also appear in the response).
- Test connection changed to verify the model actually replied, and tested in the config's real send/receive mode (#65): Previously only judged whether HTTP was successful, requests passing but model zero reply (thinking burns through budget / body swallowed by safety policy / compatibility layer drops `content`) was also reported as "connection successful", disconnected from "not replying" in the conversation; and the test always used streaming, so non-streaming configs were actually testing another channel. Now empty reply is directly judged as failure; on success, the model's reply is shown in the prompt, and the current streaming toggle is also brought into the test, so "test passed" stays consistent with "conversation works".

### Agent

#### Changed

- `list_facts` changed to pagination + keyword filter, avoiding a single call blowing up the context when there are many facts (#74): Default returns the latest 20, supports `limit` (cap 100) / `before` cursor / `q` summary keyword, returns `{facts, total, has_more, next_before}`; single summary that's too long is truncated by character count (full text still goes through `node_detail`). Worker / planner prompts synced to pagination semantics. Old databases refresh the new parameter schema into the tool catalog table via a one-time migration (`SeedTool` insert-only, otherwise the tool management page shows "no parameters").

### Tools

#### Added

- "Tool execution" page adds tool call count statistics (#72): The "Stats" button in the tool bar opens a dialog showing per-tool call count, percentage, and failure count (descending by count); reuses the list's task / keyword filter, stats cover the whole result set rather than the current page, fetched when the dialog opens.

### Dependencies

#### Changed

- Upgrade norma v0.3.1 → v0.3.2: Fix silently dropped reasoning/refusal in OpenAI-family responses (Responses adds `reasoning_text`), reasoning field three-name dedup, bounded retry on empty response (zero content blocks), compression boundary cut off tool pairing causing gateway 400 (also fix the existing orphans baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM config supports streaming/non-streaming toggle (default streaming): On uses streaming SSE; off uses true non-streaming (`stream:false`, one-shot return complete JSON), can bypass some gateways' terrible SSE implementation (empty frames, thinking field dropped frames), at the cost of losing real-time progress and real-time Token counting during runtime. worker / planner / mainagent / chat / goals all dynamically take value from the currently active config; `llmpool` / `llmrec` / task runtime three-layer Provider wrappers all support non-streaming; `llm_profiles` adds `streaming` column and `ALTER` for old databases (default `true`, old configs unaffected).
- Support OpenAI Responses API format LLM config: Each config adds a third format `openai-responses` (hits `POST /v1/responses`), parallel to Chat Completions / Anthropic; `BaseURL` normalized, default model `gpt-5`; `llm_profiles`'s `format` constraint adds `openai-responses` and idempotent migration for old databases; front-end format dropdown adds "OpenAI (Responses API)". Depends on norma v0.3.1 (includes `reasoning_content` callback fix).
- Record LLM request/response HTTP raw body: Captures the real wire body at the HTTP transport layer, preserving tool schema, `tool_use` blocks, and raw SSE frames that the normalized view doesn't see; norma's internal retries' multiple attempts are preserved one by one; `llm_records` adds `raw_request` / `raw_response` columns and `ALTER` for old databases; the recording page detail panel adds "Raw" view toggle and request/response copy buttons (with `execCommand` fallback for non-secure context).

### Conversation

#### Changed

- Conversation list multi-select changed to "Multi-select" mode toggle: by default the list no longer has per-row checkboxes always shown (cleaner look), header changed to "Total N + Multi-select" button; click "Multi-select" to enter selection mode (checkbox, select all, batch delete), "Done" exits and clears selection, batch delete auto-exits on full success (stays in selection mode on failure for retry); single rename / pin / delete still goes through per-row ⋯ menu.

### UI

#### Changed

- Task management, conversation operations, and traffic viewing experience enhancements (#57): Task list column sort preferences persist, inline rename now uses icon directly (no longer through menu), drawer (sheet) interaction and traffic viewing details optimized.

#### Fixed

- Worker asset tags only show domain and IP, and correctly handle the empty tag scenario.

### Agent

#### Added

- Planner's target judgement adds "Quantitative acceptance check": When the target has quantifiable acceptance conditions (asset test coverage reaches X%, get N flags, get some permission), `prove_goal` must check the actual measured values in `graph_overview` (`coverage.pct`, findings count, etc.); if not met, `prove_goal` is forbidden, intents should be dispatched to fill the gap, must not mark met on the grounds of "main part completed / broadly achieved". Fixes the problem where the target required 100% coverage but only 40% was actually measured yet judged complete.

#### Changed

- Rewrote the planner prompt's "0 intents this turn" decision basis: The original text described 0 intents as "the most common, most important principle", which would cause the planner to wrap up too early when the target was unmet and untested areas remained in scope. Now changed to only produce 0 intents in two specific cases — ① every direction thought of is already covered by open/running/recent_done intents; ② the next step depends on the output of currently running work, and that output hasn't appeared (should wait for it to finish, replan on next wakeup after the graph updates). Also added the reverse constraint: when there is a new direction not covered and not depending on running work, or the target is unmet and untested areas remain, don't wrap up because "0 intents is common".
- Strengthen the "evidence bar for negative conclusions" in the worker prompt: For negative conclusions like "not injectable / port closed / no login entry" that could make the planner give up a whole direction, require exhausting reasonable means within the intent (change encoding / parameter / path / method) before concluding; if means aren't exhausted or evidence is weak, mark `confidence=inferred`, avoiding using rash `observed` negation to weld a whole route shut (an erroneous negation early in the task especially leads the direction astray and is hard to self-heal).
- The above planner / worker default prompt changes are appended as new versions via a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each with settings flag guard) and switched to current; old versions are kept in version history, and users with customized prompts can restore them in the version record.

### Operations

#### Added

- New `reset-password.sh` to reset the admin (username fixed `ARTEX`) password: Supports local / docker deployments, connection info can be explicitly specified or auto-read from `--dsn` / `$ARTEX_PG_DSN` / `config.json`; in-database use `pgcrypto` to generate a bcrypt hash compatible with back-end login and write back to `settings.auth.password_hash`, no need to restart the service after reset. Password is passed via environment variable, not in process argv, and is escaped against injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting multiple tasks in the task list, batch modify category, target category supports "Uncategorized" to move tasks out of the current category; the whole batch write is completed in one transaction, failures will only be tasks deleted after being selected.

#### Changed

- New task "Task category" changed from dropdown to searchable input: typing filters existing categories, names not in the library are created on Enter (or click "Create" in dropdown) and selected, selected item shown as removable tag; still limited to one category.

### Conversation

#### Fixed

- Fix the case where a conversation has selected an LLM config but there's no global active config, and sending a message is blocked by "LLM not configured, cannot chat". The pre-send check only looked at the global active config, while the actual execution prioritized the conversation-selected config — the two resolution logics were inconsistent. Now unified to the same set (conversation/Agent binding priority, global fallback).
- The "cannot chat" prompt is split by status: no config at all prompts "Add config", config exists but not active prompts "Activate or specify a config for this conversation", no longer uniformly showing "Not configured", easier to locate.

### Agent

#### Added

- When all goals are met, intents issued by the main Agent via `add_intent` can be directly claimed and executed by Workers, running to completion: at this point the Planner no longer runs (no open goals means no planning), avoiding it re-judging goal completion and cancelling the just-issued intent; after the frontier is drained the task returns to completed. Before issuing an intent, the main Agent will ask the user based on the intent content whether to register it as a formal goal; if registered, the task resumes normal autonomous planning.

#### Changed

- `get_worker_trace` / `get_task_worker_trace` `step_ids` exceeding the one-time cap (5) no longer errors directly, but returns the full content of the first 5 steps, and tells which were taken and which were not via `returned_step_ids` / `omitted_step_ids` / `notice` in the result, with a hint "if that's enough, no need to fetch more". Duplicate and illegal ids in the input are first deduped and excluded before counting.

#### Fixed

- Task list changed to descending by task id (newest first), replacing the original sort by creation time. Multiple tasks created at the same time have the same timestamp, sort is unstable, combined with the 10-second polling and random iteration of the in-memory map, causing them to frequently swap positions in the list.

### Assets

#### Fixed

- Fix enterprise ownership recomputation being killed by a single dirty row: when `assets.ip` stores a hostname (Agent or asset API write), `a.ip::inet` throws `22P02`, causing all add/modify enterprise asset scope and delete enterprise operations to fail and roll back. Switched to safe conversion `try_inet()`, illegal values skipped rather than aborting the whole statement.
- Unparseable `ip` is no longer silently skipped: After enterprise scope is saved, a clear prompt tells how many assets have `ip` that isn't a legal IP (including specific ids and values), reminding that these assets won't be owned by IP/CIDR rules; the same info is also written to server logs, covering paths without front-end response (delete enterprise, scopesentry sync, Agent write).
- Task test scope IP/CIDR match also switched to `try_inet()`, replacing the original regex guard that only checked character set (all-hex-letter hostnames like `abc.def` previously slipped through and triggered the same error).

#### Changed

- `ip`, `service`, `endpoint` assets' `ip` field no longer accepts hostnames: `insert_assets` and asset API return per-line errors with `index`, and directly give the fix (use `type=subdomain` with `domain`, or resolve A/AAAA records first); other assets in the same batch are unaffected and stored normally.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- Running Workers support per-item pause, resume, and cancel; pause preserves intent, fact, and vulnerability, cancel transactionally cleans the current intent and its direct output after execution exits.
- Add `paused` intent state, execution fence, and named termination reason, preventing late blackboard writes after pause, cancel, or task delete.
- Add optional task concurrency cap; new, resume, vulnerability drill-in, and queue fill-in all go through a persistent FIFO admission path.

#### Changed

- Worker single-run wall-clock default duration adjusted from 600s to 1200s; timed-out task revival retimes on the next real execution.
- Worker, Planner, and main Agent's real execution state uniformly drives the task status badge, fixing "Idle" still showing while Worker is running.
- Worker conversation preserves per-item control and current conversation Token summary, model name moved to the right of the current conversation title.

#### Removed

- Remove Worker multi-select, select all, and batch pause/continue UI; also remove Worker batch control API and Mock contract.
- Remove Token badge and Tooltip in Worker list rows; underlying complete Token ledger and task aggregate API remain.

### LLM

#### Added

- Tasks support ordered LLM config chain; explicitly identify when provider quota is insufficient and auto-switch to next config, persisting current config, exhaustion state, and error summary.
- Running and paused tasks support editing the full config chain, reordering, and manually switching the current config, taking effect from the next LLM call.
- Auto-switch, manual switch, and whole-chain exhaustion are written to structured system activity, and deduplicable reminders pop up via the task activity stream.
- Add task role model resolution interface, uniformly resolving the config and model used by Main Agent, Planner, and Worker for the next call.
- Add optional global LLM Pool, supporting call order, designated model failure fallback, health status, cooldown recovery, and manual reset.
- Add always-on LLM usage ledger, aggregating input, output, and cache Tokens by task, conversation, model, and config.

#### Changed

- Goal decomposition, Planner, Worker, and main Agent share task-level LLM runtime (resolution priority below).
- Failover only responds to explicit quota, balance, or billing errors; ordinary rate limiting, auth, network, server, and context errors don't mistakenly switch.
- LLM config page changed to config card list with right-side drawer editing, and provides Pool polling order, priority, exclusions, health status, and recovery operations.
- Config chain exhaustion info supports narrow-screen line break; current model changed from conversation list icon to text label next to the current conversation title with full Tooltip.
- In-task role LLM resolution order adjusted to "Agent binding → Task config chain → Global": roles with explicit model binding always run on that model, unbound roles fall to task chain, task chain empty falls to global.
- Outbound proxy left blank means direct connection, no longer falling back to `HTTP_PROXY` / `HTTPS_PROXY` environment variables (explicit `ARTEX_LLM_PROXY` unaffected); proxy input supports `socks5://user:pass@host:port` with account/password.

#### Fixed

- Transient streaming failure before any output is produced safely backoff-retries on the same provider, significantly reducing "ran only one or two tools then broke off, no summary, terminal `model_error`"; quota exhausted, context too long, and 4xx deterministic errors don't retry, still respectively handled by failover and compaction recovery.

#### Removed

- Remove LLM icon marker in conversation list, avoiding confusion with Worker status icons.

### UI

#### Added

- Tasks support global category create, rename, delete, and filter; new tasks can directly select category.
- Add global task template CRUD and right-side management drawer; new tasks can load preset description and goal, or save current content as template.
- New tasks support associating multiple source tasks and multiple enterprise asset scopes; enterprise scope auto-recognizes domain, URL, IP, CIDR, ICP, and enterprise keyword through a single multi-line text box.
- Task test assets support real-time add and delete, recording source as human, enterprise, inherited, or Agent-discovered; Worker conversation title next to shows current test assets and source summary.
- Findings page group by task and provide independent in-group pagination, support filling in vulnerability description and creating high-priority drill-in intents.
- Conversation support rename, pin, unpin, and delete; task list support batch pause and continue on current page.
- Traffic detail and tool execution detail changed to right-side drawer; HTTP message adds `Host` and highlights request line, status code, header, JSON, and markup body.
- Task list action column adds "Details" and "Pause/Continue" buttons; conversation send key can be configured in system settings (`Enter` / `Cmd+Enter`, etc.); Web search area adds proxy input.
- Conversation title right-side badge changed to show LLM config name, model ID moved to hover Tooltip; tasks can set optional name, task list adds name column and search (empty falls back to description).
- Skills page adds usage count, last used time, and missing dependency stats, for locating non-effective or uninstalled skills.

#### Changed

- Task title changed to focusable detail link; task assets, enterprise assets, finding task groups, and in-group vulnerabilities use server-side real pagination, stable sort, and exact totals.
- New enterprise uses right-side drawer, scope entry unified as multi-line text with real-time recognition, validation, and type preview.
- Main Agent input box supports auto-grow, `Enter` send, `Shift+Enter` newline, and avoids Chinese IME composition state mistakenly sending.
- Agent preview, task report, and related details uniformly use shared Markdown rendering; delete confirm, long errors, and mobile drawer width uniformly fixed.
- Task template selector displays and searches by template name, submits by template id; restore system default font size, app version uniformly displayed as `0.3.3`.
- Task total and current conversation Token only highlight input, cache read, and output values; send button uses a simpler up arrow icon.
- Task overview test scope shows enterprise name rather than id.

#### Fixed

- Plain text with dots is no longer misjudged as ICP record number.
- Fix variable directory colliding with global runtime variables (like `{{.Now}}`), causing Agent editor variable list to render duplicate keys.

#### Removed

- Remove task card independent "Enter" button, unify clicking task title to enter detail.
- Remove dashboard "New task" button, enterprise Logo URL input item, and findings count Badge at top of findings page and in task groups.
- Undo global font size 10% increase style, and remove Worker selector, model icon, and row-level Token stats in conversation list.

### Agent

#### Added

- New tasks can directly associate multiple existing tasks, real-time read-only inherit direct source tasks' goal, fact, vulnerability, completed intent, asset scope, and blackboard context.
- Blackboard read tools support on-demand query of source task nodes, facts, vulnerabilities, and execution traces; inherited nodes have source markers and all write tools refuse to modify.
- Enterprise scope tools support domain, URL, IP, CIDR, ICP, and enterprise keyword; keyword is only used as Agent scope hint, not participating in asset auto-attribution.
- Vulnerability drill-in creates a high-priority human Worker intent with asset anchors and `derived_from` edge in the original task.
- Overview adds "Goal management" card, supporting human view, add, modify, and delete goals; add or modify notifies Planner and revives the task; delete hard-deletes the goal node (cascade delete edges and anchors) but doesn't revive.
- Main Agent by default gets `steer_work` tool, can real-time inject correction instructions to running Workers without interrupting or losing progress (still verifies intent belongs to this task).

#### Changed

- Source task intents don't enter the new task's frontier; the new task continues to use independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: change to stop at `stopped`, require deletion reason, hang the reason as a fact on the intent and write to payload, planner triggers awareness on `cancelled` (preserves intent content and reason).
- Main Agent conversation changed to server activity stream driven, send failure can recover input; auto-stick bottom on opening conversation and keep last reply visible after detail lazy load.
- Task pause will terminate the current Main Agent, Planner, and Worker calls, but doesn't prevent the user from actively initiating new main Agent orchestration conversations during pause.
- Cancel, close, and stream interrupt uniformly preserve real termination reason, generated content, run rounds, duration, Token, and unreturned tool calls.

#### Removed

- Remove main Agent client optimistic message echo, avoiding duplicate messages and cross-conversation stream mixing under pause, failure, or concurrent activity.

### Constraints

#### Added

- Goal decomposition phase first extracts operating constraints (allow/deny) then decomposes goals, main Agent runtime can supplement; constraints injected at highest priority into Planner and Worker system prompts, after de-biasing diversity and broad exploration explicitly obey constraints.
- Overview adds "Constraint management" card and CRUD interface; constraint injection scope can be toggled by Planner / Worker separately (default both on, read each turn, immediate effect). New table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, old databases auto-create.

### Intercept

#### Added

- Command intercept adds model-fallback approval beyond regex/string rules: when no rule hits, the model makes `ALLOW` / `ASK` / `DENY` semantic judgment, with configurable fallback on failure and approval timeout actions; the intercept page splits into "Intercept rules / Model config" two Tabs, model judgment results marked with `[Model]` prefix and reason.

#### Fixed

- Harden adjudicator output parsing, avoiding correct `DENY` judgment being mistakenly treated as allow.

### Traffic

#### Changed

- Traffic recording changed to whole exchange written to SQLite (large body overflows to a hash-deduplicated blob bucket), text body builds trigram full-text index, supports arbitrary substring and Chinese search; delete degrades to single SQL transaction, from hours to milliseconds, and recording no longer pauses. Add large body streaming download and `traffic_search` full-text parameter; old file tree data no migration, still readable, searchable, deletable.

### Tasks and assets

#### Added

- New task adds "Asset coverage feature" toggle (default on): when off, coverage is not computed or shown, the situation graph only shows assets, doesn't auto-accumulate `task_scope`, related tools removed from Planner / main Agent; enterprise association not affected by this toggle.
- `insert_assets` adds `related` flag per asset (default `true`): only effective when coverage is on; `false` only enters the shared asset library, not counted in this task's coverage (e.g. incidentally discovered sibling sites or unrelated assets).
- Enterprise scope and task test assets uniformly support domain, URL, IP, CIDR, ICP, and keyword text recognition; domain/IP can create or reuse global assets, CIDR/ICP/keyword remain as task scope context.

### Build

- `build.sh --release` supports one-time build of Linux amd64/arm64, macOS amd64/arm64, and Windows amd64, and generates zip release packages with `skills/`, config examples, and README.
- Use Go linker to strip debug info and generate zip release packages; UPX changed to explicit optional, avoiding its self-extracting ELF segfaulting on some Linux environments at startup.
- Release Workflow runs startup smoke test on Linux amd64 binary, and provides `SHA256SUMS` with release packages.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks support associating multiple existing tasks, real-time, read-only inheriting direct source tasks' facts, vulnerabilities, completed intents, asset scope, and blackboard context; new tasks still use independent execution queue, working directory, and conversation history.
- Tasks support ordered LLM config chain. When provider quota insufficient is explicitly identified, auto-switch to next config, persisting current config, exhaustion state, and structured audit activity.
- Running and paused tasks support editing LLM config chain, reordering, and manually switching current config; auto-switch, manual switch, and whole-chain exhaustion are prompted on the task detail page.
- Running Workers support pause, resume, and cancel. Pause preserves blackboard data; cancel transactionally cleans the intent and its directly produced facts, vulnerabilities, and execution records after the Worker stops writing.
- Delete task can choose to also clean up associated assets, traffic, vulnerabilities, and task landing files, and adds delete fence, concurrency protection, and auditable delete stats.
- Add optional task concurrency cap; new tasks reaching the cap queue in FIFO, and auto-start after a running slot is released.
- Add task asset pagination, company asset pagination, task category statistics, and paginated intent Mock contract.
- Add `build.sh` single-binary build script, supporting static front-end export, resource embedding, cross-platform targets, and build version injection.

### Changed

- Task-level explicit LLM config chain coexists with the existing global LLM Pool; explicit chain continues with strict quota failover semantics, with no explicit chain falling back to Agent binding and global config rules.
- Main Agent input box supports auto-growing multi-line input, `Enter` send, `Shift+Enter` newline, and avoids Chinese IME composition state mistakenly sending.
- Planner, Worker, and main Agent share task-level LLM runtime; task chain uses the smallest context window among candidate models as the safe compaction threshold.
- Task detail page shows running, idle, and paused states per real LLM call state; source task facts, vulnerabilities, intents, asset references, and graph nodes are uniformly marked with source and kept read-only.
- Agent preview, task report, and related detail views uniformly use shared Markdown rendering component.
- LLM model config uses card and drawer interaction, model list supports independent scrolling within the drawer.
- Task pause no longer blocks main Agent conversation: main Agent orchestration conversation is independent of task pause, new messages can still be sent during pause (pause only terminates the current in-progress round).
- Worker single-run wall-clock default duration adjusted from 600s to 1200s.

### Fixed

- Remove main Agent console optimistic echo, change to pure server-side data rendering, fixing message disorder and spilling into other conversations under pause or send failure scenarios.
- Open main Agent conversation default scroll to bottom and fully display the last reply: the last reply's full content is lazy-loaded and expanded to auto-stick bottom, no longer pushed off screen.
- Fix main Agent conversation still running after pausing a task, and orchestration Agent not synchronously stopping main Agent when pausing a task.
- Fix task status badge and action button state out of sync when task is complete or Planner, Worker, or main Agent is actually running.
- Fix possible late blackboard writes or residual files between Worker cancel, task delete, and concurrent writes.
- Fix Markdown preview failure, delete confirm long text overflow, mobile width, and some task detail buttons not aligned.
- Add named termination reason for all Agent cancel paths, activity details can show canceller, terminal state, run rounds, duration, Token usage, and unreturned tool calls.
- Fix race where parent context may preemptively override the named `shutdown` reason on back-end shutdown, and Chinese activity summary byte-truncation causing garbled characters.
- Fix cancel events with partial stream output losing the real termination reason, while preserving the content already generated before cancel.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
