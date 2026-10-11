---
name: api-recon
description: Invoke this skill when collecting a website's API endpoints.
---

# API Recon (frontend endpoint reconnaissance)

Under an **authorized** premise, discover as completely as possible: the **backend APIs** (paths, methods, parameters, response bodies), the **frontend routes**, and the **UI feature triggers** (tabs, modals, table actions, etc.).

---

## Boundaries and prohibitions (Agent must read)

Violating these is going out of scope. This skill **only does API / parameter-surface reconnaissance**; it is not a vulnerability-hunting or exploitation phase.

### Task boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | enumerate paths, methods, parameters, routes, UI triggers | SQLi/XSS/privilege-escalation/brute-force/fuzzing vulns, packet-tampering attacks, destructive operations |
| **Auth** | hook + stub/mock to bypass the **client-side** login gate | asking the user for or guessing credentials; attempting a real login-form submission |
| **Runtime** | without credentials, hook endpoints and use mock responses to get the SPA into its post-login shell | flows that require a real backend session to proceed |

### Credential-free dynamic analysis (Phase 3 default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap endpoints such as login, permissions, and menus;
2. For business query endpoints, return a mock body that is **structurally correct, with a success business code, and may have empty data**;
3. Let the frontend still render its post-login pages in an environment with no backend or with 401, thereby triggering more XHR/fetch/WebSocket;
4. **Empty data, blank tables, and placeholder UI are all expected** — do not switch to real login or vulnerability testing because of this.

**In one sentence**: use mocks to prop open the frontend routes and component mounting, and **only record outbound requests**; what the backend returns does not matter — what matters is **which other endpoints the frontend will still fire**.

### Hard process prohibitions

| Prohibited | Alternative |
|---|---|
| grep/curl/Read the main entry `index-*.js` to extract API paths before Phase 1 is complete | run `OUTDIR/harvest_static.py` |
| hand-writing scripts like `extract_apis.py` that replace harvest | edit `OUTDIR/harvest_static.py` and rerun |
| repeating the same grep/command after it has failed ≥2 times | change strategy: read tool_logs, edit harvest, check the reference |
| skipping gates A/B and running the original `scripts/` directly | copy to OUTDIR and adapt to the target |
| real usernames/passwords, OTP, OAuth, etc. for auth | stub/mock (see above) |
| skipping the stub on the grounds of "getting real data" to do privilege-escalation/injection testing | only record outbound; that is the recon boundary |
| irreversible operations such as deleting, exporting sensitive data, or bulk writes | the same applies to coverage clicks |
| claiming all pages and endpoints are obtained without completing runtime + dynamic enumeration | see "Definition of done" or note the limitation |
| claiming all parameters are known without completing the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| inferring required/optional from a single runtime sample | multi-sample diff, or infer from validation rules/errors |

---

## Two-layer model + run modes

| Layer | Output | Ceiling |
|---|---|---|
| **Static** (JS bundle) | full endpoint paths, draft routes, request-assembly field candidates | no HTTP method; parameters need Phase 1b; misses runtime-concatenated URLs |
| **Runtime** (live session) | method + body + response + dynamic URL + WS/SSE; multi-sample diff fills in parameters | the page must actually render to fire a request; a single sample is insufficient to decide required/optional |

| Run mode | Engine | Suited for |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response body, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | click Tabs/modals/tables, deeper feature-point coverage |
| **both** | depth first, then coverage | most complete, longest running |

**Parameter methodology** (no generic script): paths via harvest/regex; parameters via **anchor window expansion + UI binding chain + multi-sample diff + error inference** (grep recipes in section J of [reference.md](reference.md)).

---

## Definition of done

Recon may only be claimed complete when all of the following are satisfied:

- [ ] **Static**: Phase 1 harvest produced `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both require **the Hook taking effect + the dynamic enumeration loop**
- [ ] **Into the shell**: visiting a business path is not `/login` (watch out for hash routes)
- [ ] **Parameters**: coverage/both complete the parameter trigger matrix + `param_samples.json`; Phase 5 merges `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission-tree reconstruction and a rerun, until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Deliverables**: Phase 5 outputs are complete (see the Phase 5 output table); `insert_assets` writes the service and endpoint assets

---

## Scripts and gates

`scripts/` are reference templates only; **do not** run the originals directly and treat them as the final result.

**Rule**: read first → adapt to the target → write into `OUTDIR` (e.g. `recon/`) → record `CHANGES.md`; if it doesn't fit, rewrite per the methodology and only borrow the structure.

| Gate | When | Reference script → OUTDIR copy | Common things to change |
|---|---|---|---|
| **A (static)** | after Phase 0, before running harvest/spider for the **first time** | `harvest_static.py` / `spider_mpa.py` | **the default regex runs directly on most sites**; only when the manifest/dialect doesn't match, change the endpoint regex, the webpack/Vite `publicPath`, the MPA exclude/cookie |
| **B (runtime)** | after Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**SPA mandatory ordering** (not interchangeable; the Phase numbers take priority over "explore first, then script"):

| Step | Must | Prohibited |
|---|---|---|
| after Phase 0 completes | next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | copy the script → make small edits as needed → **run immediately** | manually extracting the API first, then deciding whether to harvest |
| before Phase 1 completes | verify output with `wc -l`; on 404 fix harvest and retry | hand-written extract script; repeatedly greping an undownloaded URL |
| from Phase 1b on | grep only `OUTDIR/js/*.js` | using the main bundle in place of harvest |

- ✅ copy `harvest_static.py` → (optionally) change the regex → **run immediately**
- ❌ curl the main bundle → grep repeatedly → write a temp extract → harvest only at the end
- **MPA**: after Phase 0, next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and output constraints

| Constraint | Note |
|---|---|
| large files | `index-*.js` >100KB **must not** be Read/greped into context; batch-process with an OUTDIR script |
| grep output | must be `\| head -20` or `-m 5`; keep only path summaries in the conversation, do not paste bundle fragments |
| verification | use `wc -l`, `ls \| wc -l`; do not Read whole directories |
| regex first look | optional, ≤1 time, only on a ≤50KB small chunk or HTML; the formal static pass is governed by harvest |
| reference | recipes/templates/troubleshooting are in [reference.md](reference.md); do not re-inline the full text |

---

## Execution roadmap

```
Phase 0 classify + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reverse-engineering
  → Phase 2 the three auth gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (when needed) → rerun Phase 3
  → Phase 5 merged report + insert_assets to bulk-insert every discovered service and endpoint API asset; under no circumstances may any discovered asset be omitted at insert time
```

Check off in order; **you may not enter the next Phase until the previous item is complete**.

1. [ ] **Phase 0**: first look at SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy the script → **immediately** harvest → `wc -l` verify → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: the three auth gates → `config.json` → [Phase 2](#phase-2--the-three-auth-gates)
5. [ ] **Gate B**: adjust the runtime script → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm into-the-shell; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-reconstruction)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch the entry HTML and **create `OUTDIR`** (do not modify the skill's own `scripts/`):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produces `forms.txt`, `links.txt`, `api_inline.txt`. If an SPA has forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts and gates](#scripts-and-gates) · [Tool and output constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML scripts → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: on 404, fix harvest and retry, do not manually curl chunks one by one
- too few entries in `api_static.txt` → loosen the endpoint regex inside OUTDIR and rerun (see reference)

### Phase 1b — Parameter reverse-engineering

Paths come from Phase 1; parameter fields must be reconned separately. For grep rules see [Tool and output constraints](#tool-and-output-constraints).

**Completion criterion**: for important endpoints you can answer — field name, transport location, type inference, whether required, sample value, confidence.

#### 1b.0 — Transport form

| Form | Where the parameters are | What static looks at first |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` next to the path anchor |
| GraphQL | `variables` | gql templates, `$page: Int` |
| traditional form | urlencoded | `<form>`, `FormData` |
| file upload | multipart | `FormData.append` |
| path parameter | `/user/:id` | route table + `useParams` / `$route.params` |
| encrypted/signed | wrapped into `sign`/`data` | hook the encryption function's arguments (reference section D) |

Output: annotate each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor window expansion

Using a known path as an anchor, expand the window to find the request-assembly object:

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
| React Query / SWR | hook's second argument |
| Vue composable | composable arguments |

Type residue: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding layer

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

From the DevTools call stack, trace upward from `fetch`/`XHR.send` to the assembly function.

#### 1b.3 — The three assembly questions (≠ the three auth gates of Phase 2)

| Question | What to answer |
|---|---|
| **Assembly** | where the payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / header |

The interceptor gate (Phase 2) also reads global injected fields (Authorization, `X-Tenant-Id`, sign) along the way.

#### 1b.4 — Handoff to Phase 3

Candidate fields come from the static/binding layer; **required/optional/conditional dependencies** require the Phase 3 parameter matrix + diff + Phase 5 error inference.

---

## Phase 2 — The three auth gates

grep in `OUTDIR/js/` (with `head`), and write into `config.json` (recipes in the reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | how is login determined? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | what triggers a jump to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat a localStorage key name as a credential — confirm it from the chunk/request chain.

**Exit = Gate B**: land the conclusions in `config.json`, and edit `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API observation (optional)

Use `preload.js` inside OUTDIR to confirm the session key names, Authorization, and nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observation |
| `extractUrlsFromResponse: true` | sub-APIs inside responses |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage exports each round: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Must have passed Gate B; follow [Boundaries and prohibitions](#boundaries-and-prohibitions-agent-must-read) · the credential-free mock strategy.

In `config.json` set `"runtimeMode": "depth" | "coverage" | "both"` (templates in the reference).

### Hook and stub (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | stub auth/permissions/bootstrap | pass first-screen auth |
| L2 negative correction | all JSON responses | not-logged-in code → success |
| L3 fallback | `/api` etc. not matched by L1 | empty success body, props open the UI |

- **depth**: fake auth + `forward` to change the business code + `stubs`; traverse `routes` (hash/history); produce `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or a Userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; a business path does not go back to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage dynamic enumeration (required)

1. Main nav/sidebar — click each item, wait 1–3s for network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — view/edit/detail on the first row
4. Toolbar — export, filter, create (**avoid irreversible deletes**)
5. On entering each module — merge APIs/routes
6. SPA — for paths in `routes.txt` not covered, do a controlled `pushState` (prohibited for MPA)

**Parameter trigger matrix** (required): record once per operation type in each module, and **diff multiple samples**:

| Operation | Parameters usually added |
|---|---|
| list first screen | pagination + default filters |
| click search | keyword, filter |
| advanced filter | more optional ones |
| create/edit | the complete entity |
| bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Under the stub, the outbound body/headers are still real** — go by the request. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — Permission-tree reconstruction

**Trigger**: module pages blank / each route only bootstrap (e.g. locale) → the content gate was not passed.

| Symptom | Meaning |
|---|---|
| into the shell succeeds | the render gate + interceptor gate are passed |
| sidebar items missing / clicking blank | the stub shape or permission codes are incomplete |
| every route's APIs are the same and very few | `v-if permission` did not pass |
| `routes.txt` far fewer than the bundle | must be completed from the auth module |

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

stub check: the outer `response_code` matches the interceptor gate; the flat codes align with the tree; `routes` covers all links in `route_map`.

After updating `config.json`, **rerun Phase 3**. For a large SPA you can tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — Merge and report

### Output table

| File | Stage | Contents |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | static bundle and paths |
| `param_candidates.json` | 1b | static parameter-field candidates |
| `config.json` | 2 | the three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | multiple samples, click log, detail |
| `route_map.json` etc. | 4 | permission-tree intermediate files (if run) |
| `params_merged.json` | 5 | merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | write all service and endpoint assets into the asset store |

### 5b — Parameter merge

Diff from `param_samples.json`; **there is no generic merge script**. For confidence rules see reference J7 (high/medium/low/pending-trigger).

### 5c — Error inference

Within the authorized scope, send incomplete requests to read 400 (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Watch for the `data` wrapper, `variables`, and the pre-encryption `bizData`.

The report must note: runtimeMode, the static/runtime API count, parameter confidence, uncovered modules, and a `CHANGES.md` summary relative to the reference scripts.

Suggested structure for `site_map.json`:

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

For more fields and grep recipes see [reference.md](reference.md).

---

## General notes

- **Framework-agnostic**: webpack/Vite/Angular lazy loading use the same method
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions are not fully enumerable
- **Blind spots**: JSVMP, WASM, strong HMAC/mTLS validation → static + note the limitation
- **Parameter blind spots**: conditional linkage, hidden params, WASM assembly → "pending-trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional resources

- Grep recipes, `config.json` templates, troubleshooting, Hook, parameter reverse-engineering section J, site_map template: **[reference.md](reference.md)**
- For reference-script paths see the [Scripts and gates](#scripts-and-gates) table
