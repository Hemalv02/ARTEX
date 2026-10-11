package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the JSONB column
// notification_channels.filter: the filter conditions of a channel
// instance. All fields are optional; missing defaults to "no filter"
// — this is the fallback semantics for malformed config, see
// ParseFilter.
type Filter struct {
	// MinSeverity is the lowest severity threshold
	// (low/medium/high/critical); empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs: an empty array means unlimited; non-empty
	// requires the event to intersect with them.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude: empty means accept all; non-empty requires
	// vulnclass to hit any of these keywords.
	// VulnClassExclude: hitting any of these keywords excludes the
	// event (exclude takes priority over include).
	// Matching is case-insensitive substring — safer than regex: a
	// user who misconfigures a regex won't cause the channel to
	// silently fail.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether the channel receives
	// finding-status-change events (only meaningful in realtime
	// mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses the channel filter config.
//
// **It never returns an error.** This is a deliberate design choice:
// when the filter config is malformed, it always falls back to a
// zero-value Filter (= no filter = all hit), because for a
// vulnerability notification system **pushing one more is far better
// than silently missing a high-severity one**. Making a parse failure
// become "don't push" is equivalent to giving the user a channel
// that looks configured but actually pushes nothing — that is the
// worst failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure f stays at the zero value, i.e. no filter.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a legal severity threshold
// (empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate validates the **value-restricted** fields in the filter
// config, called when saving the channel.
//
// Why it must be blocked at write time: Match's judgment for an
// unknown threshold is `rank >= 0`, which is always true — that
// is, a single typo in min_severity ("hgih") will **silently
// disable** the filter and make it "push everything". This is in
// line with this package's "better to over-push than to under-push"
// trade-off (it won't miss), but the consequence is that the user
// thinks they are doing severity-graded push but is actually flooding
// every finding into the group, with no sign that they misconfigured
// it. This kind of "silent degradation" is exactly what should be
// blocked at the entry point.
//
// Note that Validate is only used for the **write** path. The read
// path still uses ParseFilter's lenient semantics, so already
// existing bad values in historical data won't prevent the channel
// from being read out.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("invalid minimum severity %q, allowed: low / medium / high / critical, or leave blank for no limit", f.MinSeverity)
	}
	return nil
}

// Match determines whether an event should be delivered to a channel
// with this filter.
//
// **It never returns an error**, for the same reason as ParseFilter:
// any internal exception is treated as "match". Decision order:
// event type → severity threshold → task/asset scope → vulnclass
// keyword.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are only received by channels that have
	// explicitly turned it on. Off by default, because the vast
	// majority of users expect "push" to mean "new finding found",
	// not a journal of every status transition.
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
	// Exclude takes priority: hitting any exclude keyword eliminates
	// the event, even if it also hits the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan of the small set is sufficient; the scale on
	// both sides is "a few dozen manually selected", where the
	// overhead of building a map exceeds the benefit.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of the keywords
// (case-insensitive).
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
