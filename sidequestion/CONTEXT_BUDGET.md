# Side-question long conversations and context budget

Fix on 2026-09-11. The original implementation treated the request JSON character count directly as tokens, and inherited the main task's 32K output reservation, so requests with heavy HTML, JS, and tool results would reject normal side questions prematurely.

## Open-source implementation review

- [Grok CLI side-question context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts fragments from recent user/assistant text, character budget about 2000, at most 400 characters per entry. Continuous side-question Q&A history is not maintained on this path.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): independent cancel signal; output limit 2048 tokens when supported by the model; no tools provided.
- [Grok CLI main session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, preserves recent content, updates new content into old summaries, and handles cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full request estimation, output/buffer reservation, recent content plus rolling summary, no-tool summary request; this implementation defaults recent budget 8000, summary output limit 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): only attempts overflow recovery when assistant output has not yet started; recovered calls no longer enter the same overflow recovery path.

ARTEX borrows the independent output budget, recent content plus rolling summary, and limited recovery approaches. It keeps norma v0.3.6's structured messages and tool pairing, and does not copy Grok's text extraction; it does not write OpenCode's main-session compaction events into the ARTEX main transcript.

## Request budget and execution

- Messages reuse norma's per-content-block UTF-8 byte estimation with a 4/3 margin; additionally accounts for system prompt, tool schema, and message-encapsulation overhead. The estimate is not a precise model token count.
- Side-question output defaults to at most 8192 tokens, and does not exceed the output limit already set in the main configuration. Can be set between 256–32768 via the service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS`; does not modify the product default model or main task parameters.
- Input budget is context window minus output limit minus safety margin; unknown window uses platform default 200K. Safety margin is 5% of the window, minimum 128, maximum 8192 tokens.
- Successful Q&A is loaded in batches of at most 20 by incrementing ordinal. At most 20 pairs of verbatim text are kept, with token budget at most 1/4 of the input budget, and no more than 16K.
- Overflow Q&A updates into the rolling summary. The summary carries historical source and context time; historical assistant answers are not equivalent to new tool evidence — on conflict, the latest main snapshot takes priority.
- When the main context is still too long, only old messages in the copy are summarized, keeping at most 8K tokens of recent content; truncation points will not split tool calls and results. A single oversized pair goes entirely into the summary.
- Summary input is UTF-8 safely chunked according to the actual remaining window, with output limit 2048 tokens; empty summaries, truncation, tool calls, or exceeding summary budget are not written to cache. At most 12 summary calls per side question, also subject to the same 120-second timeout; reaching the limit fails explicitly, no infinite loop.
- If the model first returns a context overflow and has not yet output text or tool calls, it may retry at most once after further reduction; if the estimated size has not decreased, recovery stops immediately. Other model errors and partial streamed output do not trigger this recovery.
- All obtained usage, including summary, failed attempts, and usage on cancel, accumulates into the same side-question request. When the Provider does not return usage, only zero can be recorded; estimates cannot be disguised as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the old Q&A summary, the covered ordinal, and the main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation stage, actual replay group count, summary usage, and budget estimate.

Summaries are only saved while the original request is still running and the cleanup version matches; clear also clears the cache; late writes do not restore cleared data. New snapshots do not reuse old snapshot summaries. Summary fields are saved with the v3 task archive; when restoring old v3 with missing fields, empty objects are filled in; v1/v2 remain compatible.

POST first accepts and returns the request; preparation and compaction run in the background, without holding an admission lock or database transaction. SSE/history display preparation, organizing Q&A, compacting the copy, and the answer stage; compaction failure is saved as the failure terminal state for that side-question request. The frontend retains the error and restores the draft of the failed question, without using floating notifications to obscure the input box; history polling no longer clears submission errors.

## Validation records

- Replay of 19/20/21/50 groups, cross-20-group old-conclusion preservation, summary cache restart reuse — automation passes.
- Extra-long Chinese answers and code context, chunked request budget, tool pairing, snapshot invariance, new-snapshot cache invalidation — automation passes.
- Summary failure/cancel/truncation/extra-long/tool returns, clear race, call limit, only one overflow recovery and partial stream no retry — automation passes.
- Independent PostgreSQL pagination, restart, v1/v2/v3 archive, summary cache and budget metadata archive recovery, old v3 missing new fields, 20 parent sessions sharing four concurrent slots — passes.
- Go candidate service build, frontend TypeScript check, Biome check for modified components, and Next.js production build in an independent directory — passes.
- Built-in browser uses independent UI fixtures, verifies at 1280×720 and 390×844 the organizing stage, summary scope indicator, draft recovery after failure, no floating error notifications, no horizontal overflow, and no console errors. Temporary fixtures have been removed.
- Read-only replay of existing local Worker snapshots, new budget check passes; for example Worker #3's 293085-character snapshot is no longer rejected by local character count misjudgment. This item has no external model call.
- This time sending a private Worker snapshot to a real Grok conversation test was rejected by automated approval review, not executed, and not recorded as a pass.
- 2026-09-11 00:37 local backend restarted per user request, using the original database, data directory, and login configuration. The running file and candidate binary SHA-256 match; `/api/health` for both backend and frontend proxy returns normal.

Validation commands (using only independent test libraries):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses an independent copy to avoid overwriting the current preview `.next`. The candidate service is located at `/private/tmp/artex-btw-budget-candidate`, has been copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.