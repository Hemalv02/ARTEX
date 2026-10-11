# api-recon — reference manual

Grep recipes, `config.json` templates, and troubleshooting. All greps run against the `js/` directory. When the bundle is on a single line, you can first run `js-beautify` or `sed 's/}/}\n/g'`, though a raw grep with a context window usually suffices.

## About the scripts

Every file under `scripts/` is a **reference template** and must be adapted to the target site before running. Typical things to change:

| Script | Common things to change |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub matching rules and body structure, route source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether L3 is enabled, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookie, depth/max, same-origin filter |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field name |
| `config.json` | the single entry point for all the site-specific parameters above |

Adjusted files should be placed in the task working directory (e.g. `recon/`), and the report should note the specific changes relative to the reference scripts.

---

## A. Reverse-engineering the three gates

### A1. Render gate — "how is login determined?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Find the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))`, and determine the **storage key**, the **container** (Cookie vs localStorage), and the **encoding**:

| Encoding | How to forge in config |
|---|---|
| plaintext string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | an unsigned/`alg:none` JWT, or sign with a key found in the bundle |
| encrypted (SM2/AES/RSA) | find the hardcoded key; the render gate only needs a decodable blob to forge; otherwise fall back to static |

→ write into `cookies` / `localStorage`.

### A2. Interceptor gate — "what triggers a jump to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: the **field name**, the **success value** (usually `0` or `200`), and the **failure value that triggers the jump**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ write into `neutralize.fields` + `neutralize.success`.

### A3. Content gate — "where do menus/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two-layer data** (common in enterprise admin panels):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | sidebar menu rendering |
| `userRouteAuth` in the bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in the bundle | `{ KEY: { name, link } }` | alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload → `stubs`.

**Full permission-tree reconstruction** (large sites, where the sidebar/submodules are still blank): see **section I**.

---

## B. config.json template

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
- `forward: true` forwards the real request and rewrites the code field; `false` is a fully offline stub
- `mockTier`: the preload tier enabled in coverage mode, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging menus the harness auto-appends `<a href>`
- `captureResponses` / `recordWs` only take effect in depth mode
- `waitUntil`: use `domcontentloaded` for a large SPA to avoid `networkidle2` hanging
- `routeTimeout`: the per-route `page.goto` timeout (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; `HTTP_PROXY` / `HTTPS_PROXY` can also be set

### B1. Dual-stub template (role_permissions + permissions/all)

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

The outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must cover all leaf codes in the tree.

---

## C. coverage mode: preload configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before CDP injection:

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

Verify: `window.__API_RECON_PRELOAD__ === true` and the pathname is stable.

Export the recorded result:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook capabilities

The built-in browser Hook capabilities and their coverage in preload (coverage) and runtime_harvest (depth):

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | discover headers such as Authorization | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | confirm session key names | ⚠️ optional `observe.storageReads/cookieReads` |
| Vue route retrieval | fill in frontendRoutes | ✅ `__API_RECON_ROUTES__` (loaded routes) |
| Vue route-guard neutralization / login-jump blocking | prop open modules to trigger APIs | ✅ `neutralizeVueRouter` + native-jump neutralization |
| React route retrieval | fill in routes | ⚠️ static + clicks; no dedicated Hook |
| Page-jump blocking (login path) | stay on the page to analyze | ⚠️ only blocks the login path, to avoid blocking business navigation |
| Hook the crypto library (CryptoJS/SM, etc.) | encrypted params → plaintext API body | ❌ must manually Hook the encryption function's arguments; write the conclusion to config |
| Anti-debugging bypass | otherwise runtime cannot record APIs | ❌ must handle manually; static still works |

---

## E. Endpoint extraction regex (when static is too sparse)

Loosen `extract_endpoints` in `harvest_static.py`, or manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → handling |
|---|---|
| very few static APIs | endpoint dialect doesn't match → loosen the regex (section D) |
| chunk count ≪ manifest | CSS-only or undeployed chunks; 404 already retried |
| runtime still shows the login page | render gate wrong → recheck A1: key name, container, encoding, domain |
| into the shell but modules blank | content gate → forge menus (A3); the `routes` paths may be wrong |
| each route only bootstrap/locale | permission codes incomplete → section I permission-tree reconstruction; check the `role_permissions` + `permissions/all` dual stub |
| sidebar has items but subpages blank | the tree is missing an intermediate node, or the code doesn't match `userRouteAuth` |
| every API jumps to login | interceptor gate → confirm `neutralize`; nested fields need the walk logic extended |
| 0 WS frames | subscription only happens after user interaction; increase `perRouteMs` |
| empty response body | there is a real response only with `forward: true` |
| Chromium missing | install chromium or set `config.chromium` / `CHROMIUM` |
| lots of mocks but still back to login | Hook is too late or the `location.href` setter is missing → document-start + preload |
| lists all empty | an L3 empty array is normal; keep clicking Tab/Settings/detail |
| mistaking a Redux action for a route | filter out internal paths containing get/set/change/clear/toggle/upload |
| Vue still jumps to login | preload not at document-start → change injection timing; or clear guards manually when `neutralizeVueRouter: false` |
| URLs in a response not entering the log | enable `extractUrlsFromResponse`; or manually extract from `__API_RECON_DETAIL__` |
| don't know the Authorization header name | enable `observe.xhrHeaders` or check the request headers in DevTools |
| runtime very slow / timing out | change `waitUntil: domcontentloaded`; lower `routeTimeout`; do not use `networkidle2` |
| proxy connection fails | check `proxy` / environment variables; keep Puppeteer and curl on the same proxy port |

---

## G. Hardened targets

When the server validates the session step by step (an unforgeable signed cookie, a server-rendered and un-stubbable menu), runtime gets stuck at the shell. Expected behavior:

- **Static is sufficient for endpoint enumeration** — the module paths are in the code
- If authorized, run the same harness with a **real session**: `forward: true`, no neutralize needed, capturing the real methods/params/responses

---

## H. Single-task checklist

1. Confirm the authorized scope
2. **Read** `scripts/harvest_static.py` → adapt to the target → run → review `api_static.txt`, `routes.txt`
3. **Phase 1b**: path anchor window expansion + binding layer → `param_candidates.json` (section J)
4. Reverse-engineer A1/A2/A3 → write the site-specific `config.json`
5. **Read and adjust** `runtime_harvest.js` / `preload.js` before running
6. `runtimeMode=depth`: `npm install` → run the adjusted harvest script
7. `runtimeMode=coverage/both`: inject the adjusted preload at document-start → browser MCP dynamic enumeration + the **parameter trigger matrix**
8. module doesn't render → **section I permission-tree reconstruction** → patch stubs → rerun
9. multi-sample parameter diff + error inference → `params_merged.json`
10. merge → `site_map.json` + `api_merged.txt`, honestly noting coverage, gaps, and script changes

---

## I. Permission-tree reconstruction (Phase 4 deep dive)

Use this when forging a simple `menus: [{ path, show: true }]` doesn't work and submodules still don't mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, the **response field names**, and the **consuming chunk filename**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases `He=o.DASHBOARD`)
2. Use `route_map.json` to resolve alias → real path
3. Infer the parent from the code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, automatically write `config.json`'s `stubs` and extend `routes`

**Adapt to the target** (at the top of the script):
- `DEFAULT_ROOTS`: the list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: the `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes with non-prefix relationships

### I4. Verify stub consistency

```bash
# the permissions count should ≈ the number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover all links in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare the runtime_api.json counts before and after forging; check whether module APIs such as /attack, /asset appear
```

| Before forge | After forge (success) |
|---|---|
| the same 3–5 bootstrap per route | different routes trigger different module APIs |
| only `/api/locale/language` | `/api/web/...` module endpoints appear |
| single-digit routes in `routes.txt` | 80–110+ `routes` from route_map |

### I6. When it still fails

- **coverage mode**: click the sidebar + Tabs; permission gating may only request after interaction
- **stub fields**: compare a real API (curl + a real session) against the stub's nesting
- **extra guards**: grep `hasPermission|checkRole|func.` etc. for button-level checks, and extend `role_permissions.permissions`
- **static fallback**: the module API paths are still in `api_static.txt`, runtime only fills in METHOD/body; keep parameters in `param_candidates.json` + the already-recorded samples

---

## J. Parameter reverse-engineering (Phase 1b / 5b / 5c)

**A methodology, not a generic script.** Find paths with regex; find parameters with anchor window expansion + the UI binding chain + multi-sample diff + error inference.

### J1. Anchor window expansion — find the assembly object from the path

```bash
# use a path known from Phase 1 as the anchor
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper layer and transport form

```bash
# axios / unified request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation gate — required / format / enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding layer — form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime backfill: DevTools → Network → request → **Initiator** (call stack), tracing upward from `fetch`/`send` to the assembly function.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Do not guess fields on the ciphertext** — Hook the encryption function's **arguments** and record the plaintext payload before encryption; write the conclusion to `config.json` / `param_candidates.json`.

### J6. Parameter trigger matrix (required in Phase 3)

Record once per operation for each module, and diff the request body/query:

| Operation | Focus |
|---|---|
| list first screen | pagination defaults |
| search | keyword, filters |
| advanced filter | optional fields |
| create/edit | the complete entity |
| bulk/export | `ids[]`, `exportType` |
| sort/paginate | `sortField`, `order` |

Produces `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **High** | static callsite + runtime ≥2 consistent samples |
| **Medium** | static only, or only 1 runtime |
| **Low** | inferred from response/error, not re-verified |
| **Pending-trigger** | field known statically, but UI/permission not reached |

### J8. Quick scenario config

| Scenario | Order |
|---|---|
| REST list page | J1 assembly object → J6 four diffs → J3 rules |
| create/edit form | J3 Form name → J4 submit chain → runtime submit + deliberately leave blank to see 400 |
| GraphQL | J2 variables declaration → runtime record variables for each operation |
| encrypted body | J5 Hook arguments → the fields before encryption are the real params |

### J9. Mapping to api-recon phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 static | J1 anchor window expansion |
| Phase 2 A2 interceptor | global injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | different modules have different forms → full fields only trigger with enough permissions |
| Phase 5 merge | `params_merged.json` + confidence; do not decide required from a single sample |

### J10. Troubleshooting

| Symptom | Handling |
|---|---|
| a field name exists statically but never appears in runtime | mark "pending-trigger"; fill in the permission tree / click advanced filters / each option of a linked select |
| different body shapes for the same path | normal — record separately by `action`, do not force-merge the schema |
| stub response is fake but you want the params | **look at the outbound request** body/headers, do not infer from the stub response |
| 400 reports a nested field | watch for the outer wrapper `data`/`bizData`/`variables` |
| GraphQL only shows the operation name | expand the `variables` JSON; find `$var: Type` statically |

---
