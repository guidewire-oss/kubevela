# Plan: KEP-2.15 Re-execution for Operations

Implements the [Re-execution](https://github.com/guidewire-oss/kubevela/blob/design/kep-2.15-operations/design/vela-core/keps/2.15-operations/README.md#re-execution)
section on top of the merged POC (PR #29). Branch `feat/kep-2.15-add-retry-logic`
is currently exactly that merge commit, clean.

**Idempotency gating was considered and explicitly rejected** (see design
decision #5). Earlier drafts of this plan had an `Idempotent` field, a
server-side admission gate, `--force`, and a whole companion doc of edge
cases around it — all removed. Any step, idempotent or not, can be
freely restarted; the operator is trusted to know what they're doing.

## Current state (verified against the actual code)

- `operation_controller.go`: one `Reconcile` = one run to completion. `IsTerminal()`
  only knows `Succeeded`/`Failed`. No restart entrypoint exists.
- `OperationPhase` has `Pending`/`Running`/`Succeeded`/`Failed` only. The embedded
  engine (`github.com/kubevela/workflow`) already reports a `suspending`
  `WorkflowRunPhase` and a `Status.Suspend` bool — `Reconcile`'s phase switch
  doesn't handle it and silently falls through to `Running`.
- `OperationWorkflowStatus` embeds `workflowv1alpha1.WorkflowRunStatus` verbatim.
  Upstream's `StepStatus` has no attempt-history field, only
  `FirstExecuteTime`/`LastExecuteTime` — we can't add to a vendored type, so
  attempt history has to live in our own API type, tracked alongside the
  embedded status, not inside it.
- `github.com/kubevela/workflow/pkg/utils` already ships the `WorkflowOperator`/
  `WorkflowStepOperator` interfaces and `SuspendWorkflow`/`ResumeWorkflow`/
  `RestartWorkflow`/`RestartFromStep`/`TerminateWorkflow`, used today by
  `vela workflow suspend/resume/restart/terminate` against `WorkflowRun`.
  `RestartWorkflow`'s whole-workflow path literally does
  `run.Status = WorkflowRunStatus{}` — a full wipe. Attempt history must be
  snapshotted *before* that wipe; the exported step-phase helper
  `wfUtils.OperateSteps` is reusable, the free functions are not (they PATCH a
  `*WorkflowRun`, not our `Operation`).
- CLI (`references/cli/operation.go`) has `list`/`run`/`status` only. `run`
  creates via `GenerateName` and busy-polls `IsTerminal()` every
  `operationPollInterval` (2s).
- No dispatch/children (composition) exists yet, so the KEP's `--failed-only`
  (re-run failed children of a `dispatch-operations` step) is not applicable
  until that lands.
- **No TTL/GC for the `Operation` CR itself.** Checked `apis/core.oam.dev/v2alpha1/`,
  the operation controller, and `cmd/core/app`'s flags — nothing deletes a
  terminal `Operation`, and there's no config flag for it. (The KEP's
  "Resource ownership and cleanup" section only covers resources a *step*
  creates, e.g. the restart Job's `ttlSecondsAfterFinished` — that's a
  separate, already-handled concern.) The closest existing precedent in this
  codebase is `--application-revision-limit`/`--revision-limit`/
  `--definition-revision-limit`, which cap retained `ApplicationRevision`/
  definition-revision objects via `resourcekeeper.AppRevisionLimitGCOption`
  (`pkg/resourcekeeper/gc_rev.go`) — nothing equivalent is wired up for
  `Operation`.

## Design decisions

1. **Attempt history is a new KubeVela-owned field, not an extension of the
   vendored type.** Add `Attempts int64` to `OperationStatus` and a
   `StepAttempts map[string][]OperationStepAttempt` to `OperationWorkflowStatus`,
   keyed by step name.
2. **Snapshot, then reset.** Any restart path copies the current `Steps`
   (and sub-steps) into `StepAttempts` and increments `Attempts` before
   clearing them, mirroring what `RestartWorkflow` does today but capturing
   what it currently throws away.
3. **Reuse the engine's step-phase mechanics, not its resource assumptions.**
   Implement `OperationWorkflowOperator`/`OperationWorkflowStepOperator`
   against the `wfUtils.WorkflowOperator`/`WorkflowStepOperator` interfaces,
   using `wfUtils.OperateSteps` for phase transitions and `r.Status().Update`
   (on `Operation`, not `WorkflowRun`) to persist.
4. **`Suspended` becomes a first-class `OperationPhase`.** Map
   `workflowv1alpha1.WorkflowStateSuspending` to it in `Reconcile`'s phase
   switch. Add `Cancelled` too. Only `Succeeded`/`Failed`/`Cancelled` are
   terminal — `Suspended` keeps renewing the concurrency lease, per the KEP.
5. **No idempotency gate, at all.** The KEP's text (`idempotent: false` as a
   default, `--force` plus a named confirmation to override) was the
   starting point, but the decision here is to not build it: no `Idempotent`
   field on `WorkflowStepDefinitionSpec`, no resolution logic, no admission
   check, no `--force`. A restart is triggered the simple way — the CLI
   patches `.status` directly via the `pkg/workflow/operation` operator,
   exactly mirroring `WorkflowRun`'s existing `RestartWorkflow`/
   `RestartFromStep` precedent — because there is nothing left that needs an
   unbypassable, server-side enforcement point. This also settles what would
   otherwise be an open question (spec-level trigger vs. direct status
   patch) in favor of the simpler option, since the only reason to prefer
   the former was to make a gate tamper-proof.
6. **No changes needed to parameter/context resolution.** Phase 1 already
   snapshots `op.Status.Template` once and rebuilds `process.Context` fresh
   every reconcile — that already matches "parameters frozen, context/source
   resolve live." `--refresh-inputs` is N/A: `SourceDefinition`/Option 3 isn't
   implemented in this codebase.
7. **Restart isn't gated on the target step's current phase.** The upstream
   helper this mirrors (`CleanStatusFromStep`) refuses to restart a step
   unless it's currently `Failed` — that's an Application/`WorkflowRun`-
   oriented policy choice, not a KEP requirement, and we deliberately don't
   carry it. An operator can restart from a `Succeeded` or `Skipped` step
   too (e.g. to force a redo, or because an `if:` condition would now
   evaluate differently). Combined with decision #5, restart is now
   completely operator-trusted: no phase check, no idempotency check. The
   `--only` flag still carries its own, separate, non-idempotency-related
   risk: restarting an already-`Succeeded` step's output without cascading
   to downstream steps can leave the record internally inconsistent (the
   KEP already flags this for `--only` generally) — the CLI should still
   warn about that specifically, since it's a data-consistency concern, not
   a safety gate.
8. **Terminal `Operation`s get a TTL, deletion-based rather than count-based.**
   Unlike `ApplicationRevision` (many revisions of one named resource, so a
   count limit makes sense), each `Operation` is a standalone object — a
   fixed-duration TTL since `CompletionTime`, mirroring Kubernetes' own Job
   TTL controller, is the better structural fit than copying the revision-limit
   pattern. This has to interact correctly with re-execution: the clock starts
   only once `IsTerminal()` is true (so a `Suspended` operation, which is
   non-terminal, is naturally exempt), and a `restart` on an already-terminal
   `Operation` must reset it (an `Operation` still being restarted is not done
   being useful, TTL or not).

## Tasks

**API — `apis/core.oam.dev/v2alpha1/operation_types.go`**
- [ ] `OperationPhase`: add `Suspended`, `Cancelled`
- [ ] `OperationStatus`: add `Attempts int64`
- [ ] new `OperationStepAttempt` type; `OperationWorkflowStatus.StepAttempts map[string][]OperationStepAttempt`
- [ ] `IsTerminal()`: include `Cancelled`
- [ ] `OperationSpec`: optional `TTLSecondsAfterFinished *int32` (per-Operation override, same shape/name as the Job field template authors already use)
- [ ] regenerate deepcopy + CRD YAML

**Controller — `pkg/controller/core.oam.dev/v2alpha1/operation/`**
- [ ] `OperationWorkflowOperator`/`OperationWorkflowStepOperator` (new file), backed by `wfUtils.OperateSteps` + `r.Status().Update`
- [ ] snapshot-before-reset helper (populates `StepAttempts`, bumps `Attempts`)
- [ ] `Reconcile`: map `WorkflowStateSuspending` → `OperationPhaseSuspended`; slower requeue while suspended
- [ ] TTL sweep: once `IsTerminal()` and `CompletionTime + ttl` has passed, delete the `Operation`; ttl = `spec.ttlSecondsAfterFinished` if set, else a new cluster-wide default flag (see Config below); requeue at the remaining TTL window instead of hot-looping

**CLI — `references/cli/operation.go`**
- [ ] `vela operation restart <name> [--step s] [--only] [--cluster c]` (no `--failed-only` yet — no children to re-run; no `--force` — there's nothing to force past) — calls the operator's `Restart` directly, same as `resume`/`suspend`
- [ ] `vela operation resume <name>`
- [ ] `vela operation suspend <name> [--step s]` — mirrors `vela workflow suspend`; the underlying operator method is already required by the `wfUtils.WorkflowOperator` interface regardless, so this is close to free once `restart`/`resume` exist
- [ ] `restart`/`resume` reuse the existing poll-until-terminal loop + `printOperationStatus`; `suspend` polls until phase reaches `Suspended` instead (it's non-terminal, so `IsTerminal()` won't do)
- [ ] `--only` warning when the target step is currently `Succeeded` — its output is about to change and, under `--only`, nothing downstream re-reads it (data-consistency advisory, not a safety gate)
- [ ] `status` prints `Attempts` + per-step attempt history

**Config — `cmd/core/app` flags / `core.oam.dev` controller Args**
- [ ] new `--default-operation-ttl-seconds` flag (mirrors the naming precedent of `--application-revision-limit` and the permissions story's `--default-operation-service-account`), threaded into `core.Args` and read by the TTL sweep above. `0`/unset = no default TTL (today's behavior, so this ships opt-in)

**Tests**
- [ ] unit: snapshot-then-reset, suspended-phase mapping, `Suspend`/`Resume` phase-transition correctness (whole-workflow and `--step`-scoped), restarting a step regardless of its current phase, and the `suspend` CLI command polling on phase `== Suspended` rather than `IsTerminal()`
- [ ] e2e: restart a failed step (attempts grows, prior failure retained), suspend/resume round-trip, terminal `Operation` deleted after its TTL elapses and NOT deleted while still restartable within the window

**Docs**
- [ ] check off "Re-execution" in `POC.md`
- [ ] document the new `--default-operation-ttl-seconds` flag alongside the KEP's other cluster-wide settings (`AuthenticateOperation`, `OperationsRunAsInvoker`)

## Open questions

- Backoff while `Suspended`: fixed interval vs. exponential — KEP doesn't specify.
- Where `Cancelled` comes from (deletion/finalizer vs. a new `vela operation cancel`) — needed before `IsTerminal()`'s `Cancelled` branch is anything but dead code.
- TTL default value and whether it should ship enabled by default at all — the KEP itself is silent on `Operation`-level retention (only step-created resources are addressed), so this is a gap being filled without KEP text to anchor the number.

## Explicitly out of scope

- Idempotency gating of any kind — considered, deliberately rejected (design decision #5)
- `--failed-only` / dispatch-children re-run — needs composition (not implemented)
- Duplicate-`Operation` prevention / admission gating — GWCP-108272
- Permission model — GWCP-108269
- `--refresh-inputs` / `SourceDefinition` caching — KEP-2.16, not implemented here
