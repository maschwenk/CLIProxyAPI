# Reset-aware Claude routing

Set `routing.prefer-soonest-reset: true` to prefer eligible Claude accounts whose
weekly allowance resets soonest. Five-hour resets break ties between weekly
deadlines. For Fable requests, its separate observed weekly window also counts.

```yaml
routing:
  strategy: fill-first
  prefer-soonest-reset: true
  session-affinity: true
  session-affinity-ttl: 1h
  session-affinity-rebind-on: quota
```

The preference runs inside session affinity: existing healthy bindings keep their
account even when another account's reset moves ahead. Model availability,
credential priority, disabled accounts, retry exclusions, and quota cooldowns
remain authoritative. The configured strategy resolves ties and supplies the
fallback. This includes the HTTP handler's mixed scheduling path when all
candidates are Claude accounts. Other providers and routes containing multiple
providers retain their existing policy.

Reset information comes from two places: normal upstream response headers, and
Anthropic's OAuth usage endpoint (`/api/oauth/usage`), which the proxy reads for
every Claude OAuth account every five minutes while this setting is on. Polling
covers accounts that have not served a request through this proxy since it
started, and accounts used directly by Claude Code elsewhere. For each window the
newer of the two observations wins, so a fresh rejection from a real response
outranks an older poll. Nothing extra happens on the selection path.

Known future deadlines are preferred over unknown ones; if no eligible account
has usable reset data, selection falls back to the configured strategy. Expired,
malformed, exhausted, and implausibly distant windows do not receive preference.
A failed poll is logged and leaves the previous observation in place.

Logs report how many eligible accounts had usable observations and the preferred
deadlines, plus one `polled claude usage` line per poll with each account's weekly
usage and reset. These observations only rank accounts; they never clear or shorten a
cooldown. Changing routing settings or restarting the process clears in-memory
session bindings, so activate changes between active requests.

Prompt-cache lifetime is independent of routing affinity. Claude Code clients
using a gateway can explicitly request `"promptCacheTtl": "1h"` in their own
settings. Verify `usage.cache_creation.ephemeral_1h_input_tokens` in an actual
Claude Code result; an affinity hit alone does not establish prompt-cache reuse.
