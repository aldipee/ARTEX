package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side parsing of the retry policy, see docs/LLM重试设计.md. Of the five layers:
//   - connection / empty response / same-provider safety window "follow the endpoint" and each LLM
//     configuration can override the global default (leave a profile field empty to inherit global,
//     and if global is unset use the built-in default);
//   - circuit-breaker / intent replay are process-wide, with only one global instance.
//
// The global policy reads one settings row from the DB once; call sites are all low-frequency paths
// (building the provider, work wrap-up, saving config), so an extra cache layer isn't worth it; the
// circuit-breaker parameter is the exception — it is read on every failure path, so applyRetryPolicy
// pushes it to the Registry to store.

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
		// The counts keep the original "0=default / negative=off" semantics here: the SDK's MaxRetries /
		// EmptyResponseRetries are fully isomorphic, so just let it parse them itself.
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

// Defaults for circuit-breaker (polling cooldown), consistent with llmpool's built-ins — only
// overridden here when the user configured a value. See engine.go's modelErrorRetries /
// modelErrorRetryBackoff for the intent replay defaults.

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
// retries": the two are two means to the same thing. The SDK layer handles "not a single content
// block", by re-sending the same request as-is; here we handle "only thinking, no body and no tool",
// by appending an instruction for the model to continue with its existing thinking (re-sending as-is
// is meaningless for this empty loop that is determined by context shape). The emptiness criteria
// differ because the SDK uses "whether any event was yielded", while thinking increments are themselves
// events — but when the user configures "empty response retries" they mean "if the model produced no
// substantive content, try again", and sharing one count across the two layers is what matches that
// intent.
//
// Reads the global policy rather than a profile's override: a run may switch profiles mid-way due to
// failover, and this is a total gate for the whole intent, so it should not change with the endpoint.
// Semantics are isomorphic with the SDK's emptyRetries(): 0 = default defaultEmptyTurnNudges; -1
// (negative) = disable empty-turn continuation; >0 = use that value.
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
