# Changelog

All notable changes to this project are recorded in this file. The format is based on [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

## [0.3.15] - 2026-10-07

### Intercept

#### New features

- **Added built-in intercept rule "deletion-type endpoint paths"**: the previous built-in HTTP destructive-action rule only recognized the DELETE **method**, yet most apps trigger their delete endpoints with GET/POST, so a call like `curl 'http://t/api/user/delete?id=1'` would really delete the target's data. We now add a `deny` rule covering `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, and `/delta` are not falsely intercepted). Existing instances also receive it after upgrading; it can be disabled or deleted under "System → Command Intercept".
- **Token consumption of model-fallback approval can be viewed separately**: approval calls previously had no independent metering attribution, so you could only guess how much they cost. They are now metered on their own, and a usage card appears beneath the "Model Fallback Approval" toggle under "System → Command Intercept" — cumulative totals for call count, input, output, and cache read/write (five metrics), plus a mini bar chart of daily consumption over the last 30 days.

### Finding notifications

#### New features

- **Findings can be pushed to IM**: a new "System → Notifications" page lets you push scan findings to six channel types — **DingTalk, Feishu, WeCom, generic Webhook, Telegram, and email**. You can configure any number of instances of the same type, each with its own enable/disable, rate limit, and filter rules.
- **Push timing supports two modes, real-time and digest**: real-time mode sends each finding individually as soon as it matches; digest mode merges findings into a single message on a global cycle (30 minutes by default), leading with the count of new findings and their severity distribution. To do "high-severity real-time, everything else digested", create two channels configured separately.
- **Filter rules support four dimensions**: minimum severity level, limiting to a task/asset scope, finding-type keyword inclusion and exclusion (exclusion takes precedence), and whether to receive disposition-status changes (off by default). Messages carry a "View details" button whose target address is set by the global "callback URL"; leave it blank and no button is included.
- **Delivery history and manual resend**: lists the status, attempt count, failure reason, and owning channel of each delivery, filterable by channel and status; failed items can be resent with one click (a resend zeroes the retry counter).
- **Channel credentials echoed masked**: Webhook addresses, signing secrets, Bot Tokens, SMTP passwords, and the like are echoed back only as masked values with a trailing-digit hint; submitting them unchanged means "do not modify", while clearing the input field deletes that field.

#### Bug fixes

> This section records the conclusions of a security audit performed after the first version of the feature was complete; each item was first reproduced, then fixed, then backed by a regression test.

- **Fixed a mask bypass via "change the target address, keep the credentials" (critical)**: config merging unconditionally preserved the stored value for "any key not mentioned", so by changing only the address and saying nothing about the credentials, you could make the server send the real stored credentials to an arbitrary address, completely silently. We now require that once the target address changes, every credential field must be stated explicitly — either give a new value, or explicitly leave it blank to indicate it is no longer needed.
- **Fixed zero escaping of untrusted content in markdown-based channels**: finding titles and summaries come from model output and asset URLs come from scan results, so a crafted title would render as a clickable external link in DingTalk / WeCom / Feishu, and image syntax would be fetched by the client at render time, leaking the reader's IP. We now uniformly collapse content to a single line and escape markdown metacharacters, with the escaping happening at each channel's rendering exit rather than in the shared title function.
- **Fixed the SSRF surface of delivery addresses**: previously loopback, internal-network, and cloud metadata addresses were all deliverable, and the first 200 bytes of a failed response body would go into `last_error` and be echoed in delivery history, forming a semi-blind read primitive. We now defend at the **dial stage**, covering DNS rebinding and rejecting cross-host redirects; loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private networks remain allowed (a self-hosted internal SMTP relay is very common).
- **Fixed push credentials leaking through error messages**: DingTalk's `access_token`, WeCom's `key`, and the Telegram bot token are all in the URL, and on delivery failure the full URL would flow into `last_error`, delivery history, the server log, and the frontend prompt — four places. We now uniformly redact it, keeping only `scheme://host` and the underlying reason.
- **Fixed silent loss caused by marking a whole digest batch as delivered after the message was truncated**: the truncated-off findings were neither in the message nor in the failure list, yet delivery history showed success. We now package by **whole finding**, mark only those that fit into the current message as sent, and return the rest to the queue to be continued in the next message without consuming a retry; the message header also states how many more will follow in the next one.
- **Fixed a typo in the minimum-severity threshold silently disabling the filter**: when `min_severity` was mistyped as something like `hgih`, the unknown level's ordinal was 0 and the check degenerated to always-true, so the user thought they were only receiving high-severity findings while in fact every finding was being dumped into the group. We now validate the value on the write path and list the allowed values.
- **Fixed `rate_per_min = 0` (no rate limit) being unreachable**: the write-to-DB layer silently rewrote an explicit 0 into the default value, so the operator thought they had lifted the rate limit while in fact it was still being throttled. The default value is now filled in at the API layer when the field is absent.
- **Fixed digest channels completely bypassing the token bucket**: `rate_per_min` previously had no effect at all in digest mode. The number of findings claimed is now constrained by both this round's quota and an in-memory upper bound.
- **Fixed failure disposition judged by the whole batch's maximum attempt count, letting old deliveries implicate new ones**: one old delivery that had already retried twice would drag brand-new deliveries in the same batch into failed, so a new finding was permanently lost without a single retry being used. We now decide item by item.
- **Fixed deliveries with an unparseable snapshot in a digest batch being silently marked successful**: such entries were skipped at the render stage yet counted as successful along with the whole batch. They are now explicitly judged as failed with a reason given.
- **Fixed the per-round delivery count of a single channel potentially exceeding the lease duration**: in multi-instance deployments, rows whose lease had expired would be re-claimed, sent again, and have their attempt count double-incremented. We now derive the per-round upper bound backwards from "lease / single-attempt timeout", with an assertion pinning the relationship between these constants.
- **Fixed Telegram truncation possibly cutting off an HTML entity**: truncation only avoided a half-cut tag, not an entity fragment like `&amp`, which could cause the parser to reject the **entire** message. Both are now avoided.
- **Fixed the email channel judging a transient SMTP failure as permanent**: 4xx codes like a greylist `450` are a temporary rejection and should be retried later, but previously they were all judged fatal, so on a server with greylisting enabled every push would fail after its first attempt. We now distinguish by the first digit of the response code: 4xx is retryable, 5xx is a permanent failure.
- **Fixed submitting a nested structure actually writing the mask literal into the DB**: an object field like `webhook.headers` can only be masked as a whole or submitted as a whole, so stuffing the mask sentinel inside the object would store it as a real value, silently breaking authentication. Such a submission is now explicitly rejected.

#### Design notes

- **The transaction that writes a finding does only one blind INSERT**: `notification_events` and the finding are written within the same transaction, so a commit guarantees the two are atomically consistent. This INSERT deliberately does not read the channel table and does not run the user's filter rules, and it is wrapped by a `SAVEPOINT` — otherwise one misconfigured filter condition could abort the transaction and keep a high-severity finding from being stored.
- **Delivery claims work via leases rather than long transactions**: after claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and its `next_attempt_at` is pushed into the future to serve as a lease, so no database lock is held during delivery; rows left behind by a crashed process are re-claimed after the lease expires — self-healing and without infinite retries.
- **Rate limiting does not consume the retry budget**: this round's quota is computed from the token bucket first, and then that number of items is claimed. Reversing the order would waste an attempt on deliveries the rate limiter blocked, and after three budgeted attempts were burned on pure waiting the item would fall into failed.

### Test infrastructure

#### Bug fixes

- **Fixed cleanup ordering in the company ICP-attribution test cases causing permanent asset residue**: `defer d.Close()` runs first when the function returns and `t.Cleanup` runs after it, so all the cleanup statements landed on an already-closed connection, the errors were discarded, and the test assets and companies remained in the DB permanently — which would also make other cases that assert on asset counts fail for no apparent reason. We now route connection closing through `t.Cleanup` as well and register it first, and also make cleanup failures visible.

### Traffic

#### New features

- **The traffic list supports content search, filtering by path / status code / length, and column sorting**: the filter row supports filtering by **response content** (via the full-text index), **path**, **status code** (an exact value or a bucket like `2xx`), and **response-length range**, the header can sort by time, status code, and response length, and the sort preference is remembered locally. Two new indexes are added and take effect automatically after existing instances upgrade; the query contract of the Agent tool is unchanged.
- **The traffic list adds "Clear all"**: deletes all traffic records at once (ignoring the current filter) and cleans up historical host directories the index no longer records, then performs a full compaction afterward and reports how much space was actually freed. Traffic evidence already bound to a finding is stored in a separate evidence database and is unaffected.

#### Bug fixes

- **Fixed disk space not being released after deleting traffic**: the index database did not enable `auto_vacuum` when created, so the file never shrank; and because the full-text index is `contentless_delete`, a `DELETE` only writes a tombstone without reclaiming the original postings — so deleting traffic actually made the index grow (in testing, capturing 6MB of bodies → a 16MB index, still 16MB after deleting everything). Newly created index databases now enable `auto_vacuum=incremental` and, after each delete, perform full-text index merging and incremental reclamation in chunks in the background, gradually returning space to the system (the same scenario drops back to 104KB); reclamation releases the write lock between chunks and does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set when the database is created, so **an existing instance's index database is still in the old mode**, and incremental reclamation is a no-op on it (a one-line notice is printed at startup). After upgrading, such a database's tombstone growth has already stopped, and the space already occupied can be reclaimed once with "Clear all" — that step converts the database to incremental-reclamation mode, after which routine deletes take effect on their own.

### LLM

#### Bug fixes

- **Fixed a custom session header not taking effect on the "one-shot LLM call" path**: goal decomposition (round 0) and cold-node compaction are both one-shot calls that do not attach to the transcript store, so there is no session id on the context and the gateway side therefore cannot read the header — for an endpoint like opencode zen that 400s outright on a missing header, this showed up as "round 0 fails while subsequent planner rounds are completely normal". We now explicitly attach a session id stable per exploration to these two paths, which incidentally lets token usage be correctly attributed to the corresponding exploration (previously it was not recorded at all).

### Tools

#### Bug fixes

- **Fixed output truncation not taking effect for MCP and custom tools, with large results flooding the context in one go**: MCP tool returns previously went through no truncation at all, while HTTP-type custom tools, being passed an empty tool context, kept using a hard-coded 6000 characters rather than the session-configured limit. Both paths now go uniformly through `actool.Capture`: truncating by the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured, spilling the full output to disk and leaving only a leading snippet and a file pointer in the message.
- **Upgraded norma to v0.4.3**: the harness layer now uniformly performs `CaptureOnce`, so no tool (including any added in the future) will ever again flood the context by missing truncation.

### Deployment

#### Bug fixes

- **Fixed the activity stream being unreachable under a reverse-proxy deployment**: the SSE address was previously unconditionally assembled as `${hostname}:8787`, so in a deployment reverse-proxied to 443 the page loaded from `https://domain/` while SSE was pointed at port 8787, which is not open to the public network — showing up as the activity stream reconnecting repeatedly. We now distinguish by `NODE_ENV`: production defaults to same-origin, and only `next dev` keeps `:8787` to bypass Next.js's buffering of SSE; the `NEXT_PUBLIC_SSE_BASE` override is retained. The README adds reverse-proxy deployment notes.

### Findings

#### Bug fixes

- **Fixed the asset list in the "By asset" view having no scrollbar after overflow**: the outer card was given only a `max-height` and no definite height, so the scroll viewport's percentage height could not be resolved, and with many assets the list would either burst the card or be truncated with no way to scroll. We now put the height cap directly on the asset tree's native scroll container, adapting to the window height.

### Database

#### Bug fixes

- **Set an upper bound on the connection pool**: `database/sql` by default does not limit the number of connections the application itself may open, and when the pool has no idle connection it unconditionally creates a new one, climbing all the way until PostgreSQL's `max_connections` (default 100) rejects it. We now cap it at 32, and over-quota queries instead wait for an idle connection, so under the same load the behavior is slower rather than erroring; if `max_connections` is lowered, this should be lowered in step.
- **Improved connection reuse**: the default for `MaxIdleConns` is 2, so when concurrency exceeds 2 a connection is closed as soon as it is done and the next request must redo the TCP and PostgreSQL authentication handshake. We now raise the idle cap to match the total cap and add a 30-minute connection lifetime and a 5-minute idle eviction, letting the pool naturally replace long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### New features

- **The task list adds a "Findings" column**: it shows the counts for Critical / High / Medium / Low by severity tier, colors the non-zero tiers by severity, and lets you see each task's finding scale and distribution at a glance.

#### Changed features

- **The "Description / Goal" columns are narrower**: overly long content is ellipsized, the full text can be viewed on hover, and long text takes up less horizontal space in the list.

### Exploration graph and planning situational overview

#### Changed features

- **Streamlined the planning situational overview (graph_overview) and set count caps on each list**: recent facts, ended intents, pending intents, cold-region digests, and confirmed-finding details are all changed to "the latest single window + a count fallback", with what is not shown queryable on demand; this significantly reduces the LLM context size per round and avoids context bloat on long tasks (the cold region of a related task's overview is throttled along with it).

#### Bug fixes

- **Eliminated the reverse duplicate edge between digest and finding in the exploration graph after cold nodes are collapsed.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset intercept

#### New features

- **Added global asset intercept rules (blacklist)**: supports entering exact and fuzzy-match domains / IPs / URLs and CIDR ranges, with create/read/update/delete for rules and enable/disable, on a management page at "System → Asset Intercept"; by default it includes fuzzy intercepts for government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites.
- **Asset intercept is wired into the execution chain**: before dispatching an intent (`add_intent`) or inserting assets (`insert_assets`), the Agent first runs an intercept check on the target assets — an intercepted intent is not dispatched, an intercepted asset is not inserted, and the asset info and intercept reason are returned to the Agent.
- **Added task-level intercept / allow (whitelist) rules**: independent of the global rules and effective only for the current task, with the evaluation order "intercept first, then allow" — a match on intercept forbids it; if intercept does not match but the task has allow rules configured and none of them match, then it is "not allowed to test"; when no allow rules are configured the whitelist is not enabled. These can be entered when creating a task, and can also be added/edited/removed and enabled/disabled in the task details "Overview".
- **Task templates support preset categories and task-level intercept/allow rules**: a template can save a task category and a set of task-level rules, which are carried into the new-task form when the template is applied.

### Operation review

#### Bug fixes

- **Tightened the model-judge output protocol to reduce false allows caused by truncation**: the upper bound on the verdict explanation (comment) was lowered from 500 Chinese characters to 120 Chinese characters and "output only JSON, with no preamble or code block" was enforced, to avoid a verdict being truncated by `MaxTokens` and unparseable, which would then be allowed through under the model-failure policy (fail-open).

### Task archiving

#### Bug fixes

- **Archiving skips a symbolic link rather than failing the whole package**: the archive format supports only regular files and directories end to end, so previously any symbolic link in the working directory would cause the entire task archive to fail; it now skips that symbolic link and logs it, archiving the rest of the files normally (without following the link or escaping the directory tree).

### Accounts and compliance

#### New features

- **Added a "Terms of Use and Disclaimer" dialog before login**: you must check agreement before you can log in.

### License and dependencies

#### Changed features

- **The project adopts the AGPL-3.0 open-source license**, and the README's license and disclaimer notes are completed.
- **Upgraded norma to v0.4.1.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration-chain broadcast board

#### New features

- **The task details page adds an "Exploration-chain broadcast board"** (#144): it presents the task's exploration nodes (start / goal / intent / fact / finding / hint / compaction) in a timeline-stream view, with filtering by type, keyword search, ascending/descending order, pagination, and auto-refresh; while on the "newest-first first page" it is a live slot and refreshes every round, and when you leave that slot it only accumulates an unread count without disturbing your current reading, with a one-click "N new broadcasts · back to latest" to return. It groups by day, so even when paging through a long task you can tell "which day this happened".
- **Each row of the broadcast board shows the node id** (#145), making it easy to cross-reference the exploration-chain graph and locate a specific node.
- **Expanding a broadcast node's details shows upstream/downstream and anchored assets** (#147): expanding a broadcast reveals that node's upstream (where it came from) / downstream (what it produced) relationships, and hovering over a related entry pops up that node's card (type/status/source/time/summary/payload snippet); it also lists the assets anchored by that node (a type label + identifiable text). The related data is delivered along with the broadcast page, so expanding sends no extra request.
- **Broadcast-board search supports filtering by node id** (#150): beyond "content / source", the search box adds node-id matching — enter a plain number or the "#41" form shown in the UI to precisely locate the corresponding node.

### Intent management

#### New features

- **Intent deletion supports two modes, "soft delete / hard delete"** (#149): intents that are pending-claim, running, or paused can all be deleted, deletion requires entering a reason, and the deletion mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is set to "deleted" and the reason is recorded in a separate field, preserving the intent node and all of its outputs and lineage.
  - **Hard delete**: physically removes the intent, along with the exclusive descendant nodes "supported only by it" (cascading along the output / intent chain down to the leaves), to avoid leaving orphaned data; the deleted intent's token metering is archived and kept under its original dates; shared nodes (still referenced by other intents), the goal, and task-root facts are all preserved, and the hard-delete dialog shows the estimated number of nodes the cascade will affect.

  Both modes notify the planner that "this intent was deleted by the user + reason" and re-plan accordingly.

### Operation review

#### New features

- **Approval records support filtering by status and verdict source** (#139): approval records can be filtered by approval status and verdict source to quickly locate the target record.

#### Bug fixes

- **Pending approval requests load independently of the history pagination** (#133): pending requests are no longer affected by the history list's pagination and remain fully visible while browsing the history.

### Agent

#### Changed features

- **The Worker role description was changed to a generic cybersecurity-platform wording** (#138).

#### Bug fixes

- **Fixed cold-node compaction never triggering**: the Compactor was wired into the planner that runs per task, as cold-node compaction (cold-digest) previously never ran because it was not wired in; it is now restored.

### Chat

#### New features

- **Chat supports @-referencing multiple record types and scroll pagination** (#135): you can @-reference many types of records in chat, and the reference candidates support scroll-paginated loading.

#### Bug fixes

- **Long message bubbles are constrained within the conversation panel** (#137): overly long message bubbles no longer overflow the conversation area.
- **Fixed "missing uploaded file" when uploading a file before a new conversation is created**: the Composer's file picker snapshots the `FileList` into an array before clearing the input and then invokes the callback; previously the draft state had to asynchronously create the conversation first, and by the time execution resumed the `FileList` live-bound to the input had already been cleared, so the upload was missing the file field and the backend returned 400.

### Traffic

#### Bug fixes

- **The traffic-recording proxy listens only on 127.0.0.1 by default** (#129, #130): to avoid the default configuration exposing an open proxy on other network interfaces.

### Web

#### Bug fixes

- **Fixed the statically-exported task-list page losing the global header.**
- **Added the related-traffic mock for the demo finding details page, fixing a blank screen.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### New features

- **Approval records support source location** (#125): clicking an approval record's "source" jumps back to the single tool execution that triggered that approval — opening the original full conversation, auto-paginating to load up to the target position, expanding the command and result in place, centering and highlighting it in the scroll area, and keeping the preceding and following messages readable; auto-correction stops when the user scrolls manually. Across ordinary conversations, task Workers, the planner, and main-Agent segmented sessions, it locates precisely via the persisted `tool_use_id` and the task mapping (a new scope-internal call-ID index), and when the record is missing, duplicated, or ambiguously associated it shows an explicit notice rather than jumping to some other execution. When the conversation is deleted or the task is archived/the record is missing, a clear notice is given.
- **Approval records support pagination** (#115).

#### Changed features

- **Streamlined the model-review input** (#124, #125): the model-review (the LLM judge after no rule matches) input is converged to a versioned JSON of "the current single full tool call + an explicitly selected brief background + the local working directory". The background takes only the real user message (the current user message of the chat / task main Agent), the Worker no longer carries an intent summary and clears the background inherited from a higher-level Agent, and the planner and auto-triggered sessions do not fabricate a user message. It no longer carries the task description, goal, operation constraints, global exploration situation, call history, or the Worker's full intent — these are still used by the original logic for Agent execution and standalone session auditing, just not fed into operation review. All verdicts are required to output JSON with a `decision`/`comment`, and the explanation must include "the actual operation, the consequences on success, and the matched rule"; instructions in parameters or background cannot change the review policy. A snapshot and fingerprint of the input actually sent to the model are saved, and old snapshots are retained and labeled with their version — old inputs are not fabricated from current data.

#### Bug fixes

- **A model verdict wrapped in a code block is no longer silently allowed through** (#126): if the verdict JSON returned by the model was wrapped in a ``` code block, parsing previously failed and it was silently allowed through under the model-failure policy; now the code-block fences are stripped before parsing, and only if parsing still fails is the configured failure policy applied.

### Agent

#### New features

- **Added experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature that can be enabled in system settings, off by default. When enabled, noa (model-driven context compaction) takes over context compaction for the four Agent types — main Agent, planner, Worker, and chat — replacing the built-in compaction; the compacted originals are persistently archived, centrally under `<workDir>/noa/<sessionID>/` (not scattered within each task directory), with a globally unique subdirectory per session ID. On integration failure it automatically falls back to the built-in compaction without interrupting a real task; the toggle is read once per run, so switching it affects only runs started afterward.

#### Bug fixes

- **The exploration-graph tools reject rather than nil-crash when there is no task context**: calling the exploration-graph-related tools in a non-task context now returns an explicit error, no longer crashing on a nil-store dereference.

### MCP

#### New features

- **Support for legacy SSE MCP services** (#117): compatible with MCP servers that provide only the old-style SSE transport.

### Traffic

#### Bug fixes

- **Traffic search matches a record's host port-aware** (#114): `traffic_search`'s host matching now includes the port, to avoid same-host records on different ports interfering with each other.
- **A `traffic_search` description migration error no longer interrupts the subsequent reporter migration**: a single migration failure is isolated and does not affect the execution of subsequent migrations.

### Web

#### Changed features

- **Added a divider to the task-retest finding options** (#122): a divider is added between the retest-finding choices for clearer visuals.

#### Bug fixes

- **Fixed a full-page crash on the demo task details page**: in mock mode the task details "Conversation" tab would fetch `GET /api/tasks/<id>/side-questions` (the side-question history), but the mock handler had no route for it, and the read-fallback returned `[]` under the rule "a path ending in s is a collection", so `data.items` was `undefined` and the side-question hook's `merge()` threw `TypeError: t is not iterable` iterating over it; that exception occurred inside the `setItems` updater and was deferred by React to the render phase and re-thrown, where the caller's `catch` could not catch it, and the whole page was taken over by the error boundary showing "This page couldn't load". Now the mock handler explicitly returns an empty side-question history, and `sideAPI.history` also normalizes the return value (a non-array `items` is coerced to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### New features

- **Web search adds DeepSeek as an official source**: it directly reuses the currently active LLM configuration. It differs in nature from the other three sources — DeepSeek has no directly callable search API, and search exists only inside its Anthropic-compatible interface (the `web_search_20250305` server-side tool), executed by the DeepSeek server. It therefore **supports only the DeepSeek official endpoint + the anthropic protocol** (an OpenAI-protocol endpoint rejects server-side tools outright), each search **consumes an extra model call**, the request **does not go through the search egress proxy**, it **is not counted in traffic recording**, and the returned results **have only a title and a link** (no summary; use WebFetch to grab the body when needed). The settings page explains the above limitations but **does not perform validation enforcement** — whether they are met is for the user to confirm, and you can run a real check once with the "Test search" button.

### Agent

#### New features

- **The worker gains cross-work look-back ability**: added `search_all_worker_traces` (globally search for matching steps across the execution of all works in this task by keyword, without needing to know the intent_id first) and `get_worker_trace` (after locking onto one work, list its step stream, search within it by keyword in place, and fetch the full content by step_id), used to reuse observations another work saw but did not write into a fact, and to avoid repeated effort.
- **`node_detail` is pushed down to the worker**: together with the look-back tools above, once the worker has the intent_id / node id it can directly query that node's full details.
- **The planning round triggered by `add_hint` is now explicitly broadcast**: previously a new hint only folded itself into the situational overview and left the planner to discover it; now one `add_hint` records a trigger for the planner reading "a human added N strategic hints: …" (a batch counts as one, without flooding the screen item by item), and the planner is explicitly told "this round was triggered by a new hint" and sees the hint content directly.

#### Changed features

- **The global situational hint is changed to a "loosening" tone**: it encourages divergent exploration and timely reporting of cross-intent leads, rather than premature convergence.
- **Streamlined the worker's boundary wording**: being blocked the first time does not mean the area is fully explored; focus on "finish all the bypass approaches within this intent before drawing a conclusion".
- `insert_assets` removes the per-asset `related` input parameter: its only effect was to decide whether to add the asset to this task's scope, the value is not stored (re-registering the same asset voids it, and the UI does not show who was judged irrelevant), so in practice it just made the model do one more judgment that could not be retained.
- **The test scope (`task_scope`) is no longer affected by the asset-coverage toggle**: `insert_assets`'s automatic scope entry (`source='auto'`) runs whether coverage is on or off, and `add_task_scope` is always provided to the planner / in-task main Agent / goal-decomposition Agent. The scope is the task's authorization boundary and the filter baseline for asset queries, and the coverage toggle only decides whether to use it as the denominator for a metric — it should not decide whether to accumulate the scope itself. Previously turning off coverage disabled both the `auto`/`agent` write paths at once, leaving `task_scope` with only the rows manually added in the UI. `list_untested_assets` is still hidden when coverage is off (it is itself a pure coverage view).

#### Bug fixes

- **Fixed assets leaking across tasks** (#59): `list_assets` previously hard-coded the task id to 0 and queried the entire shared asset database raw, and the model had no way to write a scope filter, so it would take another task's assets (IPs especially) as this task's targets, pulling the test direction off course. Now it returns only the assets that fall within the test scope (`task_scope`) of [this task and directly related tasks], matching by **attribution** rather than literal value: a root domain in scope finds all of its subdomains/services/endpoints, and a network range in scope finds the hosts and services within that range; fetching an out-of-scope asset directly by id also returns nothing. A non-task context (Auto/pentest) has no scope to rely on and still falls back to the full database. It incidentally fixes the attribution blind spot where an IP-direct host (such as `http://1.2.3.4/api`) could not be matched by a network-range scope. The UI's "Test assets" view (filtered by task producer) is unaffected.
- **Fixed the worker's cross-work look-back tools being deleted by mistake**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default tool set, an old migration that "converges the worker's tool surface" unbound them again at startup, so the worker in fact did not get these tools. We have moved them out of that unbind list and, for databases that had already run the old migration, re-bound them to the worker once.
- **Fixed occasional `/btw` side-question write failures**: before the side_question checkpoint is written, the JSONB-unsupported NUL (`\u0000`) escape is stripped, to avoid a write error for content containing that character.

### Triggers

#### Bug fixes

- **A merged trigger session deduplicates task descriptions/goals by task**: when multiple tasks trigger a merge, the same task's description/goal is no longer concatenated repeatedly into the message, avoiding long goals piling up over and over and blowing out the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### New features

- The in-task main Agent supports **multiple sessions**: you can create, switch, and independently reset context, and interact in each session.
- Support for persistent `/btw` **side questions**: ask a follow-up without interrupting the main thread, with the Q&A stored and still present after a restart.
- `spawn_task` adds `source_task_ids`, so a new task can **read-only inherit** the source task's assets and findings.

#### Changed features

- Disabled the worker's cross-engagement memory and converged its default tool surface: reading context and cross-work review are the planner's responsibility, and the worker only handles the execution and write-back of a single intent.
- Streamlined the worker's default prompt; `get_worker_output` removes the `terminated` and `worker_name` fields; streamlined the descriptions of `insert_assets` / `list_assets`.

#### Bug fixes

- Fixed a work being cut short early due to an **idle round**: the model sometimes spends a whole round outputting only its thinking, giving neither a body nor a tool call, and at that point the harness sees a natural end (`end_turn` with no `tool_use`) and wraps up directly with `completed` + an empty summary — an intent that is not yet done is cut off midway, showing up as "the task ended normally but with no text summary at all". None of the five layers of LLM retries could reach it: it is not an error, the entry point for a same-provider safety-window retry is a "stream failure", the circuit breaker judges success directly when `err == nil`, an intent rerun only recognizes `model_error`, and the SDK's empty-response retry also cannot reach it because that layer judges emptiness by "whether any event was yielded", and a thinking delta is itself an event. Now the work's Stop hook recognizes this kind of round and injects a continuation instruction, letting the model carry on with the next step using the thinking it has already produced — we deliberately did not choose "resend as-is", because this kind of idle round is usually determined by the prompt and the shape of the context, a stable behavior rather than a random glitch, and resending would only make the model think it through once more.
- The **empty-response retry count** on the LLM "Retry and backoff" page is reused for the count (both answer "the model finished but produced no substantive content", just with different criteria and means), defaulting to 2, and setting `-1` turns it off and reverts to the old "idle round means wrap up" behavior. It is a **total** cap for one intent rather than a consecutive count: the harness itself hard-limits consecutive idle rounds to one push, and the quota is only refreshed when a real tool round has actually happened, so this number guards against loops like "tool → idle → push → tool → idle" burning the intent budget dry. When it triggers, the log records `[work <worker> · #<intent>] idle round… injecting continuation instruction (n/N)`. See the design in `docs/LLM-retry-design.md` §1.1.
- Fixed the context budget and input layout of `/btw` under a long conversation; in a non-secure context (non-HTTPS access) it degrades the generation of the side-request ID.

### Traffic

#### New features

- A finding supports **associating multiple pieces of traffic evidence**, which can be sorted, annotated, and distinguished by role (request/response/supporting).
- The report Agent **automatically associates** the relevant traffic evidence before writing the report.
- Added an Agent **traffic-binding toggle**, and completed the handoff of evidence between Agents.

### Tasks

#### New features

- Support for **session-level finding retesting**: a retest runs in a standalone Agent session, with the list and details showing the run status.

### Intercept

#### New features

- Approval records add **details and an execution audit**: you can view the tool-request context, the model/rule initial judgment, the execution output, and the argument fingerprint.

### Assets

#### New features

- Task test assets support **DSL search**.

### LLM

#### Bug fixes

- Fixed test-connection not re-sending the custom session header, causing opencode zen to return 400.

### Network

#### New features

- MCP's HTTP transport supports **skipping TLS certificate verification**, making it easy to connect to services with self-signed certificates.

### UI

#### New features

- The conversation list is grouped by Agent, with expand/collapse and independent pinning; conversations can be filtered by Agent.
- The attack-chain graph renders compaction (digest) nodes and collapses their members.

#### Bug fixes

- Fixed a blank screen caused by login credentials being out of sync.
- The intercept prompt text was changed to clearly indicate "platform control", to avoid it being misread as a defense on the target side.

### Deployment and updates

#### New features

- Support for **one-click page updates**: the system configuration page adds a "Version and updates" card, and the top bar lights up a prompt when a new version is available. The flow is: download the release package → verify `SHA256SUMS` → smoke test → stage → exit, with the daemon script relaunching to complete the swap, and the page refreshing automatically.
- Added the daemon launch scripts `start.sh` / `start.bat` as the official entry point (bundled in both the release package and the Docker image, `install.sh` unchanged), which decide whether to relaunch based on the exit code and are responsible for forwarding SIGTERM to artex. The verification and swap logic is all in Go, keeping the scripts dead simple.
- Automatic fallback on failure: if verification or the smoke test does not pass, it is discarded and the current version keeps running; if the new version fails to start 3 times in a row, it rolls back to the previous version. The settings page also has a manual rollback (note that the database schema is not rolled back).
- Updates only recognize the GitHub domain and force HTTPS, the release source is not configurable, and one-click updates are disabled in development builds. GitHub query results are cached for 30 minutes to keep the top-bar prompt from exhausting the API quota.

#### Known limitations

- Under Docker, only the program is swapped, not the image: the toolchain is not upgraded along with it, and rebuilding the container reverts to the version bundled in the image, so use `docker compose pull artex` when needed.
- The `skills/` in the release package are not synced, so built-in skills newly added in a new version do not take effect automatically.
- An update means a restart, which interrupts a running task.

### Dependencies

#### Changed features

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### New features

- The LLM page adds a "Retry and backoff" tab: both the **count** and the **interval** of the five retry layers can be configured. A model call's failure passes from the inside out through connection-establishment retry (SDK, a connection reset / timeout / 429 / 5xx before the stream starts), empty-response retry (SDK, a normal end with no content at all, openai format only), same-provider safety-window retry (a stream-break replay before any output has been delivered to the caller), round-robin circuit breaking (cool down and skip after consecutive failures reach a threshold), and intent rerun (the worker reruns the whole intent after ending in model_error) — five layers, with the outer one taking over only once the inner one is exhausted. Each layer has two knobs with uniform semantics: leave blank = use the original default count and exponential backoff; fill in a count to use that count; fill in an interval to replace exponential backoff with a fixed interval; fill in -1 = turn off this layer's retry. The first three layers follow the endpoint and can be overridden field by field in each model configuration against the global default (one that pins only the interval still inherits the global count); circuit breaking and intent rerun are process-level semantics and have only one global copy. It takes effect hot after saving, with no restart needed. Leaving everything blank is the current behavior, and an old database is byte-for-byte unchanged after upgrading (new columns default to 0, and an absent settings key means all defaults). The connection test deliberately does not carry these parameters — it has a 30s hard timeout, and layering the user-configured retries on top would only turn a usable endpoint into a timeout failure. See the design in `docs/LLM-retry-design.md`.
- Each LLM configuration supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries that HTTP header, with the header value being the current run's session id (a chat session is `conv-<id>`, a worker is `exp<x>-worker-i<intent>`, etc.), for gateways that do prompt caching / sticky routing by a session-id header. The implementation reads the session id from the request context and injects it, with no change to norma needed, so even a shared provider can emit different header values per session. The old database carries a migration, and the frontend LLM configuration dialog adds an input box.

#### Bug fixes

- Fixed saving an LLM configuration reporting `23502` outright because the session-header field was empty: that column is `NOT NULL DEFAULT ''`, and previously `NULLIF($n,'')` was mistakenly applied, writing an unfilled session header as NULL and triggering the not-null constraint; it now passes the parameter directly with empty-string semantics.

### Agent

#### New features

- The wall-clock timeout is changed to **wrap up in place**: relying on norma v0.3.6, when `MaxDuration` is reached it interrupts the running tool and continues on the live ctx for a configured number of wrap-up rounds (writing back what has been identified + a summary, with a final state of timeout), no longer relying on the worker's/planner's own `maxDur+90s` external hard ctx to kill a stuck run outright into `aborted_tools`. Chat goes through the same harness and gets the same behavior automatically, and the mainagent has no `MaxDuration` and is unaffected.
- Stall fallback: when the planner wakes on a heartbeat / a no-change wake and the whole graph has no open or running intent at all, the opening line is changed to a stall alert, clearly stating that no worker is running and no direction is queued, and this round must produce one or more mutually non-duplicate new intents (it must not produce 0 intents).

#### Changed features

- The worker prompt was restructured: the intent / startup instruction / anchored-asset raw JSON are moved into the system prompt, reassembled every round so they are never compacted away, and a continuation no longer depends on the transcript's first message being retained; the startup user message is slimmed to just the global situational overview (which can be degraded and tolerates being stale). The cost is that the system prompt mixes in per-intent data and loses cross-intent cache reuse — a deliberate trade-off for "not losing the intent".
- Streamlined the planner / worker default bodies and corrected several real-world issues: the planner distinguishes `recent_done` by status (for blocked/exhausted it checks the trace first before deciding, neither treating it as a dead end nor blindly rerunning), negative conclusions are changed to "observation / in doubt, not definitive" and evidence is checked before being trusted, and when the goal is unmet and there is no open/running intent it must produce one (a hard bottom line), with depth prioritized over coverage; the worker writes only an "observation + tentative reading" for negative-type conclusions, the verdict power belongs to the planner, and cross-intent leads are written into a fact's summary and handed to the planner rather than chased by the worker itself. A reseed appends the new defaults as a new version and switches to it, while user customizations / old versions are retained in history and can be rolled back.
- Converged the work agent's default tool set and asset write-back prompt: the worker is responsible only for the execution and write-back of a single intent, and reading context / cross-work review is the planner's responsibility, so `list_facts` / `node_detail` / `list_companies` and cross-work search (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) are removed from its default tools, leaving only `list_findings` (dedup check before reporting a finding) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt drops the stale field notes that conflict with the `insert_assets` schema (`type=tech`/`on_url`/`props`). The old database carries a one-time migration that strips the corresponding bindings, while the planner/main bindings of the same names are left alone.

### Exploration graph

#### New features

- **Exploration-graph cold-node compaction (cold-digest)**: old and long-inactive intents / facts are folded into a digest node shown in `graph_overview`, with the original nodes permanently retained and fully restorable by id (lossless storage, compacting only the presentation, with the folding reversible). The cold/hot judgment is by reverse reachability + "hot if any live branch", with an R=6-round debounce and connected-component grouping; background compaction (minor folds uncovered cold blocks, major re-sources and re-compacts to merge fragments) carries an activity recheck and a cooldown mutex, and it never goes on the hot path and never overwrites a revived node. The overview side provides `cold_digests` and an asset-indexed `cold_index`, with `expand_digest` / `expand_index` doing the restoration. The overview of a related / inherited task likewise reuses its own folded view, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are given only to the planner and the main agent, not the worker. All carry an old-database migration.
- `graph_overview` outputs `finding_list` in full: findings are the task's highest-value product and are usually few per task, so they are now carried out in full in the overview (unlike facts, which get only a recent window), letting the planner/worker see all confirmed findings at a glance each round without having to call `list_findings`. Each is slimmed to `{id, summary, evidence?, from_intent?, assets?}`, where the affected assets are given as readable content directly (url / domain / ip:port) rather than a bare id.

#### Changed features

- `graph_overview` no longer flattens `hosts`: the coverage block removes the host list (in a large-scope task each round carried up to 500 host strings, of limited value for a planning decision), keeping only `host_count`, with specific hosts queried on demand via `list_assets`. Added a top-level `done_intents_total` (the total number of ended intents), parallel to `recent_done_intents` which is truncated to ≤15, so the planner knows when deduplicating whether anything was truncated.

### Tools

#### Changed features

- `add_company_scope`'s default binding changes from worker to planner: defining a company's asset scope is the responsibility of planning / main control / Auto, and the worker only performs exploration. A new database seeds the default binding as mainagent/planner/auto, and an old database carries a one-time migration.
- The planner binds `list_assets` by default: it now has both `list_assets` (DSL full-database search) and `list_untested_assets` (untested within scope), with an old database back-filled once and not overriding a user's unbinding.

### Tasks

#### New features

- The task list adds a "Running Workers" column: it counts the number of intent nodes under that task with `state='running'`, with the same definition as the "Running Workers" in the task details overview, showing 0 when there is no running Worker.

### Network

#### New features

- Added a **global egress proxy** configuration: all target traffic can egress through a unified global proxy (http/https/socks5, supporting `user:pass`). When traffic capture is on, it acts as the upstream of the MITM recording proxy (traffic is still recorded and then egresses through the proxy, with both the intercept and pass-through paths going through the upstream and not leaking the source IP); when capture is off, it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` adds `ALL_PROXY` for socks5 support). The configuration is stored in the settings KV table (no migration needed), and the frontend system configuration page adds a "Global proxy" card, independent of the web-search proxy and the LLM proxy.

### UI

#### Bug fixes

- Corrected the semantic errors of the intent status labels: `exhausted` "exhausted" → "budget exhausted" (in fact the step / time budget was cut short midway, with only partial results written back, not that the direction is fully explored), `blocked` "intercepted" → "execution error" (in fact the model / API / network fault exhausted retries and the intent basically never truly explored, not that the target / WAF intercepted it), and added the previously missing `stopped` "stopped" (a work manually stopped by the user).

### Dependencies

#### Changed features

- Upgraded norma to v0.3.4: MCP tool output adds truncation and disk spillover (see `651b961`), and the subsequent v0.3.6 supports wall-clock wrap-up in place.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### New features

- The model configuration adds an "output cap", with an optional choice of which request field name the cap uses: the cap limits how many tokens a single reply may generate at most and is sent with every request, with 0 (default) = do not send the field and let the server's default decide; it is a different thing from the "context window" — the latter is the model's total capacity, used only locally to compute the compaction threshold and does not appear in the request. The field-name toggle is meaningful only for the `openai` (Chat Completions) format: blank (default) sends `max_tokens`, which almost all compatible gateways recognize; OpenAI's official reasoning models (the o-series / GPT-5) conversely recognize only `max_completion_tokens` and report `unsupported_parameter` on receiving `max_tokens`, so such endpoints need to be manually switched to the new field. Each of the two formats fixes its field name (Anthropic is `max_tokens`, the Responses API is `max_output_tokens`), so for a non-openai format the toggle is grayed out and cleared on save. We also wired up a path that had never been connected before: the planner / worker / chat / main Agent / goal-decomposition — none of these five had ever set an output cap, the openai family simply did not send it, Anthropic fell back on the SDK's 8192, and merely adding the config option without connecting this path meant a filled-in number would not take effect; now the value is resolved per round like "streaming output" by configuration, so after a failover switch it takes effect on the next round. After upgrading, an old database's behavior is completely unchanged (the new column defaults to 0 and the field name is blank).
- When clicking "Test" in the background, the status code and the gateway's raw response body of each HTTP attempt are logged to the server log (the body is trimmed to 4K), to help diagnose cases like 401, quota text, empty frames, and a returned HTML page, rather than seeing only the UI-collapsed ok / err. This log does not depend on the "LLM recording" toggle.

#### Changed features

- The task LLM configuration chain is changed to be editable in any state, no longer restricted to "running / paused / configuration chain exhausted": after a task ends (done / failed / timeout) the main Agent conversation still goes through this chain, and when a model on the chain had a problem it previously could neither be changed nor continue interacting. The final-state interception in both the backend HTTP and the DB transaction is removed, and the frontend dialog opens editing and saving for final-state tasks too. When saving a final-state task, quota-blocked intents are no longer reopened (those intents would be moved to open, with no worker to execute them and no longer meeting the "rerun intent" condition), and to keep running you still go through rerun intent / add goal, which pull the task back into the running state.

### Agent

#### Changed features

- "Send a message to a running Worker to adjust direction" is changed to reuse the existing pause / resume + transcript resume mechanism, behaving the same as the main Agent conversation: previously it went through a self-built intervention-persistence protocol that touched the scheduling barrier, the resume flow, and a dozen-plus activity-query filters. Now the message is injected into the next round's input, the intent runs directly in a dedicated goroutine, unconstrained by the 3-slot worker pool and running as soon as sent; the frontend keeps the message box and restores the "continue directly" button, with sending now carried over SSE. There is no DB schema change. The trade-off: the message is stored only in memory with no crash recovery, and when sending, that task may momentarily exceed concurrency by one worker (a low-frequency scenario, acceptable).

### Tasks

#### Bug fixes

- Fixed a memory-exhaustion (OOM) crash when archiving a large task: archiving is changed to stream the snapshot to disk, no longer reading the whole task into memory at once; it also completes three recovery gaps on the cold-archive path — on a modern install, traffic is stored only in SQLite, and previously a crash between the PostgreSQL commit and the SQLite commit would leave an already-archived task's traffic stranded in hot storage with no way to recover it, and now a staging log is written whether or not there is a historical directory, and a missing `journal.json` is also treated as discardable.
- Fixed the system lagging when there are many tasks: there was a noticeable delay loading conversations and loading the UI. The queries for the task list, task context, and exploration records are all optimized, and the frontend's dashboard, conversation page, and task details page reduce duplicate requests in step.

### Assets

#### Changed features

- Removed the "at most 256 rules" limit on a company's asset scope: a company that enters its scope IP by IP / domain by domain easily hits this cap, and once hit it could only be split into multiple companies, while the scope itself is not slower for having more entries. The cap is removed in all three places — backend, frontend, and demo mock — while a single rule is still limited to 1024 characters, and the 2 MiB request-body cap is kept as a fallback (roughly forty to fifty thousand rules).

### Skills

#### Bug fixes

- Fixed a skill package upload reporting "Upload failed: zip: unsupported compression": the Go standard library only has built-in Store / Deflate decompressors, so bzip2 and Zstandard packages written by compression software at a non-default level could not be unpacked at all. We now add these two decompressors (a pure-Go implementation, introducing no new external dependencies); for methods that genuinely cannot be unpacked — Deflate64 / LZMA / XZ / PPMd and the like — and for encrypted packages, we now report a Chinese prompt before decompression, naming directly which file used which compression method and how to repackage it, rather than throwing the underlying English error at the user.
- Fixed the in-skill filename validation judging Chinese filenames as illegal: the path validation was originally an ASCII allowlist of `[A-Za-z0-9-_./]`, so if the package contained even one file with a Chinese name (such as `参考/说明.md`), the whole upload would fail with "the package contains an illegal path". It is changed to a Unicode blocklist: filenames in various languages and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments are still rejected, with the directory-traversal protection behavior unchanged. Skill names are likewise loosened — ASCII is still limited to lowercase letters, digits, and hyphens (the agentskills.io spec), non-ASCII letters such as Chinese can be used directly as a skill name, but spaces, dots, and path separators are not accepted.
- Fixed Chinese packages produced by Windows compression software having garbled filenames or being rejected wholesale after decompression: such zips do not set the UTF-8 flag bit and write filenames in GBK, and we now decode with a GBK fallback before running the path validation. At the same time, a quoted frontmatter like `name: "中文技能"` can now also have its skill name read correctly.

### UI

#### New features

- The findings page adds an "All findings" flat view and makes it the default, switched with the original "Grouped by task" view via a page-header Tab: the grouped view requires expanding task by task to see the findings, and when you just want to scan the cross-task list it is instead a detour. The flat view is one big cross-task table (10/20/50/100 per page), behaving exactly like the grouped view — checkbox export, in-row expand to see evidence and the detailed report, in-row editing of name/category/severity, changing the disposition status, drilling in, and deleting. The two views share the stat cards and the filter bar, switching does not lose the filter, and the current view and filters are both recorded locally; polling only hits the current view, and in-row changes are written to both caches in sync, so switching over you do not see stale data.
- The findings page adds a "By asset" view: on the left is an asset tree (company → root domain / IP → subdomain → service → endpoint, with the company layer appearing only when the asset genuinely has an attribution), and on the right are the findings under the entire subtree of the selected node, sharing the same table and the same set of filters as the other two views. The tree collects only assets that have findings, and the ancestor chain is filled in on demand (when a finding hangs only on the deepest endpoint, the layers above can still be assembled), and the count on a node is the subtree aggregate deduplicated by finding; findings whose asset has been deleted or that never had an associated asset go into an "Unassociated assets" bucket. Unlike the other two views, the asset view does not poll — it queries only when you enter the view, change filters, add/delete/edit findings on the page, or click the refresh button on the tree, since the left tree is a navigation structure with no need to recompute every 5 seconds. Each layer of the tree shows only the increment relative to the layer above (a subdomain drops the root-domain suffix, a service shows `https :443`, and an endpoint shows only the path), with the full value in the hover tooltip and the breadcrumb; when there are too many nodes, the endpoint/service levels of a whole layer are dropped with a notice, and the counts are still tallied into the upper layer.

#### Bug fixes

- Fixed conversation records being squeezed and not fully visible in the mobile conversation details: on a narrow screen the conversation list is collapsed, yielding the width to the records themselves.

### Dependencies

#### Changed features

- Upgraded norma v0.3.2 → v0.3.3: added `Config.MaxTokensField`, letting OpenAI Chat Completions' output cap be sent as `max_completion_tokens` instead (reasoning models recognize only this key). The two keys are mutually exclusive, only one is sent, and the default is still `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security warnings.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Bug fixes

- Fixed the "non-streaming" toggle reverting to "streaming" after being saved and reopened (#69): the configuration-list API's DTO was missing the `streaming` field, the response never returned it, and after reading `undefined` the frontend uniformly fell back to the default of streaming; the actual write and DB storage were fine, it just could not be read back. The DTO adds the field back (without `omitempty`, so `false` must also appear in the response).
- Test-connection is changed to verify the model actually replies and to test using that configuration's real send/receive mode (#65): previously it only judged whether the HTTP succeeded, so a request that went through but with a zero reply from the model (thinking burned the budget / the body was swallowed by the safety policy / the compat layer dropped `content`) also reported "connection successful", divorced from the "no reply" behavior in the conversation; and the test always went through streaming, so a non-streaming configuration was actually testing a different channel. Now an empty reply is judged a failure outright, on success the model's reply is shown in the prompt, and the current streaming toggle is carried into the test, keeping "test passed" consistent with "the conversation works".

### Agent

#### Changed features

- `list_facts` is changed to pagination + keyword filtering, to avoid blowing out the context on a single call when there are many facts (#74): it returns the latest 20 by default, supports `limit` (cap 100) / a `before` cursor / a `q` summary keyword, and returns `{facts, total, has_more, next_before}`; an overly long single summary is truncated by character count (the full text still goes through `node_detail`). The worker / planner prompts are synced to the paginated semantics. The old database flushes the new-parameter schema into the tool-catalog table via a one-time migration (`SeedTool` inserts on first-insert only, otherwise the tool-management page shows "no parameters").

### Tools

#### New features

- The "Tool execution" page adds tool-call count statistics (#72): the toolbar's "Statistics" button opens a dialog showing per-tool the call count, share, and failure count (by count, descending); it follows the list's task / keyword filter, counts the whole result set rather than the current page, and fetches only when the dialog is opened.

### Dependencies

#### Changed features

- Upgraded norma v0.3.1 → v0.3.2: fixed reasoning/refusal being silently dropped in OpenAI-family responses (Responses adds `reasoning_text`), deduplicated the three names of the reasoning field, added bounded retries for empty responses (zero content blocks), and fixed the gateway 400 caused by a compaction boundary cutting a tool pairing (also fixing pre-existing orphans already baked into the transcript).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### New features

- Each LLM configuration supports a streaming/non-streaming toggle (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, returning the complete JSON at once), which can work around some gateways' poor SSE implementations (empty frames, dropped thinking-field frames), at the cost of losing real-time progress and real-time token counting during a run. The worker / planner / mainagent / chat / goals all take the value dynamically from the currently active configuration; the three Provider-wrapper layers — `llmpool` / `llmrec` / the task runtime — are all compatible with non-streaming; `llm_profiles` adds a `streaming` column and `ALTER`s an old database (defaulting to `true`, so old configurations are unaffected).
- Support for LLM configurations in the OpenAI Responses API format: each configuration adds a third format, `openai-responses` (hitting `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` is normalized, with the default model `gpt-5`; `llm_profiles`'s `format` constraint adds `openai-responses` and idempotently migrates an old database; and the frontend format dropdown adds "OpenAI (Responses API)". Depends on the norma v0.3.1 upgrade (which includes a `reasoning_content` passthrough fix).
- Recording of the raw HTTP of LLM requests/responses: the real wire body is captured at the HTTP transport layer, preserving the tool schema, `tool_use` blocks, and raw SSE frames that the normalized view cannot see; each of norma's internal retry attempts is preserved one by one; `llm_records` adds the two columns `raw_request` / `raw_response` and `ALTER`s an old database; and the recording page's details panel adds a "Raw" view toggle and request/response copy buttons (compatible with the non-secure-context `execCommand` fallback).

### Conversation

#### Changed features

- The conversation-list multi-select is changed to a "multi-select" mode toggle: by default the list no longer has a persistent per-row checkbox (a cleaner look), and the header is changed to a "N total + Multi-select" button; clicking "Multi-select" enters selection mode (checkboxes, select-all, batch delete), "Done" exits and clears the selection, a batch delete that fully succeeds exits automatically (if there is a failure it stays in selection mode for an easy retry); single-item rename / pin / delete still go through the per-row ⋯ menu.

### UI

#### Changed features

- Enhanced the experience of task management, conversation operations, and traffic viewing (#57): the task-list column sort preference can be persistently remembered, in-row rename is triggered directly by an icon (no longer via a menu), and the sheet interaction and traffic-viewing details are refined.

#### Bug fixes

- Worker asset labels show only domains and IPs, and handle the empty-label case correctly.

### Agent

#### New features

- The planner adds a "quantified acceptance check" when judging a goal: when the goal contains a quantifiable acceptance condition (asset test coverage reaching X%, obtaining N flags, gaining a certain privilege), before `prove_goal` it must check the measured value in `graph_overview` (`coverage.pct`, the finding count, etc.), and if the measured value falls short, `prove_goal` is forbidden and it instead dispatches intents to close the gap, not marking it met early on the grounds of "the main part is done / basically achieved". This fixes the issue where a goal requiring 100% coverage was judged complete at a measured 40%.

#### Changed features

- Rewrote the basis for "0 intents this round" in the planner prompt: the original text described 0 intents as "the most common and most important principle", which would make the planner wrap up prematurely while the goal was unmet and there were still untested surfaces in scope. It is now changed so that 0 intents should only be produced in two specific cases — ① every direction thought of is already covered by an open/running/recent_done intent; ② the next step depends on the output of a currently running work and that output has not yet appeared (one should wait for it to finish and re-plan at the next wake after the graph updates). And a reverse constraint is added: when there genuinely is a new direction not covered and not dependent on a running work, or the goal is unmet and there are still untested surfaces, do not wrap up on the grounds that "0 intents is common".
- Strengthened the "evidence threshold for negative conclusions" in the worker prompt: for negative conclusions like "not injectable / port closed / no login entry" that may make the planner abandon a whole direction, it is required to exhaust the reasonable approaches within that intent (changing encoding/parameters/paths/methods) before drawing a conclusion; if the approaches are not exhausted or the evidence is weak it must be marked `confidence=inferred`, to avoid welding a whole route shut with a hasty `observed` negative (an incorrect negative early in a task especially pulls the direction off course and is hard to self-heal).
- The above planner / worker default-prompt changes are appended as new versions and switched to the current version via a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each with a settings-flag guard); old versions are retained in the version history, and a user who has customized the prompt can restore it from the version record.

### Operations

#### New features

- Added `reset-password.sh` to reset the admin (the username is fixed as `ARTEX`) password: it supports both local and docker deployments, and the connection info can be specified explicitly or read automatically from `--dsn`/`$ARTEX_PG_DSN`/`config.json`; it uses `pgcrypto` within the database to generate a bcrypt hash compatible with the backend login and writes it back to `settings.auth.password_hash`, with no service restart needed after the reset. The password is passed in via an environment variable and does not enter the process argv, and it is escaped to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### New features

- After checking multiple tasks in the task list you can batch-change their category, the target category supports "Uncategorized" to move tasks out of the current category, the whole batch write completes within one transaction, and the only failed items will be tasks that were already deleted after being checked.

#### Changed features

- The "Task category" for a new task is changed from a dropdown selection to a searchable input box: typing filters the existing categories, a name not in the database is created and selected instantly on Enter (or by clicking "Create" in the dropdown), and the selected item is shown as a removable tag; it is still limited to a single category.

### Conversation

#### Bug fixes

- Fixed the case where a session had selected an LLM configuration but there was no globally active configuration, and sending a message was blocked by "LLM not configured, cannot converse". The pre-send check originally looked only at the globally active configuration, while the actual execution prefers the session's selected configuration, so the two resolution logics were inconsistent; they are now unified into the same one (the session/Agent binding takes precedence, with the global as a fallback).
- The prompt when conversation is unavailable is broken out by state: no configuration at all prompts "add a configuration", a configuration that exists but is not active prompts "activate one or assign one to this session", and it no longer always shows "not configured", making it easier to locate.

### Agent

#### New features

- For a task whose goals are all achieved, an intent dispatched by the main Agent via `add_intent` can be claimed and executed directly by a Worker and stops once done: at this point the Planner no longer runs (a task with no open goal does not enter planning), avoiding it re-judging the goal as achieved and canceling the just-dispatched intent; once the frontier is drained the task returns to completed. Before dispatching an intent, the main Agent asks the user, based on the intent content, whether to register it as a formal goal, and registering it returns the task to regular autonomous planning.

#### Changed features

- When `get_worker_trace` / `get_task_worker_trace`'s `step_ids` exceeds the per-call cap (5), it no longer errors outright, but returns the full content of the first 5 steps and reports via `returned_step_ids` / `omitted_step_ids` / `notice` which ones were taken this time and which remain, with a prompt "no need to fetch more if these are enough". Duplicate and illegal ids passed in are deduplicated and removed before counting.

#### Bug fixes

- The task list is changed to descending by task id (newest-created first), replacing the original sort by creation time. Multiple tasks created at the same instant have identical timestamps and sort unstably, and combined with the 10-second polling and the random traversal of an in-memory map, they would frequently swap positions in the list.

### Assets

#### Bug fixes

- Fixed company-attribution recomputation being broken by a single row of dirty data: when `assets.ip` stored a hostname (written by the Agent or the asset API), `a.ip::inet` would throw `22P02`, causing adding/modifying a company asset scope and deleting a company to all fail and roll back. It is changed to the safe conversion `try_inet()`, which skips an illegal value rather than interrupting the whole statement.
- An unparseable `ip` is no longer silently skipped: after a company scope is saved, it explicitly reports how many assets have an `ip` that is not a valid IP (including the specific id and value), noting that these assets will not be attributed by the IP/CIDR rules; the same information is also written to the server log, covering the paths with no frontend response (deleting a company, scopesentry sync, Agent writes).
- The IP/CIDR matching of a task's test scope is likewise changed to `try_inet()`, replacing the original regex guard that only checked the character set (a hostname that is all hex letters, like `abc.def`, previously still slipped through and triggered the same error).

#### Changed features

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts a hostname: `insert_assets` and the asset API return an error with an `index` per item and give the correction method directly (use `type=subdomain` and fill in `domain`, or resolve the A/AAAA records first), so the Agent can self-correct and reinsert. Other assets in the same batch are unaffected and are stored as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### New features

- A running Worker supports per-item pause, resume, and cancel; a pause preserves the intent, facts, and findings, and a cancel transactionally cleans up the current intent and its direct products after the execution exits.
- Added the `paused` intent status, an execution fence, and named termination reasons, to prevent a late blackboard write after a pause, cancel, or task deletion.
- Added an optional task concurrency cap; new tasks, resumes, finding drill-ins, and queue backfill all go through a unified, persistent FIFO admission path.

#### Changed features

- The default wall-clock duration for a single Worker run is changed from 600 seconds to 1200 seconds; a timed-out task, after being revived, re-times from its next real execution.
- The real execution state of the Worker, Planner, and main Agent uniformly drives the task status badge, fixing the issue where the task still showed "idle" while the Worker was running.
- The Worker session keeps per-item controls and a current-session token summary, and the model name is moved to the right of the current session title.

#### Removed features

- Removed the Worker multi-select, select-all, and batch pause/resume UI, and also removed the Worker batch-control API and the Mock contract.
- Removed the Token badge and Tooltip in a Worker list row; the underlying full Token ledger and the task-aggregation API are still retained.

### LLM

#### New features

- Tasks support an ordered LLM configuration chain; when a provider is clearly identified as out of quota it automatically switches to the next configuration, persisting the current configuration, the exhausted state, and an error summary.
- Running and paused tasks support editing the full configuration chain, adjusting the order, and manually switching the current configuration, which takes effect from the next LLM call.
- Automatic switch, manual switch, and whole-chain exhaustion are written to structured system activity and pop a deduplicated reminder via the task activity stream.
- Added a task role-model resolution API, uniformly resolving the configuration and model that the Main Agent, Planner, and Worker will use for their next call.
- Added an optional global LLM Pool, supporting a configured call order, a specified model's failure fallback, health status, cooldown recovery, and a manual reset.
- Added an always-on LLM usage ledger, aggregating input, output, and cache tokens by task, session, model, and configuration.

#### Changed features

- Goal decomposition, the Planner, the Worker, and the main Agent share a task-level LLM runtime (the resolution priority is shown below).
- Failover responds only to an explicit quota, balance, or billing error; an ordinary rate limit, authentication, network, server, or context error does not switch incorrectly.
- The LLM configuration page is changed to a configuration-card list with a right-side drawer for editing, and provides the Pool's round-robin order, priority, exclusions, health status, and recovery actions.
- The configuration-chain-exhausted message supports wrapping on a narrow screen; the current model is changed from an icon in the session list to a text label and a full Tooltip next to the current session title.
- The order in which each in-task role resolves its LLM is changed to "Agent binding → task configuration chain → global": a role with an explicitly bound model always runs on that model, only an unbound role falls to the task chain, and when the task chain is empty it falls to the global.
- An outbound proxy that is left blank means direct connection and no longer falls back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); the proxy input supports `socks5://user:pass@host:port` with credentials.

#### Bug fixes

- A transient streaming failure before submission (before any output has been produced) is safely backoff-retried on the same provider, significantly reducing cases of "only ran one or two tools then cut off, no summary, final state `model_error`"; quota exhaustion, over-long context, and deterministic 4xx errors are not retried and are still handled respectively by failover and compaction recovery.

#### Removed features

- Removed the LLM icon-style marker in the session list, to avoid confusion with the Worker status icon.

### UI

#### New features

- Tasks support the creation, renaming, deletion, and filtering of a global category; a new task can select a category directly.
- Added global task-template CRUD and a right-side management drawer; a new task can load a preset description and goal, and can also save the current content as a template.
- A new task supports associating multiple source tasks and multiple company asset scopes; a company scope automatically recognizes domains, URLs, IPs, CIDRs, ICPs, and company keywords through a single multi-line text box.
- Task test assets support real-time adding and deleting, recording sources such as manual, company, inherited, or Agent-discovered; the current test assets and a source summary are shown next to the Worker session title.
- The findings page is grouped by task and provides independent in-group pagination, supporting filling in a description for a finding and creating a high-priority drill-in intent.
- Conversations support rename, pin, unpin, and delete; the task list supports batch pause and resume on the current page.
- Traffic details and tool-execution details are changed to a right-side drawer; the HTTP message completes the `Host` and highlights the request line, status code, Header, JSON, and markup-language body.
- The task-list action column adds "Details" and "Pause/Resume" buttons; the session send key can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and the Web search area adds a proxy input box.
- The badge to the right of the session title is changed to show the LLM configuration name, with the model ID moved to the hover Tooltip; a task can be given an optional name, and the task list adds a name column and search (falling back to the description when blank).
- The skills page adds use count, last-used time, and missing-dependency statistics, to help locate a skill that is not in effect or not installed.

#### Changed features

- The task title is changed to a focusable details link; task assets, company assets, finding task groups, and in-group findings use real server-side pagination, stable sorting, and an exact total count.
- Adding a company is changed to a right-side drawer, and scope entry is unified to multi-line text with real-time recognition, validation, and a type preview.
- The main Agent input box supports auto-growth, `Enter` to send, and `Shift+Enter` for a newline, and avoids a mistaken send during a Chinese IME composition state.
- The Agent preview, task report, and related details uniformly use a shared Markdown render; the delete confirmation, long errors, and mobile drawer width are uniformly fixed.
- The task-template selection box displays and searches by template name and submits by template ID; the system's original font size is restored, and the app version is uniformly shown as `0.3.3`.
- The task total and current-session tokens highlight only the input, cache-read, and output values; the send button uses a cleaner up-arrow icon.
- The task overview's test scope shows the company name rather than the ID.

#### Bug fixes

- Ordinary text containing a dot is no longer mistaken for an ICP filing number.
- Fixed the issue where a name collision with a global runtime variable (such as `{{.Now}}`) in the variable catalog caused the Agent-editor variable list to render a duplicate key.

#### Removed features

- Removed the task card's standalone "Enter" button, unifying on clicking the task title to enter the details.
- Removed the dashboard's "New task" button, the Logo URL input when adding a company, and the finding-count Badge at the top of the findings page and in task groups.
- Reverted the style that enlarged the global font by 10%, and removed the Worker selection box, the model icon, and the row-level Token statistics in the session list.

### Agent

#### New features

- A new task can directly associate multiple existing tasks, inheriting in real time and read-only the goals, facts, findings, completed intents, asset scope, and blackboard context of the direct source tasks.
- The blackboard-read tools support querying source-task nodes, facts, findings, and execution traces on demand; inherited nodes carry a source marker and all write tools refuse to modify them.
- The company-scope tools support domains, URLs, IPs, CIDRs, ICPs, and company keywords; keywords serve only as an Agent scope hint and do not participate in automatic asset attribution.
- A finding drill-in operation creates, in the original task, a high-priority manual Worker intent with an asset anchor and a `derived_from` edge.
- The overview adds a "Goal management" card, supporting manual viewing, adding, modifying, and deleting of goals; adding or modifying notifies the Planner and revives the task, while deleting hard-deletes the goal node (cascading edges and anchors) but does not revive it.
- The main Agent gains the `steer_work` tool by default, which can inject a real-time corrective instruction into a running Worker without interrupting it or losing progress (it still verifies that the intent belongs to this task).

#### Changed features

- Source-task intents do not enter the new task's frontier, and the new task continues to use an independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: it is instead stopped to `stopped`, a deletion reason is required, the reason is hung onto that intent as a fact and written into the payload, and the planner perceives it via a `cancelled` trigger (keeping the intent content and reason).
- The main Agent conversation is changed to be driven by the server-side activity stream, and a failed send can restore the input; opening a session auto-sticks to the bottom and keeps the last reply visible after the details lazy-load.
- Pausing a task terminates the current Main Agent, Planner, and Worker calls, but does not prevent the user from actively starting a new main-Agent orchestration session during the pause.
- Cancel, close, and stream interruption uniformly preserve the real termination reason, the generated content, the number of run rounds, the elapsed time, the tokens, and the unreturned tool calls.

#### Removed features

- Removed the main Agent client's optimistic message echo, to avoid duplicate messages and cross-session stream mixing under pause, failure, or concurrent activity.

### Constraints

#### New features

- The goal-decomposition stage first extracts operation constraints (allow/deny) before decomposing the goals, and the main Agent can supplement them at runtime; constraints are injected into the Planner and Worker system prompts at the highest priority, and after de-biasing, diversity and surface-expansion exploration explicitly obey the constraints.
- The overview adds a "Constraint management" card and add/edit/delete APIs; the constraint-injection scope can be toggled separately for Planner / Worker (both on by default, read every round, effective immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, and an old database auto-creates it.

### Intercept

#### New features

- Beyond regex/string rules, command intercept adds a model-fallback approval: when not a single rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, with a configurable fallback action on failure and approval-timeout action; the intercept page is split into two tabs, "Intercept rules / Model configuration", and the model's verdict result is annotated with a `[model]`/`[模型]` prefix and a reason.

#### Bug fixes

- Hardened the judge's output parsing, to avoid a correct `DENY` verdict being mistaken for an allow.

### Traffic

#### Changed features

- Traffic recording is changed to persist the whole exchange into SQLite (a large body overflows into a hash-deduplicated blob bucket), with a trigram full-text index built on the text body, supporting arbitrary-substring and Chinese search; deletion degenerates into a single SQL transaction, dropping from hours to milliseconds and no longer stalling recording in the meantime. Added large-body streaming download and a `traffic_search` full-text parameter; old file-tree data needs no migration and is still readable, searchable, and deletable.

### Tasks and assets

#### New features

- Task creation adds an "asset coverage feature" toggle (on by default): when off, coverage is not computed or shown, the situational graph shows only assets, `task_scope` is not accumulated automatically, and the related tools are removed from the Planner / main Agent; company association is unaffected by this toggle.
- `insert_assets` adds a `related` marker per asset (default `true`): it takes effect only when coverage is on, and `false` only enters the shared asset database and does not count toward this task's coverage (such as an incidentally discovered side-site or an unrelated asset).
- The company scope and task test assets uniformly support text recognition of domains, URLs, IPs, CIDRs, ICPs, and keywords; a domain/IP can create or reuse a global asset, while a CIDR/ICP/keyword is retained as task-scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one go, and produces a zip release package with `skills/`, a configuration example, and the README.
- Uses the Go linker to strip debug information and produce a zip release package; UPX is changed to be explicitly optional, to avoid its self-extracting ELF causing a segfault at startup in some Linux environments.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and provides `SHA256SUMS` along with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- A new task supports associating multiple existing tasks, inheriting in real time and read-only the facts, findings, completed intents, asset scope, and blackboard context of the direct source tasks; the new task still uses an independent execution queue, working directory, and conversation history.
- Tasks support an ordered LLM configuration chain. When a provider is clearly identified as out of quota it automatically switches to the next configuration, persisting the current configuration, the exhausted state, and structured audit activity.
- Running and paused tasks support editing the LLM configuration chain, adjusting the order, and manually switching the current configuration; automatic switch, manual switch, and whole-chain exhaustion are shown on the task details page.
- A running Worker supports pause, resume, and cancel. A pause preserves the blackboard data, and a cancel transactionally cleans up that intent and its directly produced facts, findings, and execution records after the Worker stops writing.
- Deleting a task can optionally also clean up the associated assets, traffic, findings, and task-landed files, and adds a deletion barrier, concurrency protection, and auditable deletion statistics.
- Added an optional task concurrency cap; a new task that reaches the cap is queued FIFO and starts automatically once a run slot is freed.
- Added task-asset pagination, company-asset pagination, task-category statistics, and a paginated-intent Mock contract.
- Added the `build.sh` single-binary build script, supporting a static frontend export, resource embedding, cross-platform targets, and build-version injection.

### Changed

- The task-level explicit LLM configuration chain coexists with the existing global LLM Pool; the explicit chain continues to use strict quota-failover semantics, and when there is no explicit chain it follows the Agent-binding and global-configuration rules.
- The main Agent input box supports auto-growing multi-line input, `Enter` to send, and `Shift+Enter` for a newline, and avoids a mistaken send during a Chinese IME composition state.
- The Planner, Worker, and main Agent share a task-level LLM runtime; the task chain uses the smallest context window among the candidate models as the safe compaction threshold.
- The task details page shows running, idle, and paused states by the real LLM-call status; a source task's facts, findings, intents, asset references, and graph nodes are uniformly marked with their source and kept read-only.
- The Agent preview, task report, and related detail views uniformly use a shared Markdown render component.
- The LLM model configuration uses a card-and-drawer interaction, and the model list supports independent scrolling within the drawer.
- Pausing a task no longer blocks the main Agent conversation: the main Agent orchestration session is independent of the task pause, and new messages can still be sent during a pause (the pause only terminates the round currently in progress).
- The default wall-clock duration for a single Worker run is changed from 600 seconds to 1200 seconds.

### Fixed

- Removed the optimistic echo of the main Agent console and changed it to pure server-side data rendering, fixing message disorder and content leaking into other sessions under scenarios like a pause or a send failure.
- Opening a main Agent session scrolls to the bottom by default and fully shows the last reply: after the last reply's full content lazy-loads and expands, it auto-sticks to the bottom and is no longer pushed off the screen.
- Fixed the main Agent session still running after a task was paused, and the issue where the orchestration Agent did not stop the main Agent in sync when pausing a task.
- Fixed the issue where the task status badge and the action-button state were out of sync when the task completed or the Planner, Worker, or main Agent was actually running.
- Fixed the issue where a late blackboard write or residual files could be produced between a Worker cancel, task deletion, and a concurrent write.
- Fixed the issues of Markdown preview failing, the delete confirmation's long text overflowing, mobile width, and some task-details buttons not being aligned.
- Added a named termination reason to all Agent cancel paths, so the activity details can show the canceler, the final state, the number of run rounds, the elapsed time, the token usage, and the unreturned tool calls.
- Fixed a race where the parent context could preempt and overwrite the named `shutdown` reason when the backend shut down, and the issue of Chinese activity summaries being garbled by byte-boundary truncation.
- Fixed the issue where a cancel event with partial streaming output lost the real termination reason, while preserving the content generated before the cancel.

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
