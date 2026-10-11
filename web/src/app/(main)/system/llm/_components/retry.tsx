"use client";

// Shared pieces of the LLM retry config: the "attempts + interval" for each of the five retry layers.
//
// The five layers, inner to outer: connect (SDK) → empty response (SDK) → same-provider safety window → polling circuit breaker → intent rerun.
// The first three travel with the endpoint, so each model profile can override the global default; the last two are process-level, global only.
//
// All inputs follow the same "empty = unset" semantics, consistent with the backend db.RetryRule:
//   attempts   empty/0 = use the built-in default | -1 = disable this layer | >0 = use this count
//   interval   empty/0 = use this layer's original exponential backoff | >0 = use this fixed interval in ms

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
  /** Where this retry happens, and who performs it */
  where: string;
  /** Which errors reach this layer — down to the status code, so no guessing */
  trigger: string;
  /** Errors that look similar but do NOT hit this layer, so tweaking a knob with no effect is not mistaken for a bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default attempts when left empty, used as the placeholder */
  defAttempts: number;
  /** Default interval policy when left empty, used as the placeholder */
  defInterval: string;
  /** What -1 attempts means */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connect retry",
    where: "SDK · before a 200 is received",
    trigger:
      "Cannot connect or no 200 yet: network-layer errors like connection reset / read-write timeout / DNS failure, plus HTTP 408, 429, 500, 502, 503, 504.",
    skips: "All other status codes (400 / 401 / 403 / 404 / 413 / 422, etc.) are deterministic rejections — resending fails the same way, so they are thrown immediately.",
    desc: "Resends the same request as-is. Once the stream has started (a 200 was received), a mid-stream drop is no longer handled by this layer.",
    attemptsLabel: "Retry attempts",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = no retries at all, throw immediately on failure",
  },
  empty: {
    title: "Empty-response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200, finish_reason is a normal stop, but the whole response has no content block at all — gateway empty frames, dropped thinking-field frames, and sampling hiccups all look like this.",
    skips: "Not for empties caused by max_tokens truncation (that needs a higher output cap; resending just hits it again).",
    desc: "Resends the whole prompt, so it is expensive on long contexts — do not set the count high.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = hand back the empty response as-is",
  },
  stream: {
    title: "Same-provider safety-window retry",
    where: "this project · before any output is delivered",
    trigger:
      "Problems after the stream is established (a 200 was received): the connection drops mid-stream, the provider is overloaded, in-stream 429 / 5xx error events — and not a single token has been handed to the caller yet.",
    skips:
      "No retry for: quota exhausted (402 / insufficient_quota, left to polling to switch profiles), context too long (413 / context length, left to compaction), and 400 / 401 / 403 / 404 / 422 deterministic rejections.",
    desc: "Replays the same request on the same profile. Since no output was delivered yet, the replay does not duplicate model output or tool execution.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = a dropped stream goes straight to the outer intent rerun",
  },
  breaker: {
    title: "Polling circuit breaker",
    where: "this project · process-level, one global copy",
    trigger:
      "Trips when transient failures (429, 5xx, network errors) accumulate to the threshold; deterministic failures like insufficient balance (402), invalid key (401 / 403), and nonexistent model (404) ignore the threshold and trip on the first.",
    skips: "One success resets the count, so an occasionally-flaky profile is not slowly accumulated into a trip.",
    desc: "After tripping it cools down, and during cooldown polling skips this profile. The state is persisted and survives restarts.",
    attemptsLabel: "Consecutive failures to trip",
    defAttempts: 3,
    defInterval: "1min→5min→30min gradient",
    offHint: "-1 = transient failures never trip (deterministic failures still do)",
  },
  intent: {
    title: "Intent rerun",
    where: "this project · process-level, one global copy",
    trigger:
      "None of the earlier layers caught it: the worker ends in model_error — the inner retries were exhausted, or the stream dropped after it had begun delivering output (replay is then unsafe, so the whole thing reruns).",
    skips: "Quota exhaustion is already handled by polling switching profiles, not reran here; when the task is paused / terminated / entering wrap-up, it yields immediately and does not consume backoff time.",
    desc: "Reruns the whole intent from the start. As the outermost layer, one rerun multiplies the inner layers' counts again.",
    attemptsLabel: "Rerun attempts",
    defAttempts: 2,
    defInterval: "fixed 3s",
    offHint: "-1 = no rerun; the intent is marked blocked directly",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Humanized milliseconds, only echoed next to the input so you need not count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled numeric input: empty string ↔ 0; intermediate states ("-", "1e") stay local as-is without disturbing the parent. */
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
  // Follow along when the parent swaps the whole value set (policy loaded, profile switched); typing yourself never reaches here,
  // because then value already equals the parsed local text.
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

/** The two knobs of one retry layer. idPrefix keeps the label's htmlFor unique when it appears multiple times on a page. */
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
  /** true = compact variant for the config drawer: drops the expanded notes, keeping only the "which errors hit this layer" line */
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
        {/* Which errors hit this layer, down to the status code — if a knob has no effect, the error most likely does not land in this layer. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Trigger</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Does not hit this layer</span>: {meta.skips}
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
            Interval ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Empty = use the default; {meta.offHint}.</p>}
    </div>
  );
}

/** The three-layer overrides in the model-config drawer (the three that travel with the endpoint). */
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
        <Label className="text-sm">Retry override</Label>
        <p className="text-muted-foreground text-xs">
          Applies to this profile only, overriding the global default in "Retries & backoff". Each cell empty = follow global; -1 attempts = disable that layer;
          a set interval replaces exponential backoff with a fixed one. The circuit breaker and intent rerun are process-level and can only be tuned on the global page.
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

/** The "Retries & backoff" tab: the global defaults for the five layers. */
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
      toast.error(`Failed to read retry policy: ${(e as Error).message}`);
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
      // The backend clamps out-of-range values back into range and returns them; refresh from the returned value, so what you see is what is stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved; takes effect immediately (the call currently in flight still uses the old parameters)");
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
        <Loader2Icon className="size-4 animate-spin" /> Reading retry policy…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A failed model call passes through five retry layers in order, inner to outer:
        <span className="text-foreground"> connect → empty response → same-provider safety window → polling circuit breaker → intent rerun</span>
        . The outer layer only takes over once the inner is exhausted, so the counts
        <span className="text-foreground">multiply</span>
        — max out every layer and one hiccup can burn dozens of requests.
        Leaving everything empty yields the current defaults, identical to the behavior without this page. The first three layers can be overridden per model profile.
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
          Restore all defaults
        </Button>
      </div>
    </div>
  );
}
