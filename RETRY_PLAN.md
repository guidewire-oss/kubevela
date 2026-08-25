# Plan: KEP-2.15 Re-execution for Operations

Implements the [Re-execution](https://github.com/guidewire-oss/kubevela/blob/design/kep-2.15-operations/design/vela-core/keps/2.15-operations/README.md#re-execution)
section on top of the merged POC (PR #29). Branch `feat/kep-2.15-add-retry-logic`
is currently exactly that merge commit, clean.

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
- `WorkflowStepDefinitionSpec` (`apis/core.oam.dev/v1beta1/workflow_step_definition.go`)
  is shared by Application/WorkflowRun/Operation steps. Adding `idempotent`
  here touches a type other controllers also read, even though only the
  Operation controller will enforce it.
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
5. **Idempotency is enforced server-side (admission), not by the CLI.** Add
   optional `Idempotent *bool` to the shared `WorkflowStepDefinitionSpec`
   (nil defaults to "not idempotent" for an Operation-scoped restart, per the
   KEP; Application/WorkflowRun controllers ignore it). A restart is
   triggered by setting `OperationSpec.Restart` (a one-shot field the
   controller clears once processed), not by the CLI patching `.status`
   directly — this is what makes a real gate possible: the `Operation`
   admission webhook (extending GWCP-108269's) rejects the update unless
   every non-idempotent step in scope is named in
   `Restart.AcknowledgedNonIdempotent`. A CLI-only check is a courtesy, not a
   control — anything that talks to the API directly bypasses it — so the
   CLI's role shrinks to submitting the request and reacting to a structured
   denial, not deciding anything itself. This also resolves what used to be
   an open question (spec vs. status trigger).
6. **No changes needed to parameter/context resolution.** Phase 1 already
   snapshots `op.Status.Template` once and rebuilds `process.Context` fresh
   every reconcile — that already matches "parameters frozen, context/source
   resolve live." `--refresh-inputs` is N/A: `SourceDefinition`/Option 3 isn't
   implemented in this codebase.
7. **Restart is gated on idempotency, not on the target step's current
   phase.** The upstream helper this mirrors (`CleanStatusFromStep`) refuses
   to restart a step unless it's currently `Failed` — that's an
   Application/`WorkflowRun`-oriented policy choice, not a KEP requirement,
   and we deliberately don't carry it. An operator can restart from a
   `Succeeded` or `Skipped` step too (e.g. to force a redo, or because an
   `if:` condition would now evaluate differently). This is safe because the
   idempotency gate (now enforced by the admission webhook, see decision #5)
   already runs independent of phase — a non-idempotent step needs
   acknowledgment whether the restart was triggered by a failure or not.
   The tradeoff: `--only` on an already-succeeded step is a sharper
   version of the "record goes inconsistent" risk the KEP already flags for
   `--only` on a failed one, since the step's output is now known-good and
   about to change without anything downstream re-reading it — the CLI needs
   its own warning for that case, separate from the idempotency prompt.
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
- [ ] `OperationSpec`: optional `Restart *OperationRestart` (`Step`, `Only`, `AcknowledgedNonIdempotent []string`) — the one-shot restart trigger; see design decision #5
- [ ] regenerate deepcopy + CRD YAML

**Shared API — `apis/core.oam.dev/v1beta1/workflow_step_definition.go`**
- [ ] optional `Idempotent *bool` on `WorkflowStepDefinitionSpec`
- [ ] regenerate deepcopy/CRD (flag for review — shared CRD, other controllers read this type)

**Admission webhook — extends GWCP-108269's `Operation` validator**
- [ ] on an update setting `spec.Restart`: resolve the non-idempotent steps in scope, deny (with the step list in `Result.Details.Causes`, not just a message) unless all are in `AcknowledgedNonIdempotent`
- [ ] cross-story dependency, not optional polish — see the "server-side enforcement" note in `RETRY_IMPLEMENTATION_PLAN.md`; ship the interim controller-side fallback there if this can't land first

**Controller — `pkg/controller/core.oam.dev/v2alpha1/operation/`**
- [ ] `OperationWorkflowOperator`/`OperationWorkflowStepOperator` (new file), backed by `wfUtils.OperateSteps` + `r.Status().Update`
- [ ] snapshot-before-reset helper (populates `StepAttempts`, bumps `Attempts`)
- [ ] `Reconcile`: map `WorkflowStateSuspending` → `OperationPhaseSuspended`; slower requeue while suspended
- [ ] `Reconcile`: on `spec.Restart != nil`, invoke the operator, then clear `spec.Restart` via a separate `r.Update` (distinct write from the status update — sequencing matters, see implementation plan)
- [ ] TTL sweep: once `IsTerminal()` and `CompletionTime + ttl` has passed, delete the `Operation`; ttl = `spec.ttlSecondsAfterFinished` if set, else a new cluster-wide default flag (see Config below); requeue at the remaining TTL window instead of hot-looping

**CLI — `references/cli/operation.go`**
- [ ] `vela operation restart <name> [--step s] [--only] [--cluster c] [--force]` (no `--failed-only` yet — no children to re-run) — patches `spec.Restart` and submits; does **not** resolve idempotency itself
- [ ] `vela operation resume <name>`
- [ ] `vela operation suspend <name> [--step s]` — mirrors `vela workflow suspend`; the underlying operator method is already required by the `wfUtils.WorkflowOperator` interface regardless, so this is close to free once `restart`/`resume` exist. No idempotency gate (pausing isn't re-executing anything), and stays a direct status patch rather than going through `spec.Restart` — only `restart` needs the gate, so only `restart` needs the indirection.
- [ ] `restart`/`resume` reuse the existing poll-until-terminal loop + `printOperationStatus`; `suspend` polls until phase reaches `Suspended` instead (it's non-terminal, so `IsTerminal()` won't do)
- [ ] on webhook denial, parse `Result.Details.Causes` (structured, not the message string) for the unacknowledged step names, prompt, resubmit with them in `AcknowledgedNonIdempotent`; `--force` skips the prompt but must never swallow a denial for any other reason
- [ ] separate `--only` warning when the target step is currently `Succeeded` — its output is about to change and, under `--only`, nothing downstream re-reads it. This one stays CLI-side (advisory, not a safety gate).
- [ ] `status` prints `Attempts` + per-step attempt history

**Config — `cmd/core/app` flags / `core.oam.dev` controller Args**
- [ ] new `--default-operation-ttl-seconds` flag (mirrors the naming precedent of `--application-revision-limit` and the permissions story's `--default-operation-service-account`), threaded into `core.Args` and read by the TTL sweep above. `0`/unset = no default TTL (today's behavior, so this ships opt-in)

**Tests**
- [ ] unit: snapshot-then-reset, suspended-phase mapping, idempotency allow/deny matrix, `Suspend`/`Resume` phase-transition correctness (whole-workflow and `--step`-scoped), and the `suspend` CLI command polling on phase `== Suspended` rather than `IsTerminal()`
- [ ] e2e: restart a failed step (attempts grows, prior failure retained), suspend/resume round-trip, non-idempotent step blocked without `--force`, terminal `Operation` deleted after its TTL elapses and NOT deleted while still restartable within the window

**Docs**
- [ ] check off "Re-execution" in `POC.md`
- [ ] document the new `--default-operation-ttl-seconds` flag alongside the KEP's other cluster-wide settings (`AuthenticateOperation`, `OperationsRunAsInvoker`)

## Open questions

- ~~What triggers a restart server-side~~ — resolved: `spec.Restart`, admission-validated. See design decision #5.
- Backoff while `Suspended`: fixed interval vs. exponential — KEP doesn't specify.
- Where `Cancelled` comes from (deletion/finalizer vs. a new `vela operation cancel`) — needed before `IsTerminal()`'s `Cancelled` branch is anything but dead code.
- TTL default value and whether it should ship enabled by default at all — the KEP itself is silent on `Operation`-level retention (only step-created resources are addressed), so this is a gap being filled without KEP text to anchor the number.
- Sequencing against GWCP-108269: does the admission webhook land before or after this work? See the "Dependency" section in `RETRY_IMPLEMENTATION_PLAN.md` for the interim fallback if not.
- Edge cases around repeated force-restarts of a non-idempotent step (does acknowledgment need to be re-given every time, is there any cap/escalation) — see `RETRY_EDGE_CASES.md`.

## Explicitly out of scope

- `--failed-only` / dispatch-children re-run — needs composition (not implemented)
- Duplicate-`Operation` prevention / admission gating — GWCP-108272
- Permission model — GWCP-108269
- `--refresh-inputs` / `SourceDefinition` caching — KEP-2.16, not implemented here
