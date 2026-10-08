# Long side-question conversations and context budgeting

Fixed on 2026-09-11. The original implementation treated the character count of the request JSON directly as tokens, and inherited the main task's 32K output reservation, so when there was a lot of HTML, JS, or tool results it would reject legitimate side questions prematurely.

## Open-source implementation review

- [Grok CLI side-question context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts snippets from the most recent user and assistant text, with a character budget of about 2000 and at most 400 characters taken per entry. It does not maintain a continuous side-question Q&A history on this path.
- [Grok CLI standalone request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): an independent cancellation signal; an output cap of 2048 tokens when the model supports it; no tools.
- [Grok CLI main-session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimate tokens, keep recent content, update the old summary with new content, and handle cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): a full-request estimate, an output/buffer reservation, recent content plus a rolling summary, and a tool-less summary request; this implementation defaults to a recent budget of 8000 and a summary output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): overflow recovery is only attempted before assistant output has begun, and a call after recovery does not re-enter the same overflow-recovery path.

ARTEX borrows the independent output budget, the recent-content-plus-rolling-summary approach, and the bounded recovery. It keeps norma v0.3.6's structured messages and tool pairing, does not copy Grok's text extraction, and does not write OpenCode's main-session compaction events into ARTEX's main transcript.

## Request budget and execution

- Messages use norma's per-content-block UTF-8 byte estimate with a 4/3 margin. The estimate also includes the system prompt, tool schema, and message-wrapping overhead; it is not an exact model token count.
- Side-question output is at most 8192 tokens by default, and never exceeds the output cap already set in the main configuration. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` can set a cap of 256–32768; it does not modify the product's default model or the main task's parameters.
- The input budget is the context window minus the output cap and a safety margin; an unknown window uses the platform default of 200K. The safety margin is 5% of the window, with a minimum of 128 and a maximum of 8192 tokens.
- Successful Q&A pairs are loaded in batches of up to 20 in ordinal order. The request keeps at most 20 pairs in their original form, with a token budget capped at one quarter of the input budget and at 16K tokens.
- Q&A pairs beyond the cap are folded into the rolling summary. The summary carries the historical source and the context time; a historical assistant answer is not equivalent to new tool evidence, and on conflict the latest main snapshot takes precedence.
- If the main context is still too long, only the old messages in the copy are summarized, keeping at most 8K tokens of recent content; the truncation point does not split a tool call from its result. A single pair that is too large goes into the summary as a whole.
- The summary input is split into UTF-8-safe chunks sized to fit the remaining context window, with an output cap of 2048 tokens. Empty or truncated summaries, summaries that return a tool call, and summaries that exceed the budget are not cached. A single side question makes at most 12 summary calls, all subject to the same 120-second timeout; reaching the limit fails explicitly rather than looping forever.
- If the model first returns a context-exceeded error and has not yet emitted text or a tool call, it shrinks further and retries at most once; if the estimated size does not decrease, recovery stops immediately. Other model errors and partial streaming output do not trigger this recovery.
- All usage obtained — including summaries, failed attempts, and usage at cancellation time — is accumulated into the same side-question request. When the Provider returns no usage, only zero can be recorded; an estimate must not be passed off as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores the summary of old Q&A, the covered ordinals, and the main-context summary cached by snapshot identity. `side_question_requests.context_info` stores the preparation stage, the actual number of replayed pairs, the summary usage, and the budget estimate.

The summary is saved only while the original request is still running and the clear version matches; a clear also removes the cache, and a late write does not restore cleared data. A new snapshot does not reuse the old snapshot's summary. The summary fields are saved with the v3 task archive; when recovering an old v3 that lacks the fields, they are filled with empty objects, and v1/v2 remain compatible.

POST accepts the request and returns immediately; preparation and compaction then run in the background without holding an admission lock or database transaction. SSE and history show the preparation, Q&A-history organization, side-context compaction, and answer-generation stages. A compaction failure is saved as the request's terminal failure state. The frontend preserves the error and restores the failed question as a draft without showing a toast that blocks the input box; history polling no longer clears submission errors.

## Validation record

- 19/20/21/50-pair replay, preserving old conclusions across 20 pairs, and summary-cache reuse across restarts: automated, passed.
- Very long Chinese answers and code context, chunked request budgets, tool pairing, snapshot immutability, and new-snapshot cache invalidation: automated, passed.
- Summary failure/cancel/truncation/over-length/tool-return, clear races, call-limit, only-once overflow recovery, and no retry on partial streams: automated, passed.
- Pagination, restart, v1/v2/v3 archive, summary-cache and budget-metadata archive recovery on an independent PostgreSQL, old v3 lacking new fields, and 20 parent sessions sharing four concurrency slots: passed.
- Go candidate-service build, frontend TypeScript check, Biome check on modified components, and a Next.js production build in an independent directory: passed.
- The built-in browser used an independent UI fixture to verify, at 1280×720 and 390×844, the organizing stage, the summary-scope hint, draft recovery after failure, the absence of an input-blocking toast, no horizontal overflow, and no console errors. The temporary fixture has been removed.
- Read-only replay of this machine's existing Worker snapshots passed the new budget check; e.g. Worker #3's 293085-character snapshot is no longer wrongly rejected by a local character count. This item involves no external model calls.
- A real-conversation test that would send a private Worker snapshot to Grok was rejected by the auto-approval review and was not executed, so it does not count as a passing item.
- At 2026-09-11 00:37, at the user's request, the local backend was restarted, reusing the original database, data directory, and login configuration. The running file and the candidate binary have matching SHA-256; both the backend and the frontend proxy's `/api/health` return normal.

Validation commands (using only an independent test DB):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses an independent copy to avoid overwriting the currently previewed `.next`. The candidate service is located at `/private/tmp/artex-btw-budget-candidate`, copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.
