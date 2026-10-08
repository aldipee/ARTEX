---
name: api-recon
description: Invoke this skill when collecting a website's API endpoints.
---

# API Recon (frontend endpoint reconnaissance)

With **authorization** in place, discover as completely as possible: the **backend API** (paths, methods, parameters, response bodies), the **frontend routes**, and the **UI feature trigger points** (tabs, dialogs, table actions, etc.).

---

## Boundaries and prohibitions (the Agent must read · crossing them is out of bounds)

This skill is limited to **API and parameter-surface reconnaissance**; it is not for vulnerability discovery or exploitation.

### Task boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Goal** | enumerate paths, methods, parameters, routes, UI trigger points | SQL injection, XSS, privilege escalation, brute force, vulnerability fuzzing, packet-tampering attacks, destructive operations |
| **Auth** | Hook + stub/mock to bypass the **client-side** login gate | asking the user for or guessing a username/password; attempting a real login-form submission |
| **Runtime** | hook endpoints without credentials, using mock responses to let the SPA enter the post-login shell | flows that require a real backend session to continue |

### Credential-free dynamic analysis (the default for Phase 3)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap endpoints such as login, permissions, and menus;
2. Return a mock body with **the correct structure, a successful business-status code, and data that may be empty** for business query endpoints;
3. Make the frontend still render the post-login pages even with no backend or a 401 environment, thereby triggering more XHR/fetch/WebSocket;
4. **Empty data, blank tables, and placeholder UI are all expected** — do not switch to a real login or vulnerability testing because of this.

**In short**: use mocks to make frontend routes render and components mount, and **only record outbound requests**. The backend's response content does not matter; what matters is **which endpoints the frontend still calls**.

### Strict workflow prohibitions

| Prohibited | Alternative |
|---|---|
| grep/curl/Read the main entry `index-*.js` to extract API paths before Phase 1 is done | run `OUTDIR/harvest_static.py` |
| hand-write scripts like `extract_apis.py` to substitute for harvest | edit `OUTDIR/harvest_static.py` and rerun |
| repeat the same grep/command ≥2 times after it failed | change strategy: read tool_logs, edit harvest, check reference |
| skip gates A/B and run the original `scripts/` directly | copy into OUTDIR and adapt to the target |
| real username/password, OTP, OAuth, or other authentication | stub/mock (see above) |
| skipping the stub to "get real data" and doing privilege-escalation/injection testing | only record outbound — it's within the recon boundary |
| irreversible operations such as deleting, exporting sensitive data, bulk writes | the same applies to coverage clicks |
| claiming all pages and endpoints are obtained without completing runtime + dynamic enumeration | see "Definition of done" or annotate the limitation |
| claiming all parameters are known without completing the parameter-trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| inferring required/optional from a single runtime sample | multi-sample diff or back-inference from validation rules/errors |

---

## Two-layer model + run modes

| Layer | Output | Limitations |
|---|---|---|
| **Static** (JS bundle) | full endpoint paths, route draft, field candidates at packing sites | no HTTP method; parameters need Phase 1b; misses URLs assembled at runtime |
| **Runtime** (live session) | method + body + response + dynamic URL + WS/SSE; multi-sample diff to complete parameters | the page must actually render to send a request; a single sample is not enough to decide required/optional |

| Run mode | Engine | Suited for |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, methods/parameters/response bodies, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | clicking tabs/dialogs/tables for deeper feature coverage |
| **both** | depth first, then coverage | most complete; takes the longest |

**Parameter methodology** (there is no generic script): use harvest/regex for paths; for parameters use **anchor window expansion + UI binding chain + multi-sample diff + error back-inference** (grep recipes in section J of [reference.md](reference.en.md)).

---

## Definition of done

Recon may be claimed complete only when all of the following hold:

- [ ] **Static**: Phase 1 harvest produced `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both requires **a functioning Hook and dynamic enumeration**
- [ ] **Shell entered**: a business path does not go to `/login` (watch for hash routing)
- [ ] **Parameters**: coverage/both completed the parameter-trigger matrix + `param_samples.json`; Phase 5 merged into `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission-tree reconstruction and rerun, until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Deliverables**: Phase 5 outputs complete (see the Phase 5 output table); `insert_assets` wrote the service and endpoint assets

---

## Scripts and gates

`scripts/` are reference templates only; **do not** run the originals directly and treat them as the final result.

**Rule**: read first → adapt to the target → write into `OUTDIR` (e.g. `recon/`) → record `CHANGES.md`; if it doesn't fit, rewrite per the methodology, borrowing only the structure.

| Gate | When | Reference script → OUTDIR copy | Common must-change items |
|---|---|---|---|
| **A (static)** | after Phase 0, before the **first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **most sites work with the default regex as-is**; only when the manifest/dialect doesn't match, change the endpoint regex, the webpack/Vite `publicPath`, or the MPA exclude/cookie |
| **B (runtime)** | after Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**Mandatory SPA order** (steps cannot be reordered; Phase numbering takes precedence over "explore first, then script"):

| Step | Must | Prohibited |
|---|---|---|
| after Phase 0 is done | the next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | copy the script → small edits as needed → **run immediately** | manually extract APIs first, then decide whether to harvest |
| before Phase 1 is done | `wc -l` to validate the output; on 404, edit harvest and retry | hand-write an extract script; repeatedly grep a URL that wasn't downloaded |
| from Phase 1b | grep only `OUTDIR/js/*.js` | use the main bundle instead of harvest |

- ✅ copy `harvest_static.py` → (optionally) edit the regex → **run immediately**
- ❌ curl the main bundle → grep many times → write a temp extract → harvest only at the end
- **MPA**: after Phase 0, the next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and output constraints

| Constraint | Note |
|---|---|
| large files | **do not** Read/grep an `index-*.js` larger than 100KB into context; batch-process it with an OUTDIR script |
| grep output | must use `\| head -20` or `-m 5`; keep only a path summary in the conversation, don't paste bundle fragments |
| validation | use `wc -l`, `ls \| wc -l`; don't Read a whole directory |
| regex first-pass | optional, at most once, and only on a small chunk of 50KB or less or on HTML; harvest is the authoritative static source |
| reference | recipes/templates/troubleshooting are in [reference.md](reference.en.md); don't duplicate the full text inline |

---

## Execution roadmap

```
Phase 0 classify + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reversing
  → Phase 2 three auth gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (if needed) → rerun Phase 3
  → Phase 5 merged report + insert_assets to bulk-insert all discovered service and endpoint assets; in no case may a discovered asset be omitted on insert
```

Check off in order; **do not enter the next Phase before the previous item is done**.

1. [ ] **Phase 0**: first-pass SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classify)
2. [ ] **Gate A + Phase 1**: copy script → **immediately** harvest → `wc -l` validate → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reversing)
4. [ ] **Phase 2**: the three auth gates → `config.json` → [Phase 2](#phase-2--three-auth-gates)
5. [ ] **Gate B**: adjust the runtime script → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm the shell is entered; parameter-trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-reconstruction)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — classify

Fetch the entry HTML and **create `OUTDIR`** (don't edit the skill's own `scripts/`):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Outputs `forms.txt`, `links.txt`, `api_inline.txt`. For an SPA, if forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — static

Follow [Scripts and gates](#scripts-and-gates) · [Tool and output constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse the HTML script → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: on a 404, edit harvest and retry; don't manually curl each chunk one by one
- too few in `api_static.txt` → relax the endpoint regex inside OUTDIR and rerun (see reference)

### Phase 1b — parameter reversing

Paths come from Phase 1; parameter fields must be discovered separately. For grep rules see [Tool and output constraints](#tool-and-output-constraints).

**Completion criterion**: for an important endpoint you can answer — field names, transport location, type inference, whether required, sample value, confidence.

#### 1b.0 — transport form

| Form | Where the parameters are | Static priority to look at |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` near the path anchor |
| GraphQL | `variables` | gql templates, `$page: Int` |
| HTML form | urlencoded | `<form>`, `FormData` |
| file upload | multipart | `FormData.append` |
| path parameter | `/user/:id` | route table + `useParams` / `$route.params` |
| encrypted/signed | packed into `sign`/`data` | Hook the encryption function's arguments (reference section D) |

Output: annotate each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — anchor window expansion

Using the known path as an anchor, widen the window to find the packing object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clue |
|---|---|
| axios instance | `data` / `params` |
| unified request | interceptor injects global fields |
| OpenAPI client | generated method signature |
| React Query / SWR | the hook's second argument |
| Vue composable | the composable's arguments |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| form submit | follow submit → transform → API |
| table search | `getFieldsValue()` → `params` |
| route | `:id` / `?tab=` |
| interceptor | global `tenantId`, pagination, sign |
| enum select | `options` → API enum values |

The DevTools call stack traces up from `fetch`/`XHR.send` to the packing function.

#### 1b.3 — the three packing questions (≠ the Phase 2 three auth gates)

| Question | What to answer |
|---|---|
| **Assembly** | where the payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / header |

The interceptor gate (Phase 2) also reads the globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — hand-off to Phase 3

Candidate fields come from the static/binding layer; **required/optional/conditional dependencies** need the Phase 3 parameter matrix + diff + Phase 5 error back-inference.

---

## Phase 2 — three auth gates

grep in `OUTDIR/js/` (with `head`), and write into `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | how does the app determine whether the user is logged in? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | what triggers a redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | where do the menus and permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat a localStorage key name as a credential — confirm it from the chunk/request chain.

**Exit = Gate B**: land the conclusions in `config.json` and edit `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API observation (optional)

Use `preload.js` inside OUTDIR to confirm the session key names, Authorization, and nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | header observation |
| `extractUrlsFromResponse: true` | sub-APIs inside responses |
| `observe.storageReads/cookieReads: true` | back-fill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — runtime

Must have passed Gate B; follow [Boundaries and prohibitions](#boundaries-and-prohibitions-the-agent-must-read--crossing-them-is-out-of-bounds) · the credential-free mock strategy.

In `config.json` set `"runtimeMode": "depth" | "coverage" | "both"` (templates in reference).

### Hook and stub (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | auth/permission/bootstrap stub | pass the first-screen authentication |
| L2 negative fix | all JSON responses | not-logged-in code → success |
| L3 fallback | `/api` etc. not hit by L1 | empty success body, so the UI can render far enough to expose API calls |

- **depth**: fake auth + `forward` to change the business code + `stubs`; traverse `routes` (hash/history); produce `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or a Userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; a business path does not return to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage dynamic enumeration (mandatory)

1. Main nav/sidebar — click each item, wait 1–3s for the network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — first row view/edit/detail
4. Toolbars — export, filter, new (**avoid irreversible delete**)
5. Upon entering each module — merge API/routes
6. SPA — controlled `pushState` to paths not covered by `routes.txt` (prohibited for MPA)

**Parameter-trigger matrix** (mandatory): for each module, record one per operation type, then **diff the multiple samples**:

| Operation | The parameters it usually adds |
|---|---|
| list first screen | pagination + default filters |
| click search | keyword, filter |
| advanced filter | more optional |
| new/edit | full entity |
| batch/export/sort | `ids[]`, `exportType`, `sortField` |

**Under a stub, the outbound body/headers are still real** — go by the request. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — permission-tree reconstruction

**Trigger**: module pages are blank / each route only has bootstrap (e.g. locale) → the content gate was not passed.

| Symptom | Meaning |
|---|---|
| shell entered successfully | render gate + interceptor gate passed |
| missing sidebar items / click shows blank | stub shape or permission codes incomplete |
| every route's API is the same and minimal | `v-if permission` not passed |
| `routes.txt` far fewer than the bundle | must complete from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub checks: the outer `response_code` matches the interceptor gate; the flat codes align with the tree; `routes` cover all links in `route_map`.

After updating `config.json`, **rerun Phase 3**. For a large SPA you can tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — merge and report

### Output table

| File | Stage | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | static bundle and paths |
| `param_candidates.json` | 1b | static parameter-field candidates |
| `config.json` | 2 | the three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | multi-sample, click log, detail |
| `route_map.json` etc. | 4 | permission-tree intermediate files (if executed) |
| `params_merged.json` | 5 | merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | write all service and endpoint assets into the asset repository |

### 5b — parameter merge

Diff from `param_samples.json`; **there is no generic merge script**. For confidence rules see reference J7 (high/medium/low/to-be-triggered).

### 5c — error back-inference

Within the authorized scope, you may send an incomplete request to elicit a 400 response (**this is parameter recon, not vulnerability testing**), such as `field 'x' is required` or an enum error. Watch for the `data` wrapper, `variables`, and the pre-encryption `bizData`.

The report must note: runtimeMode, the static/runtime API counts, parameter confidence, uncovered modules, and a `CHANGES.md` summary relative to the reference scripts.

Recommended `site_map.json` structure:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

For more fields and grep recipes see [reference.md](reference.en.md).

---

## General notes

- **Framework-agnostic**: webpack/Vite/Angular lazy load — the method is the same
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions are not fully enumerable
- **Blind spots**: JSVMP, WASM, strong HMAC/mTLS validation → static + annotate the limitation
- **Parameter blind spots**: conditional dependencies, hidden parameters, WASM packing → "pending trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional resources

- Grep recipes, `config.json` templates, troubleshooting, Hooks, parameter-reversing section J, site_map template: **[reference.md](reference.en.md)**
- For reference-script paths see the table in [Scripts and gates](#scripts-and-gates)
