# api-recon — Reference Manual

Grep recipes, `config.json` templates, and troubleshooting. All greps target the `js/` directory. For minified single-line bundles, run `js-beautify` first or `sed 's/}/}\n/g'`; usually raw grep with a context window is sufficient.

## Script Notes

All files in `scripts/` are **reference templates**; they must be adjusted for the target site before execution. Typical modification points:

| Script | Commonly adjusted items |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-app publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub match rules and body structure, routes sources, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookie, depth/max, same-domain filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming patterns |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | Unified entry for all site-specific parameters above |

Adjusted files should go in the task working directory (e.g., `recon/`); the report should note specific changes relative to the reference scripts.

---

## A. Three Reverse-Engineering Gates

### A1. Render Gate — "How does the app decide 'logged in'?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` to determine the **storage key**, **container** (Cookie vs localStorage), **encoding**:

| Encoding | config forgery method |
|---|---|
| Plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | Unsigned / `alg:none` JWT, or signed with in-bundle key |
| Encryption (SM2/AES/RSA) | Find hardcoded key; render gate can forge if blob is decodable; otherwise fall back to static |

→ Write to `cookies` / `localStorage`.

### A2. Interceptor Gate — "What triggers redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: **field name**, **success value** (usually `0` or `200`), **failure value that triggers redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Write to `neutralize.fields` + `neutralize.success`.

### A3. Content Gate — "Where do menus/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise back offices):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | sidebar menu rendering |
| In-bundle `userRouteAuth` | `{ CODE: { url, name? } }` | code → front-end path |
| In-bundle `routeMap` | `{ KEY: { name, link } }` | alias resolution (webpack `o.DASHBOARD`) |

Read consumer code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): construct permissive payload → `stubs`.

**Full permission tree recovery** (large sites, sidebar/submodules still blank): see **section I**.

---

## B. config.json Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field notes:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), `both`
- `cookies[].value` prefixes: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal
- `forward: true` forwards real requests and rewrites the code field; `false` fully offline stubs
- `mockTier`: preload enable tiers in coverage mode, e.g., `L1+L2`, `L1+L2+L3`
- `routes` come from `routes.txt`; after forging menus, it auto-adds `<a href>`
- `captureResponses` / `recordWs` are valid only in depth mode
- `waitUntil`: for large SPAs use `domcontentloaded`, avoid `networkidle2` hang
- `routeTimeout`: per-route `page.goto` timeout (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; also set `HTTP_PROXY` / `HTTPS_PROXY`

### B1. Dual Stub Template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

Outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must cover all leaf codes in the tree.

---

## C. Coverage Mode: preload Configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace before CDP injection:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify: `window.__API_RECON_PRELOAD__ === true` and pathname is stable.

Export recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook Capabilities

Browser hook capabilities built into preload (coverage) and runtime_harvest (depth), and their coverage:

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | Record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Discover Authorization and other headers | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | Confirm session key names | ⚠️ optional `observe.storageReads/cookieReads` |
| Vue fetch routes | Complete frontendRoutes | ✅ `__API_RECON_ROUTES__` (loaded routes) |
| Neutralize Vue route guards / 登录 redirect | Open modules to trigger APIs | ✅ `neutralizeVueRouter` + native redirect neutralization |
| React fetch routes | Complete routes | ⚠️ static + click; no dedicated hook |
| Block page redirect (login path) | Stay on page for analysis | ⚠️ only block login path, avoid blocking business nav |
| Hook crypto libraries (CryptoJS/SM etc.) | Encrypted params → plaintext API body | ❌ must manually hook encrypt function args; write conclusions to config |
| Anti-debug bypass | Otherwise runtime records no APIs | ❌ must handle manually; static still works |

---

## E. Endpoint Extraction Regex (When Static Is Too Sparse)

Relax `extract_endpoints` in `harvest_static.py`, or run manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → Action |
|---|---|
| Very few static APIs | Endpoint dialect mismatch → relax regex (section D) |
| Chunk count ≪ manifest | CSS-only chunks or undeployed chunks; 404 already retried |
| Runtime still shows login page | Render gate error → recheck A1: key, container, encoding, domain |
| In shell but modules blank | Content gate → forge menus (A3); `routes` paths may be wrong |
| Only bootstrap/locale per route | Permission codes incomplete → section I permission tree recovery; check `role_permissions` + `permissions/all` dual stubs |
| Sidebar has items but subpages blank | Tree missing intermediate nodes or codes mismatch with `userRouteAuth` |
| Every API redirects to login | Interceptor gate → confirm `neutralize`; nested fields need extended walk logic |
| WS frames = 0 | Subscription requires user interaction; increase `perRouteMs` |
| Empty response body | Only available when `forward: true` |
| Chromium missing | Install chromium or set `config.chromium` / `CHROMIUM` |
| Many mocks yet redirects to login | Hook too late or missing `location.href` setter → document-start + preload |
| Lists all empty | L3 empty array is normal; continue clicking tabs/settings/details |
| Mistaking Redux actions for routes | Filter internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload not at document-start → adjust injection timing; or `neutralizeVueRouter: false` and manually clear guards |
| URLs in responses not logged | Enable `extractUrlsFromResponse`; or manually extract from `__API_RECON_DETAIL__` |
| Unknown Authorization header name | Enable `observe.xhrHeaders` or check request headers in DevTools |
| Runtime very slow / timeout | Change `waitUntil: domcontentloaded`; lower `routeTimeout`; do not use `networkidle2` |
| Proxy connection failure | Check `proxy` / env vars; Puppeteer and curl proxy port consistent |

---

## G. Hardened Targets

When the server-side progressively validates sessions (signature cookies that cannot be forged, server-rendered and un-stubbable menus), runtime will stall at the shell. Expected behavior:

- **Static is sufficient for endpoint enumeration** — module paths are in code
- If authorization allows, run the same harness with a **real session**: `forward: true`, no neutralize needed, capture real methods/params/responses

---

## H. Single-Task Checklist

1. Confirm authorization scope
2. **Read** `scripts/harvest_static.py` → adjust for target → run → review `api_static.txt`, `routes.txt`
3. **Phase 1b**: path anchor expansion + binding layer → `param_candidates.json` (section J)
4. Reverse-engineer A1/A2/A3 → write site-specific `config.json`
5. **Read and adjust** `runtime_harvest.js` / `preload.js` before running
6. `runtimeMode=depth`: `npm install` → run adjusted harvest script
7. `runtimeMode=coverage/both`: document-start inject adjusted preload → browser MCP dynamic enumeration + **parameter trigger matrix**
8. Module not rendering → **section I permission tree recovery** → patch stubs → rerun
9. Parameter multi-sample diff + error inference → config_merged.json
10. Merge → `site_map.json` + `api_merged.txt`, honestly annotate coverage, gaps, and script modification points

---

## I. Permission Tree Recovery (Phase 4 Deepening)

Use when forging simple `menus: [{ path, show: true }]` does not work and submodules still do not mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: **permission API path**, **response field names**, **consuming chunk filename**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If `[!] no routeMap pattern found`: relax the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases `He=o.DASHBOARD`)
2. Use `route_map.json` to resolve aliases → real paths
3. Infer parents by code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, auto-write `stubs` and extended `routes` into `config.json`

**Adjust for target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes without prefix relations

### I4. Validate stub consistency

```bash
# permissions count should ≈ userRouteAuth entry count
wc -l recon/perm_codes_all.txt
# routes should cover all route_map links
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare runtime_api.json entry counts before/after forging; check whether /attack, /asset etc. trigger module APIs
```

| Before forge | After forge (success) |
|---|---|
| Each route same 3–5 bootstrap entries | Different routes trigger different module APIs |
| Only `/api/locale/language` | `/api/web/...` module endpoints appear |
| `routes.txt` single-digit routes | `routes` 80–110+ from route_map |

### I6. If Still Failing

- **coverage mode**: click sidebar + tabs; permission gating may only request after interaction
- **stub fields**: compare nesting of real API (curl + real session) vs stub
- **extra guards**: grep `hasPermission|checkRole|func.` and other button-level checks; extend `role_permissions.permissions`
- **static fallback**: module API paths are still in `api_static.txt`; runtime only supplements METHOD/body; parameters preserved in `param_candidates.json` + recorded samples

---

## J. Parameter Reverse Engineering (Phase 1b / 5b / 5c)

**Methodology, not a universal script.** Find paths with regex; find parameters with anchor expansion + UI binding chain + multi-sample diff + error-based inference.

### J1. Anchor Expansion — From Path to Payload Object

```bash
# Use Phase 1 known paths as anchors
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper Layer and Transport Mode

```bash
# axios / unified request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# Path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation Gate — Required / Format / Enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding Layer — Form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime supplement: DevTools → Network → request → **Initiator** (call stack), trace upward from `fetch`/`send` to the payload-building function.

### J5. Encrypted Parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Do not guess fields on ciphertext** — Hook the encrypt function **arguments**, record plaintext payload before encryption; conclusions go in `config.json` / `param_candidates.json`.

### J6. Parameter Trigger Matrix (Required in Phase 3)

For each module, record one sample per operation, diff request body/query:

| Operation | Focus |
|---|---|
| List first screen | default pagination values |
| Search | keyword, filters |
| Advanced filter | optional fields |
| New/Edit | full entity |
| Batch/Export | `ids[]`, `exportType` |
| Sort/Paginate | `sortField`, `order` |

Output `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence Rules

| Confidence | Condition |
|---|---|
| **High** | Static callsite + runtime ≥2 samples agree |
| **Medium** | Static only, or runtime only 1 sample |
| **Low** | Response/error inference, not re-verified |
| **Pending trigger** | Field known statically, UI/permission not yet triggered |

### J8. Scenario Quick Config

| Scenario | Order |
|---|---|
| REST list page | J1 payload object → J6 four-time diff → J3 rules |
| New/Edit form | J3 Form name → J4 submit chain → runtime submit + intentionally leave blank to see 400 |
| GraphQL | J2 variables declaration → runtime per-operation record variables |

### J10. Troubleshooting

| Symptom | Action |
|---|---|
| Field name in static but never in runtime | Mark "pending trigger"; supplement permission tree / click advanced filter / trigger each option of dependent selects |
| Same path, different body shapes | Normal — record by `action`, do not forcibly merge schema |
| Stub response fake but want to see params | **Look at outbound request** body/headers, do not infer from stub response |
| 400 reports nested field | Note outer wrapper `data`/`bizData`/`variables` |
| GraphQL only shows operation name | Expand `variables` JSON; statically find `$var: Type` |

---
