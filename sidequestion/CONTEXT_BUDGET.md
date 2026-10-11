# Side-channel long conversations and context budget

Fixed 2026-09-11. The original implementation treated the character count of the request JSON directly as tokens and inherited the main task's 32K output reservation, so when there was a lot of HTML, JS, and tool results it would reject a normal side question prematurely.

## Open-source implementation review

- [Grok CLI side-channel context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts snippets from the most recent user and assistant text, with a character budget of about 2000 and at most 400 characters taken per item. It does not maintain a continuous side-channel Q&A history on this path.
- [Grok CLI standalone request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): a standalone cancel signal; when the model supports it, the output cap is 2048 tokens; no tools provided.
- [Grok CLI main-session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, keeps recent content, folds new content into the old summary, and handles cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full-request estimation, output/buffer reservation, recent content plus a rolling summary, a tool-less summary request; this implementation defaults to a recent budget of 8000 and a summary output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): overflow recovery is only attempted before the assistant has started output, and the call after recovery does not re-enter the same overflow-recovery path.

ARTEX borrows the standalone output budget, recent content plus rolling summary, and bounded recovery. It keeps norma v0.3.6's structured messages and tool pairing and does not copy Grok's text extraction; it does not write OpenCode's main-session compaction events into ARTEX's main transcript.

## Request budget and execution

- Messages reuse norma's per-content-block UTF-8 byte estimate with a 4/3 margin; the system prompt, tool schema, and message-envelope overhead are additionally counted. The estimate is not the model's exact token count.
- Side-channel output defaults to at most 8192 tokens and does not exceed the output cap already set in the main configuration. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` can set a cap of 256–32768; it does not modify the product's default model or the main task's parameters.
- The input budget is the context window minus the output cap and a safety margin; an unknown window uses the platform default of 200K. The safety margin is 5% of the window, with a minimum of 128 and a maximum of 8192 tokens.
- Successful Q&As are loaded in batches of at most 20 by increasing ordinal. At most 20 verbatim Q&As are kept, with a token budget of at most 1/4 of the input budget and no more than 16K.
- Q&As beyond the limit are folded into the rolling summary. The summary carries historical source and context time; a historical assistant answer is not equivalent to new tool evidence, and on conflict the latest main snapshot takes priority.
- When the main context is still too long, only old messages in the copy are summarized, keeping at most 8K tokens of recent content; the truncation point does not split a tool call from its result. An oversized single Q&A goes into the summary as a whole.
- Summary input is UTF-8-safely chunked according to the actual remaining window, with an output cap of 2048 tokens; an empty summary, truncation, a tool call, or exceeding the summary budget does not write to the cache. A single side question makes at most 12 summary calls and is subject to the same 120-second timeout; reaching the cap fails explicitly and does not loop forever.
- If the model's first return exceeds the context limit and no text or tool call has yet been emitted, it is further reduced and retried at most once; if the estimated size does not drop, recovery stops immediately. Other model errors and partial streaming output do not trigger this recovery.
- All usage obtained — including the usage from summaries, failed attempts, and at cancellation — is accumulated into the same side-channel request. When the Provider returns no usage, it can still only record zero and must not disguise an estimate as actual usage.

## Persistence and interface

`side_question_sessions.memory` stores the summary of old Q&As, the covered ordinal, and the main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation stage, the actual number of replayed Q&As, summary usage, and the budget estimate.

A summary is saved only while the original request is still running and the clear version matches; a clear also clears the cache, and a late write does not restore cleared data. A new snapshot does not reuse an old snapshot's summary. Summary fields are saved with v3 task archives; when restoring an old v3 missing a field, an empty object is filled in, and v1/v2 remain compatible.

POST first accepts and returns the request, with preparation and compaction executing in the background, holding neither the admission lock nor a database transaction. SSE/history shows the preparing, organizing-Q&A, compacting-copy, and answering stages; a compaction failure is saved as this side-channel request's failure terminal state. The frontend keeps the error and restores the draft of this failed question, without covering the input box with a floating notification; history polling no longer clears the submission error.

## Validation record

- 19/20/21/50-item replay, preservation of old conclusions across 20 groups, summary-cache reuse after restart: automation passed.
- Very long Chinese answers and code context, chunked request budget, tool pairing, snapshot immutability, new-snapshot cache invalidation: automation passed.
- Summary failure/cancel/truncation/over-length/tool return, clear races, call cap, overflow recovery only once, and no retry on partial streams: automation passed.
- Standalone PostgreSQL pagination, restart, v1/v2/v3 archiving, summary-cache and budget-metadata archive recovery, old v3 missing new fields, 20 parent sessions sharing four concurrency slots: passed.
- The Go candidate service build, the frontend TypeScript check, Biome checks on modified components, and a Next.js production build in a separate directory: passed.
- The built-in browser used a separate UI fixture and verified, at 1280×720 and 390×844, the organizing stage, summary-scope hints, draft restoration after failure, no floating error notification, no horizontal overflow, and no console errors. The temporary fixture has been removed.
- Read-only replay of this machine's existing Worker snapshots passed the new budget check; for example, Worker #3's 293085-character snapshot is no longer wrongly rejected by a local character count. This item involves no external model calls.
- The real-conversation test of sending a private Worker snapshot to Grok was rejected by the auto-approval review this time and was not executed, and does not count as a passing item.
- At 2026-09-11 00:37, at the user's request, the local backend was restarted, reusing the original database, data directory, and login configuration. The running file's SHA-256 matches the candidate binary's; `/api/health` returns OK on both the backend and the frontend proxy.

Validation commands (using a dedicated test database only):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses a separate copy to avoid clobbering the currently previewed `.next`. The candidate service lives at `/private/tmp/artex-btw-budget-candidate`, has been copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.
