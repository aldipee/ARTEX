package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Settlement (wrap-up) prompts are injected by the SDK settlement phase when an agent
// is terminated for reaching MaxTurns or timing out (run_seconds/MaxDuration). They ask
// the agent to persist recognized results first, then provide a brief summary.
//
// Each agent's settlement prompt can be overridden in the admin UI (agents.wrapup_prompt);
// an empty value uses the built-in default. Only the prompt body is editable; disabled
// tools and the settlement turn budget are fixed by code.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in settlement prompts keyed by agent. worker reuses the historical hard-coded
// settleWrapUpPrompt (defined in worker.go); planner and mainagent have their own prompts.
// Custom agents without a match use the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults contains each agent's built-in turn budget for settlement itself
// (the admin UI can override it with a value > 0). All agents get 10 turns to persist
// results; unmatched agents use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "你本轮规划的步数即将用尽——注意只是【这一轮】结束,系统之后仍会随态势变化再次唤醒你继续规划,并非任务终止,你无需在此收束整个规划。请把本轮已经想清楚的结论落地、别让这一轮白跑,但也【不要为了收尾硬凑意图】(本轮 0 个意图仍是完全正常的结果)：(1) 若已判断出【当前就该派发】的探索方向,用一次 add_intent 批量提交(想好的别憋着不发);(2) 对已被某发现/事实证明达成的目标,调 prove_goal 标记 met(别漏判);(3) 若识别出需要分步的串行利用链,用 TodoWrite 记下,便于下次唤醒接着派。做完直接结束本轮,无需输出总结文本。"

const mainAgentWrapUpDefault = "你的步数即将用尽,本次交互就要结束。不要再发起新的探索/操作。请**单独用一句话纯文本**向用户总结当前进展、关键结论,以及建议的下一步。"

const genericWrapUpDefault = "你即将因预算耗尽被终止。请先把已完成但未落库的结果写回,再**单独用一句话纯文本**总结你做了什么、得到哪些关键结论(这句会作为本次运行的结果展示)。"

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-timeout settlement prompts (see docs/任务级超时与收尾设计.md) ----------
//
// These are separate from per-run settlement: per-run means "your run budget is exhausted";
// task timeout means "the entire task is ending." Their instructions can be opposite
// (especially for planner: continue planning after per-run exhaustion, but stop and make
// a final determination at task timeout). Only worker and planner have these prompts.

// WrapupTaskTimeoutOverride / …TurnsOverride provide DB overrides for task-timeout
// settlement prompts and turn budgets (agents.task_timeout_wrapup_prompt / _max_turns;
// worker and planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是你这次 run 的预算，是整场探索到点了）。这是最后机会：(1) 把你已识别但还没写回的内容【全部】落库——新资产 insert_assets、探索结论/事实 record_fact、确认漏洞 report_finding；(2) 不要再启动任何新命令/探测；(3) **最后单独用一句话纯文本**总结你在本意图上的关键结论。"

const plannerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是本轮，是整个任务终止）。请基于当前【全部】事实与发现，做最后一次目标判定：对已被证据证明达成的目标调 prove_goal 标记 met（别漏判）。**不要再生成任何新意图**（此时派意图也不会再被执行）。判定完即收束，无需输出总结文本。"

// TaskTimeoutWrapupDefault returns an agent's built-in task-timeout settlement prompt
// (used by the admin UI to show a placeholder and restore defaults).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // Unconfigured agents (mainagent/chat) return an empty string.
}

// resolveTaskTimeoutWrapup uses a non-empty DB override before the built-in default. An
// empty string means the agent has no task-timeout prompt (anything but worker/planner),
// so callers should fall back to the per-run prompt.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // Default to the per-run turn budget.
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → The task deadline constrained this run: Timeout uses the task-timeout
//     prompt; MaxTurns uses the per-run prompt if turns ran out before the task deadline.
//   - clamped=false → The task deadline is not near: both reasons use the per-run prompt
//     (equivalent to wrapupSettlement).
//
// Pass selection to the harness's PromptByReason so it chooses at settlement time from
// the actual reason, avoiding mismatches when the config is built.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // Fallback; also used for both reasons when not clamped.
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // Task deadline reached.
				harness.ReasonMaxTurns: perRun, // Turns exhausted while time remains.
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
