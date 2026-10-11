package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "The model ended the round normally but did not leave a text summary; facts and assets are determined by the tool calls of this round",
	harness.ReasonMaxTurns:          "Step limit reached (MaxTurns): the SDK has run the wind-up and written back facts and assets, and the intent is marked exhausted so the planner can change direction and continue — not handled as a failure",
	harness.ReasonTimeout:           "Per-run wall-clock budget reached (MaxDuration): at the deadline, the in-flight tool is interrupted and wind-up runs in place, writing back the already-identified facts and assets, and the intent is marked exhausted",
	harness.ReasonModelError:        "Model or API call failed (network, auth, rate limit, vendor 5xx, etc.); after retries are exhausted, the intent is marked blocked — a transport-layer failure means this intent barely did any real probing; check its execution trace (get_worker_trace) before deciding to redispatch or change tactic",
	harness.ReasonBlockingLimit:     "Context length hit the hard cap, so the request was blocked before being sent; narrow the intent granularity or compress tool outputs",
	harness.ReasonPromptTooLong:     "Prompt was too long and context-compression retries have been exhausted; execution cannot continue",
	harness.ReasonImageError:        "The current model does not support this round's multimodal content; please switch to a vision-capable model or avoid returning images from tools",
	harness.ReasonStopHookPrevented: "The Stop hook prevented the round from ending, and execution did not continue afterwards; please check whether the task Guard rules are too strict",
	harness.ReasonHookStopped:       "A tool or hook actively stopped execution, e.g. an out-of-scope target or a blocked command; check the block explanation in the last tool_result",
	harness.ReasonAbortedStreaming:  "Run was cancelled while the model was streaming output",
	harness.ReasonAbortedTools:      "Run was cancelled while a tool was being executed",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "Failed to obtain the cancellation reason"
		}
		stage := "during execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "model output stage"
		case harness.ReasonAbortedTools:
			stage = "tool execution stage"
		}
		sum = "(Run was interrupted: " + short + "; stopped at " + stage + progressSuffix(term, tr) + ", not completed)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(Run budget reached (" + string(reason) + "), wind-up written back" + progressSuffix(term, tr) + "; no text summary this time)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(No text summary; terminal " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal state**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Abort reason** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Abort reason**: unavailable; the canceller may not have attached a named cause via context.WithCancelCause\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **Underlying error**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **Partial output generated before cancellation**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **Executed**: %d model turns\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **This run took**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Cumulative tokens**: input %d / output %d / cache read %d / cache write %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool call**: this run ended before issuing any tool call\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool executing at abort**: `%s` (ran for %s, **no result returned**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **Last tool before abort**: `%s` (returned normally)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "The run's context was cancelled but no Terminal event was produced by the underlying layer"
	}
	return "Unknown terminal state; the harness may have added a new TerminalReason — please add an entry in reasonHint"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", ran " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
