package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetTestAuth(id string, now time.Time, weekly, session time.Duration) *Auth {
	headers := http.Header{}
	for window, offset := range map[string]time.Duration{"7d": weekly, "5h": session} {
		if offset != 0 {
			headers.Set("Anthropic-Ratelimit-Unified-"+window+"-Reset", strconv.FormatInt(now.Add(offset).Unix(), 10))
			headers.Set("Anthropic-Ratelimit-Unified-"+window+"-Utilization", "0.5")
		}
	}
	auth := &Auth{ID: id, Provider: "claude", Status: StatusActive}
	auth.Quota.ObserveResponseHeadersForProvider("claude", headers, now)
	return auth
}

func TestResetAwareSelectionPrefersWeeklyDeadlineWithoutBypassingEligibility(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	const model = "claude-opus-5-5"
	for _, tc := range []struct {
		name   string
		change func(a, b *Auth)
		want   string
	}{
		{name: "weekly reset wins over earlier five-hour reset", want: "b"},
		{name: "five-hour reset breaks a weekly tie", change: func(a, b *Auth) { b.Quota = resetTestAuth("b", now, 4*24*time.Hour, time.Hour).Quota }, want: "b"},
		{name: "known deadline wins over unknown", change: func(a, b *Auth) { a.Quota = QuotaState{} }, want: "b"},
		{name: "unknown deadlines retain deterministic fallback", change: func(a, b *Auth) { a.Quota, b.Quota = QuotaState{}, QuotaState{} }, want: "a"},
		{name: "disabled account cannot win", change: func(a, b *Auth) { b.Disabled = true }, want: "a"},
		{name: "model cooldown cannot win", change: func(a, b *Auth) {
			b.ModelStates = map[string]*ModelState{model: {Unavailable: true, NextRetryAfter: now.Add(time.Hour)}}
		}, want: "a"},
		{name: "explicit priority remains authoritative", change: func(a, b *Auth) { a.Attributes = map[string]string{"priority": "1"} }, want: "a"},
		{name: "exhausted observation does not earn preference", change: func(a, b *Auth) {
			b.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Utilization")] = "1"
		}, want: "a"},
		{name: "expired weekly observation does not earn preference", change: func(a, b *Auth) { b.Quota = resetTestAuth("b", now, -time.Second, time.Hour).Quota }, want: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := resetTestAuth("a", now, 4*24*time.Hour, 2*time.Hour)
			b := resetTestAuth("b", now, 24*time.Hour, 4*time.Hour)
			if tc.change != nil {
				tc.change(a, b)
			}
			selector := &ResetAwareSelector{Fallback: &FillFirstSelector{}, now: func() time.Time { return now }}
			got, errPick := selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, []*Auth{b, a})
			if errPick != nil || got == nil || got.ID != tc.want {
				t.Fatalf("selected %v, err %v; want account %s", got, errPick, tc.want)
			}
		})
	}
}

func TestResetAwareSelectionScopesFableWindowAndKeepsItsObservation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	a := resetTestAuth("a", now, 2*24*time.Hour, time.Hour)
	b := resetTestAuth("b", now, 4*24*time.Hour, time.Hour)
	fableQuota := QuotaState{}
	fableQuota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-7d_oi-Reset": {strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10)},
	}, now.Add(-time.Minute))
	b.ModelStates = map[string]*ModelState{"claude-fable-5-1": {Quota: fableQuota}}
	selector := &ResetAwareSelector{now: func() time.Time { return now }}
	for model, want := range map[string]string{"claude-opus-5-5": "a", "claude-fable-5-1(max)": "b"} {
		got, errPick := selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, []*Auth{a, b})
		if errPick != nil || got == nil || got.ID != want {
			t.Fatalf("model %s selected %v, err %v; want %s", model, got, errPick, want)
		}
	}
}

func TestResetAwareSelectionRejectsInvalidWindowObservations(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetKey := http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Reset")
	for _, raw := range []string{"", "NaN", "1e20", "9223372036854775808", "-1", strconv.FormatInt(now.Unix(), 10), strconv.FormatInt(now.Add(9*24*time.Hour).Unix(), 10)} {
		t.Run(raw, func(t *testing.T) {
			quota := QuotaState{ObservedAt: now, Signals: map[string]string{resetKey: raw}}
			if got := claudeWindowReset(quota, "7d", now, 7*24*time.Hour); !got.IsZero() {
				t.Fatalf("invalid reset %q accepted as %v", raw, got)
			}
		})
	}
	for _, utilization := range []string{"NaN", "+Inf", "-0.1", "1", "2", "invalid"} {
		quota := resetTestAuth("a", now, time.Hour, 0).Quota
		quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Utilization")] = utilization
		if got := claudeWindowReset(quota, "7d", now, 7*24*time.Hour); !got.IsZero() {
			t.Fatalf("invalid/exhausted utilization %q received preference", utilization)
		}
	}
}

func TestResetAwareSelectionRetainsConfiguredFallbackAndOtherProviders(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	auths := []*Auth{resetTestAuth("a", now, time.Hour, 0), resetTestAuth("b", now, time.Hour, 0)}
	for _, provider := range []string{"claude", "codex", "mixed"} {
		selector := &ResetAwareSelector{Fallback: &RoundRobinSelector{}, now: func() time.Time { return now }}
		for _, want := range []string{"a", "b", "a", "b"} {
			got, errPick := selector.Pick(context.Background(), provider, "model", cliproxyexecutor.Options{}, auths)
			if errPick != nil || got == nil || got.ID != want {
				t.Fatalf("provider %s selected %v, err %v; want %s", provider, got, errPick, want)
			}
		}
	}
	auths[0].Attributes = map[string]string{AttributeWeight: "0"}
	selector := &ResetAwareSelector{Fallback: &WeightedRoundRobinSelector{}, now: func() time.Time { return now }}
	got, errPick := selector.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
	if errPick != nil || got == nil || got.ID != "b" {
		t.Fatalf("zero-weight credential was not excluded: %v, %v", got, errPick)
	}
	a := &Auth{ID: "a", Provider: "codex", Status: StatusActive}
	b := resetTestAuth("b", now, time.Hour, 0)
	selector = &ResetAwareSelector{Fallback: &FillFirstSelector{}, now: func() time.Time { return now }}
	got, errPick = selector.Pick(context.Background(), "mixed", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	if errPick != nil || got == nil || got.ID != "a" {
		t.Fatalf("mixed-provider routing changed: %v, %v", got, errPick)
	}
}

func TestResetAwareManagerKeepsSessionsPinnedAndFailsOverOnQuota(t *testing.T) {
	const model = "claude-opus-5-5"
	ctx := WithSkipPersist(context.Background())
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &ResetAwareSelector{Fallback: &FillFirstSelector{}}, TTL: time.Hour, RebindOn: SessionAffinityRebindOnQuota,
	})
	t.Cleanup(affinity.Stop)
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	for _, id := range []string{"reset-test-a", "reset-test-b"} {
		if _, errRegister := manager.Register(ctx, &Auth{ID: id, Provider: "claude", Status: StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	observe := func(id string, reset time.Duration) {
		headersCtx := logging.WithResponseHeadersHolder(ctx)
		logging.SetResponseHeaders(headersCtx, http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset": {strconv.FormatInt(time.Now().Add(reset).Unix(), 10)},
		})
		manager.MarkResult(headersCtx, Result{AuthID: id, Provider: "claude", Model: model, Success: true})
	}
	options := func(session string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: session}}
	}
	pick := func(opts cliproxyexecutor.Options, want string) {
		t.Helper()
		got, _, _, errPick := manager.pickNextMixed(ctx, []string{"claude"}, model, opts, nil)
		if errPick != nil || got == nil || got.ID != want {
			t.Fatalf("selected %v, err %v; want %s", got, errPick, want)
		}
	}
	observe("reset-test-a", 4*24*time.Hour)
	observe("reset-test-b", 24*time.Hour)
	original := options("reset-test-original")
	pick(original, "reset-test-b")
	observe("reset-test-a", time.Hour)
	pick(original, "reset-test-b")
	fresh := options("reset-test-fresh")
	pick(fresh, "reset-test-a")
	retry := time.Hour
	manager.MarkResult(ctx, Result{AuthID: "reset-test-a", Provider: "claude", Model: model, Options: fresh,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}, RetryAfter: &retry})
	pick(fresh, "reset-test-b")
	pick(original, "reset-test-b")
}
