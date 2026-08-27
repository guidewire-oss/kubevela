# Implementation Plan: KEP-2.15 Re-execution

Technical companion to `RETRY_PLAN.md` (design decisions, gaps, open questions).
This document is the execution plan: exact type changes, new files, function
signatures, and sequencing, verified against the current code on
`feat/kep-2.15-add-retry-logic` (== the PR #29 merge commit).

**Idempotency gating was considered and explicitly rejected** (`RETRY_PLAN.md`
design decision #5). An earlier draft of this plan had `Idempotent` on
`WorkflowStepDefinitionSpec`, an `OperationSpec.Restart` trigger field, an
admission webhook extending GWCP-108269's to enforce acknowledgment, a
`--force` flag, and audit fields (`ForcedNonIdempotent`/`RequestedBy`) for
all of it — plus a whole companion doc (`RETRY_EDGE_CASES.md`) of edge cases
around forcing non-idempotent steps. All of that is removed below. Restart
is unguarded: any step, whatever its nature, can be rerun on request.

## Key implementation decision: reuse `pkg/workflow/operation`, don't create a new package

`references/cli/workflow.go` already gets its `Suspend`/`Resume`/`Restart`/
`Terminate` behavior from `pkg/workflow/operation.NewApplicationWorkflowOperator`
(package `operation`, distinct from `pkg/controller/core.oam.dev/v2alpha1/operation`,
which is the *controller* package and confusingly shares a name). That package
implements `github.com/kubevela/workflow/pkg/utils`'s `WorkflowOperator`/
`WorkflowStepOperator` interfaces for `*v1beta1.Application`, using the same
constructor shape:

```go
func NewApplicationWorkflowOperator(cli client.Client, w io.Writer, app *v1beta1.Application) wfUtils.WorkflowOperator
func NewApplicationWorkflowStepOperator(cli client.Client, w io.Writer, app *v1beta1.Application) wfUtils.WorkflowStepOperator
```
backed by unexported `appWorkflowOperator{cli, outputWriter, application}`.

**Add `NewOperationWorkflowOperator`/`NewOperationWorkflowStepOperator` to this
same package**, not a new one. This gets us:
- No import cycle: both `references/cli/operation.go` and
  `pkg/controller/core.oam.dev/v2alpha1/operation` can import
  `pkg/workflow/operation` (the CLI already does, for `workflow.go`).
- One implementation of the snapshot/reset/suspend/resume logic shared by CLI
  and controller instead of two.

## API changes

### `apis/core.oam.dev/v2alpha1/operation_types.go`

```go
const (
	OperationPhasePending   OperationPhase = "Pending"
	OperationPhaseRunning   OperationPhase = "Running"
	OperationPhaseSucceeded OperationPhase = "Succeeded"
	OperationPhaseFailed    OperationPhase = "Failed"
	// OperationPhaseSuspended means the workflow is paused (a `suspend` step,
	// or a manual `vela operation suspend`). Non-terminal: the concurrency
	// lease keeps renewing.
	OperationPhaseSuspended OperationPhase = "Suspended"
	// OperationPhaseCancelled means a human stopped the operation before it
	// reached a natural terminal phase. Terminal.
	OperationPhaseCancelled OperationPhase = "Cancelled"
)

// OperationStepAttempt records one execution of a single workflow step.
// The embedded WorkflowRunStatus.Steps (from github.com/kubevela/workflow)
// only ever holds the latest attempt; this is where prior attempts that
// RestartWorkflow-style resets would otherwise discard get preserved.
type OperationStepAttempt struct {
	AttemptNumber int64                              `json:"attemptNumber"`
	Phase         workflowv1alpha1.WorkflowStepPhase `json:"phase,omitempty"`
	Message       string                             `json:"message,omitempty"`
	Reason        string                             `json:"reason,omitempty"`
	StartTime     metav1.Time                        `json:"startTime,omitempty"`
	// TriggeredBy names how this attempt started, e.g.
	// "vela operation restart --step backup". Empty for the original run.
	TriggeredBy string `json:"triggeredBy,omitempty"`
}
```

`OperationWorkflowStatus` gains a field, not a replacement:
```go
type OperationWorkflowStatus struct {
	Cluster string `json:"cluster,omitempty"`
	workflowv1alpha1.WorkflowRunStatus `json:",inline"`

	// StepAttempts is prior-attempt history per step name. Populated only on
	// restart, immediately before the corresponding entries in the embedded
	// Steps are reset.
	// +optional
	StepAttempts map[string][]OperationStepAttempt `json:"stepAttempts,omitempty"`
}
```

`OperationStatus` gains:
```go
	// Attempts is how many times this Operation's workflow has been
	// (re)started, including the original run (starts at 1).
	// +optional
	Attempts int64 `json:"attempts,omitempty"`
```

`OperationSpec` gains:
```go
	// TTLSecondsAfterFinished bounds how long a terminal Operation is kept
	// before the controller deletes it. Unset uses the cluster default
	// (--default-operation-ttl-seconds); explicit 0 disables TTL for this
	// Operation.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
```

`IsTerminal()` changes from a two-way to a three-way check:
```go
func (o *Operation) IsTerminal() bool {
	switch o.Status.Phase {
	case OperationPhaseSucceeded, OperationPhaseFailed, OperationPhaseCancelled:
		return true
	default:
		return false
	}
}
```

No changes needed to `apis/core.oam.dev/v1beta1/workflow_step_definition.go`
— there is no `Idempotent` field, so `WorkflowStepDefinitionSpec` is
untouched by this plan.

### Codegen

Status: **done**. `make manifests` itself wasn't run (it re-kustomizes and
redispatches CRDs for every group, a much bigger diff than this change
needs); instead ran the two steps it composes, scoped to
`apis/core.oam.dev/v2alpha1/...` only:
```
go run -tags generate sigs.k8s.io/controller-tools/cmd/controller-gen \
  object:headerFile=./hack/boilerplate.go.txt paths=./apis/core.oam.dev/v2alpha1/...
go run -tags generate sigs.k8s.io/controller-tools/cmd/controller-gen \
  crd:crdVersions=v1,generateEmbeddedObjectMeta=true \
  paths=./apis/core.oam.dev/v2alpha1/... output:crd:dir=/tmp/crdout
cp /tmp/crdout/core.oam.dev_operations.yaml charts/vela-core/crds/core.oam.dev_operations.yaml
```
This regenerated `zz_generated.deepcopy.go` for the new/changed types and
refreshed `charts/vela-core/crds/core.oam.dev_operations.yaml` (which also
picked up doc-comment drift already present in the Go source but not yet
reflected in the checked-in CRD -- unrelated pre-existing staleness, not
introduced by this change).

## New file: `pkg/workflow/operation/operation_v2alpha1.go`

Status: **done**. Implemented as planned below, with two adjustments made
while implementing:
- `restartFromStep`'s underlying helper (`cleanOperationStatusFromStep`)
  returns the affected `WorkflowStepStatus`/`StepStatus` entries and the
  dependency name set directly, rather than mutating a ConfigMap in place
  like upstream's `CleanStatusFromStep` -- so the caller can snapshot them
  into `StepAttempts` before they're discarded, then separately clear the
  context-backend ConfigMap using the returned dependency names.
- The whole-workflow `Restart(ctx)` path also deletes the old
  `ContextBackend` ConfigMap before wiping `ws.WorkflowRunStatus` -- the
  plan's sketch of this path (mirroring the "Restart: snapshot-then-reset"
  section above) omitted this, but upstream's `RestartWorkflow` does it for
  exactly this case, and skipping it would leak the ConfigMap object once
  the wipe drops its only reference.
- **Bug found via live-cluster testing, fixed**: `restartFromStep` never
  reset `ws.Terminated`/`ws.Suspend`/`ws.Finished`/`ws.EndTime` -- it only
  touched `ws.Steps` and the context-backend ConfigMap. Upstream's
  `RestartFromStep` explicitly clears all four before touching step status;
  I mirrored the step/ConfigMap handling but dropped this part in
  translation. Concretely: a terminal (failed) run leaves `Terminated:
  true`; a `--step` restart that resets only the target step's status but
  leaves `Terminated: true` in place causes the embedded engine to report
  the *whole run* terminated/failed again on the very next reconcile,
  regardless of whether the restarted step itself succeeds -- confirmed
  against a live cluster: all three steps showed `phase: succeeded` in
  `status.workflows[0].steps`, `status.attempts` correctly read `2`, yet
  `status.phase` stayed `Failed` because `status.workflows[0].terminated`
  was still `true` from the original run. Fixed by clearing all four at the
  top of `restartFromStep`, mirroring `RestartFromStep` exactly. (The
  whole-workflow `Restart(ctx)` path was never affected -- its full
  `ws.WorkflowRunStatus = workflowv1alpha1.WorkflowRunStatus{}` wipe already
  zeroes these implicitly.) Added a regression test
  (`TestOperationRestartFromStep`, extended with `Terminated`/`Finished`/
  `Suspend: true` and a non-zero `EndTime` in the fixture, plus assertions
  that all four are cleared after restart) -- confirmed it fails without the
  fix and passes with it.
- **Second bug found via the same live-cluster session, in the controller
  this time**: `pkg/controller/core.oam.dev/v2alpha1/operation/operation_controller.go`'s
  `Reconcile`, after every execution, rebuilds `op.Status.Workflows` with a
  struct literal that only sets `Cluster`/`WorkflowRunStatus`. This predates
  the retry work -- harmless before `StepAttempts` existed, since there was
  nothing to preserve. Once `StepAttempts` was added (this plan's API
  stage), that literal silently dropped it on every reconcile that actually
  executes the workflow. Confirmed live: a `--step` restart's operator
  correctly wrote `StepAttempts`, but by the time the restarted step
  finished and the Operation went terminal, it was gone -- the very next
  reconcile after the restart's status write had already wiped it back to
  nil. Fixed by extracting a `carryForwardStepAttempts(op)` helper
  (`generator.go`, next to `buildWorkflowInstance`, the same carry-forward
  pattern that function already uses for `WorkflowRunStatus`) and threading
  it into the literal. Added `TestCarryForwardStepAttempts`
  (`generator_test.go`) covering no-workflow, populated, and
  empty-but-present cases.
- `TriggeredBy` on `OperationStepAttempt` is left unset by every call site:
  `Restart(ctx)`/`Restart(ctx, step)` are fixed by the `wfUtils.WorkflowOperator`/
  `WorkflowStepOperator` interfaces (no room for a caller-supplied string),
  so there's no way to plumb "vela operation restart --step backup" through
  without breaking interface conformance. Not attempted; noted as a known
  gap rather than worked around.

Mirrors `operation.go`'s existing Application-side structure exactly.

```go
package operation

import (
	v2alpha1 "github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
	// ... same import shape as operation.go
)

func NewOperationWorkflowOperator(cli client.Client, w io.Writer, op *v2alpha1.Operation) wfUtils.WorkflowOperator {
	return operationWorkflowOperator{cli: cli, outputWriter: w, operation: op}
}

func NewOperationWorkflowStepOperator(cli client.Client, w io.Writer, op *v2alpha1.Operation) wfUtils.WorkflowStepOperator {
	return operationWorkflowStepOperator{cli: cli, outputWriter: w, operation: op}
}

type operationWorkflowOperator struct {
	cli          client.Client
	outputWriter io.Writer
	operation    *v2alpha1.Operation
}

type operationWorkflowStepOperator struct {
	cli          client.Client
	outputWriter io.Writer
	operation    *v2alpha1.Operation
}
```

Methods, and what each does against `op.Status.Workflows[0]`, all called
directly by the CLI — there is no controller-side trigger handling anywhere
in this plan, since there's no gate that would need one:

- **`Suspend(ctx)` / `Suspend(ctx, step)`** — same `wfUtils.OperateSteps`
  transition to `WorkflowStepPhaseSuspending` the Application-side code
  already uses, applied to `op.Status.Workflows[0].WorkflowRunStatus.Steps`,
  then `cli.Status().Update(ctx, operation)` (not `.Patch` — matches the
  `Update` call `operation_controller.go` already uses elsewhere).
- **`Resume(ctx)` / `Resume(ctx, step)`** — same, reverse transition.
- **`Restart(ctx)` / `Restart(ctx, step)`** — the new logic, see below.
- **`Terminate(ctx)`** — same phase-walk as `TerminateWorkflow`, setting
  `op.Status.Phase = v2alpha1.OperationPhaseFailed` (or `Cancelled`, see open
  question in `RETRY_PLAN.md`) instead of mutating a `WorkflowRun`.
- **`Rollback(ctx)`** — `fmt.Errorf("cannot rollback an Operation")`, same as
  the `WorkflowRun` operator's stub.

### `Restart`: snapshot-then-reset

**Called directly by the CLI**, exactly the way `Suspend`/`Resume` already
are — `NewOperationRestartCommand` builds the operator and calls
`.Restart(ctx)` / `.Restart(ctx, step)` itself, no intermediate trigger
field, no admission step in between.

```go
func (wo operationWorkflowOperator) Restart(ctx context.Context) error {
	return wo.restartFrom(ctx, "")
}

func (wo operationWorkflowStepOperator) Restart(ctx context.Context, step string) error {
	return wo.operationWorkflowOperator().restartFrom(ctx, step)
}

func (wo operationWorkflowOperator) restartFrom(ctx context.Context, step string) error {
	op := wo.operation
	if op.Status.Phase == v2alpha1.OperationPhaseRunning {
		return fmt.Errorf("cannot restart a running operation")
	}
	ws := &op.Status.Workflows[0] // single-cluster only, per Phase 1's scope

	if step != "" {
		// Mirror RestartFromStep/CleanStatusFromStep exactly
		// (github.com/kubevela/workflow/pkg/utils/operation.go), not just
		// "trim Steps" — a --step restart touches TWO stores, and getting
		// only the first one right still breaks a downstream step's inputs:
		//   1. ws.Steps: only the target step and its dependency set (in
		//      Operations' default sequential mode, that's every step
		//      positioned AFTER it — getStepDependency is purely positional
		//      when mode.Steps != DAG) get reset to Running/cleared.
		//   2. The context-backend ConfigMap (ws.ContextBackend): fetch it,
		//      run the equivalent of clearContextVars(steps, vars, step,
		//      dependency) to strip only that same step+dependency set's
		//      recorded outputs, then cli.Update the ConfigMap. Steps BEFORE
		//      the restart point are untouched in both stores — that's
		//      exactly what lets a restarted step still read an upstream
		//      step's already-recorded output via `inputs: [{from: ...}]`.
		// DELIBERATE DIVERGENCE from CleanStatusFromStep: upstream refuses
		// to restart a step that isn't currently Failed ("can not restart
		// from a non-failed step"). We don't carry that precondition — an
		// operator can force a re-run of a Succeeded or Skipped step too
		// (e.g. "redo the health check anyway", "this step's `if:`
		// condition would evaluate differently now"). There is no gate of
		// any kind standing behind this decision (see RETRY_PLAN.md #5 and
		// #7) — the operator is trusted, full stop.
		if err := restartFromStep(ctx, wo.cli, op, ws, step); err != nil {
			return err
		}
	} else {
		snapshotAttempts(ws, "") // whole-workflow: snapshot everything before the wipe
		ws.WorkflowRunStatus = workflowv1alpha1.WorkflowRunStatus{}
	}
	op.Status.Phase = v2alpha1.OperationPhaseRunning
	op.Status.CompletionTime = nil
	op.Status.Attempts++
	return wo.cli.Status().Update(ctx, op)
}
```

`restartFromStep` (new, same file) is the `--step` counterpart to the
whole-workflow wipe above: compute the dependency set the same way
`getStepDependency` does (positional — everything after `step` in
`ws.Steps`, since Operations don't use DAG mode today), snapshot *only* the
target step and that dependency set into `StepAttempts` (steps before the
restart point aren't being re-attempted, so they don't get a new attempt
entry), reset their `Steps`/`SubStepsStatus` entries, then fetch
`ws.ContextBackend`'s ConfigMap and strip the same step+dependency set's
entries from it before writing it back. No phase check on the target step —
see the divergence note above. This is real engine-integration work, not a
thin wrapper — budget for it accordingly in the sequencing below rather than
assuming it's a small delta over the whole-restart path.

**`--only` still needs its own warning**, unrelated to idempotency: the KEP
already flags `--only` as able to leave the record "internally inconsistent"
when a failed step's downstream already ran against a partial result. That
risk is just as real — arguably more likely to be hit in practice — when the
target step had already *succeeded*: its output is about to change, `--only`
means nothing downstream re-reads the new value. This is a data-consistency
warning the CLI should print, not a safety gate; see the CLI section below.

`snapshotAttempts` (new, same file or a `snapshot.go` alongside) — used
directly by the whole-workflow path above; `restartFromStep` needs the same
per-step recording but scoped to just the affected steps, so factor the
per-step-entry logic out of the loop below into something both call:
```go
// snapshotAttempts copies the result of the run about to be discarded into
// ws.StepAttempts, keyed by step name, before restartFrom clears it.
func snapshotAttempts(ws *v2alpha1.OperationWorkflowStatus, fromStep string) {
	if ws.StepAttempts == nil {
		ws.StepAttempts = map[string][]v2alpha1.OperationStepAttempt{}
	}
	for _, s := range ws.Steps {
		if fromStep != "" && s.Name < fromStep { // steps kept across the restart aren't being re-attempted
			continue
		}
		ws.StepAttempts[s.Name] = append(ws.StepAttempts[s.Name], v2alpha1.OperationStepAttempt{
			AttemptNumber: int64(len(ws.StepAttempts[s.Name]) + 1),
			Phase:         s.Phase,
			Message:       s.Message,
			Reason:        s.Reason,
			StartTime:     s.FirstExecuteTime,
		})
	}
}
```
(`fromStep != "" && s.Name < fromStep` is pseudocode for "steps before the
restart point" — actual implementation needs positional index, not string
comparison, since step order is defined by `Steps[]` position, not name.)

## Controller changes: `pkg/controller/core.oam.dev/v2alpha1/operation/operation_controller.go`

Status: **done**, with two deviations from the sketch below (both necessary
for correctness, not style choices):
- The plan's tail snippet calls `r.sweepTTL(logCtx, op)` only from the
  bottom of `Reconcile` once `op.IsTerminal()` right after execution. But
  `Reconcile`'s *existing* top-of-function early return
  (`if op.IsTerminal() { return ctrl.Result{}, nil }`, for every subsequent
  reconcile of an Operation that was already terminal) would otherwise never
  call `sweepTTL` again -- the very first `RequeueAfter: remaining` it
  returns would fire, hit that early return, and go straight back to doing
  nothing forever. Changed that early return to `return r.sweepTTL(logCtx, op)`
  as well.
- `finish` (called from both the success/failure paths and now also holds
  the `sweepTTL` call) returns `r.sweepTTL(ctx, op)` as its final result
  instead of a bare `ctrl.Result{}, nil`, so the requeue-driven TTL sweep
  starts immediately on the reconcile that first makes the Operation
  terminal, not only on some later reconcile triggered by something else.
- Also set `op.Status.Attempts = 1` at the same place `op.Status.Template`/
  `Phase`/`StartTime` are first set (the "starts at 1" contract from the
  field's own doc comment, added in the API stage) -- not spelled out as a
  separate task item below, but required by that doc comment.

**`Reconcile`'s phase switch** — add the missing case:
```go
switch phase {
case workflowv1alpha1.WorkflowStateSucceeded:
	...
case workflowv1alpha1.WorkflowStateFailed, workflowv1alpha1.WorkflowStateTerminated:
	...
case workflowv1alpha1.WorkflowStateSuspending:
	op.Status.Phase = v2alpha1.OperationPhaseSuspended
default:
	op.Status.Phase = v2alpha1.OperationPhaseRunning
}
```
And the terminal/requeue tail needs a third branch:
```go
if op.Status.Phase == v2alpha1.OperationPhaseSuspended {
	return ctrl.Result{RequeueAfter: suspendedRequeueInterval}, nil // e.g. 30s, not 5s — see open question in RETRY_PLAN.md
}
if op.IsTerminal() {
	return r.sweepTTL(logCtx, op) // replaces the current bare `return ctrl.Result{}, nil`
}
return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
```

No other `Reconcile` changes are needed for restart/resume/suspend — the CLI
writes `.status` directly via the operator, and the next reconcile just
observes the new state the same way it observes any other status change.

**New `sweepTTL` method** (new file `ttl.go` in the same package):
```go
func (r *Reconciler) sweepTTL(ctx context.Context, op *v2alpha1.Operation) (ctrl.Result, error) {
	ttl := r.DefaultOperationTTL // from Reconciler, threaded through from core.Args
	if op.Spec.TTLSecondsAfterFinished != nil {
		ttl = time.Duration(*op.Spec.TTLSecondsAfterFinished) * time.Second
	}
	if ttl <= 0 || op.Status.CompletionTime == nil {
		return ctrl.Result{}, nil
	}
	remaining := ttl - time.Since(op.Status.CompletionTime.Time)
	if remaining <= 0 {
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, op))
	}
	return ctrl.Result{RequeueAfter: remaining}, nil
}
```

**`Reconciler` struct** gains `DefaultOperationTTL time.Duration`, set in
`Setup(mgr, args)` from a new `core.Args` field (see Config below). This
mirrors how the struct already carries `APIReader`/`Recorder` as
constructor-injected fields rather than reading globals.

## Config: threading a new flag through `core.Args`

Status: **done**, following the sketch below exactly.

Exact pattern already used for `--application-revision-limit`
(`cmd/core/app/config/controller.go`):

1. `pkg/controller/core.oam.dev/oamruntime_controller.go` — add field to `Args`:
   ```go
   // DefaultOperationTTLSeconds is how long a terminal Operation is kept
   // before being deleted, when the Operation itself doesn't set
   // spec.ttlSecondsAfterFinished. 0 disables the default (no expiry).
   DefaultOperationTTLSeconds int
   ```
2. `cmd/core/app/config/controller.go`:
   - `NewControllerConfig()`: add `DefaultOperationTTLSeconds: 0,` to the
     defaults literal (ships opt-in, matching today's no-cleanup behavior).
   - `AddFlags`: add
     ```go
     fs.IntVar(&c.DefaultOperationTTLSeconds, "default-operation-ttl-seconds", c.DefaultOperationTTLSeconds,
         "default-operation-ttl-seconds is how long a terminal Operation is kept before being deleted, when the Operation itself sets no ttlSecondsAfterFinished. 0 (default) disables automatic deletion.")
     ```
3. `pkg/controller/core.oam.dev/v2alpha1/operation/operation_controller.go`'s
   `Setup(mgr, args)`: pass `DefaultOperationTTL: time.Duration(args.DefaultOperationTTLSeconds) * time.Second` into the `Reconciler{}` literal.

No changes needed to `pkg/controller/core.oam.dev/v2alpha1/setup.go` — it
already forwards `args` opaquely to every registered controller's `Setup`.

## CLI changes: `references/cli/operation.go`

Status: **done**. `run`/`status`'s namespace resolution and Get-by-name were
also folded into the new `operationNamespace`/`getOperationByName` helpers
(they existed as three separate inline copies before this change; now
five commands share them), and `pollOperationUntilSuspended` breaks out
early on any terminal phase too, not only `Suspended` -- otherwise a race
where the workflow finishes on its own while the CLI is waiting to observe
`Suspended` would hang forever.

New commands, registered alongside the existing three in
`NewOperationCommand`:
```go
cmd.AddCommand(
	NewOperationListCommand(c, ioStreams),
	NewOperationRunCommand(c, ioStreams),
	NewOperationStatusCommand(c, ioStreams),
	NewOperationRestartCommand(c, ioStreams),
	NewOperationResumeCommand(c, ioStreams),
	NewOperationSuspendCommand(c, ioStreams),
)
```

`NewOperationRestartCommand`:
- `Use: "restart <name>"`, flags: `--step`, `--only` (bool), `--cluster`
  (accepted but only `"local"` valid until multi-cluster lands — matches
  `OperationSpec.Clusters`'s current single-cluster restriction). No
  `--force` — there's nothing to force past.
- Fetch the `Operation` (same `k8sClient.Get` + namespace/env resolution as
  `NewOperationRunCommand`).
- If `--only` is set and the target step's current phase is `Succeeded`
  (not `Failed`), print a warning before proceeding: its output is about to
  be recomputed but, under `--only`, nothing downstream will re-read the
  new value. Advisory UX, not a gate.
- Build the operator via `operation.NewOperationWorkflowOperator(k8sClient, cmd.OutOrStdout(), op)` (or the step operator if `--step` given), call
  `.Restart(ctx)` / `.Restart(ctx, step)` directly — this writes `.status`
  immediately, so there's no round-trip to wait on before polling.
- Reuse the exact poll-until-terminal loop + `printOperationStatus` already
  in `NewOperationRunCommand` (factor it out into a shared helper — it's
  duplicated three ways otherwise: `run`, `restart`, `resume`).

`NewOperationResumeCommand`: same shape, calls `.Resume(ctx)`.

`NewOperationSuspendCommand`: same shape again, mirrors
`NewWorkflowSuspendCommand` (`references/cli/workflow.go`) almost exactly —
`Use: "suspend <name>"`, optional `--step`, calls `.Suspend(ctx)` /
`.Suspend(ctx, step)`. This one is essentially free: `Suspend` is part of
the `wfUtils.WorkflowOperator`/`WorkflowStepOperator` interfaces, so the
operator method has to exist the moment
`pkg/workflow/operation/operation_v2alpha1.go` is written regardless of
whether a CLI command calls it — the marginal cost is only the ~20-line
cobra wrapper and its registration, not new operator logic. Worth including
in the same pass as `restart`/`resume` rather than deferring: leaving it out
would mean an `Operation` could be suspended by the underlying workflow
engine's own `suspend` step type but not by an operator's explicit choice,
which is an asymmetry with no real savings behind it.

`status`/`list` additions: print `op.Status.Attempts` and, per step, the
`StepAttempts` history if non-empty (`printOperationStatus`, extend the
existing table/formatting rather than add a second output mode).

**Not implemented in this pass:** `--failed-only` (no dispatch/children to
target yet — see `RETRY_PLAN.md`'s out-of-scope list).

## Suggested sequencing (each stage independently buildable/testable)

1. **API only** — type changes + `make manifests` + regenerated CRDs. No
   behavior change yet; `Reconcile` doesn't read the new fields. Safe to land
   alone, unblocks everything else.
2. **`pkg/workflow/operation` additions** — the new operator + snapshot
   functions, with unit tests against a fake client. Still no wiring into
   the controller or CLI, so still behavior-inert.
   `restartFromStep`'s context-backend ConfigMap handling (mirroring
   `CleanStatusFromStep`/`clearContextVars`) is the highest-risk piece of
   this whole plan to get wrong silently — a bug here doesn't fail loudly,
   it just makes a downstream step read a stale or missing prior-step
   output. Test it explicitly, not just through the happy-path e2e case.
3. **Controller wiring** — `Suspended` phase mapping, TTL sweep,
   `core.Args`/flag threading. This is the first stage with an observable
   behavior change (suspend surfaces correctly; TTL deletes things if the
   flag is set — default keeps it off).
4. **CLI wiring** — `restart`/`resume`/`suspend` commands, `status`/`list`
   output changes.
5. **Tests + docs** — e2e scenarios from `RETRY_PLAN.md`, `POC.md` checkbox.

Status: stages 1-4 done as described above (with the deviations called out
in each stage's own status note). Stage 5's unit tests are done; its e2e
scenarios are not -- see `RETRY_PLAN.md`'s Tests section for why.

## Test plan specifics

Status: both unit sections below are implemented as described, with one
scope adjustment to the CLI suspend test -- see its note. The e2e section
was not run or written; see `RETRY_PLAN.md`'s Tests section for why.

- **Unit** (`pkg/workflow/operation/operation_v2alpha1_test.go`): snapshot
  correctness (attempt numbers increment, prior message/phase preserved),
  `--step`/`--only` trimming boundaries, `restartFromStep` preserving an
  upstream step's context-backend entry while clearing the restarted step's
  own (and anything positioned after it) — assert directly against the
  ConfigMap contents, not just against `ws.Steps` — restarting a step
  regardless of its current phase (`Succeeded`/`Skipped`, not just
  `Failed` — confirming there's no phase check left over from
  `CleanStatusFromStep`), and `Suspend`/`Resume` phase-transition
  correctness against a fake client — whole-workflow `Suspend(ctx)` moves
  every `Running` step to `Suspending` (and `SubStepsStatus` entries too),
  `Suspend(ctx, step)` moves only the named step and errors with "can not
  find step" on an unknown one, and `op.Status.Phase`/the persisted object
  reflect the change after `cli.Status().Update`.
- **Unit** (`references/cli/operation_test.go`, extending the existing file):
  `NewOperationSuspendCommand` — invokes `operationWorkflowOperator.Suspend`
  vs. the step-scoped variant depending on whether `--step` is set, polls on
  phase `== Suspended` rather than `IsTerminal()` (a fake client that never
  reaches `Suspended` should time out/not return, proving it isn't using the
  terminal check), and surfaces the operator's error (e.g. unknown step name)
  without swallowing it.
  **Adjustment made while implementing:** `pollOperationUntilSuspended`
  deliberately also stops on any terminal phase, not only `Suspended` (a
  production safety net against hanging forever if the workflow finishes on
  its own mid-poll) -- so a fake client sitting at a non-terminal,
  non-`Suspended` phase forever is what actually distinguishes "waits for
  Suspended" from "uses IsTerminal()", not a fake client that never reaches
  either. Proving that with a plain fake client means either hanging the
  test forever or racing a goroutine against `operationPollInterval` (2s,
  not injectable) — both a bad trade for a unit test. Written instead:
  `TestPollOperationUntilSuspendedAlreadyDone`/`TestOperationRestartOnlyWarning`
  covering the bounded happy paths and the extracted `--only` warning logic;
  the actual "does it wait" property is covered by e2e's suspend/resume
  round-trip once that's written (see `RETRY_PLAN.md`).
- **Unit** (`pkg/controller/.../operation/ttl_test.go`): `sweepTTL` table test
  — no TTL set, cluster default only, per-Operation override, already
  expired, not yet expired (asserts `RequeueAfter` equals the remaining
  window, not a fixed constant).
- **e2e** (`test/e2e-test/operation_test.go`, extending the existing
  `ginkgo` suite): restart a step whose Job is made to fail once
  (`test/e2e-test/testdata/operation/vela-system/workflowstepdefinition.yaml`'s
  restart step, or a new fixture step designed to fail on attempt 1) and
  assert `status.attempts == 2` with the first attempt's failure preserved in
  the restarted step's own `status.workflows[].steps[].attempts` (nested per
  step as of the reshape below -- was a step-name-keyed `stepAttempts` map
  when this line was first written); suspend/resume round-trip via a
  `suspend` step type; a two-step template where step 2 consumes step 1's
  `outputs` via `inputs`, step 2 fails, `restart --step <step2> --only`, and
  step 2 re-reads step 1's original output correctly (the actual scenario
  this section exists for); `restart --step <step1>` on a step that already
  `Succeeded` is allowed (not refused for being non-Failed) and correctly
  clears step 2's downstream result too, forcing it to re-run against step
  1's new output; `--default-operation-ttl-seconds` causes a terminal
  `Operation` to disappear after the window, and a `Suspended` one does not.

## Post-implementation: attempts nested per-step, and two bugs found live

Everything above shipped, got manually exercised on a real cluster (the
first time any of this ran against a live controller, since e2e above was
never written), and that surfaced two real bugs plus one deliberate API
reshape. All three are now fixed/implemented and covered by unit tests;
none required touching phase computation, the lock, or TTL logic.

**Bug 1 -- `restartFromStep` didn't reset `Terminated`/`Suspend`/`Finished`/
`EndTime`.** Confirmed live: after `restart --step step-three` on a 3-step
template (step-one/step-two already `Succeeded`, step-three `Failed`), all
three steps reached `succeeded`, but `status.phase` stayed `Failed` --
`status.workflows[0].terminated` was still `true` from the original run.
`restartFromStep` only ever touched `Steps` and the context-backend
ConfigMap; upstream's `RestartFromStep` clears all four flags before
touching step status, and that part didn't make it into the original
translation. Fixed at the top of `restartFromStep`
(`pkg/workflow/operation/operation_v2alpha1.go`). The whole-workflow
`Restart(ctx)` path was never affected -- its full status wipe already
zeroed these. Regression test: `TestOperationRestartFromStep` fixtures now
set all four before restarting and assert all four are cleared after --
confirmed failing without the fix, passing with it.

**Bug 2 -- the controller silently dropped `StepAttempts` on every
reconcile.** `operation_controller.go`'s `Reconcile`, after every
execution, rebuilt `op.Status.Workflows` with a literal that only set
`Cluster`/`WorkflowRunStatus` -- harmless before `StepAttempts` existed,
since there was nothing to preserve; once it did, that literal wiped it
back to nil on the very next reconcile after any restart wrote it. Fixed
by extracting a carry-forward step in `generator.go` (superseded by
`operationWorkflowStatusFromEngine` in the reshape below). Regression
test: `TestCarryForwardStepAttempts` (`generator_test.go`) -- also later
superseded, see below.

**Reshape -- attempts moved from a `stepAttempts` map to
`steps[].attempts`.** Requested after both bugs were fixed and confirmed
live: nest each step's attempt history directly under that step's own
entry in `status.workflows[].steps[]`, instead of a separate map keyed by
name. See design decision 1 (revised) in `RETRY_PLAN.md` for the shape and
why a naive approach (redeclaring `Steps` with a different element type
while still inlining the vendored `WorkflowRunStatus`) doesn't work --
verified directly with a throwaway Go program that Go's `encoding/json`
routes a given key to the *shallowest* matching field only, so the
embedded copy `buildWorkflowInstance` feeds back to the engine to resume
mid-workflow would silently unmarshal empty on every read.

Also verified directly against the engine source before implementing,
not assumed: `generateStepID` (`kubevela/workflow/pkg/generator/generator.go`)
reuses a step's existing ID if a live entry with that name is present,
regardless of phase, and `context.stepSessionID` (used to name each
step's Job) is that same ID. This is *why* a restart has to remove the
target step's live entry rather than reset it in place -- resetting in
place would keep reusing the same Job name across attempts. Which in turn
is why nesting `Attempts` directly on the step's entry can't be the whole
story: the entry a restart is about to discard has nowhere to go if it's
about to not exist. `StepAttempts` on `OperationWorkflowStatus` (see its
doc) is the resulting design: a transient hand-off for exactly the one
reconcile between "removed" and "recreated," merged into the step's own
`Attempts` the moment it reappears, empty otherwise.

Files touched by the reshape:
- `apis/core.oam.dev/v2alpha1/operation_types.go` -- new
  `OperationWorkflowStepStatus`; `OperationWorkflowStatus` stops inlining
  `WorkflowRunStatus`, declares every field explicitly; regenerated
  deepcopy + CRD.
- `pkg/controller/.../operation/generator.go` -- `toEngineStatus`
  (our shape → engine shape, used by `buildWorkflowInstance`) and
  `operationWorkflowStatusFromEngine` (engine shape → ours, reattaching
  `Attempts` from either the step's own prior entry or the pending
  `StepAttempts` map) replace the old direct embed and
  `carryForwardStepAttempts`.
- `pkg/workflow/operation/operation_v2alpha1.go` -- `suspend`/`resume` gained
  `unwrapSteps`/`rewrapSteps` around the one vendored call
  (`wfUtils.OperateSteps`, which takes the plain vendored slice type);
  `restartFrom`'s whole-workflow wipe became explicit field resets instead
  of one struct-zero assignment; `cleanOperationStatusFromStep` and its
  helpers changed element type (logic unchanged -- they only ever touched
  promoted fields); `snapshotAttempts` gained a seeding step so a step
  restarted a *second* time numbers attempts 1, 2 rather than resetting to
  1 (the one place with genuinely new logic, not just plumbing).
- `references/cli/operation.go` -- `findOperationStepStatus` returns the
  wrapped type; `printOperationStatus`'s attempt-history table now reads
  `step.Attempts` directly instead of cross-referencing a separate map
  (simpler than before, not harder).
- Test files across all three packages updated for the new shape, plus new
  cases: `TestOperationWorkflowStatusFromEngineCarriesForwardAttempts` /
  `TestToEngineStatusUnwrapsSteps` (`generator_test.go`, replacing
  `TestCarryForwardStepAttempts`), and a second-restart-of-the-same-step
  case in `TestOperationRestartFromStep` pinning the attempt-numbering fix
  above -- confirmed failing without it, passing with it.

**Follow-up: attempts now log every completion, not only restart-superseded
ones.** Requested after the reshape above landed: originally an attempt was
only recorded when a restart was about to discard a step's status, so a
step that succeeded (or failed) on its first try with no restart ever
issued got no attempt entry at all. Fixed by moving the recording point
from "the CLI, right before a restart wipes a step's status" to "the
controller, the moment a step's execution reaches a terminal phase" --
`recordStepCompletion` (`generator.go`), called from
`operationWorkflowStatusFromEngine` for every step on every reconcile.
Dedup uses the step's own execution ID (`WorkflowStepStatus.ID`, now also
copied onto `OperationStepAttempt.ID`): the same ID showing up terminal
across many reconciles is one attempt, not one per reconcile;
`wfTypes.IsStepFinish(phase, reason)` -- the same check the embedded engine
itself uses -- gates what counts as "terminal" at all, so a step whose
underlying Job is still internally retrying (`reason ==
StatusReasonExecute`) doesn't get logged prematurely.

This simplified `pkg/workflow/operation`'s restart-time snapshot logic:
`snapshotAttempts` no longer computes a new attempt from a step's current
status -- the controller's already done that by the time any restart is
possible (an Operation must be non-`Running` to restart at all, and by then
its steps have already gone through a reconcile that recorded them) -- it
just carries forward whatever `.Attempts` a step already has into the
pending `StepAttempts` map. Sub-steps are the one exception: they're not
wrapped (see `OperationWorkflowStepStatus`'s doc), so `recordStepCompletion`
can't run for them; `snapshotAttempts` still computes their attempt record
the old way, from current status, at restart time only.

New tests: `TestRecordStepCompletion` (`generator_test.go`) covers the pure
function directly -- running (not recorded), failed-but-still-retrying (not
recorded), failed-for-good/skipped (recorded), same-ID-not-duplicated,
different-ID-appended-as-next-number.
`TestOperationWorkflowStatusFromEngineCarriesForwardAttempts` gained cases
for "reaches terminal for the first time, no restart involved" and "pending
attempt reattached, then this new execution recorded too" (numbering 1,
then 2, across a simulated restart). Existing `operation_v2alpha1_test.go`
fixtures updated to pre-populate `.Attempts` on steps (matching what the
controller would already have recorded by the time any restart runs) --
all passing.

Runtime cost of the wrap/unwrap: `toEngineStatus`/
`operationWorkflowStatusFromEngine` changed the per-reconcile cost of
carrying `Steps` forward from O(1) (a slice-header copy, sharing the
backing array, when it was a direct embedded-struct assignment) to O(n)
(an explicit copy into a new slice), n = step count. Real, not "just
negligible" -- but n is bounded by the workflow template's step count
(realistically single digits to low tens, not user-scalable data), and the
same reconcile already does at least one Kubernetes API call and one CUE
compilation per step, each costing orders of magnitude more. Not a
concern at any realistic scale for this feature.
