package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side parsing of the retry policy, see docs/LLMretrydesign.md. Of the five layers:
//   - Connect / empty response / same-provider security window are "endpoint-bound"; each LLM config can override the
//     global default (a profile field left empty inherits the global, and if the global is unset, the built-in default is used);
//   - Circuit-breaker / intent replay are process-wide, only one global copy exists.
//
// The global policy reads one DB settings row; call sites are all on low-frequency paths (build provider, work wrap-up,
// save config), not worth adding another cache layer; the circuit-breaker args are an exception — they are read on every failure path,
// so applyRetryPolicy pushes them to the Registry on save.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// The count here retains the original "0=default / negative=off" semantics: the SDK's MaxRetries /
		// EmptyResponseRetries are fully isomorphic to it, so we just hand it off for the SDK to parse.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// The circuit-breaker (polling cooldown) defaults are consistent with the llmpool built-ins — here we only override when the "user has set a value".
// The intent replay defaults are in engine.go: modelErrorRetries / modelErrorRetryBackoff.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob — "empty response
// retry count": the two are two means to the same end. The SDK layer handles "no content block at all" by resending
// the same request verbatim; here we handle "has thinking but no text and no tool", by appending an instruction to
// tell the model to continue with its existing thinking (verbatim resend is meaningless for this kind of empty loop determined by the context shape). The "empty" definition
// differs because the SDK uses "has it yielded any event" as the criterion, and a thinking delta is itself an event — but when a user configures
// "how many empty-response retries" they mean "if the model produced no real content, try again"; sharing one count between the two layers
// matches that mental model.
//
// Read the global policy, not any profile's override: a run may switch profiles mid-way due to failover, and this
// is the total cap for the whole intent, not something that should change with the endpoint. Semantically isomorphic to the SDK's emptyRetries():
// 0 = default (defaultEmptyTurnNudges); -1 (negative) = off, no empty-loop continuation; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
