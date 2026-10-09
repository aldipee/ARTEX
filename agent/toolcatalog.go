package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file makes built-in tools enumerable and overridable through the database:
//   - BuiltinToolSeeds() expands the built-in tool sets for the three execution agents into
//     seed records (key, description, parameter schema, and default agent bindings), which the
//     server idempotently seeds into the tools table at startup.
//   - The ToolResolve hook filters assembled tools by agent, overrides descriptions/schemas,
//     and injects parameter defaults from database rows. Keys and handlers remain in code;
//     the database changes only descriptions and defaults. Call behavior always comes from
//     code, while the database can change what the model sees and default arguments.

// ToolSeed is a seedable snapshot of a built-in tool: key is CoreTool.Name() (tied to the
// handler and read-only in the UI); Desc/Schema come from the code definition; Agents lists
// the agents to which code binds it by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), the immutable primary key.
	Desc   string         // Top-level description; can be overridden in the UI.
	Schema map[string]any // Parameter JSON Schema; structure is read-only, but description/default can be edited in the UI.
	Agents []string       // Agent keys bound by default (worker/planner/mainagent).
}

// builtinToolsByAgent builds each execution agent's domain tool set with a read-only ToolSet
// whose stores are nil. Tool constructors only put closures into Spec and do not dereference
// stores during construction, so nil is safe. These tools are used only to read Name(),
// Description(), and InputSchema(); they are never called here.
//
// Deliberately exclude the SDK's generic actool.DefaultTools() (Read/Write/Edit/MultiEdit/LS/
// Glob/Grep/Bash): every agent always has them, so they are not subject to binding choices;
// their descriptions mostly live in Prompt(), while this catalog covers only Description(),
// which would create a misleading partial override. Without a seed or database row, ToolResolve
// passes them through unchanged. Only ARTEX's own domain tools are managed here.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// Bind set_goals and set_constraints to goals by default so it can persist decomposed goals
		// and extracted constraints. These managed tools are shared with mainagent; their descriptions
		// and schemas can be edited and agent bindings selected in the web UI.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// Bind finding-reporting and asset-management tools to auto by default; other domain tools
		// can be selected in the UI. New databases are seeded here; older ones migrate through
		// seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// Bind asset lookup/insertion, finding reporting/lookup, and company lookup to the standalone
		// pentest agent by default. New databases are seeded here; older ones migrate through
		// seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound tools remain in the catalog (visible in the web UI and available for manual
// agent binding), but bind to no agent by default. ToolResolve drops tools with no bindings for
// every agent, so they require explicit opt-in. They remain in an agent's base tool set (for
// example, goal_met in PlannerTools) so seeds can read their descriptions/schemas and runtime
// resolution can retain them if a user rebinds them.
//
// goal_met bypasses individual prove_goal calls and declares the whole task complete globally.
// It has broad impact and can be misused, and duplicates automatic completion after the last
// goal is marked with prove_goal, so it is not bound by default and must be added manually.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds deduplicates and merges built-in tool sets into one seed list: tools with
// the same name (such as list_assets in several agents) become one record with the union of
// their Agents. Tools in defaultUnbound are forced to have no bindings.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // Listed and available for manual binding, but assigned to no agent by default ([] rather than null, like other tools).
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// Default arguments get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched; only default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
