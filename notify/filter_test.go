package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Malformed JSON, empty input, and fields with incorrect types must all fall
	// back to the zero-value Filter (no filtering). This favors extra notifications
	// over silently dropping high-severity ones due to a config typo.
	cases := []struct {
		name string
		raw  string
	}{
		{"empty input", ""},
		{"invalid JSON", `{not json`},
		{"truncated JSON", `{"min_severity":`},
		{"type mismatch", `{"min_severity": 123, "task_ids": "abc"}`},
		{"top-level array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("malformed config should fall back to a zero-value Filter; got %+v", f)
			}
			// The zero-value Filter must match any event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("zero-value Filter should match every event")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// Unknown severity has rank 0 and should be blocked by any non-empty threshold.
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: want %v, got %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"empty scope means unrestricted", Filter{}, true},
		{"task matches", Filter{TaskIDs: []int64{7}}, true},
		{"task does not match", Filter{TaskIDs: []int64{8}}, false},
		{"multiple tasks include a match", Filter{TaskIDs: []int64{8, 7}}, true},
		{"assets overlap", Filter{AssetIDs: []int64{20, 99}}, true},
		{"assets do not overlap", Filter{AssetIDs: []int64{99}}, false},
		{"task and asset both match", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"task matches but asset does not", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("want %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"empty include matches all", Filter{}, "any type", true},
		{"include matches", Filter{VulnClassInclude: []string{"SQL"}}, "SQL injection", true},
		{"include does not match", Filter{VulnClassInclude: []string{"command execution"}}, "SQL injection", false},
		{"any include term matches", Filter{VulnClassInclude: []string{"command execution", "SQL"}}, "SQL injection", true},
		{"case-insensitive matching", Filter{VulnClassInclude: []string{"sql"}}, "SQL injection", true},
		{"matching exclude term rejects", Filter{VulnClassExclude: []string{"information disclosure"}}, "information disclosure", false},
		{"non-matching exclude term allows", Filter{VulnClassExclude: []string{"information disclosure"}}, "SQL injection", true},
		// Exclude takes precedence when both terms match.
		{"exclude takes precedence", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"injection"},
		}, "SQL injection", false},
		// Whitespace-only terms must be ignored rather than matching every string containing spaces.
		{"blank terms are ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("want %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Disabled by default: most users want new findings, not a stream of status changes.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("status-change events should be skipped when the option is disabled")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("status-change events should match when on_status_change is enabled")
	}
	// Finding-created events are unaffected by on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("finding-created events should not depend on on_status_change")
	}
}
