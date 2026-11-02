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

Reset information comes from normal upstream response headers. There is no
additional network request on the selection path. Observations are in memory, so
after a restart an account's deadlines are unknown until it serves a request.
Known future deadlines are preferred over unknown ones; if no eligible account
has usable reset data, selection falls back to the configured strategy. Expired,
malformed, exhausted, and implausibly distant windows do not receive preference.
This cannot compare an unused account's unobserved deadline, and does not poll
accounts that are idle or used outside this proxy.

Logs report how many eligible accounts had usable observations and the preferred
deadlines. These observations only rank accounts; they never clear or shorten a
cooldown. Changing routing settings or restarting the process clears in-memory
session bindings, so activate changes between active requests.

Prompt-cache lifetime is independent of routing affinity. Claude Code clients
using a gateway can explicitly request `"promptCacheTtl": "1h"` in their own
settings. Verify `usage.cache_creation.ephemeral_1h_input_tokens` in an actual
Claude Code result; an affinity hit alone does not establish prompt-cache reuse.
