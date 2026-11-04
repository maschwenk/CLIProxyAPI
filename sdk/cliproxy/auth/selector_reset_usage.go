package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// polledClaudeUsage holds quota windows read from Anthropic's OAuth usage
// endpoint, keyed by auth ID. Response headers only describe accounts that served
// traffic through this proxy, so without polling an idle account's deadline stays
// unknown and loses to any account with a known, later deadline. It is
// process-wide because routing selectors are rebuilt on every config reload.
var polledClaudeUsage = struct {
	sync.RWMutex
	byAuth map[string]QuotaState
}{byAuth: map[string]QuotaState{}}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudeUsageResponse struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
	Limits   []struct {
		Kind     string   `json:"kind"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

// RecordClaudeUsage stores the windows from an /api/oauth/usage response body for
// authID, translated into the rate-limit header signals ResetAwareSelector reads.
// The newer of this snapshot and the auth's own header observation wins per window.
func RecordClaudeUsage(authID string, body []byte, observedAt time.Time) (QuotaState, error) {
	var usage claudeUsageResponse
	if errDecode := json.Unmarshal(body, &usage); errDecode != nil {
		return QuotaState{}, fmt.Errorf("decode claude usage: %w", errDecode)
	}
	signals := map[string]string{}
	add := func(window string, percent *float64, resetsAt string) {
		if percent == nil {
			return
		}
		reset, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(resetsAt))
		if errParse != nil {
			return
		}
		prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
		// The usage API reports 15:59:59.99 for a window the headers report as 16:00:00.
		signals[http.CanonicalHeaderKey(prefix+"Reset")] = strconv.FormatInt(reset.Round(time.Second).Unix(), 10)
		signals[http.CanonicalHeaderKey(prefix+"Utilization")] = strconv.FormatFloat(*percent/100, 'f', -1, 64)
	}
	if usage.SevenDay != nil {
		add("7d", usage.SevenDay.Utilization, usage.SevenDay.ResetsAt)
	}
	if usage.FiveHour != nil {
		add("5h", usage.FiveHour.Utilization, usage.FiveHour.ResetsAt)
	}
	for _, limit := range usage.Limits {
		if limit.Kind == "weekly_scoped" && limit.Scope != nil && limit.Scope.Model != nil &&
			strings.EqualFold(strings.TrimSpace(limit.Scope.Model.DisplayName), "fable") {
			add("7d_oi", limit.Percent, limit.ResetsAt)
		}
	}
	quota := QuotaState{ObservedAt: observedAt, Signals: signals}
	polledClaudeUsage.Lock()
	polledClaudeUsage.byAuth[authID] = quota
	polledClaudeUsage.Unlock()
	return quota, nil
}

// RetainClaudeUsage drops polled snapshots for auths that no longer exist.
func RetainClaudeUsage(authIDs map[string]struct{}) {
	polledClaudeUsage.Lock()
	defer polledClaudeUsage.Unlock()
	for id := range polledClaudeUsage.byAuth {
		if _, ok := authIDs[id]; !ok {
			delete(polledClaudeUsage.byAuth, id)
		}
	}
}

func polledClaudeQuota(authID string) QuotaState {
	polledClaudeUsage.RLock()
	defer polledClaudeUsage.RUnlock()
	return polledClaudeUsage.byAuth[authID]
}

// newestWindowQuota returns the most recent snapshot that reported window at all,
// so an older allowed snapshot cannot override a newer rejection.
func newestWindowQuota(window string, candidates ...QuotaState) QuotaState {
	key := http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-" + window + "-Reset")
	var newest QuotaState
	found := false
	for _, candidate := range candidates {
		if candidate.Signals[key] == "" {
			continue
		}
		if !found || candidate.ObservedAt.After(newest.ObservedAt) {
			newest, found = candidate, true
		}
	}
	return newest
}
