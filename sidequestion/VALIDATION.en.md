# `/btw` validation record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Testing used a separate PostgreSQL test database and data directory. Real model credentials were injected only into that isolated test environment; they were not written into the code or this record, and the product's default model was not changed. Versions: Go 1.26.3, norma v0.3.6, and Next.js 16.2.9.

The actual model conversations, returned objects, engineering assertions, and the raw Qwen review text are saved in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Deep copy of structured messages and tool arguments | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary / compaction requests don't overwrite, full reply and terminal state published, half-finished reply excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model-pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-pair replay, budget trimming and over-limit error | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side parallelism, bidirectional cancellation isolation | Pass | blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming / non-streaming, usage on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript / activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subcases |
| Persistence, pagination, idempotency, restart preserving partial answer | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear vs. late-write race, parent-resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker archive and recovery, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| The three parent interfaces, auth, resource ownership, Worker logical deletion | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Side questions during a busy main session; independent SSE reconnect / disconnect, cancel, and clear | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| Concurrency: one request per parent session and four total | Pass | two `TestSideHTTP…` cases |
| Snapshot persisted before submission, resume question after restart, old session cannot forge a snapshot | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Reject continuation after the cached configuration is deleted or the model changes | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel before archive and wait for the final answer and usage to persist | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Record usage only once and attribute it to the side question when the streaming consumer cancels early | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| A Worker restored after restart continues to publish snapshots with its deadline-bound runtime context | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race check of the relevant packages | Pass | the command below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| Biome on the newly added frontend modules | Pass | `biome check`, 3 new modules |

After setting `ARTEX_PG_DSN` to a separate disposable database, you can reproduce the automated checks (do not point it at a production DB):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression suite does not pass completely: two existing tests in the `server` package fail during temporary-directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting the source from the unmodified baseline above and rerunning the `server` package in the same isolated environment, these two cleanup failures also reproduced. The baseline run additionally showed a target-node-count assertion failure in `TestCoreTaskLifecyclePG`; that assertion did not fail in the final `server` regression run. The other packages passed, as did the side-question-related cases and the race check for this change. The baseline failures were not counted as passing, and no existing assertions were changed to conceal them.

The Next.js build outputs the existing multi-lockfile / workspace-root inference warnings; the build completes and all pages generate successfully.

## Browser checks

The Codex In-app Browser was connected to an independent local Go service and the Next.js development server. The following checks were carried out on desktop and at a narrow-screen size of 390 × 844; screenshots and browser logs were reviewed:

- Typing `/btw` during an ordinary chat run shows the main content and the side question at the same time; the desktop side panel works.
- Consecutive follow-ups; after the side question stops, the already-generated part is kept; the main flow continues.
- Closing the panel keeps the request running, and reopening restores the completed answer; after a page refresh, an empty `/btw` restores the history.
- On the narrow-screen Drawer, input, buttons, history, and close all work, with no horizontal overflow.
- Clear uses a confirmation dialog; after clearing, the history disappears, and the main transcript and snapshot are preserved.
- The task MainAgent and two Workers are each questioned and switched between; the Agent labels and histories do not cross-talk.
- A blocking local model fixture keeps a Worker running. After `/btw` is submitted from the Worker's main input box and the side question is stopped, the Worker remains visibly active with its own pause button, while the side question saves its partial answer.
- The browser error / warning logs are empty.

Controllable fixtures were used to validate concurrency timing precisely, without relying on a real model's response speed. During debugging, two attempts to check a running Worker did not create a useful concurrency window because the task had already ended or the answer finished early. After the fixture was fixed, both checks were repeated and passed; the initial attempts are not counted as passes.

## Real model conversation

The preferred model, `grok-4.6`, was probed first via the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, the model name `grok-4.6`, and `READY` in 2.82 seconds. Since the preferred model was available, the Tingly `glm` and Zhipu `glm-5.3` fallback chains were not enabled; those fallback services were not validated this time.

| Scenario | Actual result |
| --- | --- |
| Asking about assets, goals, and markers during the main-session run | Returned `redhaze.top`, the homepage read and goal summary, and `BTW-REAL-0910`; the side question completed in 16.97 seconds |
| After the main session finished the homepage read, asking for the basis of a tool | Correctly cited WebFetch 200, the curl redirects 301 → 302 → 200, and the page title; 7.24 seconds |
| A side question asking Bash to create a test file | Refused to execute; the target file was not created; 7.74 seconds |
| A post-completion side question does not change the main context | The main transcript SHA-256 and the main activity record stayed consistent; the side question's tool-execution count was 0 |
| Following up after genuinely stopping / restarting the Go service | Kept the previous 3 side-question history entries, answering about assets, markers, and the title directly from the persisted snapshot, without rerunning the main Agent |
| A new session using Grok's non-streaming configuration | Correctly answered the assets and `ATOMIC-0910`; returned and saved usage: input 11734, output 138, cache_read 11520 |

In the asset case, the main session used WebFetch and Bash/curl to read a public homepage, landing on `https://id.redhaze.top/home`, with the title "RedHaze Group · Global Conglomerate Portal". Bash temporarily stored the response in a local test file; nothing was written to the remote. This fact is verified separately from "the side question executed no tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming responses returned no usage. A separate direct request with `stream_options.include_usage=true` was sent to verify: HTTP 200, 12 data frames, 0 usage frames. So the 0 in the streaming test means the endpoint provided no usage and must not be read as "not billed". The non-streaming usage, as well as the fixture's failure / cancellation usage, were all saved correctly.

## Qwen review

The review model `qwen-flash`, via the OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, returned HTTP 200. It received the first three real side-question conversations, evidence for the main session's tool calls, and the engineering assertions. It returned `verdict: accept` and `concerns: []`, judging the answers consistent with the asset, marker, and page-reading evidence and the refusal to execute tools in side questions consistent with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review did not cover the later-added service-restart and non-streaming tests. Qwen's generalization of "no writes" was too broad: the main-session curl did create a local temporary response file, as explicitly recorded above. Concurrency, zero tool execution, and transcript isolation are judged by engineering assertions; the model review only assists in evaluating answer quality.
