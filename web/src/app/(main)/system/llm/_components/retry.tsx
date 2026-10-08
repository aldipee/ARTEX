"use client";

// Shared components for LLM retry config: the "attempts + interval" for each of the five retry layers.
//
// The five layers from inner to outer: connect (SDK) → empty response (SDK) → same-provider safe window →
// pool breaker → intent rerun. The first three follow the endpoint, so each model profile can override the
// global defaults; the last two are process-level with only a single global copy.
//
// All inputs follow the same "leave empty = not configured" semantics, matching the backend db.RetryRule:
//   attempts  empty/0 = use the built-in default | -1 = disable this retry layer | >0 = use this count
//   interval  empty/0 = use this layer's original exponential backoff | >0 = use this fixed millisecond interval

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
  /** Where this retry layer happens and who executes it */
  where: string;
  /** What kind of errors reach this layer — down to the status code, so no guessing */
  trigger: string;
  /** Errors that look similar but do NOT go through this layer, so you don't think it's a bug when nothing happens */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** Default value when attempts is left empty, used for the placeholder */
  defAttempts: number;
  /** Default strategy when interval is left empty, used for the placeholder */
  defInterval: string;
  /** Meaning of -1 for attempts */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "Connect retry",
    where: "SDK · before receiving a 200",
    trigger:
      "Can't connect or haven't received a 200 yet: network-layer errors such as connection reset / read-write timeout / DNS failure, plus HTTP 408, 429, 500, 502, 503, 504.",
    skips: "All other status codes (400 / 401 / 403 / 404 / 413 / 422, etc.) are deterministic rejections; retrying would fail the same way, so they're rethrown directly.",
    desc: "Resend the same request as-is. Once the stream starts (a 200 has been received), a mid-stream drop is no longer this layer's job.",
    attemptsLabel: "Retry attempts",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = don't retry at all, rethrow the failure immediately",
  },
  empty: {
    title: "Empty response retry",
    where: "SDK · openai format only",
    trigger:
      "HTTP 200 with a normal stop finish_reason, but the whole response has no content chunks at all — gateway empty frames, dropped thinking-field frames, and sampling hiccups all look like this.",
    skips: "A response with no content due to max_tokens truncation doesn't count (that's solved by raising the output cap; resending would just hit it again).",
    desc: "Resends the whole prompt, so it's relatively expensive on long contexts; don't set a large count.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s exponential (capped at 8s)",
    offHint: "-1 = hand the empty response over as-is",
  },
  stream: {
    title: "Same-provider safe-window retry",
    where: "This project · before any output is delivered",
    trigger:
      "Problems that happen after the stream is established (a 200 received): mid-connection drop, provider overloaded, in-stream 429 / 5xx error events — and no token has been handed to the caller yet.",
    skips:
      "Quota exhausted (402 / insufficient_quota, handed to pooling to switch profiles), context too long (413 / context length, handed to compaction), and 400 / 401 / 403 / 404 / 422 deterministic rejections are all not retried.",
    desc: "Replays the same request on the same profile. Since no output has been delivered yet, replaying won't duplicate model output or tool execution.",
    attemptsLabel: "Retry attempts",
    defAttempts: 2,
    defInterval: "0.5s→1s exponential (capped at 4s)",
    offHint: "-1 = hand the dropped stream directly to the outer intent rerun",
  },
  breaker: {
    title: "Pool breaker",
    where: "This project · process-level, single global copy",
    trigger:
      "Trips when transient failures (429, 5xx, network errors) accumulate to the threshold consecutively; deterministic failures such as insufficient balance (402), invalid key (401 / 403), and model not found (404) ignore the threshold and trip on the first occurrence.",
    skips: "A single success resets the counter, so profiles that only occasionally hiccup won't slowly build up to a trip.",
    desc: "After tripping it enters cooldown, during which pooling skips this profile directly. State is persisted, so it survives restarts.",
    attemptsLabel: "Trip after N consecutive failures",
    defAttempts: 3,
    defInterval: "1min→5min→30min ladder",
    offHint: "-1 = transient failures never trip (deterministic failures still trip)",
  },
  intent: {
    title: "Intent rerun",
    where: "This project · process-level, single global copy",
    trigger:
      "When none of the earlier layers catch it: the worker ends with model_error — inner retries are all exhausted, or the stream dropped after output delivery already started (replaying is unsafe then, so the whole thing must be restarted).",
    skips: "Quota exhaustion is already handled by pooling switching profiles, so it isn't rerun here; when a task is paused / terminated / entering teardown it yields immediately without occupying backoff time.",
    desc: "Reruns the whole intent from scratch. It's the outermost layer, so one rerun multiplies the inner layers' counts again.",
    attemptsLabel: "Rerun count",
    defAttempts: 2,
    defInterval: "Fixed 3s",
    offHint: "-1 = don't rerun; the intent is marked blocked directly",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** Human-readable milliseconds, only used to echo next to the input so you don't have to count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled number input: empty string ↔ 0, intermediate states ("-", "1e") stay local as-is without bothering the parent. */
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
  // Follow along when the parent swaps in a whole new set of values (policy read, profile switch); typing
  // doesn't reach here because value already equals the locally-parsed text then.
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

/** The two knobs for one retry layer. idPrefix keeps the label's htmlFor unique when it appears multiple times on a page. */
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
  /** true = the compact version in the profile drawer: drops the expanded explanation, keeping only the "what errors reach this layer" line */
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
        {/* Which errors reach this layer, down to the status code — if you set the knob and see no effect, the error probably doesn't fall in this layer at all. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">Trigger</span>: {meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">Not in this layer</span>: {meta.skips}
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
            placeholder={`Default ${meta.defAttempts}`}
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
            placeholder="Default backoff"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `Fixed ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">Leave empty = use default; {meta.offHint}.</p>}
    </div>
  );
}

/** The three-layer override in the model profile drawer (the three that follow the endpoint). */
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
          Takes effect only for this profile, overriding the global defaults in "Retry & backoff". Leave each
          box empty = follow global; set attempts to -1 = disable this retry layer; fill in an interval to
          replace exponential backoff with a fixed interval. The breaker and intent rerun are process-level and
          can only be adjusted on the global page.
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

/** The "Retry & backoff" tab: global defaults for all five layers. */
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
      // The backend clamps out-of-range values back into range and returns them; refresh with the returned
      // values so what you see is what's stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("Saved; takes effect immediately (the currently running round still uses the old parameters)");
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
        <Loader2Icon className="size-4 animate-spin" /> Reading retry policy...
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        A single model-call failure passes through five retry layers, from inner to outer:
        <span className="text-foreground"> connect → empty response → same-provider safe window → pool breaker → intent rerun</span>
        . The outer layer only gets a turn after the inner layer is exhausted, so the counts are
        <span className="text-foreground">multiplied</span>
        — max out every layer and one hiccup can burn dozens of requests.
        Leaving everything empty means the current defaults, exactly matching the behavior when this page
        didn't exist. The first three layers can be overridden per model profile.
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
