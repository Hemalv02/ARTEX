# ARTEX Chinese-to-English Translation Glossary

This document locks translations of recurring Chinese terms for the
ARTEX Chinese-to-English conversion. Every translator (human or
LLM) must consult this glossary before converting prose to keep
naming consistent across UI, backend, prompts, and docs.

## Brand and product names (NEVER translate)

| Chinese | English | Notes |
|---|---|---|
| ARTEX | ARTEX | project / product name |
| 钉钉 | DingTalk | IM platform brand |
| 飞书 | Feishu | IM platform brand |
| 企业微信 / 企微 | WeCom | IM platform brand |
| GNU AGPL-3.0 | GNU AGPL-3.0 | license name |
| JWT, HTTP, HMAC-SHA256, SSE, MCP, LLM, PoC, RCE, SQL, CVE | unchanged | protocols / acronyms |
| PostgreSQL, SQLite, Redis | unchanged | database / tech names |
| redhaze.top, acme.com | unchanged | test/placeholder domains |

## UI / status / domain terms

| Chinese | English | Domain |
|---|---|---|
| 渗透测试 | Penetration testing | prompt, doc |
| 漏洞 | Vulnerability / Finding | Finding in UI list, Vulnerability in prompts |
| 任务 | Task | |
| 意图 | Intent | |
| 工具 | Tool | |
| 资产 | Asset | |
| 拦截 | Intercept / Block | Intercept in nav, Block in audit |
| 放行 | Allow | |
| 审批 / 人工审批 | Approval / Manual approval | |
| 通知推送 | Notifications | nav |
| 仪表盘 | Dashboard | |
| 对话 | Chat | |
| 流量 | Traffic | |
| 录制 | Recording / Record | |
| 探索 / 探索中 | Exploring | |
| 暂停 | Paused | |
| 已完成 | Completed | |
| 已删除 | Deleted | |
| 已停止 | Stopped | |
| 排队中 | Queued | |
| 已创建 | Created | |
| 失败 | Failed | |
| 超时 / 已超时 | Timed out | |
| 待领 | Pending (intent) | |
| 执行中 | Running / In progress | |
| 已确认 | Confirmed | |
| 已处理 / 已修复 | Resolved / Fixed | |
| 误报 | False positive | |
| 忽略 | Ignored | |
| 风险接受 | Risk accepted | |
| 重复 | Duplicate | |
| 空闲 | Idle | |
| 停滞 | Stalled | |
| 进行中 | In progress | |
| 已达成 | Achieved | |
| 已放弃 | Abandoned | |
| 观测 | Observed | |
| 确认 | Confirmed | |
| 废弃 | Discarded / Tombstoned | |
| 待发送 | Pending (delivery) | |
| 发送中 | Sending | |
| 已送达 | Delivered | |
| 已跳过 | Skipped | |
| 待处理 | To-do | |
| 处理中 | In progress | |
| 子域名 | Subdomain | |
| 根域名 | Root domain | |
| 端点 | Endpoint | |
| 应用 | Application | |
| 服务 | Service | |
| 缓存读 | Cache read | |
| 缓存写 | Cache write | |
| 输入 | Input | |
| 输出 | Output | |
| 系统配置 | Settings | |
| 系统 | System | nav group |
| 功能 | Features | nav group |
| 工具执行 | Tool execution | nav |
| LLM 录制 | LLM recordings | nav |
| 资产同步 | Asset sync | nav |
| 工作空间 | Workspace | nav |
| 拦截规则 | Intercept rules | nav |
| 资产拦截 | Asset intercept | nav |
| 审批记录 | Approval history | nav |
| 日志 | Logs | nav |
| 严重 | Critical | severity |
| 高危 | High | severity |
| 中危 | Medium | severity |
| 低危 | Low | severity |

## Common phrases

| Chinese | English |
|---|---|
| 当前 | Current |
| 上次 | Last / Previous |
| 创建时间 | Created at |
| 更新时间 | Updated at |
| 确认发现 | Confirm finding |
| 上传文件 | Upload file |
| 发送消息 | Send message |
| 停止本次运行 | Stop current run |
| 暂无任务 | No tasks yet |
| 暂无数据 | No data |
| 请稍后重试 | Please try again later |
| 加载失败 | Failed to load |
| 发送失败 | Failed to send |
| 操作成功 | Operation succeeded |
| 操作失败 | Operation failed |
| 删除成功 | Deleted successfully |
| 保存成功 | Saved successfully |
| 网络错误 | Network error |
| 未知错误 | Unknown error |
| 取消 | Cancel |
| 确认 | Confirm |
| 删除 | Delete |
| 编辑 | Edit |
| 新建 | New |
| 搜索 | Search |
| 刷新 | Refresh |
| 导出 | Export |
| 导入 | Import |
| 复制 | Copy |
| 全选 | Select all |
| 详情 | Details |
| 返回 | Back |
| 关闭 | Close |
| 重试 | Retry |
| 应用 | Apply |
| 重置 | Reset |
| 启用 | Enable |
| 禁用 | Disable |
| 必填 | Required |
| 可选 | Optional |
| 备注 | Notes |
| 描述 | Description |
| 类型 | Type |
| 状态 | Status |
| 优先级 | Priority |
| 严重度 | Severity |
| 风险 | Risk |
| 授权 | Authorization |
| 授权范围 | Scope of authorization |
| 目标 | Target |
| 范围 | Scope |
| 凭证 / 凭据 | Credentials |
| 报告 | Report |
| PoC / 漏洞证明 | PoC / Proof of concept |
| 复现步骤 | Reproduction steps |
| 影响 | Impact |
| 修复建议 | Remediation / Fix recommendation |
| 临时回退 | Temporary workaround |
| 不可达 | Unreachable |
| 不可用 | Unavailable |
| 数据源 | Data source |
| 数据源暂时不可用 | Data source temporarily unavailable |
| 暂未实现 | Not implemented yet |
| 限流 | Rate limit |
| 关键词 | Keyword |
| 签名 | Signature |
| 签名校验 | Signature verification |
| 错误码 | Error code |
| 错误信息 | Error message |
| 请求过于频繁 | Too many requests |
| 缺少参数 | Missing parameter |
| 参数无效 | Invalid parameter |
| 未授权 | Unauthorized |
| 禁止 | Forbidden |
| token 无效或已过期 | Token invalid or expired |
| 密码已设置 | Password already set |
| 密码长度至少 N 位 | Password must be at least N characters |
| 密码长度不能超过 N 字节 | Password length cannot exceed N bytes |
| 渠道类型无效 | Invalid channel type |
| 缺少渠道名称 | Missing channel name |
| 推送模式无效 | Invalid push mode |
| 限流值不能为负 | Rate limit cannot be negative |
| 可选 | Optional / Allowed values |
| 定时 | Scheduled |
| 立即 | Immediate |
| 手动 | Manual |
| 自动 | Automatic |
| 任务执行 | Task execution |
| 漏洞等级 | Vulnerability severity |
| 漏洞名称 | Vulnerability name |
| 详细报告 | Detailed report (Markdown) |
| 摘要 | Summary |
| 发现 | Findings |
| 概述 | Overview |
| 结论 | Conclusion |
| 附录 | Appendix |
| 渗透测试报告 | Penetration test report |
| 触发条件 | Trigger conditions |
| 实际效果 | Actual effect |
| 命中规则 | Matched rule |
| 实际操作 | Actual operation |
| 成功后的后果 | Post-success impact |
| 拦截规则 [name] 禁止执行此工具 | Intercept rule [name] blocks this tool |
| 拦截规则 [name] 要求用户审批 | Intercept rule [name] requires user approval |
| 放行 | Allow |
| 拦截 | Block |
| 转人工审批 | Forward to manual approval |
| 人工拒绝执行 | Manually rejected |
| 工作已取消 | Work cancelled |
| 平台安全管控 | Platform safety control |
| 此调用被平台拦截 | This call was blocked by the platform |
| ARTEX 使用须知 | ARTEX Terms of Use |
| 与免责声明 | and Disclaimer |
| 免责 | Disclaimer |
| 条款 | Clauses / Terms |
| 协议 | Agreement |
| 用户 | User |
| 贡献者 | Contributors |
| 软件 | Software |
| 许可 | License |
| 开源 | Open source |
| 项目 | Project |
| 作者 | Author |

## Files intentionally left in Chinese

| File | Reason |
|---|---|
| `sidequestion/validation-2026-09-10.json` | Frozen historical validation record; translating breaks reproducibility. |

## Translation conventions

1. **Format strings** — preserve all `%s`, `%d`, `%v`, `${var}` placeholders
   and their order. Do not translate inside placeholders.
2. **Go identifier comments** — translate the Chinese prose but keep the
   identifier name in English (identifiers are already English).
3. **TSX/JSX attribute values** — translate the human-readable string,
   keep identifiers (key names, route paths, icon imports) untouched.
4. **Mermaid / diagram labels** — translate the displayed text inside
   `[]` brackets and `{}` braces.
5. **MCP tool names, function names, file paths** — never translate.
6. **Severity and status enum values** (`critical`, `high`, `medium`,
   `low`, `done`, `failed`, etc.) — keep as English enum keys. The
   user-facing `label` field is the only thing that translates.
7. **Punctuation** — keep Chinese full-width punctuation only when
   inside code comments that have not been reformatted; in user-facing
   strings, prefer ASCII `,` `.` `:` `?` `!` `()` for consistency.
8. **Tone** — concise, professional, security-domain English. Avoid
   flowery marketing language. Match the style of `not-found.tsx`
   ("Page not found. Go back home.") and the Go doc-comments already
   in English.

## Intentional CJK preserves

Some Chinese strings remain in the codebase after translation. They are
deliberate — see the table below. Cross-checkers and follow-up passes
must NOT translate them.

| Location | Why CJK is preserved |
|---|---|
| `sidequestion/validation-2026-09-10.json`, `sidequestion/VALIDATION.md` | Frozen historical validation record from before the i18n pass; not user-facing prose. |
| `CHANGELOG.md` | Append-only history; retroactively translating would falsify the record. |
| `db/task_assets_test.go`, `db/company_scope*.go`, `server/skill_upload_test.go`, `agent/blackboard_inheritance_test.go`, `agent/tools_overview_test.go`, `intercept/trace_test.go`, `notify/render_test.go` | CJK is test fixture data for ICP / skill-name / asset-type validators that must accept Unicode. |
| `server/chat_mentions.go` (`域名`, `子域名`) | Regex keys preserved for backwards compatibility with existing chat messages that contain those tokens; the lookup map has been translated. |
| `skills/api-recon/` (`reference.md`, `preload.js`, `runtime_harvest.js`) | Skill content follows Chinese pentest conventions; not product UI. |
