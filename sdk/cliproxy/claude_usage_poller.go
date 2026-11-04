package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL          = "https://api.anthropic.com/api/oauth/usage"
	claudeUsagePollInterval = 5 * time.Minute
	// A stalled background poll must not block every later poll.
	claudeUsageTimeout = 20 * time.Second
)

// startClaudeUsagePoller feeds prefer-soonest-reset the deadlines of every Claude
// OAuth account, including accounts idle on this proxy or used directly elsewhere.
func (s *Service) startClaudeUsagePoller(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(claudeUsagePollInterval)
		defer ticker.Stop()
		for {
			s.pollClaudeUsage(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) pollClaudeUsage(ctx context.Context) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || !cfg.Routing.PreferSoonestReset || s.coreManager == nil || ctx.Err() != nil {
		return
	}
	present := map[string]struct{}{}
	var summary []string
	for _, auth := range s.coreManager.List() {
		if auth == nil || auth.Disabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
			continue
		}
		present[auth.ID] = struct{}{}
		token, _ := auth.Metadata["access_token"].(string)
		if strings.TrimSpace(token) == "" || strings.TrimSpace(auth.Attributes["api_key"]) != "" {
			continue // API-key credentials have no OAuth usage endpoint.
		}
		quota, errPoll := pollClaudeAccountUsage(ctx, cfg, auth, strings.TrimSpace(token))
		if errPoll != nil {
			log.Warnf("reset-aware: claude usage poll failed | auth=%s err=%v", auth.ID, errPoll)
			continue
		}
		summary = append(summary, auth.ID+" "+describeClaudeWindow(quota, "7d"))
	}
	coreauth.RetainClaudeUsage(present)
	if len(summary) > 0 {
		sort.Strings(summary)
		log.Infof("reset-aware: polled claude usage | %s", strings.Join(summary, "; "))
	}
}

func pollClaudeAccountUsage(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, token string) (coreauth.QuotaState, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if errRequest != nil {
		return coreauth.QuotaState{}, errRequest
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	resp, errDo := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, claudeUsageTimeout).Do(req)
	if errDo != nil {
		return coreauth.QuotaState{}, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("reset-aware: close claude usage body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return coreauth.QuotaState{}, errRead
	}
	if resp.StatusCode != http.StatusOK {
		return coreauth.QuotaState{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return coreauth.RecordClaudeUsage(auth.ID, body, time.Now())
}

func describeClaudeWindow(quota coreauth.QuotaState, window string) string {
	prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
	reset, errParse := strconv.ParseInt(quota.Signals[http.CanonicalHeaderKey(prefix+"Reset")], 10, 64)
	if errParse != nil {
		return window + "=unknown"
	}
	used := quota.Signals[http.CanonicalHeaderKey(prefix+"Utilization")]
	return fmt.Sprintf("%s_used=%s %s_reset=%s", window, used, window, time.Unix(reset, 0).UTC().Format(time.RFC3339))
}
