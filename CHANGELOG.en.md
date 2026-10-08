# Changelog

Notable changes to this project are recorded in this file, following the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### Added

- **New built-in interception rule for delete endpoint paths**: the previous built-in HTTP destructive rule only recognized the DELETE **method**, yet most applications trigger their delete endpoints with GET/POST, so a call like `curl 'http://t/api/user/delete?id=1'` would really delete the target data. A `deny` rule has been added covering `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, `/delta` are not falsely intercepted). Existing instances get it after upgrading, and it can be disabled or deleted under "System → Command interception".
- **Model-fallback approval token usage is now tracked separately**: approval calls previously had no independent accounting attribution, so how much they cost could only be guessed. They are now metered separately, and a usage card has been added below the "Model-fallback approval" switch under "System → Command interception" — a cumulative tally of five items (call count, input, output, cache read, cache write), plus a mini bar chart of daily consumption over the last 30 days.

### Vulnerability push

#### Added

- **Vulnerability findings can be pushed to IM**: a new "System → Notification push" page lets you push scan findings to six channels: **DingTalk, Feishu, WeCom, a generic Webhook, Telegram, and email**. You can configure any number of instances of the same type, each with its own on/off, rate limit, and filter rules.
- **Two push timing modes: real-time and digest**: real-time mode sends each item as it hits; digest mode merges them into a single message on a global cycle (default 30 minutes), leading with the count of new items and the severity distribution. To do "high-severity real-time, the rest in a digest", create two channels configured separately.
- **Filter rules support four dimensions**: minimum severity level; limiting to a task/asset scope; vulnerability-type keyword include and exclude (exclude takes priority); and whether to receive disposition-status changes (off by default). The message has a "View details" button, whose target address is set by the global "Back-link address"; left blank, there is no button.
- **Delivery history and manual resend**: lists each delivery's status, attempt count, failure reason, and channel, filterable by channel and status; a failed item can be resent with one click (resending zeroes the retry count).
- **Masked echo of channel credentials**: the Webhook address, signing secret, Bot Token, SMTP password, etc. are echoed only as masked values with a trailing-digit hint; submitting them as-is means "do not modify", and clearing the input box deletes that field.

#### Fixed

> This section is the conclusion of a round of security auditing done after the feature's first version was complete; each item was first reproduced, then fixed, then covered with a regression test.

- **Fixed a mask bypass via "change the target address, keep the credentials" (critical)**: config merging kept the DB's original value for any "unmentioned key", so by changing only the address and saying nothing about the credentials, you could get the server to send the real credentials from the DB to an arbitrary address, completely silently. Now, once the target address changes, every credential field must explicitly take a stance — give a new value, or explicitly leave it blank to indicate it is no longer needed.
- **Fixed untrusted content going unescaped in markdown-style channels**: vulnerability titles and summaries come from model output, and asset URLs come from scan results, so a crafted title would render as a clickable external link in DingTalk / WeCom / Feishu, while image syntax would be fetched by the client at render time and leak the reader's IP. Now we uniformly collapse to a single line + escape markdown metacharacters, and the escaping happens at each channel's render exit rather than in the shared title function.
- **Fixed the delivery address's SSRF surface**: previously loopback, intranet, and cloud-metadata addresses could all be delivered to, and the first 200 bytes of a failure response body went into `last_error` and were echoed in the delivery history, forming a semi-blind read primitive. Now the defense is at the **dial stage**, covering DNS rebinding and rejecting cross-host redirects; loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`, while RFC1918 private networks are still allowed (a self-hosted intranet SMTP relay is very common).
- **Fixed push credentials leaking via error messages**: DingTalk's `access_token`, WeCom's `key`, and Telegram's bot token are all in the URL, and on a delivery failure the full URL flowed into four places — `last_error`, the delivery history, the server log, and the frontend hint. They are now uniformly redacted, keeping only `scheme://host` and the underlying reason.
- **Fixed silent loss where a truncated digest message had the whole batch marked as delivered**: the truncated-away vulnerabilities were neither in the message nor in the failure list, yet the delivery history showed success. Now packing is done **per message**, marking only those packed into this one; the rest return to the queue for the next message to continue without consuming a retry, and the message header states how many more will continue in the next one.
- **Fixed a typo in the minimum-level threshold silently disabling the filter**: when `min_severity` was a typo like `hgih`, the unknown level's ordinal was 0 and the check degenerated to always-true — the user thought they only received high-severity, but in fact all vulnerabilities were flooded into the group. The value is now validated on the write path with the allowed values listed.
- **Fixed `rate_per_min = 0` (no rate limit) being unreachable**: the DB-write layer silently changed an explicit 0 to the default value, so the operator thought the rate limit was lifted but it was in fact still capped. The default is now applied at the interface layer when the field is absent.
- **Fixed digest channels completely bypassing the token bucket**: `rate_per_min` previously had no effect in digest mode. Now the number of items claimed is constrained by both this round's quota and the in-memory upper bound.
- **Fixed failure disposition judging by the whole batch's max attempt count, letting old deliveries drag down new ones**: an old delivery that had already retried twice would drag brand-new deliveries in the same batch into `failed`, so a new vulnerability was permanently lost without a single retry used. It is now decided per item.
- **Fixed deliveries with an unparseable snapshot in a digest batch being silently marked as success**: such items would be skipped at the render stage yet counted as success along with the whole batch. They are now explicitly judged as failures with a reason.
- **Fixed per-round delivery batches that could outlast their lease**: in a multi-instance deployment, a row whose lease expired would be re-claimed, sent again, and have its attempt count incremented twice. The per-round cap is now derived from "lease / single timeout", with an assertion pinning down the relationship between these constants.
- **Fixed Telegram truncation possibly cutting an HTML entity**: truncation only avoided a half tag, not an entity fragment like `&amp`, and the parser could therefore reject the **entire** message. Now both are avoided.
- **Fixed the email channel judging a transient SMTP failure as permanent**: a 4xx such as a greylist `450` is a transient rejection that should be retried later, but was previously judged final, so a greylist-enabled server would make every push fail after the first attempt. It is now distinguished by the first digit of the response code: 4xx is retryable, 5xx is a permanent failure.
- **Fixed a mask literal being actually written to the DB when submitting a nested structure**: an object field like `webhook.headers` can only be masked or submitted as a whole, so stuffing the mask sentinel inside the object would store it as a real value, silently breaking authentication. Such a submission is now explicitly rejected.

#### Design notes

- **The vulnerability-write transaction only does a single blind INSERT**: `notification_events` and the vulnerability are written in the same transaction, so a commit guarantees the two are atomically consistent. This INSERT deliberately does not read the channel table or run the user's filter rules, and is wrapped in a `SAVEPOINT` — otherwise a single misconfigured filter condition could abort the transaction and prevent a high-severity vulnerability from being stored.
- **Delivery uses lease-based claiming rather than a long transaction**: after claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as the lease, holding no database lock during delivery; a row left behind by a process crash is re-claimed after the lease expires — self-healing and not infinitely retried.
- **Rate limiting does not consume the retry budget**: this round's quota is computed by the token bucket first, then that number of items is claimed. Doing it the other way around would make a rate-limited delivery wastefully count an attempt, falling into failure after three budgets are consumed by pure waiting.

### Test infrastructure

#### Fixed

- **Fixed permanent asset residue caused by the cleanup order in the company ICP-attribution test case**: `defer d.Close()` runs first when the function returns and `t.Cleanup` runs after it, so all the cleanup statements landed on an already-closed connection and the errors were discarded, leaving test assets and companies permanently in the DB, which would also make other cases that assert on asset counts fail inexplicably. Now closing the connection also goes through `t.Cleanup` registered first, and cleanup failures are surfaced.

### Traffic

#### Added

- **The traffic list supports content search, path / status-code / length filtering, and column sorting**: the filter row supports filtering by **response content** (using the full-text index), **path**, **status code** (an exact value or a bucket like `2xx`), and **response-length range**, and the header can sort by time, status code, and response length, with the sort preference stored locally. Two indexes are added and take effect automatically after existing instances upgrade; the Agent tool's query contract is unchanged.
- **The traffic list adds "Clear all"**: deletes all traffic records at once (ignoring the current filter), cleans up historical host directories the index no longer records, and after clearing does a full compaction and reports how much space was actually freed. Traffic evidence already bound to vulnerabilities lives in a separate evidence store and is unaffected.

#### Fixed

- **Fixed disk space not being freed after deleting traffic**: the index DB did not enable `auto_vacuum` at creation, so the file never shrank; and the full-text index is `contentless_delete`, so `DELETE` only writes a tombstone without reclaiming the original postings — so deleting traffic actually made the index grow (measured: capturing 6MB of bodies → a 16MB index, still 16MB after deleting everything). Now a newly created index DB enables `auto_vacuum=incremental`, and after each delete it runs full-text index merging and incremental reclamation in chunks in the background, gradually returning space to the system (the same scenario falls back to 104KB); reclamation releases the write lock between chunks and does not block traffic recording.

> Upgrade note: `auto_vacuum` can only be set at DB creation, so **an existing instance's index DB is still in the old mode**, and incremental reclamation is a no-op on it (a hint line is printed on startup). For such a DB, the tombstone growth has stopped after upgrading, and the already-occupied size can be reclaimed once with "Clear all" — that step converts the DB to incremental-reclaim mode, after which routine deletes take effect on their own.

### LLM

#### Fixed

- **Fixed the custom session header not taking effect on the "one-off LLM call" path**: goal decomposition (round 0) and cold-node compaction are both one-off calls that don't attach a transcript store, so there is no session id on the context, and the gateway side therefore cannot read that header — for an endpoint like opencode zen that returns 400 outright on a missing header, this showed up as "round 0 fails but subsequent planner rounds are perfectly fine". These two paths are now explicitly attached with a session id stable per exploration, which also lets token usage be correctly attributed to the corresponding exploration (previously not recorded at all).

### Tools

#### Fixed

- **Fixed MCP and custom tools' output truncation not taking effect, with large results flooding the context**: MCP tool returns previously went completely untruncated, and HTTP-type custom tools, being passed an empty tool context, always used a hardcoded 6000 characters rather than the session-configured cap. Both paths now uniformly go through `actool.Capture`: truncating by the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured, spilling the full output to disk and leaving only a leading snippet and a file pointer in the message.
- **Upgraded norma to v0.4.3**: the harness layer uniformly runs `CaptureOnce`, so no tool (including future ones) will overflow the context from a missed truncation.

### Deployment

#### Fixed

- **Fixed the activity stream not connecting in a reverse-proxy deployment**: the SSE address was previously unconditionally assembled as `${hostname}:8787`, so in a deployment reverse-proxied to 443 the page loaded from `https://domain/` but SSE was pointed at port 8787 not exposed to the public, showing up as the activity stream repeatedly reconnecting. It is now distinguished by `NODE_ENV`: production defaults to same-origin, and only `next dev` keeps `:8787` to bypass Next.js's buffering of SSE; the `NEXT_PUBLIC_SSE_BASE` override is kept. The README adds reverse-proxy deployment notes.

### Findings

#### Fixed

- **Fixed the "by asset" view's asset list having no scrollbar on overflow**: the outer card only had a `max-height` with no definite height, so the scroll viewport's percentage height could not be resolved, and with many assets the list would either burst out of the card or be truncated and unscrollable. The height cap is now placed directly on the asset tree's native scroll container, adapting to the window height.

### Database

#### Fixed

- **Set an upper bound on the connection pool**: `database/sql` by default doesn't limit the number of connections the application itself can open, and when the pool has no idle connections it creates new ones unconditionally, all the way up to PostgreSQL's `max_connections` (default 100) before being rejected. It is now capped at 32, and excess queries wait for an idle connection, showing up under the same load as slowness rather than an error; if `max_connections` is lowered, lower this accordingly.
- **Improved connection reuse**: `MaxIdleConns`'s default is 2, so with concurrency above 2 the connections are closed as soon as they're done, and the next request has to redo the TCP and PostgreSQL authentication handshake. The idle cap is now raised to match the total cap, with a 30-minute connection lifetime and a 5-minute idle eviction added, so the pool can naturally replace long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### Added

- **The task list adds a "Vulnerabilities" column**: shows the counts of Crit / High / Med / Low by severity tier, with non-zero tiers colored by severity, so you can see each task's vulnerability volume and distribution at a glance.

#### Changed

- **The "Description / Target" columns are narrowed**: overly long content is ellipsized and viewable in full on hover, reducing how much long text crowds the list's horizontal space.

### Exploration graph and planning situation overview

#### Changed

- **Streamlined the planning situation overview (`graph_overview`) and capped list sizes**: recent facts, finished and pending intents, cold-zone summaries, and confirmed-vulnerability details now use a "latest items plus total count" format, with omitted items available on demand. This significantly reduces the LLM context per round and prevents context growth on long tasks. The cold zone in a related task's overview is also capped.

#### Fixed

- **Eliminated the reverse duplicate edge between digest and finding in the exploration graph after collapsing cold nodes.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset interception

#### Added

- **New global asset-interception rules (blocklist)**: supports entering exact- and fuzzy-match domains / IPs / URLs as well as CIDR ranges, with create/read/update/delete and enable/disable of rules; the management page is at "System → Asset interception"; fuzzy interception of government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites is built in by default.
- **Asset interception wired into the execution chain**: before dispatching an intent (`add_intent`) and inserting assets (`insert_assets`), the Agent first makes an interception decision on the target assets — an intercepted intent is not dispatched, an intercepted asset is not inserted, and the asset info and interception reason are returned to the Agent.
- **New task-level interception / allow (allowlist) rules**: independent of the global rules and effective only for this task, with the decision order "intercept first, then allow" — a hit on interception forbids it; if interception is not hit but the task has allow rules configured and none match, then "not allowed to test"; when no allow rules are configured, the allowlist is not enabled. They can be entered when creating a task, and created/updated/deleted and enabled/disabled in the task detail's "Overview".
- **Task templates support preset categories and task-level interception/allow rules**: a template can save a task category and a set of task-level rules, which are carried into the new-task form when the template is applied.

### Operation review

#### Fixed

- **Tightened the model-judge output protocol to reduce false allows from truncation**: the verdict comment cap was lowered from 500 Chinese characters to 120, and "output JSON only, with no preamble or code block" was enforced, to avoid a verdict being unparseable after being truncated by `MaxTokens` and then allowed per the model-failure policy (fail-open).

### Task archive

#### Fixed

- **Archiving skips a symlink rather than failing the whole package**: the archive format end-to-end only supports regular files and directories, so previously any symlink in the working directory would fail the entire task archive; now it skips the symlink and logs it, archiving the other files normally (not following the link, not escaping the directory tree).

### Account and compliance

#### Added

- **A "Usage notice and disclaimer" dialog is added before login**: you must check agreement before you can log in.

### License and dependencies

#### Changed

- **The project adopts the AGPL-3.0 open-source license**, and the README's license and disclaimer notes are improved.
- **Upgraded norma to v0.4.1.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration-chain broadcast board

#### Added

- **The task detail page adds an "Exploration-chain broadcast board"** (#144): presents the task's exploration nodes (start/goal/intent/fact/vulnerability/hint/compaction) as a timeline feed, supporting filtering by type, keyword search, ascending/descending order, pagination, and auto-refresh; while on the "newest-first first page" it is the live position and refreshes each round, and when you leave it only accumulates an unread count without disturbing your current reading, with a one-click "N new broadcasts · back to latest" to return. Grouped by day, so even when paging through a long task you can tell "which day this is about".
- **The broadcast board shows each row's node id** (#145), making it easy to cross-reference the exploration-chain graph and locate a specific node.
- **Expanding a broadcast node's detail shows upstream/downstream and anchored assets** (#147): expanding a broadcast shows that node's upstream (where it came from) / downstream (what it produced) relationships, and hovering a related item pops up that node's card (type/status/source/time/summary/payload snippet); it also lists the assets that node anchors (type label + recognizable text). The related data is delivered along with the broadcast page, so expanding sends no extra request.
- **The broadcast-board search supports filtering by node id** (#150): the search box adds node-id matching beyond "content / source"; entering a plain number or the UI's "#41" form precisely locates the corresponding node.

### Intent management

#### Added

- **Intent deletion supports two modes, "soft delete / hard delete"** (#149): pending, running, and paused intents can all be deleted, and deletion requires a reason; the delete mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is set to "deleted" and the reason is recorded in a separate field, keeping the intent node and all its output and lineage.
  - **Hard delete**: physically removes this intent and the exclusive descendant nodes "supported only by it" (cascading along the output / intent chain to the leaves), to avoid leaving orphaned data; the deleted intent's token accounting is archived by its original date; shared nodes (still referenced by other intents), goals, and task-root facts are all kept, and the hard-delete dialog shows the estimated number of cascade-affected nodes.

  Both modes notify the planner "this intent was deleted by the user + reason" and replan accordingly.

### Operation review

#### Added

- **Approval records support filtering by status and verdict source** (#139): you can filter approval records by approval status and verdict source to quickly locate a target record.

#### Fixed

- **Pending approval requests are loaded independently of the history pagination** (#133): pending requests are no longer affected by the history list's pagination and remain fully visible while browsing the history.

### Agent

#### Changed

- **The Worker role description now uses generic cybersecurity-platform wording** (#138).

#### Fixed

- **Fixed cold-node compaction never triggering**: the Compactor was wired into the per-task planner; cold-node compaction (cold-digest) previously never ran because it wasn't connected, and is now restored.

### Chat

#### Added

- **Chat supports @-referencing multiple types of records with scroll pagination** (#135): you can @-reference multiple types of records in chat, and the reference candidates support scroll-pagination loading.

#### Fixed

- **Long message bubbles are constrained within the session panel** (#137): an overly long message bubble no longer overflows the session area.
- **Fixed "missing upload file" when uploading a file before a new conversation is created**: the Composer's file picker snapshots the `FileList` into an array before clearing the input and then calls back; previously the draft state had to asynchronously create the conversation first, and by the time execution resumed the `FileList` live-bound to the input had already been cleared, so the upload was missing the file field and the backend returned 400.

### Traffic

#### Fixed

- **The traffic-recording proxy listens only on 127.0.0.1 by default** (#129, #130): to avoid the default config exposing the open proxy on other network interfaces.

### Web

#### Fixed

- **Fixed the static-export task-list page losing the global header.**
- **Added the linked-traffic mock on the demo vulnerability detail page, fixing a white screen.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### Added

- **Approval records support source navigation** (#125): clicking an approval record's "Source" jumps back to the tool execution that triggered that approval — opening the original full session, auto-paginating to load up to the target position, expanding the command and result in place and centering and highlighting it in the scroll area, keeping surrounding messages readable; auto-correction stops when the user scrolls manually. Across ordinary conversations, task Workers, the planner, and main-Agent segment sessions, it locates precisely via the persisted `tool_use_id` and the task mapping (a new within-scope call-ID index is added), and when there is a missing, duplicate, or ambiguous association it gives an explicit hint rather than jumping to another execution. When the session is deleted or the task is archived / the record is missing, it gives a clear hint.
- **Approval records support pagination** (#115).

#### Changed

- **Streamlined the model-review input** (#124, #125): the model review (the LLM judge after no rule matches) input is converged to a versioned JSON of "this one full tool call + an explicitly selected short background + the local working directory". The background takes only genuine user messages (the current user message of chat / the task's main Agent); a Worker no longer carries an intent summary, and the background inherited from the parent Agent is cleared; the planner and auto-triggered sessions do not fabricate a user message. It no longer carries the task description, goals, operation constraints, global exploration situation, call history, or the Worker's full intent — these still drive Agent execution and standalone-session auditing per the original logic, they just don't enter the operation review. All verdicts are required to output JSON with `decision`/`comment`, where the comment must contain "the actual operation, the consequence on success, the rule hit"; instructions in the arguments or the background cannot change the review policy. The snapshot and fingerprint of the input actually sent to the model are saved, with old-version snapshots retained and labeled by version, and the current data is not used to fabricate an old input.

#### Fixed

- **A verdict wrapped in a code block is no longer silently allowed** (#126): if the model's returned verdict JSON was wrapped in a ``` code block, parsing previously failed and it was silently allowed via the model-failure policy; now the code-block fence is stripped before parsing, and only if parsing still fails is the configured failure policy applied.

### Agent

#### Added

- **New experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature toggleable in system settings, off by default. When on, noa (model-driven context compaction) takes over the context compaction of the four Agent types — main Agent, planner, Worker, and chat — replacing the built-in compaction; the compaction source text is persistently archived, centralized under `<workDir>/noa/<session ID>/` (not scattered within each task directory), with a globally unique subdirectory per session ID. If wiring in fails, it automatically falls back to the built-in compaction without interrupting the real task; the switch is read once per run, and toggling only affects runs started afterward.

#### Fixed

- **Exploration-graph tools reject rather than nil-crash when there is no task context**: calling exploration-graph-related tools in a non-task context returns an explicit error, no longer crashing from dereferencing an empty store.

### MCP

#### Added

- **Support for legacy SSE MCP services** (#117): compatible with MCP servers that only provide the old-style SSE transport.

### Traffic

#### Fixed

- **Traffic search matches a record's host in a port-aware way** (#114): `traffic_search`'s host matching now includes the port, avoiding cross-talk between same-host records on different ports.
- **A `traffic_search` description migration error no longer interrupts subsequent reporter migrations**: a single migration failure is isolated and does not affect subsequent migrations.

### Web

#### Changed

- **The task retest vulnerability options get a divider** (#122): a divider is added between retest vulnerability selection items for visual clarity.

#### Fixed

- **Fixed a full-page crash on the demo task detail page**: in mock mode the task detail "Sessions" tab would fetch `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, and the read fallback returned `[]` via "a path ending in s is a collection", so `data.items` was `undefined` and the side-question hook's `merge()` iterating over it threw `TypeError: t is not iterable`; this exception occurred inside `setItems`'s updater, deferred by React to the render phase and rethrown, where the caller's `catch` couldn't catch it, and the whole page was taken over by the error boundary showing "This page couldn't load". Now the mock handler explicitly returns an empty side-question history, and `sideAPI.history` also normalizes the return value (a non-array `items` is coerced to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **Web search adds a DeepSeek official source**: directly reuses the currently active LLM configuration. It is different in nature from the other three sources — DeepSeek has no directly callable search API; search exists only inside its Anthropic-compatible interface (the `web_search_20250305` server-side tool) and is executed on DeepSeek's server. So it **only supports the DeepSeek official endpoint + the anthropic protocol** (an OpenAI-protocol endpoint rejects server-side tools outright), and each search **consumes one extra model call**, the request **does not go through the search egress proxy**, and it is **not counted in traffic tracing**, and the results have **only a title and a link** (no summary; use WebFetch to grab the body when needed). The settings page explains the above limitations but **does not enforce a validation block** — whether it's satisfied is up to the user to confirm, and the "Test search" button can run one real check.

### Agent

#### Added

- **Workers gain cross-work look-back**: adds `search_all_worker_traces` (search by keyword across all works' execution processes in this task for hit steps, without needing to know the intent_id first) and `get_worker_trace` (after locking onto a work, list its step flow, search in place by keyword, and fetch the full content by step_id), used to reuse observations another work saw but didn't write into a fact, avoiding duplicated effort.
- **`node_detail` is given to workers**: paired with the look-back tools above, a worker with the intent_id / node id can directly query that node's full detail.
- **A planning round triggered by `add_hint` is explicitly broadcast**: previously a new hint only folded the hint into the situation overview and let the planner discover it itself; now one `add_hint` records a trigger for the planner "a human added N strategic hints: …" (a batch counts as one, not flooding the screen one by one), and the planner is explicitly told "this round was triggered by a new hint" and sees the hint content directly.

#### Changed

- **The global situation prompt now takes a more open-ended tone**: it encourages broader exploration and timely reporting of leads across intents instead of converging too early.
- **Streamlined the worker's boundary wording**: an initial setback does not mean the direction is exhausted, focusing on "finish the bypass techniques within this intent before drawing a conclusion".
- `insert_assets` removes the per-asset `related` argument: its only effect was to decide whether to add the asset to the task scope, and the value wasn't persisted (re-registering the same asset voided it, and the UI couldn't show who was judged irrelevant), so in practice it just made the model do an un-retained judgment one more time.
- **The test scope (`task_scope`) is no longer affected by the asset-coverage switch**: `insert_assets`'s auto-into-scope (`source='auto'`) runs whether coverage is on or off, and `add_task_scope` is always provided to the planner / in-task main Agent / goal-decomposition Agent. The scope is the task's authorization boundary and the filter basis for asset queries; the coverage switch only decides whether to use it as the denominator for a metric, and shouldn't decide whether to accumulate the scope at all. Previously turning off coverage disabled both the `auto` and `agent` write paths, leaving `task_scope` with only manually-added rows from the UI. `list_untested_assets` is still hidden when coverage is off (it is itself a pure coverage view).

#### Fixed

- **Fixed cross-task asset bleed** (#59): `list_assets` previously hardcoded the task id to 0 and queried the entire shared asset repository bare, and the model had no way to write a scope filter, so it took another task's assets (especially IPs) as this task's target and was led off course. Now it only returns assets within the test scope (`task_scope`) of 【this task and directly related tasks】, matching by **attribution** rather than literal value: if the scope has a root domain, all subdomains/services/endpoints under it are queryable; if it has a CIDR range, hosts and services in the range are queryable; fetching an out-of-scope asset by id also fails. A non-task context (Auto/pentest) has no scope to rely on and still falls back to the whole repository. It also fixes the attribution blind spot where an IP-direct host (e.g. `http://1.2.3.4/api`) could not be matched by a CIDR scope. The UI's "test assets" view (filtered by task producer) is unaffected.
- **Fixed the worker cross-work look-back tools being wrongly removed**: `search_all_worker_traces` / `get_worker_trace` (and `node_detail`), after being added to the worker's default tool set, were unbound again at startup by an old migration that "converges the worker's tool surface", so workers didn't actually have these tools. They have been moved out of that unbind list, and a one-time rebind to the worker is applied to DBs that already ran the old migration.
- **Fixed occasional storage failures for `/btw` side questions**: before persisting the side_question checkpoint, strip the NUL (`\u0000`) escape unsupported by JSONB, avoiding a write error for content containing that character.

### Triggers

#### Fixed

- **A merged trigger session deduplicates the task description/goals by task**: when triggering multiple tasks merged, the same task's description/goals are no longer concatenated into the message repeatedly, avoiding a long goal piling up repeatedly and blowing up the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main Agent supports **multiple sessions**: you can create, switch, and independently reset context, and interact in each session.
- Support for persistent `/btw` **side questions**: follow up without interrupting the main line, with the Q&A stored and still present after a restart.
- `spawn_task` adds `source_task_ids`, so a new task can **read-only inherit** the source task's assets and findings.

#### Changed

- Turned off the worker's cross-engagement memory and converged its default tool surface: reading context and cross-work review are planning responsibilities, and the worker only handles the execution and write-back of a single intent.
- Streamlined the worker's default prompt; `get_worker_output` removes the `terminated` and `worker_name` fields; streamlined the descriptions of `insert_assets` / `list_assets`.

#### Fixed

- Fixed a work being cut short early by an **idle round**: the model sometimes uses a whole round to output only thinking, giving neither body text nor a tool call, and at that point the harness sees a natural end (`end_turn` with no `tool_use`) and wraps up directly with `completed` + an empty summary — so an intent that isn't done yet is cut off halfway, showing up as "the task ends normally but with no text summary at all". None of the five LLM retry layers can reach it: it isn't an error, the same-provider safety-window retry's entry point is "stream failure" and the circuit breaker judges `err == nil` as success, intent rerun only recognizes `model_error`; the SDK's empty-response retry can't reach it either, because that layer judges emptiness by "whether an event was yielded", and a thinking delta is itself an event. Now the work's Stop hook recognizes this kind of round and injects a continuation instruction, letting the model continue to the next step carrying the thinking it already produced — deliberately not choosing "resend as-is", because this kind of idling is usually determined by the prompt and the context shape, is stable behavior rather than random flakiness, and resending would only make the model think the same thing again.
- The retry limit reuses the **empty-response retry count** from the LLM page's "Retry and backoff" tab (both cover cases where the model finishes without substantive content, but with different criteria and remedies). It defaults to 2; `-1` disables it and restores the old "idle means wrap up" behavior. It is a **total** cap for one intent rather than a consecutive count: the harness itself limits consecutive idling to one continuation, and the quota only refreshes when a tool round actually happens, so this number guards against a loop like "tool → idle → continue → tool → idle" draining the intent budget. On trigger the log records `[work <worker> · #<intent>] idle round… injecting continuation instruction (n/N)`. See `docs/LLM重试设计.md` §1.1 for the design.
- Fixed `/btw`'s context budget and input layout under long conversations; in a non-secure context (non-HTTPS access), the side-question request ID generation is degraded.

### Traffic

#### Added

- Vulnerabilities support **linking multiple traffic evidence items**, which can be ordered, annotated, and distinguished by role (request/response/supporting).
- The report Agent **auto-links** relevant traffic evidence before writing the report.
- Added an Agent **traffic-binding switch**, and completed the hand-off of evidence between Agents.

### Tasks

#### Added

- Support for **session-level vulnerability retest**: the retest runs in an independent Agent session, with its run status shown in the list and detail.

### Interception

#### Added

- Approval records add **detail and execution auditing**: you can view the tool request context, the model/rule initial decision, the execution output, and the argument fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the test connection not re-sending the custom session header, causing opencode zen to return 400.

### Network

#### Added

- MCP's HTTP transport supports **skipping TLS certificate verification**, to make connecting to a self-signed-certificate service easier.

### UI

#### Added

- The session list is grouped by Agent, supporting expand/collapse and independent pinning; chat supports filtering by Agent.
- The attack-chain graph renders compaction (digest) nodes and collapses their members.

#### Fixed

- Fixed a white screen caused by login credentials being out of sync.
- The interception hint now clearly identifies the action as "platform control", so it is not mistaken for a defense on the target side.

### Deployment and updates

#### Added

- Added support for **one-click updates from the page**: the system configuration page now has a "Version and updates" card, and the top bar shows when a new version is available. The update flow downloads the release package, verifies `SHA256SUMS`, runs a smoke test, stages the update, then exits so the supervisor can relaunch the app and complete the swap. The page refreshes automatically afterward.
- Added the supervisor scripts `start.sh` / `start.bat` as the official startup entry (both the release package and the Docker image include them, `install.sh` unchanged), deciding whether to relaunch by exit code and responsible for forwarding SIGTERM to artex. The verification and swap logic are all in Go, keeping the scripts simple.
- Automatic failure fallback: if verification or the smoke test fails, discard it and keep running the current version; if the new version fails to start 3 times in a row, roll back to the previous version. The settings page also has a manual rollback (note that the database schema does not roll back).
- Updates only recognize GitHub domains and are forced over HTTPS; the release source is not configurable; development builds disable one-click update. GitHub query results are cached for 30 minutes to avoid the top-bar hint exhausting the API quota.

#### Known limitations

- Under Docker, only the program is replaced, not the image: the toolchain is not upgraded along with it, and rebuilding the container reverts to the versions bundled in the image; use `docker compose pull artex` when needed.
- The release package's `skills/` is not synced, so a new built-in skill added in a new version does not take effect automatically.
- Updating means restarting, which interrupts running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- The LLM page adds a "Retry and backoff" tab, where users can configure the **count** and **interval** for all five retry layers. A failed model call proceeds through these layers from inner to outer: connection-establishment retry (SDK; connection reset, timeout, 429, or 5xx before the stream starts), empty-response retry (SDK; a normal finish with no content, OpenAI format only), same-provider safety-window retry (replay after a stream breaks before any output reaches the caller), circuit breaking during provider-selection polling (cool down and skip a provider after consecutive failures reach a threshold), and intent rerun (rerun the entire intent after the worker ends with `model_error`). An outer layer runs only after the inner layer is exhausted. Each layer has two settings with consistent semantics: leave a field blank to use the original default count and exponential backoff; enter a count to use that count; enter an interval to replace exponential backoff with a fixed interval; enter -1 to disable that retry layer. The inner three layers are endpoint-specific and can be overridden field by field in each model configuration (setting only the interval still inherits the global count). Circuit breaking and intent rerun are process-level behaviors, with one global configuration for each. Changes take effect immediately after saving; no restart is needed. Leaving everything blank preserves current behavior, and upgrading an existing database leaves it byte-for-byte unchanged (the new columns default to 0, and a missing settings key means all defaults apply). The connection test deliberately omits these parameters: it has a hard 30-second timeout, and stacking user-configured retries on top could make a usable endpoint time out. See `docs/LLM重试设计.md` for the design.
- Each LLM configuration supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries this HTTP header, whose value is the currently running session id (`conv-<id>` for a chat session, `exp<x>-worker-i<intent>` for a worker, etc.), for gateways that do prompt caching / sticky routing by a session-id header. It is implemented by reading the session id from the request context and injecting it, needing no change to norma, so the same shared provider can emit different header values per session. The old DB has a migration, and the frontend LLM configuration dialog adds an input box.

#### Fixed

- Fixed a `23502` error on saving an LLM configuration when the session-header field was empty: the column is `NOT NULL DEFAULT ''`, and previously wrapping it with `NULLIF($n,'')` wrote an unfilled session header as NULL, triggering the not-null constraint; now it's passed directly with empty-string semantics.

### Agent

#### Added

- The wall-clock timeout now **wraps up in place**. With norma v0.3.6, when `MaxDuration` is reached, the harness interrupts the running tool and continues on the live context for a configured number of wrap-up rounds, persisting the results identified so far and a summary before setting the terminal state to `timeout`. It no longer relies on the worker's or planner's own external `maxDur+90s` timeout to kill a stuck run outright as `aborted_tools`. Chat uses the same harness and gets this behavior automatically; the Main Agent has no `MaxDuration` and is unaffected.
- Stall fallback: when a heartbeat or no-change event wakes the planner and the graph has no open or running intents, the opening message becomes a stall alert. It explicitly says that no worker is running and no work is queued, and requires the planner to create one or more distinct new intents that round (never zero).

#### Changed

- Reworked the worker prompt: the intent, initial instructions, and raw JSON for anchored assets are now included in the system prompt, rebuilt each round and never removed by compaction. Continuations no longer depend on the transcript retaining its first message. The initial user message is reduced to the global situation overview, designed to degrade gracefully and tolerate stale data. The trade-off is that the system prompt now includes per-intent data, preventing cache reuse across intents; this is a deliberate choice to avoid losing the intent.
- Streamlined the planner and worker default prompts and fixed several issues seen in practice. The planner now distinguishes `recent_done` intents by status: for `blocked` or `exhausted` intents, it checks the trace before deciding what to do, without treating them as dead ends or rerunning them blindly. It treats negative conclusions as observations or uncertainty rather than verdicts, and checks the evidence before accepting them. If the goal is unmet and no intent is open or running, the planner must create an intent; this is a hard requirement. It also prioritizes depth over coverage. For negative conclusions, the worker records only an observation and a tentative interpretation, leaving the verdict to the planner. It adds cross-intent leads to a fact's summary for the planner and does not pursue them itself. Reseeding now adds the new defaults as a new version and switches to it, preserving user-customized and older versions in the history for rollback.
- Converged the work agent's default tool set and asset write-back hints: the worker only handles the execution and write-back of a single intent, and reading context / cross-work review are planning responsibilities, so `list_facts` / `node_detail` / `list_companies` and the cross-work search (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) are removed from its default tools, leaving only `list_findings` (dedup before reporting a vulnerability) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt drops the stale field notes conflicting with the `insert_assets` schema (`type=tech`/`on_url`/`props`). The old DB has a one-time migration stripping the corresponding bindings, while the planner/main bindings of the same name are untouched.

### Exploration graph

#### Added

- **Exploration-graph cold-node compaction (`cold-digest`)**: folds old, long-inactive intents and facts into digest nodes shown in `graph_overview`. Original nodes are retained permanently and can be fully restored by ID; storage remains lossless, only the presentation is compacted, and folding is reversible. Cold/hot classification is based on reverse reachability, with any live branch treated as hot. It uses an R=6-round debounce, and nodes are grouped by connected component. Background compaction has two passes: a minor pass folds uncovered cold blocks, while a major pass refetches and recompacts data to merge fragments. It rechecks activity and uses a cooldown mutex; it never runs on the hot path or overwrites a revived node. The overview provides `cold_digests` and an asset-indexed `cold_index`; `expand_digest` and `expand_index` restore the full data. Overviews for related or inherited tasks also use their own folded views, and `expand_digest` supports read-only restoration across tasks. Only the Planner and Main Agent receive `expand_digest` and `expand_index`; Workers do not. These changes include a migration for existing databases.
- `graph_overview` now returns the full `finding_list`. Findings are the task's most valuable output and are usually few enough to include in full in the overview, unlike facts, which are limited to a recent window. This lets the planner and worker see all confirmed vulnerabilities at a glance each round without calling `list_findings`. Each finding is reduced to `{id, summary, evidence?, from_intent?, assets?}`; affected assets are included as readable values (URL, domain, or IP:port) instead of bare IDs.

#### Changed

- `graph_overview` no longer includes the full `hosts` list inline. The coverage block now provides only `host_count`, since a large-scope task could otherwise carry up to 500 host strings per round with little value for planning. Specific hosts can be queried on demand with `list_assets`. A new top-level `done_intents_total` gives the total number of finished intents, alongside `recent_done_intents`, which is truncated to ≤15. This lets the Planner know whether items were omitted when deduplicating.

### Tools

#### Changed

- The default binding for `add_company_scope` now changes from Worker to Planner: defining a company's asset scope belongs to planning, the Main Agent, or Auto, while the Worker only carries out exploration. New databases seed the binding for Main Agent/Planner/Auto; existing databases receive a one-time migration.
- The planner binds `list_assets` by default: it now has both `list_assets` (DSL whole-repository search) and `list_untested_assets` (untested within scope); an old DB is back-filled once, not overwriting a user's unbinding.

### Tasks

#### Added

- The task list adds a "Running Workers" column: counts the intent nodes with `state='running'` under that task, with the same accounting as the task detail page overview's "Running Workers", showing 0 when there's no running Worker.

### Network

#### Added

- Added a **global egress proxy** configuration for routing all target traffic through one proxy (HTTP, HTTPS, or SOCKS5; `user:pass` authentication is supported). When traffic capture is enabled, the global proxy becomes the upstream for the MITM recording proxy: traffic is still recorded, then sent through the upstream. Both intercepted and pass-through traffic use it, so the source IP is not exposed. When capture is disabled, proxy settings are injected directly into the Agent's Bash environment and WebFetch; `proxyEnv` adds `ALL_PROXY` for SOCKS5 support. The configuration is stored in the settings KV table, so no migration is needed. The system configuration page adds a "Global proxy" card, independent of the web-search and LLM proxies.

### UI

#### Fixed

- Fixed the semantic errors of the intent status labels: `exhausted` "exhausted" → "budget exhausted" (it actually means the step/time budget was reached and it was cut off midway, writing back only partial results, not that the direction is fully explored), `blocked` "intercepted" → "execution error" (it actually means the model / API / network fault retries were used up and the intent basically wasn't really explored, not a target / WAF interception), and the previously missing `stopped` "stopped" (a work the user manually stopped) is added.

### Dependencies

#### Changed

- Upgraded norma to v0.3.4: MCP tool output gains truncation and disk-spilling (see `651b961`), and the subsequent v0.3.6 supports wall-clock wrap-up in place.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model configuration adds an "Output cap", with an option for which request field name the cap uses: the cap limits how many tokens at most a single reply generates, sent with each request, with 0 (default) = don't send the field and let the server default decide; it is a different thing from the "Context window" — the latter is the model's total capacity, used only locally to compute the compaction threshold and not appearing in the request. The field-name switch is only meaningful for the `openai` (Chat Completions) format: blank (default) sends `max_tokens`, which almost all compatible gateways only recognize; OpenAI's official reasoning models (the o series / GPT-5) conversely only recognize `max_completion_tokens` and return `unsupported_parameter` on receiving `max_tokens`, so such endpoints need to be manually switched to the new field. Each format pins its own field name (Anthropic uses `max_tokens`, the Responses API uses `max_output_tokens`), so for a non-openai format the switch is grayed out and cleared on save. It also completes a path that had never been wired up: planner / worker / chat / main Agent / goal decomposition had never set an output cap, so the openai family didn't send it at all and Anthropic used the SDK's 8192 fallback, and just adding the config option without wiring this line meant a filled-in number didn't take effect; now the value is resolved per round like "streaming output", taking effect from the next round after a failover switch. An old DB is completely unchanged in behavior after upgrading (the new column defaults to 0 and an empty field name).
- When clicking "Test" in the backend, the status code and the gateway's raw response body of each HTTP attempt are logged to the server log (the body is trimmed to 4K), to help diagnose 401, quota wording, empty frames, an HTML page being returned, etc., rather than seeing only the UI-collapsed ok / err. This log does not depend on the "LLM recording" switch.

#### Changed

- The task's LLM configuration chain can now be edited in any state, not only while the task is running, paused, or has exhausted its configuration chain. The main Agent continues to use this chain after a task ends (`done`, `failed`, or `timeout`), so a problem with a chain model could previously prevent further interaction when the chain could not be edited. Terminal-state guards have been removed from both the backend HTTP handler and the database transaction, and the frontend now allows editing and saving for terminal-state tasks. Saving a configuration on a terminal-state task does not reopen quota-blocked intents: moving them to `open` would leave them without a Worker and make them ineligible for the "rerun intent" flow. To resume work, use "Rerun intent" or "Add goal"; either action returns the task to a running state.

### Agent

#### Changed

- Steering a running Worker by message now reuses the existing pause/resume and transcript-resume mechanism, matching the main Agent conversation. Previously, it used a custom persistence protocol for interventions that affected the scheduling barrier, resume flow, and more than a dozen activity-query filters. The message is now injected into the next round's input, and the intent runs on demand in its own goroutine, outside the three-slot Worker pool. The frontend retains the message box and restores the "continue directly" button; messages are sent over SSE. No database schema change is needed. The trade-off is that messages exist only in memory and are not recovered after a crash. When a message is sent, the task may briefly exceed its concurrency limit by one Worker; this is an uncommon, accepted case.

### Tasks

#### Fixed

- Fixed an out-of-memory (OOM) crash when archiving a large task. The archive now streams the snapshot to disk instead of loading the whole task into memory. This also closes three recovery gaps in the cold-archive path. Modern installs store traffic only in SQLite; previously, a crash between the PostgreSQL and SQLite commits could leave an archived task's traffic stranded in hot storage with no recovery path. A staging log is now written whether or not a history directory exists, and a missing `journal.json` is treated as disposable.
- Fixed system lag with many tasks: there was a noticeable delay loading conversations and loading the UI. The queries for the task list, task context, and exploration records are optimized together, and the frontend's dashboard, chat page, and task detail page simultaneously reduce duplicate requests.

### Assets

#### Changed

- Removed the "at most 256 rules" limit on a company's asset scope. A company that defines its scope by entering IPs or domains one at a time can quickly hit this limit and be forced to split across multiple companies, even though having more rules does not by itself make scope evaluation slower. The limit is removed in the frontend, backend, and demo mock. Each rule is still limited to 1024 characters, and the 2 MiB request-body cap remains as a safeguard (roughly 40,000–50,000 rules).

### Skills

#### Fixed

- Fixed "Upload failed: zip: unsupported compression" when uploading a skill archive. The Go standard library only includes Store and Deflate decompressors, so it could not open archives compressed with bzip2 or Zstandard at non-default settings. Pure-Go decompressors for both methods have been added without a new external dependency. For methods that remain unsupported (such as Deflate64, LZMA, XZ, and PPMd) and for encrypted archives, the uploader now shows a Chinese hint before decompression that names the file and compression method and explains how to repackage it, instead of exposing the underlying English error.
- Fixed skill-archive path validation rejecting Chinese filenames. It previously used the ASCII allowlist `[A-Za-z0-9-_./]`, so one Chinese-named file (for example, `参考/说明.md`) caused the entire upload to fail with "the archive contains an illegal path." It now uses a Unicode denylist: filenames in different languages and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (which can spoof names using RLO), `\ % # ? * : " < > |`, `..`, absolute paths, and empty path segments remain prohibited. Directory-traversal protection is unchanged. Skill-name validation is also more permissive: ASCII names remain limited to lowercase letters, digits, and hyphens, as required by the agentskills.io spec; non-ASCII letters such as Chinese are allowed, but spaces, dots, and path separators are not.
- Fixed garbled filenames and upload failures involving Chinese-named files in archives created by Windows compression software. These ZIP files omit the UTF-8 flag and store filenames in GBK, so the uploader now tries GBK decoding as a fallback before path validation. Quoted frontmatter such as `name: "中文技能"` now also resolves the skill name correctly.

### UI

#### Added

- The Findings page adds an "All findings" flat view and makes it the default. A header tab switches between it and the original "Grouped by task" view. In the grouped view, you have to expand each task to see its vulnerabilities, which makes it cumbersome to scan findings across tasks. The flat view shows them in one cross-task table (10/20/50/100 per page) and supports the same actions as the grouped view: select findings to export, expand a row to see evidence and the detailed report, rename or recategorize a finding, change its severity or disposition, drill down, and delete it. Both views share the stat cards and filters. Switching views preserves the filters, and the current view and filters are stored locally. Polling only refreshes the current view, while in-row changes update both caches so switching views does not show stale data.
- The Findings page adds a "By asset" view. The left pane shows an asset tree (company → root domain/IP → subdomain → service → endpoint); the company level appears only for assets with a company association. The right pane lists findings under the selected node's entire subtree, using the same table and filters as the other views. The tree includes only assets with findings and fills in ancestor nodes as needed, so higher levels still appear when a finding is attached only to the deepest endpoint. Counts are aggregated across each node's subtree and deduplicated by finding. Findings linked to deleted assets, as well as findings with no associated asset, appear in an "Unassociated assets" bucket. Unlike the other views, "By asset" does not poll. It queries when the user opens the view, changes a filter, adds, deletes, or edits a finding on the page, or clicks the tree's refresh button. Since the tree is for navigation, it does not need to be recomputed every five seconds. Each tree level shows only the part that differs from its parent (for example, a subdomain without the root-domain suffix, a service as `https :443`, and an endpoint as its path). The full value appears in the hover tooltip and breadcrumb. If there are too many nodes, the entire level omits endpoints and services and displays a hint; those counts are still included at the parent level.

#### Fixed

- Fixed the session records being squeezed and not fully visible in the mobile session detail: on a narrow screen the session list is collapsed, giving the width to the records themselves.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: adds `Config.MaxTokensField`, letting the OpenAI Chat Completions output cap be sent as `max_completion_tokens` instead (reasoning models only recognize this key). The two keys are mutually exclusive and only one is sent, defaulting to `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security advisories.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "non-streaming" switch reverting to "streaming" after being saved and reopened (#69): the configuration-list endpoint's DTO omitted the `streaming` field, so the response never returned it, and the frontend reading `undefined` always fell back to the default streaming; the actual write and DB storage were fine, it just couldn't be read back. The DTO adds the field back (without `omitempty`, so `false` must also appear in the response).
- The connection test now verifies that the model actually replies and uses the configuration's real request/response mode (#65). Previously, it checked only whether the HTTP request succeeded, so it could report "connection successful" even when the model returned an empty reply (for example, because thinking used the budget, a safety policy swallowed the body, or a compatibility layer dropped `content`). This differed from a conversation, where the model's failure to reply was visible. The test also always used streaming, so non-streaming configurations were tested through a different channel. Now an empty reply is treated as a failure, a successful test displays the model's reply, and the test uses the current streaming setting. This makes a passing test consistent with a working conversation.

### Agent

#### Changed

- `list_facts` now supports pagination and keyword filtering, preventing a single call from filling the context when there are many facts (#74). It returns the 20 most recent facts by default and supports `limit` (maximum 100), a `before` cursor, and `q` to search summary text. The response is `{facts, total, has_more, next_before}`. An overly long summary is truncated by character count; the full text remains available through `node_detail`. The Worker and Planner prompts now follow the pagination behavior. Existing databases receive a one-time migration that adds the new parameter schema to the tool catalog (`SeedTool` runs only on first insert; otherwise the tool-management page would show "no parameters").

### Tools

#### Added

- The "Tool execution" page adds tool-call statistics (#72). Its "Stats" toolbar button opens a dialog showing each tool's call count, share, and failure count, sorted by call count. The statistics use the list's task and keyword filters but cover the full result set, not only the current page. Data is fetched only when the dialog opens.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixes reasoning/refusal being silently dropped in OpenAI-family responses (the Responses API adds `reasoning_text`), deduplicates the three names of the reasoning field, adds bounded retry for an empty response (zero content blocks), and fixes gateway 400s from a compaction boundary cutting a tool pairing (also fixing the existing orphans already baked into the transcript).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM configuration supports a streaming/non-streaming toggle (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, returning the complete JSON at once), which can work around some gateways' bad SSE implementations (empty frames, dropped thinking-field frames), at the cost of losing in-progress real-time progress and real-time token counting. worker / planner / mainagent / chat / goals all take the value dynamically by the currently active configuration; the three Provider-wrapping layers `llmpool` / `llmrec` / the task runtime are all compatible with non-streaming; `llm_profiles` adds a `streaming` column and `ALTER`s an old DB (default `true`, so old configs are unaffected).
- Support for the OpenAI Responses API format in LLM configurations: each configuration adds a third format `openai-responses` (hitting `POST /v1/responses`), alongside Chat Completions / Anthropic; the `BaseURL` is normalized, the default model is `gpt-5`; `llm_profiles`'s `format` constraint adds `openai-responses` and idempotently migrates an old DB; the frontend format dropdown adds "OpenAI (Responses API)". Depends on the norma v0.3.1 upgrade (which includes the `reasoning_content` echo fix).
- Record the raw HTTP of LLM requests/responses: capture the real wire body at the HTTP transport layer, preserving the tool schema, `tool_use` blocks, and raw SSE frames invisible in the normalized view; norma's internal retry attempts are each preserved; `llm_records` adds two columns `raw_request` / `raw_response` and `ALTER`s an old DB; the recording page's detail panel adds a "Raw" view toggle and request/response copy buttons (with an `execCommand` fallback compatible with a non-secure context).

### Chat

#### Changed

- The session list now uses a "Multi-select" mode switch. By default, it no longer shows a checkbox on every row, keeping the list cleaner. The header shows an "N total + Multi-select" button; clicking it enters selection mode with checkboxes, select all, and bulk delete. "Done" exits selection mode and clears the selection. A fully successful bulk delete exits automatically; if any item fails, selection mode stays open for another try. Rename, pin, and delete for a single item remain in that row's ⋯ menu.

### UI

#### Changed

- Enhanced the task management, session operations, and traffic viewing experience (#57): the task-list column sort preference can be persistently remembered, in-row rename is triggered directly by an icon (no longer via a menu), and the sheet interaction and traffic-viewing details are refined.

#### Fixed

- Worker asset tags show only domains and IPs, and correctly handle the empty-tag case.

### Agent

#### Added

- The planner's goal judgment adds a "quantified acceptance check": when a goal contains a quantifiable acceptance condition (asset test coverage reaching X%, obtaining N flags, obtaining a certain privilege), before `prove_goal` it must check the measured value in `graph_overview` (`coverage.pct`, finding counts, etc.), and if the measurement doesn't meet the bar, `prove_goal` is forbidden and it instead dispatches an intent to close the gap, and it must not mark it met early on the grounds of "the main part is done / basically achieved". This fixes a problem where a goal required 100% coverage but was judged complete at a measured 40%.

#### Changed

- Rewrote the planner prompt's criteria for producing "0 intents this round." The original described zero intents as "the most common, most important principle," which could make the planner stop too early even when the goal was unmet and the scope still had untested surfaces. Now the planner should produce zero intents only in two cases: (1) every considered direction is already covered by an `open`, `running`, or `recent_done` intent; or (2) the next step depends on the output of currently running work that is not yet available. In the second case, it should wait for that work to finish and plan again after the graph updates. Conversely, if there is an uncovered direction that does not depend on running work, or the goal is unmet and untested surfaces remain, the planner must not stop merely because "0 intents is common."
- Strengthened the "evidence bar for a negative conclusion" in the worker prompt: for a negative conclusion like "not injectable / port closed / no login entry" that might make the planner abandon a whole direction, it is required to exhaust the reasonable techniques within that intent (change encoding/parameters/path/method) before drawing the conclusion; if the techniques aren't finished or the evidence is weak, mark `confidence=inferred`, to avoid a hasty `observed` negative welding a whole route shut (an erroneous negative early in the task especially leads the direction astray and is hard to self-heal from).
- The above planner / worker default-prompt changes are appended as a new version via a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag) and switched to as the current version; the old versions are kept in the version history, and a user who has customized the prompt can restore from the version record.

### Operations

#### Added

- Added `reset-password.sh` to reset the admin (username fixed as `ARTEX`) password: supports both local / docker deployments, with connection info specifiable explicitly or read automatically from `--dsn`/`$ARTEX_PG_DSN`/`config.json`; it uses `pgcrypto` in the DB to generate a bcrypt hash compatible with the backend login and writes it back to `settings.auth.password_hash`, needing no service restart after the reset. The password is passed via an environment variable and does not enter the process argv, with escaping to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting multiple tasks in the task list, you can bulk-change their category, with the target category supporting "Uncategorized" to move tasks out of the current category; the whole batch is written in one transaction, and the only failed items would be tasks already deleted after selection.

#### Changed

- The new-task "Task category" field is now a searchable input instead of a dropdown. Typing filters existing categories. Pressing Enter creates and selects a name that is not in the database; the same can be done by clicking "Create" in the dropdown. The selected category appears as a removable tag, and only one category can be selected.

### Chat

#### Fixed

- Fixed sending a message being blocked by "LLM not configured, cannot chat" when a session had a selected LLM configuration but there was no globally active configuration. The pre-send check originally only looked at the global active configuration, while the actual execution preferred the session's selected configuration — the two resolution logics were inconsistent; they are now unified into one (session/Agent binding first, global fallback).
- The "cannot chat" hint is subdivided by state: with no configuration at all it hints "Add a configuration", with a configuration present but not active it hints "Activate one or specify a configuration for this session", no longer always showing "not configured", making it easier to pinpoint.

### Agent

#### Added

- For a task whose goals are all achieved, an intent dispatched by the main Agent via `add_intent` can be claimed and executed directly by a Worker and stop when done: at this point the Planner no longer runs (a task with no open goal doesn't enter planning), avoiding it re-judging the goals as achieved and canceling the just-dispatched intent; after the frontier is drained the task returns to completed. Before dispatching an intent, the main Agent asks the user, based on the intent's content, whether to register it as an official goal; if registered, the task resumes regular autonomous planning.

#### Changed

- If `step_ids` exceeds the per-call limit of 5 for `get_worker_trace` or `get_task_worker_trace`, the call no longer fails. It returns the full content for the first five steps and uses `returned_step_ids`, `omitted_step_ids`, and `notice` to show which IDs were included and which remain, with a hint that no further fetch is needed if the returned content is sufficient. Duplicate and invalid IDs are removed before the limit is applied.

#### Fixed

- The task list now sorts by descending task ID (newest first) instead of creation time. Tasks created at the same moment can have identical timestamps, making the old sort unstable. Combined with 10-second polling and nondeterministic iteration over an in-memory map, this often caused tasks to swap positions in the list.

### Assets

#### Fixed

- Fixed company-attribution recomputation failing because of a single dirty row. If `assets.ip` contained a hostname written by the Agent or asset API, casting `a.ip::inet` raised `22P02`, causing company-scope changes and company deletion to fail and roll back. The code now uses the safe conversion `try_inet()`, which skips invalid values instead of aborting the statement.
- An unparseable `ip` is no longer silently skipped: after saving a company scope, it explicitly hints how many assets have an `ip` that is not a valid IP (with the specific id and value), noting that these assets will not be attributed by IP/CIDR rules; the same information is also written to the server log, covering paths with no frontend response (deleting a company, scopesentry sync, Agent writes).
- IP/CIDR matching for the task's test scope now also uses `try_inet()`. This replaces the old regex guard, which checked only the character set and let hostnames made up of hexadecimal letters, such as `abc.def`, slip through and trigger the same error.

#### Changed

- The `ip` field must now contain an IP address for `ip`, `service`, and `endpoint` assets; hostnames are rejected. `insert_assets` and the asset API return an indexed error for each affected item, along with the fix: use `type=subdomain` and provide `domain`, or resolve the A/AAAA record first. The Agent can then correct and resubmit that item. Other assets in the same batch are still stored normally.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- A running Worker supports per-item pause, resume, and cancel; pause keeps the intent, facts, and vulnerabilities, and cancel transactionally cleans up the current intent and its direct output after execution exits.
- Added the `paused` intent state, an execution barrier, and named termination reasons, to prevent late blackboard writes after a pause, cancel, or task deletion.
- Added an optional task concurrency cap; new, resume, vulnerability dive-in, and queue backfill all go through a unified persistent FIFO admission path.

#### Changed

- The Worker's default wall-clock limit for a single run increases from 600 to 1200 seconds. After a timed-out task is revived, the timer restarts when its next run actually begins.
- The real execution state of the Worker, Planner, and main Agent uniformly drives the task status badge, fixing the problem of showing "idle" while a Worker was running.
- The Worker session keeps per-item control and the current session's token summary, with the model name moved to the right of the current session title.

#### Removed

- Removed the Worker multi-select, select-all, and bulk pause/resume UI, and removed the Worker bulk-control API and mock contract.
- Removed the token badge and tooltip from the Worker list rows, while keeping the underlying full token ledger and the task-aggregation endpoint.

### LLM

#### Added

- Tasks support an ordered LLM configuration chain; when a provider's quota is clearly recognized as insufficient, it automatically switches to the next configuration, persisting the current configuration, the exhausted state, and an error summary.
- Running and paused tasks support editing the full configuration chain, reordering it, and manually switching the current configuration, taking effect from the next LLM call.
- Automatic switches, manual switches, and whole-chain exhaustion are written as structured system activity, and surfaced as a dedupable reminder via the task activity stream.
- Added a task-role model resolution endpoint, uniformly resolving the configuration and model the Main Agent, Planner, and Worker will use on their next call.
- Added an optional global LLM Pool, supporting a configured call order, a per-model failure fallback, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger, aggregating input, output, and cache tokens by task, session, model, and configuration.

#### Changed

- Goal decomposition, Planner, Worker, and main Agent share a task-level LLM runtime (resolution priority below).
- Failover only responds to an explicit quota, balance, or billing error; ordinary rate-limit, auth, network, server, and context errors do not falsely switch.
- The LLM configuration page is redesigned as a list of configuration cards with a right-side editing drawer. It also shows the Pool's round-robin order, priorities, exclusions, health status, and recovery actions.
- The configuration-chain-exhausted notice now wraps on narrow screens. The current model is shown as a text label beside the session title, with its full name in a tooltip, instead of as an icon in the session list.
- The per-role LLM resolution order within a task is adjusted to "Agent binding → task configuration chain → global": a role with an explicitly bound model always runs on that model, and only an unbound role falls to the task chain, falling to global when the task chain is empty.
- An empty outbound proxy means direct connection, no longer falling back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); the proxy input supports `socks5://user:pass@host:port` with a username and password.

#### Fixed

- A transient streaming failure before submission (before any output is produced) is safely backed off and retried on the same provider, significantly reducing the case of "ran only one or two tools then broke off, no summary, terminal state `model_error`"; quota exhaustion, over-long context, and deterministic 4xx errors are not retried, still handled by failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon-style indicator from the session list, to avoid confusion with the Worker status icon.

### UI

#### Added

- Tasks support create/rename/delete and filtering of global categories; a new task can choose a category directly.
- Added global task-template CRUD and a right-side management drawer; a new task can load a preset description and goals, or save the current content as a template.
- A new task supports linking multiple source tasks and multiple company asset scopes; a company scope auto-recognizes domains, URLs, IPs, CIDRs, ICPs, and company keywords from a single multi-line text box.
- Task test assets support real-time add and delete, recording the source as manual, company, inherited, or Agent-discovered; the current test assets and a source summary are shown next to the Worker session title.
- The Findings page is grouped by task with independent per-group pagination, and supports filling in a vulnerability description and creating a high-priority dive-in intent.
- Chat supports rename, pin, unpin, and delete; the task list supports bulk pause and resume of the current page.
- Traffic details and tool-execution details now open in a right-side drawer. HTTP messages include the `Host` header, and the request line, status code, headers, JSON, and markup-language body are highlighted.
- The task-list action column adds "Detail" and "Pause/Resume" buttons; the session send key can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and the web-search area adds a proxy input box.
- The badge beside the session title now shows the LLM configuration name; the model ID appears in a hover tooltip. Tasks can also have an optional name, and the task list adds a name column and search, falling back to the description when the name is empty.
- The Skills page adds usage count, last-used time, and missing-dependency statistics, making it easy to pinpoint a skill that isn't taking effect or isn't installed.

#### Changed

- The task title is now a detail link that can receive keyboard focus. Task assets, company assets, finding-task groups, and vulnerabilities within a group use server-side pagination, stable sorting, and exact total counts.
- The company form now opens in a right-side drawer. Scope entry uses a single multiline field with real-time recognition, validation, and a type preview.
- The main Agent input box supports auto-growth, `Enter` to send, `Shift+Enter` for a newline, and avoids a false send during a Chinese IME composition.
- Agent preview, task reports, and related details uniformly use a shared Markdown renderer; delete confirmations, long errors, and the mobile drawer width are uniformly fixed.
- The task-template select shows and searches by template name and submits by template ID; the system's original font size is restored, and the app version is uniformly shown as `0.3.3`.
- The task total and the current session's tokens highlight only the input, cache-read, and output values; the send button uses a cleaner up-arrow icon.
- The task overview's test scope shows the company name rather than the ID.

#### Fixed

- Ordinary text containing a dot is no longer misjudged as an ICP filing number.
- Fixed the problem where a name in the variable directory colliding with a global runtime variable (e.g. `{{.Now}}`) caused the Agent editor's variable list to render duplicate keys.

#### Removed

- Removed the standalone "Enter" button on the task card, unifying it to clicking the task title to enter the detail.
- Removed the dashboard's "New task" button, the Logo URL input when adding a company, and the vulnerability-count Badge at the top of the Findings page and in the task groups.
- Reverted the global 10% font enlargement, and removed the Worker select box, the model icon, and the row-level token stats from the session list.

### Agent

#### Added

- A new task can directly link multiple existing tasks, inheriting in real time and read-only the goals, facts, vulnerabilities, completed intents, asset scope, and blackboard context of its direct source tasks.
- The blackboard read tools support querying a source task's nodes, facts, vulnerabilities, and execution traces on demand; inherited nodes carry a source marker, and all write tools refuse to modify them.
- The company-scope tools support domains, URLs, IPs, CIDRs, ICPs, and company keywords; keywords are only an Agent scope hint and don't participate in automatic asset attribution.
- A vulnerability dive-in operation creates, in the original task, a high-priority manual Worker intent with an asset anchor and a `derived_from` edge.
- The overview adds a "Goal management" card, supporting manual viewing, adding, modifying, and deleting of goals; adding or modifying notifies the Planner and revives the task, while deleting hard-deletes the goal node (cascading to delete edges and anchors) but doesn't revive it.
- The main Agent gets the `steer_work` tool by default, to inject a real-time course-correction instruction to a running Worker without interrupting it or losing progress (still validating the intent belongs to this task).

#### Changed

- Source-task intents don't enter the new task's frontier, and the new task continues to use an independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys its data. Instead, the intent is stopped with state `stopped`, and the user must provide a deletion reason. That reason is attached to the intent as a fact and written to its payload; the Planner receives it through a `cancelled` trigger. The intent content and reason are retained.
- The main Agent conversation is now driven by the server-side activity stream, and failed sends can be recovered from the input box. Opening a session scrolls to the bottom automatically and keeps the latest reply visible while the details load lazily.
- Pausing a task terminates the current Main Agent, Planner, and Worker calls, but doesn't prevent the user from actively starting a new main-Agent orchestration session during the pause.
- Cancel, close, and streaming interruption uniformly preserve the real termination reason, the generated content, the run round count, the elapsed time, the tokens, and the unreturned tool calls.

#### Removed

- Removed the main Agent client-side optimistic message echo, to avoid duplicate messages and cross-session streaming under pause, failure, or concurrent activity.

### Constraints

#### Added

- The goal-decomposition stage first extracts operation constraints (allow/deny) before decomposing the goals, and the main Agent can supplement them at runtime; constraints are injected into the Planner and Worker system prompts at the highest priority, and after debiasing, diversity and surface-expansion exploration explicitly obey the constraints.
- The overview adds a "Constraint management" card and add/modify/delete endpoints; the constraint injection scope can be toggled separately for Planner / Worker (both on by default, read each round, taking effect immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, auto-created on an old DB.

### Interception

#### Added

- Command interception adds a model-fallback approval beyond the regex/string rules: when no rule is hit, the model makes an `ALLOW`/`ASK`/`DENY` semantic decision, with the fallback action on failure and the action on approval timeout configurable; the interception page is split into two tabs "Interception rules / Model configuration", and a model decision result is annotated with a `[model]` prefix and a reason.

#### Fixed

- Hardened the judge-output parsing, to avoid a correct `DENY` verdict being mistaken for an allow.

### Traffic

#### Changed

- Traffic recording now stores each complete exchange in SQLite; large bodies spill into a hash-deduplicated blob store. A trigram full-text index on text bodies supports arbitrary substring and Chinese-language searches. Deletion is now a single SQL transaction, reducing it from hours to milliseconds and avoiding pauses in recording. Large-body streaming downloads and a `traffic_search` full-text parameter are also added. Existing file-tree data needs no migration and remains readable, searchable, and deletable.

### Tasks and assets

#### Added

- Creating a task adds an "Asset coverage feature" switch (on by default): when off, coverage isn't computed or shown, the situation graph shows only assets, `task_scope` isn't auto-accumulated, and the related tools are removed from the Planner / main Agent; company association is unaffected by this switch.
- `insert_assets` adds a `related` marker per asset (default `true`): effective only when coverage is on, with `false` storing only to the shared asset repository and not counting toward this task's coverage (e.g. an incidentally discovered side-site or unrelated asset).
- Company scope and task test assets uniformly support text recognition of domains, URLs, IPs, CIDRs, ICPs, and keywords; domains/IPs can create or reuse a global asset, while CIDRs/ICPs/keywords are kept as task-scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 at once, and generates zip release packages with `skills/`, a config example, and the README.
- The Go linker strips debug information and creates ZIP release packages. UPX is now explicitly optional to avoid startup segfaults from its self-extracting ELF in some Linux environments.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and ships `SHA256SUMS` with the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- A new task supports linking multiple existing tasks, inheriting in real time and read-only the facts, vulnerabilities, completed intents, asset scope, and blackboard context of its direct source tasks; the new task still uses an independent execution queue, working directory, and session history.
- Tasks support an ordered LLM configuration chain. When a provider's quota is clearly recognized as insufficient, it automatically switches to the next configuration, persisting the current configuration, the exhausted state, and structured audit activity.
- Running and paused tasks support editing the LLM configuration chain, reordering it, and manually switching the current configuration; automatic switches, manual switches, and whole-chain exhaustion are hinted on the task detail page.
- A running Worker supports pause, resume, and cancel. Pause keeps the blackboard data, and cancel transactionally cleans up the intent and its directly produced facts, vulnerabilities, and execution records after the Worker stops writing.
- Deleting a task can optionally also clean up the associated assets, traffic, vulnerabilities, and task landing files, with a delete barrier, concurrency protection, and auditable delete statistics added.
- Added an optional task concurrency cap; a new task that reaches the cap queues FIFO and starts automatically after a run slot is freed.
- Added task-asset pagination, company-asset pagination, task-category statistics, and a paginated-intent mock contract.
- Added the `build.sh` single-binary build script, supporting static frontend export, resource embedding, cross-platform targets, and build-version injection.

### Changed

- The task-level explicit LLM configuration chain coexists with the existing global LLM Pool; an explicit chain still uses strict quota failover semantics, and without an explicit chain it follows the Agent binding and global configuration rules.
- The main Agent input box supports auto-growing multi-line input, `Enter` to send, `Shift+Enter` for a newline, and avoids a false send during a Chinese IME composition.
- The Planner, Worker, and main Agent share a task-level LLM runtime; the task chain uses the smallest context window among the candidate models as the safe compaction threshold.
- The task detail page shows running, idle, and paused states by the real LLM-call status; a source task's facts, vulnerabilities, intents, asset references, and graph nodes are uniformly marked with their source and kept read-only.
- Agent preview, task reports, and related detail views uniformly use a shared Markdown render component.
- The LLM model configuration uses a card-and-drawer interaction, with the model list independently scrollable inside the drawer.
- Pausing a task no longer blocks the main Agent conversation: the main Agent orchestration session is independent of the task pause, and new messages can still be sent during the pause (the pause only terminates the current in-progress round).
- The Worker's default wall-clock limit for a single run increases from 600 to 1200 seconds.

### Fixed

- Removed optimistic message echoes from the main Agent console and switched to rendering server-side data only. This fixes message ordering and cross-session leakage during pauses or send failures.
- Main Agent sessions now open scrolled to the bottom. After the latest reply expands lazily, its full contents remain visible instead of being pushed off screen.
- Fixed the main Agent session continuing to run after the task was paused, and the orchestration Agent not synchronously stopping the main Agent when it paused the task.
- Fixed the task status badge and action-button state being out of sync with the actual run of task completion, Planner, Worker, or main Agent.
- Fixed possible late blackboard writes or residual files among Worker cancel, task deletion, and concurrent writes.
- Fixed a broken Markdown preview, delete-confirmation long-text overflow, mobile width, and some task-detail buttons being misaligned.
- Added named termination reasons for all Agent cancel paths, so the activity detail can show the canceler, the terminal state, the run round count, the elapsed time, the token usage, and the unreturned tool calls.
- Fixed a race where the parent context on backend shutdown could preempt and overwrite the named `shutdown` reason, and the problem of a Chinese activity summary being byte-truncated into garbled text.
- Fixed a cancel event with partial streaming output losing the real termination reason, while preserving the content generated before the cancel.

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
