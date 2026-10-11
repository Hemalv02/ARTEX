# `/btw` validation records

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Independent PostgreSQL test library and data directory used; real model credentials are only injected into the independent test environment, not written into code or this record, and the product default model is not modified. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

Actual model conversations, returned objects, engineering assertions, and the raw Qwen review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, deep copy of tool parameters | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary/compaction requests do not overwrite, complete reply and terminal state published, partial reply excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-group replay, budget trimming and overflow error | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side-question parallel, bidirectional cancel isolation | Pass | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming/non-streaming, usage already on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript/activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming sub-cases |
| Persistence, pagination, idempotency, restart preserves partial answers | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear and late-write race, parent resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent/Worker archive and recovery, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| Three parent interfaces, authentication, resource ownership, Worker soft deletion | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Busy main session can side-question, independent SSE reconnect/disconnect, cancel, clear | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 per parent session / global 4 concurrency | Pass | Two `TestSideHTTP…` cases |
| Snapshot persisted before submission, restart continued question, old sessions cannot forge snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Cached configuration rejected after deletion or model change | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel and wait for final answer and usage to persist before archive | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Streaming consumer early cancel records usage only once and belongs to the side question | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| Restart auto-recovered Worker/deadline runtime context continues publishing new snapshots | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check on related packages | Pass | Commands listed below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| New frontend modules Biome | Pass | `biome check`, 3 new modules |

After configuring `ARTEX_PG_DSN` against a separate disposable database (do not point at the production database), automated checks can be reproduced:

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: the `server` package has two existing tests that fail in the temp directory cleanup phase, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting the source code from the unmodified baseline above, rerunning the `server` package in the same isolated environment also reproduces these two cleanup failures. The baseline run additionally shows an assertion failure on target node count for `TestCoreTaskLifecyclePG`; the final modified `server` regression does not have this assertion failure. Other packages pass; this time's side-question-related cases and race check pass. Baseline issues are not marked as accepted by this side-question validation, and existing assertions are not modified to hide issues.

The Next.js build output has existing multi-lockfile / workspace root inference warnings; the build completes and all pages generate successfully.

## Browser checks

Using Codex In-app Browser, connecting the independent local Go service and Next.js development server. On desktop and the 390 × 844 narrow screen, complete the following manual automated operations, checking screenshots and browser logs:

- Typing `/btw` during normal chat, main content and side question display simultaneously; desktop sidebar works normally.
- Continuous follow-up questions; after stopping the side question, the generated portion is kept; the original tool flow continues.
- Closing the panel lets the request continue, and reopening restores the completed answer; refreshing the page and empty `/btw` restore history.
- Narrow-screen Drawer's input, buttons, history, and close operations are normal, no horizontal overflow.
- Clear uses a confirmation dialog; after clearing, history disappears, main transcript and snapshot are preserved.
- Task MainAgent and two Workers each ask questions and switch; Agent tabs and history do not cross-talk.
- A blocking local model fixture keeps the Worker running; submitting `/btw` from the Worker main input box, after stopping the side question the Worker still shows real-time running and its own pause button, and the side question saves a partial answer.
- Browser error/warning logs are empty.

Controllable fixtures are used to precisely verify concurrency timing without depending on real model output speed. During debugging, two Worker runtime checks did not form an effective concurrency window (task had ended / answer ended early); fixtures were corrected and redone and passed; these initial operations are not recorded as effective passes.

## Real model conversations

Priority probe `grok-4.6`, OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. Probe HTTP 200, returning model name `grok-4.6` and `READY`, taking 2.82 seconds. First choice available, so the Tingly `glm` or Zhipu `glm-5.3` fallback chain was not enabled; these two fallback services were not validated this time.

| Scenario | Actual result |
| --- | --- |
| Asking about assets, targets, and body during main session running | Returns `redhaze.top`, homepage read and summary target, `BTW-REAL-0910`; side question complete, 16.97 seconds |
| Asking about tool basis after main session homepage read | Correctly references WebFetch 200, curl redirect 301 → 302 → 200, page title; 7.24 seconds |
| Side question asks Bash to create test file | Refuses to execute, target file not created; 7.74 seconds |
| Side question after completion does not change main context | Main transcript SHA-256 remains consistent with main activity record; side-question tool execution count is 0 |
| Truly stop/restart Go service and continue asking | Keeps the previous 3 side-question history entries, answers assets, body, and title directly from the persisted snapshot, without rerunning the main Agent |
| New session uses Grok non-streaming configuration | Correctly responds to assets and `ATOMIC-0910`; returns and saves usage: input 11734, output 138, cache_read 11520 |

The asset case's main session used WebFetch and Bash/curl to read the public homepage; the landing page is `https://id.redhaze.top/home`, and the title is "红幕科技 RedHaze Group · 全球综合集团门户". Bash temporarily stores the response in a local test file; no remote write is performed. This fact is verified separately from "the side question did not execute tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming response does not return usage. Separately sending `stream_options.include_usage=true` for verification, HTTP 200, 12 data frames, 0 usage frames. Therefore the 0 in streaming tests means the endpoint did not provide usage, not that there is no billing. Non-streaming usage, and fixture failure/cancel usage, are all saved correctly.

## Qwen review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. Provided the first three real side-question conversations, main session tool basis, and engineering assertions; returned `verdict: accept`, `concerns: []`, judging the answer consistent with assets, body, and page read evidence, and side-question tool refusal consistent with constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review scope does not include the later-added service restart and non-streaming test. Qwen's generalization of "no write" is too broad: the main session curl did create a local response temporary file, explicitly recorded above. Concurrency, zero tool execution, and transcript isolation are determined by engineering assertions; model review only helps assess answer quality.