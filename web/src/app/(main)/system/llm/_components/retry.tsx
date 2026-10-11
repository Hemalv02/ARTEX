"use client";

// Shared components for LLM retry configuration: each of the five retry layers
// has its own "attempts + interval" pair.
//
// The five layers, from innermost to outermost: connect (SDK) → empty response
// (SDK) → same-provider safety window → pool circuit breaker → intent replay.
// The first three follow the endpoint, so each model profile can override the
// global defaults; the last two are process-wide and only have a single global
// configuration.
//
// All inputs follow the same "empty = not configured" semantics, matching the
// backend's db.RetryRule:
//   attempts   empty/0 = use built-in default | -1 = disable this layer | >0 = use this count
//   interval   empty/0 = use the layer's default exponential backoff | >0 = use this fixed ms interval

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** Where this retry happens, and who executes it */
  where: string;
  /** Which errors land at this layer — be specific about status codes so users
      can predict behaviour */
  trigger: string;
  /** Similar-looking errors that do NOT land here, so configuring the layer
      isn't a no-op while users think it is broken */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default value when attempts is left empty, used in the placeholder */
  defAttempts: number;
  /** Default policy when interval is left empty, used in the placeholder */
  defInterval: string;
  /** Meaning of entering -1 in the attempts field */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connect retry",
    where: "SDK · before receiving 200",
    trigger:
      "Connection failures or no 200 yet: connection reset, read/write timeout, DNS errors, and HTTP 408, 429, 500, 502, 503, 504.",
    skips: "Other status codes (400 / 401 / 403 / 404 / 413 / 422, etc.) are deterministic refusals; retrying will just fail again and is bubbled up immediately.",
    desc: "Resends the same request verbatim. Once streaming has started (a 200 was received), mid-stream disconnects are not this layer's concern.",
    attemptsLabel: "Attempts",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (cap 8s)",
    offHint: "-1 = no retries; first failure is bubbled up",
  },
  empty: {
    title: "Empty-response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200 with finish_reason=stop but no content blocks at all — gateway empty frames, dropped thinking fields, sampling hiccups all look like this.",
    skips: "Empty results due to max_tokens truncation don't count (raise the output cap; retrying just hits the same wall).",
    desc: "Resends the full prompt, which is expensive for long contexts, so keep the count low.",
    attemptsLabel: "Attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (cap 8s)",
    offHint: "-1 = empty response is returned as-is",
  },
  stream: {
    title: "Same-provider safety window",
    where: "ARTEX · before any output is delivered",
    trigger:
      "Streaming was established (200 received) but broke afterwards: mid-stream disconnect, provider overloaded, in-stream 429 / 5xx error events — and not a single token has been delivered to the caller yet.",
    skips:
      "Out of quota (402 / insufficient_quota → handled by the pool swap), context too long (413 / context length → handled by compaction), and deterministic refusals 400 / 401 / 403 / 404 / 422 are not retried.",
    desc: "Replays the same request on the same profile. Because no output was delivered, replaying cannot duplicate model output or tool execution.",
    attemptsLabel: "Attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (cap 4s)",
    offHint: "-1 = broken stream is handed off to the outer intent replay",
  },
  breaker: {
    title: "Pool circuit breaker",
    where: "ARTEX · process-wide, single global config",
    trigger:
      "Trips after transient failures (429, 5xx, network errors) accumulate to a threshold; deterministic failures (out of quota 402, bad key 401 / 403, model not found 404) skip the threshold and trip on the first occurrence.",
    skips: "One success clears the count, so profiles that occasionally hiccup don't slowly accumulate to a trip.",
    desc: "After tripping, the profile enters cooldown and is skipped by the pool during that window. State is persisted to DB so it survives restarts.",
    attemptsLabel: "Failures before trip",
    defAttempts: 3,
    defInterval: "1min→5min→30min ladder",
    offHint: "-1 = transient failures never trip (deterministic failures still trip)",
  },
  intent: {
    title: "Intent replay",
    where: "ARTEX · process-wide, single global config",
    trigger:
      "Everything inner couldn't catch: the worker ended in model_error — inner retries exhausted, or the stream broke after output was already being delivered (where replaying is unsafe and the whole intent has to be redone).",
    skips: "Out-of-quota is already handled by the pool swap and isn't replayed here; paused / stopped / finalising tasks yield immediately rather than burning backoff time.",
    desc: "Re-runs the entire intent from scratch. It is the outermost layer, so one replay means the inner layers' counts get multiplied.",
    attemptsLabel: "Replays",
    defAttempts: 2,
    defInterval: "Fixed 3s",
    offHint: "-1 = no replay; the intent is marked blocked",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Friendly rendering of milliseconds for the input's adjacent echo, so users
    don't have to count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled numeric input: empty string ↔ 0; intermediate states ("-", "1e")
    are kept locally so they don't disturb the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // Sync when the parent switches to a different set of values (e.g. loaded a
  // policy or switched profiles). When the user is typing, value already
  // equals the parsed local text, so this branch is skipped.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** The two knobs for one retry layer. idPrefix keeps label htmlFor unique when
    the same component is rendered multiple times on a page. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = compact version in the profile drawer: skip the long descriptions
      and keep only the "which errors land here" line */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* Which errors land at this layer, with specific status codes — if a
            knob is set but appears to have no effect, the error is usually
            not routed through this layer. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Trigger</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Does not retry</span>: {meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`default ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            Interval (ms)
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `Fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Empty = use default; {meta.offHint}.</p>}
    </div>
  );
}

/** The three layer overrides in the model profile drawer (the three layers
    that follow the endpoint). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">Retry overrides</Label>
        <p className="text-muted-foreground text-xs">
          Apply only to this profile, overriding the "Retry & backoff" globals. Each cell empty = follow global; -1 attempts = disable this layer; entering an interval replaces the exponential backoff with a fixed delay. Breaker and intent replay are process-wide and can only be tuned on the global page.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** "Retry & backoff" tab: global defaults for all five layers. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`Failed to load retry policy: ${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend clamps out-of-range values and returns the clamped copy;
      // use the returned value as the new state so what you see is what is stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved — applied live (in-flight calls still use the old parameters)");
    } catch (e) {
      toast.error(`Save failed: ${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> Loading retry policy…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A single model call's failures pass through five retry layers, innermost to outermost:
        <span className="text-foreground"> connect → empty response → same-provider safety window → pool circuit breaker → intent replay</span>
        . The inner layer must be exhausted before the next one applies, so attempt counts are
        <span className="text-foreground">multiplicative</span>
        — set every layer to its max and a single hiccup can burn dozens of requests.
        Leaving everything empty is the current default and behaves identically to having no policy page. The first three layers can be overridden per profile.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          Save
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          Reset all to defaults
        </Button>
      </div>
    </div>
  );
}
