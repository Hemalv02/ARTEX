// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: intent / coverage / task / severity each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose is used for "critical" — a solid high-emphasis fill, visually above the soft red outline of "high".
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "Pending", tone: "slate" },
  running: { label: "Running", tone: "blue" },
  paused: { label: "Paused", tone: "amber" },
  done: { label: "Completed", tone: "green" },
  // blocked = retries exhausted on model/API/network failures; this intent effectively didn't explore (not a target block).
  blocked: { label: "Execution error", tone: "red" },
  // exhausted = cut off mid-flight by the step/time budget, only partial results written back (not the direction being fully explored).
  exhausted: { label: "Budget exhausted", tone: "violet" },
  // stopped = legacy soft-delete state (kept for historical data).
  stopped: { label: "Stopped", tone: "slate" },
  // deleted = user soft-deleted this intent (node and lineage preserved; reason in the delete_reason field).
  deleted: { label: "Deleted", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "Created", tone: "slate" },
  queued: { label: "Queued", tone: "amber" },
  running: { label: "Running", tone: "blue" },
  paused: { label: "Paused", tone: "amber" },
  done: { label: "Completed", tone: "green" },
  failed: { label: "Failed", tone: "red" },
  timeout: { label: "Timed out", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "Critical", tone: "rose" },
  high: { label: "High", tone: "red" },
  medium: { label: "Medium", tone: "amber" },
  low: { label: "Low", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "To-do", tone: "amber" },
  in_progress: { label: "In progress", tone: "blue" },
  confirmed: { label: "Confirmed", tone: "red" },
  resolved: { label: "Resolved", tone: "green" },
  fixed: { label: "Fixed", tone: "green" },
  false_positive: { label: "False positive", tone: "slate" },
  ignored: { label: "Ignored", tone: "neutral" },
  duplicate: { label: "Duplicate", tone: "neutral" },
  risk_accepted: { label: "Risk accepted", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "Exploring", tone: "blue" },
  paused: { label: "Paused", tone: "amber" },
  stalled: { label: "Stalled", tone: "red" },
  idle: { label: "Idle", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "In progress", tone: "blue" },
  met: { label: "Achieved", tone: "green" },
  abandoned: { label: "Abandoned", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "Allow", tone: "green" },
  block: { label: "Block", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "Observed", tone: "slate" },
  confirmed: { label: "Confirmed", tone: "green" },
  tombstoned: { label: "Discarded", tone: "neutral" },
};

// Push delivery status. sending uses blue rather than amber: it isn't "having a problem",
// it means "picked up, in flight" — semantically distinct from pending's waiting state.
const delivery: Record<string, StatusMeta> = {
  pending: { label: "Pending", tone: "amber" },
  sending: { label: "Sending", tone: "blue" },
  sent: { label: "Delivered", tone: "green" },
  failed: { label: "Failed", tone: "red" },
  skipped: { label: "Skipped", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}
