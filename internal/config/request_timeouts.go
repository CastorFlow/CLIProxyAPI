package config

import (
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Request timeout defaults and bounds. Configured values outside the bounds are
// clamped on load so a management UI cannot put the proxy into an unusable state.
const (
	// DefaultStreamFirstByteSeconds bounds how long a streaming request may wait
	// for its first upstream data chunk.
	DefaultStreamFirstByteSeconds = 60
	// MinStreamFirstByteSeconds is the smallest accepted first-byte bound.
	MinStreamFirstByteSeconds = 1
	// MaxStreamFirstByteSeconds is the largest accepted first-byte bound.
	MaxStreamFirstByteSeconds = 120

	// DefaultStreamIdleSeconds bounds the gap between two streaming data chunks.
	// Zero disables the bound.
	DefaultStreamIdleSeconds = 300
	// MinStreamIdleSeconds is the smallest accepted enabled idle bound.
	MinStreamIdleSeconds = 60
	// MaxStreamIdleSeconds is the largest accepted idle bound.
	MaxStreamIdleSeconds = 600

	// DefaultNonStreamSeconds bounds a whole non-streaming request.
	DefaultNonStreamSeconds = 600
	// MinNonStreamSeconds is the smallest accepted non-streaming bound.
	MinNonStreamSeconds = 60
	// MaxNonStreamSeconds is the largest accepted non-streaming bound.
	MaxNonStreamSeconds = 1200
)

// RequestTimeoutConfig configures upstream request timeouts. Values are global
// defaults; Rules may override them for matching models, protocols, or providers.
type RequestTimeoutConfig struct {
	// StreamFirstByteSeconds is the maximum time to wait for the first data chunk
	// of a streaming request. Range 1-120. Zero or unset uses the default (60).
	StreamFirstByteSeconds int `yaml:"stream-first-byte-seconds,omitempty" json:"stream-first-byte-seconds,omitempty"`

	// StreamIdleSeconds is the maximum gap between data chunks of a streaming
	// request. Range 60-600. Zero disables the bound; unset uses the default (300).
	StreamIdleSeconds *int `yaml:"stream-idle-seconds,omitempty" json:"stream-idle-seconds,omitempty"`

	// NonStreamSeconds is the total time allowed for a non-streaming request.
	// Range 60-1200. Zero or unset uses the default (600).
	NonStreamSeconds int `yaml:"nonstream-seconds,omitempty" json:"nonstream-seconds,omitempty"`

	// Rules override the global values for matching requests. The first matching
	// rule wins; fields it omits fall back to the global values.
	Rules []RequestTimeoutRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// RequestTimeoutRule overrides request timeouts for matching requests.
type RequestTimeoutRule struct {
	// Models lists model name patterns (wildcards supported, e.g. "claude-*").
	Models []string `yaml:"models,omitempty" json:"models,omitempty"`
	// Protocols restricts the rule to inbound/outbound protocol formats
	// (e.g. "openai", "claude", "gemini", "codex", "antigravity").
	Protocols []string `yaml:"protocols,omitempty" json:"protocols,omitempty"`
	// Providers restricts the rule to upstream providers (e.g. "claude", "codex").
	Providers []string `yaml:"providers,omitempty" json:"providers,omitempty"`

	StreamFirstByteSeconds *int `yaml:"stream-first-byte-seconds,omitempty" json:"stream-first-byte-seconds,omitempty"`
	StreamIdleSeconds      *int `yaml:"stream-idle-seconds,omitempty" json:"stream-idle-seconds,omitempty"`
	NonStreamSeconds       *int `yaml:"nonstream-seconds,omitempty" json:"nonstream-seconds,omitempty"`
}

// ResolvedRequestTimeouts is the effective timeout set for one request.
// A zero duration disables that bound.
type ResolvedRequestTimeouts struct {
	StreamFirstByte time.Duration
	StreamIdle      time.Duration
	NonStream       time.Duration
}

// Resolve returns the effective timeouts for a request. The first matching rule
// wins; unset rule fields fall back to the global values.
func (c *RequestTimeoutConfig) Resolve(model string, protocols, providers []string) ResolvedRequestTimeouts {
	first := DefaultStreamFirstByteSeconds
	idle := DefaultStreamIdleSeconds
	nonStream := DefaultNonStreamSeconds
	if c != nil {
		if c.StreamFirstByteSeconds > 0 {
			first = c.StreamFirstByteSeconds
		}
		if c.StreamIdleSeconds != nil {
			idle = *c.StreamIdleSeconds
		}
		if c.NonStreamSeconds > 0 {
			nonStream = c.NonStreamSeconds
		}
		for i := range c.Rules {
			rule := &c.Rules[i]
			if !rule.matches(model, protocols, providers) {
				continue
			}
			if rule.StreamFirstByteSeconds != nil && *rule.StreamFirstByteSeconds > 0 {
				first = *rule.StreamFirstByteSeconds
			}
			if rule.StreamIdleSeconds != nil {
				idle = *rule.StreamIdleSeconds
			}
			if rule.NonStreamSeconds != nil && *rule.NonStreamSeconds > 0 {
				nonStream = *rule.NonStreamSeconds
			}
			break
		}
	}
	return ResolvedRequestTimeouts{
		StreamFirstByte: time.Duration(first) * time.Second,
		StreamIdle:      time.Duration(idle) * time.Second,
		NonStream:       time.Duration(nonStream) * time.Second,
	}
}

func (r *RequestTimeoutRule) matches(model string, protocols, providers []string) bool {
	if r == nil {
		return false
	}
	hasSelector := false
	if len(r.Models) > 0 {
		hasSelector = true
		matched := false
		for _, pattern := range r.Models {
			if matchWildcardPattern(pattern, model) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(r.Protocols) > 0 {
		hasSelector = true
		if !anyStringOverlap(r.Protocols, protocols) {
			return false
		}
	}
	if len(r.Providers) > 0 {
		hasSelector = true
		if !anyStringOverlap(r.Providers, providers) {
			return false
		}
	}
	return hasSelector
}

// matchWildcardPattern performs glob-style matching where '*' matches any run of characters.
func matchWildcardPattern(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	value = strings.TrimSpace(value)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	pi, vi := 0, 0
	starIdx, matchIdx := -1, 0
	for vi < len(value) {
		if pi < len(pattern) && pattern[pi] == value[vi] {
			pi++
			vi++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = vi
			pi++
			continue
		}
		if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			vi = matchIdx
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

func anyStringOverlap(patterns, values []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if strings.EqualFold(pattern, value) || matchWildcardPattern(strings.ToLower(pattern), strings.ToLower(value)) {
				return true
			}
		}
	}
	return false
}

// SanitizeRequestTimeouts clamps configured timeout values into their accepted
// ranges and drops rules that select nothing. Out-of-range values are clamped
// rather than rejected so a management UI cannot lock the proxy out of service.
func (cfg *Config) SanitizeRequestTimeouts() {
	if cfg == nil {
		return
	}
	t := &cfg.Timeouts
	if t.StreamFirstByteSeconds != 0 {
		t.StreamFirstByteSeconds = clampTimeoutSeconds(t.StreamFirstByteSeconds, MinStreamFirstByteSeconds, MaxStreamFirstByteSeconds, "requests.timeouts.stream-first-byte-seconds")
	}
	if t.StreamIdleSeconds != nil {
		clamped := clampStreamIdleSeconds(*t.StreamIdleSeconds, "requests.timeouts.stream-idle-seconds")
		t.StreamIdleSeconds = &clamped
	}
	if t.NonStreamSeconds != 0 {
		t.NonStreamSeconds = clampTimeoutSeconds(t.NonStreamSeconds, MinNonStreamSeconds, MaxNonStreamSeconds, "requests.timeouts.nonstream-seconds")
	}
	if len(t.Rules) == 0 {
		t.Rules = nil
		return
	}
	rules := make([]RequestTimeoutRule, 0, len(t.Rules))
	for i := range t.Rules {
		rule := t.Rules[i]
		rule.Models = normalizePatternList(rule.Models)
		rule.Protocols = normalizePatternList(rule.Protocols)
		rule.Providers = normalizePatternList(rule.Providers)
		if len(rule.Models) == 0 && len(rule.Protocols) == 0 && len(rule.Providers) == 0 {
			log.WithField("rule_index", i+1).Warn("requests.timeouts.rules: dropped rule without models, protocols, or providers")
			continue
		}
		field := func(name string) string { return "requests.timeouts.rules[" + strconv.Itoa(i) + "]." + name }
		if rule.StreamFirstByteSeconds != nil && *rule.StreamFirstByteSeconds != 0 {
			clamped := clampTimeoutSeconds(*rule.StreamFirstByteSeconds, MinStreamFirstByteSeconds, MaxStreamFirstByteSeconds, field("stream-first-byte-seconds"))
			rule.StreamFirstByteSeconds = &clamped
		}
		if rule.StreamIdleSeconds != nil {
			clamped := clampStreamIdleSeconds(*rule.StreamIdleSeconds, field("stream-idle-seconds"))
			rule.StreamIdleSeconds = &clamped
		}
		if rule.NonStreamSeconds != nil && *rule.NonStreamSeconds != 0 {
			clamped := clampTimeoutSeconds(*rule.NonStreamSeconds, MinNonStreamSeconds, MaxNonStreamSeconds, field("nonstream-seconds"))
			rule.NonStreamSeconds = &clamped
		}
		rules = append(rules, rule)
	}
	if len(rules) == 0 {
		t.Rules = nil
		return
	}
	t.Rules = rules
}

func clampStreamIdleSeconds(value int, field string) int {
	if value <= 0 {
		return 0
	}
	return clampTimeoutSeconds(value, MinStreamIdleSeconds, MaxStreamIdleSeconds, field)
}

func clampTimeoutSeconds(value, min, max int, field string) int {
	if value < min {
		log.WithFields(log.Fields{"field": field, "value": value, "min": min}).Warn("request timeout below minimum; clamping")
		return min
	}
	if value > max {
		log.WithFields(log.Fields{"field": field, "value": value, "max": max}).Warn("request timeout above maximum; clamping")
		return max
	}
	return value
}

func normalizePatternList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
