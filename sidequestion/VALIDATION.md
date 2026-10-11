# `/btw` validation record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Uses a dedicated PostgreSQL test database and data directory; real model credentials are injected only into the isolated test environment, are not written into the code or this record, and the product's default model was not changed. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

The real model conversations, return objects, engineering assertions, and Qwen's raw review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, deep copy of tool arguments | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests do not overwrite, complete reply and terminal state published, half-finished reply excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model-pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-item replay, budget trimming and over-limit error | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side parallelism, bidirectional cancel isolation | Pass | blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, existing usage on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript / activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subcases |
| Persistence, pagination, idempotency, partial answer preserved across restart | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs. late-write race, parent-resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archiving and recovery, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| The three parent interfaces, authentication, resource ownership, Worker logical deletion | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| A busy main session can still side-question, standalone SSE reconnect / disconnect, cancel, clear | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 per parent session / 4 global concurrency | Pass | two `TestSideHTTP…` cases |
| Snapshot persisted before submission, follow-up after restart, old session cannot forge a snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Reject continuation after a cached configuration is deleted or the model changes | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel before archiving and wait for the final answer and usage to persist | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Usage recorded only once, with side-channel attribution, when the streaming consumer cancels early | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| A Worker / deadline runtime auto-recovered on restart keeps publishing new snapshots | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check on the relevant packages | Pass | the commands below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| Biome on newly added frontend modules | Pass | `biome check`, 3 new modules |

After configuring `ARTEX_PG_DSN` against a separate, disposable database, the automated checks can be reproduced (do not point it at a production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: the `server` package has two pre-existing tests that fail during temp-directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting the source from the unmodified baseline above and rerunning the `server` package in the same isolated environment, these two cleanup failures also reproduce. The baseline run additionally showed a target-node-count assertion failure in `TestCoreTaskLifecyclePG`; the final modified `server` regression does not have that assertion failure. The other packages pass, and this round's side-channel-related cases and the race check pass. The baseline issues were not marked as passing acceptance this time, nor were existing assertions modified to hide problems.

The Next.js build outputs the existing multi-lockfile / workspace-root inference warning; the build completes and all pages generate successfully.

## Browser checks

Using the Codex In-app Browser, connected to a standalone local Go service and the Next.js dev server. The following manual-automation operations were completed on desktop and on a 390 × 844 narrow screen, checking screenshots and browser logs:

- Typing `/btw` while a normal chat is running; the main content and the side channel display simultaneously; the desktop side panel works.
- Consecutive follow-ups; after the side channel stops, the already-generated part is kept; the main flow continues.
- The request continues when the panel is closed, and the completed answer is restored on reopening; after refreshing the page, an empty `/btw` restores the history.
- On the narrow-screen Drawer, input, buttons, history, and close all work, with no horizontal overflow.
- Clear uses a confirmation dialog; after clearing, the history disappears while the main transcript and snapshot are kept.
- The task MainAgent and two Workers are each questioned and switched between; the agent labels and histories do not cross-talk.
- A blocking local-model fixture keeps the Worker running; after submitting `/btw` from the Worker's main input box and stopping the side channel, the Worker still shows live running and its own pause button, and the side channel saves a partial answer.
- Browser error / warning logs are empty.

A controllable fixture is used for precise verification of concurrency timing, independent of a real model's output speed. During debugging, two Worker-runtime checks did not form a valid concurrency window (the task had ended / the answer ended early); after fixing the fixture it was redone and passed; these initial operations are not recorded as a valid pass.

## Real model conversations

`grok-4.6` was probed first, via the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200 with model name `grok-4.6` and `READY`, in 2.82 seconds. The preferred model was available, so the Tingly `glm` or Zhipu `glm-5.3` fallback chains were not enabled; these two fallback services were not validated this time.

| Scenario | Actual result |
| --- | --- |
| Asking about assets, goals, and markers while the main session is running | returned `redhaze.top`, read and summarized the goal from the homepage, and `BTW-REAL-0910`; the side channel completed in 16.97 seconds |
| Asking for the tool basis after the main session finished reading the homepage | correctly cited WebFetch 200, the curl redirect 301 → 302 → 200, and the page title; 7.24 seconds |
| A side question asking Bash to create a test file | refused to execute; the target file was not created; 7.74 seconds |
| A completed side question does not change the main context | the main transcript's SHA-256 and the main activity record stay consistent; the side channel's tool-execution count is 0 |
| A follow-up after truly stopping / restarting the Go service | kept the previous 3 side-channel history entries and answered assets, markers, and title directly from the persisted snapshot, without re-running the main agent |
| A new session using Grok's non-streaming configuration | correctly answered the assets and `ATOMIC-0910`; returned and saved usage: input 11734, output 138, cache_read 11520 |

For the assets case, the main session used WebFetch and Bash/curl to read a public homepage; the landing page was `https://id.redhaze.top/home`, titled "RedHaze Group · Global Integrated Group Portal". Bash staged the response in a local test file; nothing was written to the remote. This fact is verified separately from "the side channel executed no tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage note:** Tingly's Grok streaming response returned no usage. A separate direct request with `stream_options.include_usage=true` confirmed HTTP 200, 12 data frames, 0 usage frames. So the 0 in the streaming test means the endpoint did not provide usage and must not be read as "not billed". The non-streaming usage, and the fixture's failure / cancel usage, are all saved correctly.

## Qwen review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. It was given the first three real side-channel conversations, the main session's tool basis, and the engineering assertions; it returned `verdict: accept`, `concerns: []`, judging the answers consistent with the asset, marker, and page-read evidence, and the side channel's tool refusal consistent with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review's scope does not include the later-added service restart and non-streaming tests. Qwen's generalization of "no writes" was too broad: the main session's curl did create a local response temp file, as clearly documented above. Concurrency, zero tool execution, and transcript isolation are judged by the engineering assertions; the model review only assists in assessing answer quality.
