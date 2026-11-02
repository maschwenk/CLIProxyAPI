package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetAwareSelector narrows eligible Claude credentials by their observed quota
// deadlines. Wrap it inside SessionAffinitySelector so it only affects new bindings
// and failover. It never performs I/O or changes cooldowns or credential priorities.
type ResetAwareSelector struct {
	Fallback Selector
	now      func() time.Time
}

type quotaResetRank struct {
	weekly  time.Time
	session time.Time
}

func isWeightedSelector(selector Selector) bool {
	if reset, ok := selector.(*ResetAwareSelector); ok {
		selector = reset.Fallback
	}
	_, weighted := selector.(*WeightedRoundRobinSelector)
	return weighted
}

func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	fallback := s.Fallback
	if fallback == nil {
		fallback = &FillFirstSelector{}
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
	case "mixed":
		// HTTP requests use mixed scheduling even for a Claude-only pool.
		for _, candidate := range auths {
			if candidate != nil && !strings.EqualFold(strings.TrimSpace(candidate.Provider), "claude") {
				return fallback.Pick(ctx, provider, model, opts, auths)
			}
		}
	default:
		return fallback.Pick(ctx, provider, model, opts, auths)
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if isWeightedSelector(fallback) {
		auths = positiveWeightAuths(auths)
	}
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	var best quotaResetRank
	var preferred []*Auth
	known := 0
	for _, candidate := range available {
		rank := claudeQuotaResetRank(candidate, model, now)
		if rank.weekly.IsZero() && rank.session.IsZero() {
			continue
		}
		known++
		order := compareQuotaResetRanks(rank, best)
		if len(preferred) == 0 || order < 0 {
			best = rank
			preferred = append(preferred[:0], candidate)
		} else if order == 0 {
			preferred = append(preferred, candidate)
		}
	}
	if len(preferred) == 0 {
		preferred = available
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Availability and priority were checked once above (or by Manager). In
	// particular, do not recheck a route alias as if it were an upstream model.
	ctx = context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true)
	selected, errPick := fallback.Pick(ctx, provider, model, opts, preferred)
	if errPick == nil {
		selectorLogEntry(ctx).
			WithField("weekly_reset", best.weekly).
			WithField("session_reset", best.session).
			WithField("known_accounts", known).
			WithField("eligible_accounts", len(available)).
			Info("reset-aware: selected using observed quota deadlines")
	}
	return selected, errPick
}

func claudeQuotaResetRank(auth *Auth, model string, now time.Time) quotaResetRank {
	rank := quotaResetRank{
		weekly:  claudeWindowReset(auth.Quota, "7d", now, 7*24*time.Hour),
		session: claudeWindowReset(auth.Quota, "5h", now, 5*time.Hour),
	}
	if strings.HasPrefix(strings.ToLower(canonicalModelKey(model)), "claude-fable-") {
		quota := auth.Quota
		if state := auth.ModelStates[canonicalModelKey(model)]; state != nil {
			key := http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d_oi-Reset")
			if quota.Signals[key] == "" || state.Quota.ObservedAt.After(quota.ObservedAt) {
				quota = state.Quota
			}
		}
		if reset := claudeWindowReset(quota, "7d_oi", now, 7*24*time.Hour); !reset.IsZero() && (rank.weekly.IsZero() || reset.Before(rank.weekly)) {
			rank.weekly = reset
		}
	}
	return rank
}

func claudeWindowReset(quota QuotaState, window string, now time.Time, maxWindow time.Duration) time.Time {
	if quota.ObservedAt.IsZero() || quota.ObservedAt.After(now) {
		return time.Time{}
	}
	prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
	value := func(suffix string) string { return quota.Signals[http.CanonicalHeaderKey(prefix+suffix)] }
	if strings.EqualFold(strings.TrimSpace(value("Status")), "rejected") {
		return time.Time{}
	}
	if raw := value("Utilization"); raw != "" {
		used, errParse := strconv.ParseFloat(raw, 64)
		if errParse != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used >= 1 {
			return time.Time{}
		}
	}
	seconds, errParse := strconv.ParseInt(strings.TrimSpace(value("Reset")), 10, 64)
	if errParse != nil || seconds <= now.Unix() || seconds > now.Add(maxWindow+time.Hour).Unix() {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func compareQuotaResetRanks(a, b quotaResetRank) int {
	if order := compareKnownReset(a.weekly, b.weekly); order != 0 {
		return order
	}
	return compareKnownReset(a.session, b.session)
}

func compareKnownReset(a, b time.Time) int {
	if a.Equal(b) {
		return 0
	}
	if a.IsZero() {
		return 1
	}
	if b.IsZero() {
		return -1
	}
	return a.Compare(b)
}
