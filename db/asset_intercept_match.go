package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Asset-intercept rule match/execution layer. asset_intercept.go
// handles rule storage; this file is responsible for matching
// "target asset" domain/IP/URL values against the enabled rules.
// Called by agent tools (add_intent, insert_assets) before intents
// are dispatched or assets inserted; a match rejects the call.

// AssetInterceptKindLabel returns the human-readable label for a
// kind, used in messages shown to the agent.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "domain (exact)"
	case "exact_ip":
		return "IP (exact)"
	case "exact_url":
		return "URL (exact)"
	case "fuzzy_domain":
		return "domain (fuzzy)"
	case "fuzzy_ip":
		return "IP (fuzzy)"
	case "fuzzy_url":
		return "URL (fuzzy)"
	case "cidr":
		return "CIDR range"
	}
	return kind
}

// Reason returns a human-readable hit reason, in the form:
// "matched asset intercept rule [domain (fuzzy): .gov.cn] (note)".
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("matched asset intercept rule [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += " (" + note + ")"
	}
	return s
}

// matchOne checks whether a single enabled rule matches the given
// domain/IP/URL candidate strings, and returns the specific value
// that matched.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules returns the first enabled rule that
// matches the given domain/IP/URL candidate strings, along with
// the value that matched. Used by insert_assets to match the raw
// input (an assetInputItem not yet persisted).
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates extracts the domain/IP/URL candidate strings
// from a persisted asset for intercept matching. The URL's host is
// split out and categorized, so service-class assets that only
// have a URL can still be hit by domain/IP rules.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel returns a short identifier for an asset, used in
// messages shown to the agent.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("asset#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule reports whether at least one rule in the set is
// enabled.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision is the verdict of the
// "block-then-allow" gate on a set of candidate strings.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // rejection reason (no asset identifier); empty when Allowed=true
}

// EvaluateAssetGate runs the task-level gate decision:
//  1. Match any enabled blockRules → reject (intercept reason).
//  2. Otherwise, if allowRules has any enabled entries and none
//     match → reject (not in the allowed scope).
//  3. Otherwise, allow.
//
// When allowRules is empty / has no enabled entries, the
// allow-gate is inactive (the white-list is not in effect and
// everything is allowed), so that "no allow rule configured" does
// not block every asset.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "not in the task's allowed (white-list) scope — testing is not permitted"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit describes one asset rejected by the gate
// (either an intercept hit or outside the allow scope).
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // human-readable reason
}

// Describe returns a human-readable description: asset info +
// reason.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules is a pass-through to *DB's same-named
// method, so callers holding only an AssetStore (e.g. agent
// tools) can also read the rules.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept loads assets by id, runs the
// "block-then-allow" gate on each, and returns every rejected
// asset. Block rules = global ∪ task-level block; allow rules =
// task-level allow (this task only). Returns immediately when ids
// is empty. Uses global GetByIDs (not task-scoped) so intercept is
// not weakened by scope filtering.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// No block rules and no enabled allow rules → nothing to
	// check, allow everything.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
