# Edge Cases: Non-Idempotent Step Re-execution

Companion to `RETRY_PLAN.md` / `RETRY_IMPLEMENTATION_PLAN.md`. Two scenarios
that came up while designing the idempotency gate, detailed enough to build
and test against, not just gestured at.

Both assume the server-side design already committed to in those two docs:
a restart is requested via `OperationSpec.Restart` (`Step`, `Only`,
`AcknowledgedNonIdempotent`), admitted or denied by a validating webhook
extending GWCP-108269's, and only then executed by the controller. **The
CLI never decides whether a step is safe to re-run — it only reacts to the
server's decision.**

## 1. A non-idempotent step fails, gets force-restarted, and fails again

### Why the gate fires even though the step "just failed"

It's tempting to think a `Failed` step is obviously safe to retry — nothing
happened, so what's there to double up on? That's not guaranteed.
`idempotent: false` means "re-running this is not known to be safe," and a
step can fail *after* performing its side effect: a Job that ran `promote`
successfully but then had its pod evicted before the Job controller marked
it `Succeeded` looks identical, from the `Operation`'s point of view, to one
that failed before doing anything. The model has no way to tell those apart.
That's exactly why design decision #7 in `RETRY_PLAN.md` keeps the
idempotency gate active regardless of phase — "it failed" is not evidence
that nothing happened.

### No standing acknowledgment — every restart is checked again

`AcknowledgedNonIdempotent` lives on `spec.Restart`, a field the controller
clears once processed. There is no field anywhere that remembers "the
operator already approved re-running `promote` on this `Operation`." So:

```
Attempt 1: promote fails
Attempt 2: restart --step promote --force  → denied, then acknowledged, then runs → fails again
Attempt 3: restart --step promote --force  → denied, then acknowledged, then runs → fails again
```

Attempts 2 and 3 each go through the full admission cycle independently.
`--force` makes the CLI skip its own interactive prompt on each call, but it
does not skip the webhook, and it does not let a script "approve once, then
loop retries unattended" — every single invocation still has to pass
`AcknowledgedNonIdempotent` again for the specific denial it gets back. This
is deliberate: the risk this gate exists for (silently redoing a destructive
action) is exactly the risk of an unattended loop retrying it, so removing
the friction on repeat is removing the point of the gate.

### What the record looks like after three attempts

With `ForcedNonIdempotent`/`RequestedBy` (from `RETRY_IMPLEMENTATION_PLAN.md`'s
`OperationStepAttempt`):

```yaml
status:
  attempts: 3
  workflows:
  - stepAttempts:
      promote:
      - attemptNumber: 1
        phase: failed
        message: "connection reset promoting replica"
        forcedNonIdempotent: false        # the original run, nothing to force
      - attemptNumber: 2
        phase: failed
        message: "promote timed out after 30s"
        forcedNonIdempotent: true
        requestedBy: alice
      - attemptNumber: 3
        phase: failed
        message: "promote timed out after 30s"
        forcedNonIdempotent: true
        requestedBy: alice
```

Three failed attempts at the same non-idempotent step, by the same person,
is a legible signal on its own — `kubectl get operation -o yaml` or
`vela operation status` surfaces it without anyone having to reconstruct it
from logs.

### No automatic cap — this is a deliberate open question, not an oversight

Nothing in this design stops attempt 4, 5, or 50. There's no KEP text to
anchor a limit against, and inventing one (e.g. "block after 3 forced
retries") would be a real product decision, not a detail — it changes what
"stuck" means for an on-call runbook. What the design *does* give you for
free, without picking a number: the `NonIdempotentStepForced` Kubernetes
`Event` (emitted via the `Recorder` already sitting unused on `Reconciler`)
fires on every one of these attempts, individually observable. That's
enough to build alerting on top of ("page if this fires 3 times for the same
Operation in an hour") without the controller itself needing to enforce a
cap. Left as an open question in `RETRY_PLAN.md` rather than decided here.

### This doesn't create a lock/deadlock risk

A repeatedly-failing `Operation` still reaches `Failed` each time, which
releases the concurrency lease (the `defer` in `Reconcile` that calls
`releaseLock` on `IsTerminal()`) — so hammering `restart --step promote
--force` doesn't starve some other `Operation` waiting on the same target.
It's a visibility/audit concern, not a concurrency one.

## 2. Using `--force`

### What `--force` actually is, under the server-side design

`--force` is a CLI-only convenience for skipping the interactive
confirmation prompt. It has no effect on the server side and it cannot —
the CLI doesn't know which steps are non-idempotent until it tries and gets
told, so `--force` can't "pre-authorize" anything; it can only decide how
the CLI reacts to a denial that already happened.

Concretely, in `NewOperationRestartCommand`:

| Situation | Without `--force` | With `--force` |
|---|---|---|
| Nothing non-idempotent in scope | Submits, admitted immediately | Submits, admitted immediately — `--force` is a silent no-op |
| Denied for unacknowledged non-idempotent steps | Prints the steps (from `Result.Details.Causes`), prompts for a typed confirmation naming them, resubmits with them acknowledged | Skips the prompt, resubmits with them acknowledged immediately |
| Denied for any other reason (`Operation` `Running`, permission, unknown step) | Surfaces the error | **Still surfaces the error** — `--force` must never swallow this |

That last row matters enough to test explicitly (see both plan docs' test
sections): a `--force` that silently eats a permission-denied error because
it was written to just "retry once more on any denial" would be a real bug,
not a convenience.

### `--force` does not grant a standing exemption

Following directly from edge case 1: passing `--force` on every call in a
script is fine mechanically (each call still gets its own admission check
and its own `RequestedBy` stamp from whoever's credentials the script runs
under), but it is not the same thing as disabling the gate. If the intent is
"this automation should be allowed to redo `promote-replica` without a
human present," that's a different, larger decision — it's granting an
identity standing permission to bypass a safety confirmation — and this
design deliberately doesn't provide a way to do that. It *could* be added
later as its own thing (e.g. an RBAC verb an identity can hold, the same
shape as the permission model's `use`/`invoke` verbs in GWCP-108269), but
that would be a deliberate, separately-reviewed decision, not a side effect
of `--force` existing.

### Non-interactive use (CI, scripts)

`--force` is the intended path for anything that can't answer an
interactive prompt. What it does *not* do: know in advance whether a given
restart will hit the idempotency gate. A script that always passes
`--force` is safe under this design (rows 1 and 2 in the table above both
resolve correctly), but it should not assume every `--force`'d restart
succeeds — row 3 is exactly the case where a script needs to actually check
the exit code/error rather than treating `--force` as "always proceeds."
