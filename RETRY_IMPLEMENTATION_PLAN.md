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
```
make manifests
```
(`go generate ./pkg/... ./apis/...` + CRD dispatch into `charts/vela-core/crds`
 — confirmed as the single Makefile target that does both deepcopy and CRD YAML).

## New file: `pkg/workflow/operation/operation_v2alpha1.go`

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

## Test plan specifics

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
- **Unit** (`pkg/controller/.../operation/ttl_test.go`): `sweepTTL` table test
  — no TTL set, cluster default only, per-Operation override, already
  expired, not yet expired (asserts `RequeueAfter` equals the remaining
  window, not a fixed constant).
- **e2e** (`test/e2e-test/operation_test.go`, extending the existing
  `ginkgo` suite): restart a step whose Job is made to fail once
  (`test/e2e-test/testdata/operation/vela-system/workflowstepdefinition.yaml`'s
  restart step, or a new fixture step designed to fail on attempt 1) and
  assert `status.attempts == 2` with the first attempt's failure preserved in
  `stepAttempts`; suspend/resume round-trip via a `suspend` step type; a
  two-step template where step 2 consumes step 1's `outputs` via `inputs`,
  step 2 fails, `restart --step <step2> --only`, and step 2 re-reads step 1's
  original output correctly (the actual scenario this section exists for);
  `restart --step <step1>` on a step that already `Succeeded` is allowed
  (not refused for being non-Failed) and correctly clears step 2's downstream
  result too, forcing it to re-run against step 1's new output;
  `--default-operation-ttl-seconds` causes a terminal `Operation` to
  disappear after the window, and a `Suspended` one does not.
