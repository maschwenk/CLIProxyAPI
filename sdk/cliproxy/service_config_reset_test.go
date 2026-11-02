package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResetAwareConfigReachesSelectorAndKeepsAffinityOutsidePreference(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			contents := fmt.Sprintf("config-version: 8\nrouting:\n  strategy: fill-first\n  prefer-soonest-reset: %t\n  session-affinity: true\n  session-affinity-rebind-on: quota\n", enabled)
			if errWrite := os.WriteFile(path, []byte(contents), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			cfg, errLoad := internalconfig.LoadConfig(path)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
			affinity, ok := selector.(*coreauth.SessionAffinitySelector)
			if !ok {
				t.Fatalf("outer selector = %T, want session affinity", selector)
			}
			t.Cleanup(affinity.Stop)
			auths := []*coreauth.Auth{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}
			observe := func(auth *coreauth.Auth, reset time.Duration) {
				auth.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
					"Anthropic-Ratelimit-Unified-7d-Reset": {strconv.FormatInt(time.Now().Add(reset).Unix(), 10)},
				}, time.Now())
			}
			observe(auths[0], 4*24*time.Hour)
			observe(auths[1], 24*time.Hour)
			want := "a"
			if enabled {
				want = "b"
			}
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"config-reset-session"}}}
			for i := range 2 {
				got, errPick := selector.Pick(context.Background(), "mixed", "claude-opus-5-5", opts, auths)
				if errPick != nil || got == nil || got.ID != want {
					t.Fatalf("pick %d = %v, err %v; want sticky account %s", i, got, errPick, want)
				}
				observe(auths[0], time.Hour)
			}
		})
	}
}
