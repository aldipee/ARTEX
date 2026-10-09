package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract of the notification_channels.filter JSONB column: a channel instance's filter conditions.
// All fields are optional; omitted means "no filtering" — exactly the fallback semantics for malformed config, see
// ParseFilter.
type Filter struct {
	// MinSeverity is the lowest severity threshold (low/medium/high/critical), empty=no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs empty arrays mean no limit; non-empty requires the event to intersect with them.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude empty means accept all; non-empty requires vulnclass to match any of its keywords.
	// VulnClassExclude matching any keyword excludes (exclusion takes priority over inclusion).
	// Matching is case-insensitive substring — safer than regex: a user mis-configuring a regex won't silently break
	// the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives vulnerability status-change events (meaningful only in
	// realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses a channel's filter config.
//
// **Never returns an error.** This is a deliberate design choice: a malformed filter config always degrades to the
// zero-value Filter (= no filtering = matches everything), because for a vulnerability-notification system,
// **pushing one too many is far better than silently missing one high-severity finding**. Turning a parse failure into
// "don't push" would hand the user a channel that looks configured but actually pushes nothing — the worst failure
// mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure f stays zero-valued, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate validates the value-restricted fields of the filter config, called when saving a channel.
//
// Why it must be blocked at write time: Match judges an unknown threshold as `rank >= 0`, which is always true — so a
// one-character typo in min_severity ("hgih") makes the filter **silently degrade** into "push everything". This is
// consistent with the package's "push too many rather than miss any" tradeoff (nothing is missed), but the consequence
// is that the user believes they're doing tiered pushes while actually flooding the group with every vulnerability,
// with no signal that they misconfigured. This kind of "silent degradation" is exactly what should be blocked at the
// entry point.
//
// Note Validate is only used on the **write** path. The read path still uses ParseFilter's lenient semantics, so bad
// values already present in historical data won't make an entire channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("minimum severity %q is invalid; options: low / medium / high / critical, or leave empty for no limit", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel with this filter.
//
// **Never returns an error**, for the same reason as ParseFilter: any internal anomaly is treated as a "match".
// Judgment order: event type → severity threshold → task/asset scope → vulnerability-class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are received only by channels that explicitly enable them. Off by default, because the vast
	// majority of users expect "push" to mean "a new vulnerability was found", not a running ledger of every status
	// transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion first: matching any exclude keyword eliminates the event, even if it also matches the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A small set can be linearly scanned; both sides are on the order of "a few dozen hand-picked items",
	// so building a map costs more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
