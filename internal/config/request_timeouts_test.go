package config

import (
	"testing"
	"time"
)

func intPtrValue(v int) *int { return &v }

func TestRequestTimeoutsResolveDefaults(t *testing.T) {
	cfg := &RequestTimeoutConfig{}
	got := cfg.Resolve("gpt-5", []string{"openai"}, []string{"codex"})
	want := ResolvedRequestTimeouts{
		StreamFirstByte: 60 * time.Second,
		StreamIdle:      300 * time.Second,
		NonStream:       600 * time.Second,
	}
	if got != want {
		t.Fatalf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestRequestTimeoutsResolveGlobals(t *testing.T) {
	cfg := &RequestTimeoutConfig{
		StreamFirstByteSeconds: 90,
		StreamIdleSeconds:      intPtrValue(0),
		NonStreamSeconds:       1200,
	}
	got := cfg.Resolve("", nil, nil)
	want := ResolvedRequestTimeouts{
		StreamFirstByte: 90 * time.Second,
		StreamIdle:      0,
		NonStream:       1200 * time.Second,
	}
	if got != want {
		t.Fatalf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestRequestTimeoutsResolveRules(t *testing.T) {
	cfg := &RequestTimeoutConfig{
		StreamFirstByteSeconds: 60,
		StreamIdleSeconds:      intPtrValue(300),
		NonStreamSeconds:       600,
		Rules: []RequestTimeoutRule{
			{
				Models:                 []string{"claude-*"},
				Protocols:              []string{"claude"},
				StreamFirstByteSeconds: intPtrValue(90),
				StreamIdleSeconds:      intPtrValue(0),
			},
			{
				Providers:        []string{"codex"},
				NonStreamSeconds: intPtrValue(1200),
			},
		},
	}

	claude := cfg.Resolve("claude-sonnet-4-5", []string{"claude"}, []string{"claude"})
	if claude.StreamFirstByte != 90*time.Second {
		t.Errorf("claude StreamFirstByte = %s, want 90s", claude.StreamFirstByte)
	}
	if claude.StreamIdle != 0 {
		t.Errorf("claude StreamIdle = %s, want 0 (disabled by rule)", claude.StreamIdle)
	}
	if claude.NonStream != 600*time.Second {
		t.Errorf("claude NonStream = %s, want global 600s", claude.NonStream)
	}

	codex := cfg.Resolve("gpt-5", []string{"openai"}, []string{"codex"})
	if codex.NonStream != 1200*time.Second {
		t.Errorf("codex NonStream = %s, want 1200s", codex.NonStream)
	}
	if codex.StreamFirstByte != 60*time.Second || codex.StreamIdle != 300*time.Second {
		t.Errorf("codex stream timeouts = %+v, want globals", codex)
	}

	// The model does not match but the protocol does: the first rule must not apply.
	other := cfg.Resolve("gpt-5", []string{"claude"}, []string{"codex"})
	if other.StreamFirstByte != 60*time.Second {
		t.Errorf("gpt-5/claude StreamFirstByte = %s, want global 60s", other.StreamFirstByte)
	}
	if other.NonStream != 1200*time.Second {
		t.Errorf("gpt-5/claude NonStream = %s, want rule 1200s", other.NonStream)
	}
}

func TestRequestTimeoutsResolveFirstRuleWins(t *testing.T) {
	cfg := &RequestTimeoutConfig{
		Rules: []RequestTimeoutRule{
			{Models: []string{"*"}, NonStreamSeconds: intPtrValue(120)},
			{Models: []string{"gpt-*"}, NonStreamSeconds: intPtrValue(900)},
		},
	}
	got := cfg.Resolve("gpt-5", nil, nil)
	if got.NonStream != 120*time.Second {
		t.Fatalf("NonStream = %s, want first matching rule 120s", got.NonStream)
	}
}

func TestRequestTimeoutRuleRequiresSelector(t *testing.T) {
	cfg := &RequestTimeoutConfig{
		Rules: []RequestTimeoutRule{
			{NonStreamSeconds: intPtrValue(900)},
		},
	}
	got := cfg.Resolve("gpt-5", nil, nil)
	if got.NonStream != 600*time.Second {
		t.Fatalf("NonStream = %s, want default; a selector-less rule must not match", got.NonStream)
	}
}

func TestSanitizeRequestTimeoutsClamps(t *testing.T) {
	cfg := &Config{}
	cfg.Timeouts = RequestTimeoutConfig{
		StreamFirstByteSeconds: 500,
		StreamIdleSeconds:      intPtrValue(5),
		NonStreamSeconds:       5,
		Rules: []RequestTimeoutRule{
			{Models: []string{"claude-*"}, NonStreamSeconds: intPtrValue(99999), StreamIdleSeconds: intPtrValue(-3)},
			{Models: []string{"gpt-*"}},
			{},
		},
	}
	cfg.SanitizeRequestTimeouts()

	if cfg.Timeouts.StreamFirstByteSeconds != MaxStreamFirstByteSeconds {
		t.Errorf("StreamFirstByteSeconds = %d, want %d", cfg.Timeouts.StreamFirstByteSeconds, MaxStreamFirstByteSeconds)
	}
	if cfg.Timeouts.StreamIdleSeconds == nil || *cfg.Timeouts.StreamIdleSeconds != MinStreamIdleSeconds {
		t.Errorf("StreamIdleSeconds = %v, want %d", cfg.Timeouts.StreamIdleSeconds, MinStreamIdleSeconds)
	}
	if cfg.Timeouts.NonStreamSeconds != MinNonStreamSeconds {
		t.Errorf("NonStreamSeconds = %d, want %d", cfg.Timeouts.NonStreamSeconds, MinNonStreamSeconds)
	}
	if len(cfg.Timeouts.Rules) != 2 {
		t.Fatalf("rules = %d, want 2 (only the selector-less rule dropped)", len(cfg.Timeouts.Rules))
	}
	rule := cfg.Timeouts.Rules[0]
	if rule.NonStreamSeconds == nil || *rule.NonStreamSeconds != MaxNonStreamSeconds {
		t.Errorf("rule NonStreamSeconds = %v, want %d", rule.NonStreamSeconds, MaxNonStreamSeconds)
	}
	if rule.StreamIdleSeconds == nil || *rule.StreamIdleSeconds != 0 {
		t.Errorf("rule StreamIdleSeconds = %v, want 0", rule.StreamIdleSeconds)
	}
}

func TestSanitizeRequestTimeoutsKeepsDisabledIdle(t *testing.T) {
	cfg := &Config{}
	cfg.Timeouts.StreamIdleSeconds = intPtrValue(0)
	cfg.SanitizeRequestTimeouts()
	if cfg.Timeouts.StreamIdleSeconds == nil || *cfg.Timeouts.StreamIdleSeconds != 0 {
		t.Fatalf("StreamIdleSeconds = %v, want explicit 0 preserved", cfg.Timeouts.StreamIdleSeconds)
	}
	if got := cfg.Timeouts.Resolve("", nil, nil).StreamIdle; got != 0 {
		t.Fatalf("resolved StreamIdle = %s, want 0", got)
	}
}

func TestMatchWildcardPattern(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"*", "anything", true},
		{"claude-*", "claude-sonnet-4-5", true},
		{"claude-*", "gpt-5", false},
		{"*-5", "gpt-5", true},
		{"gemini-*-pro", "gemini-2.5-pro", true},
		{"", "gpt-5", false},
	}
	for _, tc := range cases {
		if got := matchWildcardPattern(tc.pattern, tc.value); got != tc.want {
			t.Errorf("matchWildcardPattern(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}
