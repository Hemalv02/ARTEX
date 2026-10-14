// Channel field table and config-value parsing utilities.
//
// Split out from the page because this is **data**, not view: it describes which fields
// each channel has, which control each should use, and the two-way conversion between
// form text and config values (JSON). With it in its own file, adding a channel only
// touches here and the page itself need not change.
// Display names of the channel types. Kept on the front end since it only affects copy; the backend does not need to know.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Feishu",
  wecom: "WeCom",
  webhook: "Generic Webhook",
  telegram: "Telegram",
  email: "Email",
};

// Config field definitions for each channel.
//
// A front-end field table is kept deliberately rather than having the backend push a schema:
// the backend only does validation (required/format), while the UI needs layout and control
// types — the two are concerned with different things. The one coupling point is secret_keys —
// which fields render as password boxes is provided by the backend, because only a channel's
// implementation knows which values count as credentials (WeCom's entire webhook is a credential,
// while DingTalk's is just one of its secrets). Missing an entry here when adding a channel only
// leaves the form blank; it does not fail silently (hasFields below flags it).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "Signing secret",
      kind: "password",
      help: "Fill in when the bot's security setting is \"Signing\"; leave empty for \"Custom keywords\" or when no security setting is enabled",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "Signature-verification secret", kind: "password", help: "Fill in when the bot has \"signature verification\" enabled, otherwise leave empty" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "Target URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "Request method",
      kind: "select",
      options: [
        { value: "POST", label: "POST (with body)" },
        { value: "PUT", label: "PUT (with body)" },
        { value: "PATCH", label: "PATCH (with body)" },
        { value: "GET", label: "GET (no body)" },
      ],
    },
    { key: "headers", label: "Custom request headers", kind: "kv", help: "One KEY=VALUE per line, e.g. Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "Request body template",
      kind: "textarea",
      help:
        "Leave empty to use the built-in default template. Variables: {{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}, " +
        "plus .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel under range .Items. " +
        "To insert a string use {{json .Xxx}} rather than {{.Xxx}}, otherwise quotes in the title would break the JSON.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API URL",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "Leave empty for the official URL; fill in when reverse-proxying a self-hosted Bot API",
    },
  ],
  email: [
    { key: "host", label: "SMTP server", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "Port",
      kind: "number",
      placeholder: "587",
      help: "587 uses STARTTLS; for 465 turn on \"Implicit TLS\"",
    },
    { key: "username", label: "Username", kind: "text" },
    { key: "password", label: "Password / app password", kind: "password" },
    { key: "from", label: "From", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "To", kind: "list", help: "Separate multiple addresses with commas" },
    { key: "tls", label: "Implicit TLS", kind: "switch", help: "On for port 465; off for 587 (STARTTLS is automatic)" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "Any" },
  { value: "low", label: "Low and above" },
  { value: "medium", label: "Medium and above" },
  { value: "high", label: "High and above" },
  { value: "critical", label: "Critical only" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV parses a "one KEY=VALUE per line" textarea.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs parses a comma/whitespace-separated list of ids.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords parses a newline/comma-separated keyword list (vuln-class names may contain spaces, so split by line or comma).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
