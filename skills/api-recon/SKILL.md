---
name: api-recon
description: Invoke this skill when collecting a website's API endpoints.
---

# API Recon (Front-End Endpoint Reconnaissance)

Under **authorization**, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **front-end routes**, and **UI feature trigger points** (tabs, dialogs, table actions, etc.).

---

## Boundaries and Prohibitions (Required Reading for Agents — Violating These Crosses the Line)

This skill performs **API / parameter surface reconnaissance only**, not vulnerability discovery or penetration exploitation.

### Task Boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate paths, methods, parameters, routes, UI trigger points | SQLi / XSS / privilege escalation / brute force / fuzzing; tampering attacks; destructive operations |
| **Authentication** | Hook + stub/mock to bypass **client-side** login gates | Asking users for credentials or guessing usernames/passwords; submitting real login forms |
| **Runtime** | Hook endpoints without credentials; use mock responses so the SPA enters its post-login shell | Flows that require a real backend session to continue |

### Credential-Less Dynamic Analysis (Phase 3 Default)

1. **Intercept and stub** login, permission, menu, and other bootstrap endpoints via `preload.js` / `runtime_harvest.js`;
2. Return **structurally correct, business-code-success, data-may-be-empty** mock bodies for business query endpoints;
3. Make the front end render post-login pages even without a backend or in a 401 environment, triggering more XHR/fetch/WebSocket;
4. **Empty data, blank tables, and placeholder UI are all expected** — do not pivot to real logins or vulnerability testing because of them.

**In one sentence**: use mocks to open up front-end routes and component mounting; **record only outbound requests**; what the backend returns does not matter — what matters is **what other endpoints the front end will issue**.

### Hard Process Prohibitions

| Prohibited | Alternative |
|---|---|
| Before Phase 1 completes, grep/curl/Read the main entry `index-*.js` to extract API paths | Run `OUTDIR/harvest_static.py` |
| Write replacement scripts like `extract_apis.py` that bypass harvest | Modify `OUTDIR/harvest_static.py` and rerun |
| Repeat the same failed grep/command ≥2 times | Switch strategy: read tool_logs, modify harvest, consult reference |
| Skip gates A/B and run the original `scripts/` directly | Copy to OUTDIR and modify for the target |
| Real usernames/passwords, OTP, OAuth, etc. for authentication | stub/mock (see above) |
| Skip stubs on the grounds of "getting real data" to perform privilege escalation / injection testing | Record only outbound — this is the recon boundary |
| Destructive operations: delete, export sensitive data, batch writes, etc. | Same applies to coverage clicks |
| Claiming all pages and endpoints are known without completing runtime + dynamic enumeration | See "Definition of Done" or document limitations |
| Claiming all parameters are known without completing the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff or validation rule / error-based inference |

---

## Two-Layer Model + Runtime Modes

| Layer | Output | Upper bound |
|---|---|---|
| **Static** (JS bundles) | Full endpoint paths, route draft, payload-building field candidates | No HTTP methods; parameters require Phase 1b; misses runtime-concatenated URLs |
| **Runtime** (live session) | Method + body + response + dynamic URLs + WS/SSE; multi-sample diff completes parameters | Pages must actually render to fire requests; a single sample is insufficient to determine required/optional |

| Runtime mode | Engine | Use case |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/responses, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Click tabs / triggers / tables for deeper feature coverage |
| **both** | depth first, then coverage | Most complete; longest duration |

**Parameter methodology** (no universal script): paths use harvest/regex; parameters use **anchor expansion + UI binding chain + multi-sample diff + error-based inference** (grep recipes in [reference.md](reference.md) section J).

---

## Definition of Done

All must be satisfied before declaring recon complete:

- [ ] **Static**: Phase 1 harvest produces `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both requires **hooks taking effect + dynamic enumeration loop**
- [ ] **In shell**: visiting a business path does not land on `/login` (note hash routing)
- [ ] **Parameters**: coverage/both completes the parameter trigger matrix + `param_samples.json`; Phase 5 merges into `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission tree recovery and rerun, until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs are complete (see Phase 5 output table); `insert_assets` writes services and endpoint assets

---

## Scripts and Gates

`scripts/` are reference templates only; **running the originals directly and treating them as final results is prohibited**.

**Rule**: read first → modify for target → write to `OUTDIR` (e.g., `recon/`) → record `CHANGES.md`; if they do not match, rewrite per methodology, borrow only the structure.

| Gate | When | Reference script → OUTDIR copy | Common required modifications |
|---|---|---|---|
| **A (Static)** | After Phase 0, **before the first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **Default regex works for most sites**; only modify endpoint regex, webpack/Vite `publicPath`, MPA exclude/cookie when manifest/dialect does not match |
| **B (Runtime)** | After Phase 2, before depth/coverage runs | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**Mandatory SPA order** (not interchangeable; phase number takes priority over "explore first, then script"):

| Step | Required | Prohibited |
|---|---|---|
| After Phase 0 completes | Next Bash call = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read main entry `index-*.js` (usually >500KB) |
| Gate A | Copy script → minor edits as needed → **run immediately** | Manually extract APIs before deciding whether to harvest |
| Before Phase 1 completes | `wc -l` to validate output; on 404, modify harvest and retry | Hand-write extract scripts; repeatedly grep not-yet-downloaded URLs |
| From Phase 1b onward | Grep only `OUTDIR/js/*.js` | Use main bundle as a substitute for harvest |

- ✅ Copy `harvest_static.py` → (optional) modify regex → **run immediately**
- ❌ curl main bundle → grep multiple times → write a temporary extract → only then harvest
- **MPA**: After Phase 0, the next Bash call = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and Output Constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` files >100KB **prohibited** from Read/grep into context; use OUTDIR scripts for batch processing |
| grep output | Must use `\| head -20` or `-m 5`; keep only path summaries in conversation, never paste bundle fragments |
| Validation | Use `wc -l`, `ls \| wc -l`; do not Read entire directories |
| Probe regex | Disallowed at this phase. The mandatory first move is harvest; do not manually extract APIs before it |
| reference | Recipes/templates/troubleshooting in [reference.md](reference.md); do not repeat inline full text |

---

## Execution Roadmap

```
Phase 0 Classification + OUTDIR
  → Gate A → Phase 1 harvest (★ RUN IMMEDIATELY ★)
  → Phase 1b Parameter reverse engineering
  → Phase 2 Three auth gates → config.json
  → Gate B → Phase 3 Runtime + parameter matrix
  → Phase 4 Permission tree (if needed) → rerun Phase 3
  → Phase 5 Merge report + insert_assets (batch-insert every discovered service and endpoint asset; never omit known assets)
```

Check off in sequence; **the previous item must complete before entering the next phase**.

1. [ ] **Phase 0**: probe SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy script → **immediately** harvest → `wc -l` validate → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: three auth gates → `config.json` → [Phase 2](#phase-2--three-auth-gates)
5. [ ] **Gate B**: adjust runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm in-shell; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-recovery)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch entry HTML, **create `OUTDIR`** (do not modify `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Outputs `forms.txt`, `links.txt`, `api_inline.txt`. If SPA has forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts and Gates](#scripts-and-gates) · [Tool and Output Constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML scripts → webpack/Vite manifest → download all lazy chunks → outputs `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- Chunk count vs manifest: 404 requires modifying harvest and retrying, do not manually curl chunks one by one
- If `api_static.txt` is too sparse → relax the endpoint regex inside OUTDIR and rerun (see reference)

### Phase 1b — Parameter Reverse Engineering

Paths come from Phase 1; parameter fields must be reversed separately. Grep rules in [Tool and Output Constraints](#tool-and-output-constraints).

**Completion standard**: important endpoints should be answerable — field names, transport location, type inference, required-ness, sample values, confidence.

#### 1b.0 — Transport Mode

| Mode | Where parameters live | Static priority look |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` near path anchor |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameters | `/user/:id` | route table + `useParams` / `$route.params` |
| Encryption/signing | wrapped in `sign`/`data` | Hook encrypt function arguments (reference section D) |

Output: each endpoint annotated with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor Expansion

Using known paths as anchors, widen the window to find the payload-building object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clue |
|---|---|
| axios instance | `data` / `params` |
| Unified request | interceptor injects global fields |
| OpenAPI client | generated method signatures |
| React Query / SWR | second arg of hook |
| Vue composable | composable argument |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding Layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | Trace submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

In DevTools call stack, trace upward from `fetch`/`XHR.send` to the payload-building function.

#### 1b.3 — Three Payload-Building Questions (≠ Phase 2 auth gates)

| Question | Answer |
|---|---|
| **Assembly** | Where payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

The interceptor gate (Phase 2) also reads globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Phase 3 Hand-off

Candidate fields originate from static/binding layers; **required/optional/conditional** must be determined by Phase 3 parameter matrix + diff + Phase 5 error-based inference.

---

## Phase 2 — Three Auth Gates

Grep in `OUTDIR/js/` (with `head`), write to `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How does the app decide "logged in"? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat localStorage keys as credentials — must verify via chunk/request chain.

**Exit = Gate B**: conclusions land in `config.json`, and modify `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API Observation (Optional)

Use `preload.js` inside OUTDIR to confirm session key names, Authorization, nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observation |
| `extractUrlsFromResponse: true` | child API URLs in responses |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Coverage per-round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Must have passed Gate B; follow [Boundaries and Prohibitions](#boundaries-and-prohibitions-required-reading-for-agents--violating-these-crosses-the-line) · credential-less mock strategy.

`config.json` sets `"runtimeMode": "depth" | "coverage" | "both"` (template in reference).

### Hooks and Stubs (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 precise | auth/permission/bootstrap stubs | Pass first-screen authentication |
| L2 negative patch | all JSON responses | Convert "not logged in" code to success |
| L3 fallback | `/api` etc. not hit by L1 | Empty success body, opens UI |

- **depth**: fake auth + `forward` patch business code + `stubs`; iterate `routes` (hash/history); produces `runtime_api.json`
- **coverage**: **document-start** inject `preload.js` (CDP `addScriptToEvaluateOnNewDocument` or userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business path does not redirect to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — Coverage Dynamic Enumeration (Required)

1. Main nav / sidebar — click each item, wait 1–3s for network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — view/edit/details on the first row
4. Toolbar — export, filter, new (**avoid irreversible deletion**)
5. Per module entry — merge APIs/routes
6. SPA — for paths not covered by `routes.txt`, controlled `pushState` (MPA prohibited)

**Parameter trigger matrix** (required): per module, record one sample per operation type, **diff multiple samples**:

| Operation | Typically extra parameters |
|---|---|
| List first screen | pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | more optional |
| New/Edit | full entity |
| Batch/Export/Sort | `ids[]`, `exportType`, `sortField` |

**Outbound body/headers under stubs are still real** — trust the request. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar click + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — Permission Tree Recovery

**Trigger**: module page blank / only bootstrap per route (e.g., locale) → content gate not passed.

| Symptom | Meaning |
|---|---|
| In-shell success | Render gate + interceptor gate passed |
| Sidebar missing items / blank click | Stub shape or permission code incomplete |
| Same few APIs per route | `v-if permission` not passing |
| `routes.txt` far less than bundle | Must complete from auth module |

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

Stub checks: outer `response_code` matches interceptor gate; flat codes align with tree; `routes` covers all links in `route_map`.

After updating `config.json`, **rerun Phase 3**. For large SPAs, tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — Merge and Report

### Output Table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundles and paths |
| `param_candidates.json` | 1b | Static parameter field candidates |
| `config.json` | 2 | Three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multi-samples, click logs, detail |
| `route_map.json` etc. | 4 | Permission tree intermediate files (if executed) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, features, limitations |
| **insert_assets** | 5 | Write all services and endpoint assets to the asset library |

### 5b — Parameter Merge

Diff from `param_samples.json`, **no universal merge script**. Confidence rules in reference J7 (high/medium/low/pending-trigger).

### 5c — Error-Based Inference

Within the authorization scope, send incomplete requests to read 400 (**parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Note `data` wrapping, `variables`, pre-encryption `bizData`.

Report must note: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, `CHANGES.md` summary relative to reference scripts.

`site_map.json` suggested structure:

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

More fields and grep recipes in [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: webpack/Vite/Angular lazy-load methods are the same
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions cannot be fully enumerated
- **Blind spots**: JSVMP, WASM, HMAC/mTLS strong validation → static + document limitations
- **Parameter blind spots**: conditional linkage, hidden params, WASM payload → "pending-trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` templates, troubleshooting, hooks, parameter reverse engineering section J, site_map template: **[reference.md](reference.md)**
- Reference script paths in [Scripts and Gates](#scripts-and-gates) table
