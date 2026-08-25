# Implementation Plan: KEP-2.15 Re-execution

Technical companion to `RETRY_PLAN.md` (design decisions, gaps, open questions).
This document is the execution plan: exact type changes, new files, function
signatures, and sequencing, verified against the current code on
`feat/kep-2.15-add-retry-logic` (== the PR #29 merge commit).

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
	// ForcedNonIdempotent is true when this attempt re-ran a step that was
	// not idempotent, i.e. it required an explicit acknowledgment in
	// spec.Restart.AcknowledgedNonIdempotent to be admitted at all. False
	// for the original run and for any restart of an idempotent step.
	// +optional
	ForcedNonIdempotent bool `json:"forcedNonIdempotent,omitempty"`
	// RequestedBy is the identity that requested this attempt, taken from
	// the admission request's UserInfo -- not client-supplied, so it can't
	// be spoofed the way a free-text TriggeredBy could be. Empty for the
	// original run.
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`
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

	// Restart requests a re-execution of this Operation's workflow. Setting
	// it is the ONLY way to trigger a restart -- there is deliberately no
	// client-side path that mutates .status directly (see "Restart moves
	// server-side" below). The controller clears this field once processed,
	// so its mere presence means "a restart is pending admission/execution."
	// +optional
	Restart *OperationRestart `json:"restart,omitempty"`
```

```go
// OperationRestart is a one-shot restart request. Cleared by the controller
// once processed -- it is a trigger, not persistent state.
type OperationRestart struct {
	// Step restarts from this step onward. Empty means the whole workflow.
	// +optional
	Step string `json:"step,omitempty"`

	// Only restarts Step alone; downstream steps keep their prior results.
	// Only meaningful when Step is set.
	// +optional
	Only bool `json:"only,omitempty"`

	// AcknowledgedNonIdempotent must name every non-idempotent step that
	// this restart would re-run, or the admission webhook rejects it. This
	// is the actual enforcement point -- see "Restart moves server-side".
	// +optional
	AcknowledgedNonIdempotent []string `json:"acknowledgedNonIdempotent,omitempty"`
}
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

### `apis/core.oam.dev/v1beta1/workflow_step_definition.go`

```go
type WorkflowStepDefinitionSpec struct {
	Reference common.DefinitionReference `json:"definitionRef,omitempty"`
	Schematic *common.Schematic          `json:"schematic,omitempty"`
	Version   string                     `json:"version,omitempty"`

	// Idempotent declares whether re-executing this step is safe, i.e. has
	// no effect beyond its first successful run. Consulted only by the
	// Operation controller's restart gate (KEP-2.15) — the Application and
	// WorkflowRun controllers do not read it. Unset is treated as false for
	// an Operation-scoped restart.
	// +optional
	Idempotent *bool `json:"idempotent,omitempty"`
}
```
This is a shared CRD (Application/WorkflowRun/Operation steps all use
`WorkflowStepDefinition`) — additive and optional, but flag for review by
whoever owns those controllers since it's not Operation-exclusive real estate.

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

Methods, and what each does against `op.Status.Workflows[0]`:

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

**Called by the controller now, not the CLI.** The CLI's job ends at
patching `spec.Restart` (see the CLI section below); once the webhook admits
it, `Reconcile` is what actually invokes
`operation.NewOperationWorkflowOperator(r.Client, io.Discard, op).Restart(ctx)`
/ `.Restart(ctx, step)`. The method bodies below are unchanged by this shift
— only the caller moved. This is also why `Suspend`/`Resume` deliberately
stay CLI-invoked direct status patches while `Restart` alone goes through
`spec.Restart` + admission: only `Restart` needs a safety gate, so only
`Restart` needs the trigger indirection that makes a gate possible. Don't
"fix" that asymmetry into uniformity later without re-deriving why it's there.

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
		// from a non-failed step"). We don't carry that precondition —
		// an operator should be able to force a re-run of a Succeeded or
		// Skipped step too (e.g. "redo the health check anyway", "this
		// step's `if:` condition would evaluate differently now"). That's
		// safe here in a way it wouldn't be for Applications/WorkflowRun,
		// because CheckIdempotent is already the actual safety gate,
		// independent of the step's current phase — a non-idempotent step
		// requires --force whether it's being restarted because it failed
		// or because someone just wants it redone.
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

**`--only` needs a sharper warning now that any step can be the restart
target, not just a failed one.** The KEP already flags `--only` as able to
leave the record "internally inconsistent" when a failed step's downstream
already ran against a partial result; that risk is just as real — arguably
more likely to be hit in practice — when the target step had already
*succeeded*: its output is about to change, `--only` means nothing
downstream re-reads the new value, so the CLI should say so explicitly
rather than only warning about non-idempotent steps. Add this to
`NewOperationRestartCommand`'s confirmation text (see CLI section below),
not just the idempotency prompt — they're two different risks.

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

### Idempotency gate: enforced server-side, not by the CLI

**The CLI must not be the thing deciding whether a restart is safe.** A
client-side-only check is not a control, it is a courtesy — anything that
talks to the API directly (a script, a different tool, `kubectl edit`)
bypasses it entirely, taking with it any hope of an audit trail. This has to
be enforced where it can't be skipped: admission.

```go
// pkg/workflow/operation/idempotency.go
func NonIdempotentSteps(ctx context.Context, cli client.Client, op *v2alpha1.Operation, fromStep string) ([]string, error)
```
Same resolution as before — walk `op.Status.Template.Workflow.Steps` at/after
`fromStep`, resolve each `WorkflowStepDefinition` the two-tier way
`generator.go`'s `resolveTemplate` already does, collect names where
`Spec.Idempotent` is nil-or-false. What changes is **who calls it**: the
`Operation` validating admission webhook (extending the one GWCP-108269 is
already building, now also validating *updates* where `spec.Restart`
transitions from empty to set, not just creates), not the CLI and not the
CLI-invoked operator.

**Webhook logic on an `Operation` update that sets `spec.Restart`:**
1. Call `NonIdempotentSteps` for the requested `Step`/whole-workflow scope.
2. Any name not present in `spec.Restart.AcknowledgedNonIdempotent` →
   reject, via `admission.Denied` with `Result.Details.Causes` listing the
   unacknowledged step names (a structured field, not just a message string
   — see the CLI section below for why that matters).
3. Otherwise admit. The webhook does not itself write anything — the
   controller does the actual restart work once the (now-admitted) object is
   observed, exactly as `restartFromStep`/the whole-workflow path already
   describe, then clears `spec.Restart`.

**Getting the requester's identity onto `RequestedBy`** needs one more
piece: a *mutating* handler (the validating check above can run alongside
it, same as KubeVela's existing Application webhook does both) stamps the
admission request's `UserInfo` onto an annotation —
`operation.oam.dev/restart-requested-by` — mirroring exactly how
`app.oam.dev/username` already gets stamped for `runAs` identity resolution
(`pkg/webhook/core.oam.dev/v1beta1/application/mutating_handler.go`). The
controller reads that annotation when writing `OperationStepAttempt`, rather
than trying to derive identity itself — by the time `Reconcile` runs
asynchronously, there is no request context left to read a user from.

This runs identically regardless of the target step's current phase — since
restart isn't gated on phase (see the divergence note above), this webhook
check is the *only* thing standing between an operator and silently redoing
a step that already succeeded, so it has to fire on every restart request,
not just ones following a failure. It also means `AcknowledgedNonIdempotent`
is a one-shot acknowledgment tied to one `spec.Restart` object, not a
standing permission — see `RETRY_EDGE_CASES.md` for what that implies when
the same non-idempotent step fails and gets restarted repeatedly.

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

**New restart-trigger handling in `Reconcile`**, before the `IsTerminal()`
check that currently runs first: if `op.Spec.Restart != nil`, the webhook
has already admitted it (idempotency-acknowledged), so just execute it —
```go
if op.Spec.Restart != nil {
	operator := operation.NewOperationWorkflowOperator(r.Client, io.Discard, op)
	var err error
	if op.Spec.Restart.Step != "" {
		err = operation.NewOperationWorkflowStepOperator(r.Client, io.Discard, op).Restart(logCtx, op.Spec.Restart.Step)
	} else {
		err = operator.Restart(logCtx)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	// Restart(ctx) above already wrote .status (new Attempts/StepAttempts/
	// Phase) via cli.Status().Update. Clearing spec.Restart is a SEPARATE
	// write to a different subresource -- sequence it after the status
	// write succeeds, not before and not in the same call. If the
	// controller crashes between the two, spec.Restart is still set on the
	// next reconcile and this block just runs again; Restart(ctx) has to be
	// safe to invoke twice in a row for exactly that reason (it already is,
	// structurally -- see whether attempt-numbering stays correct on a
	// re-invocation with no new failure in between as an explicit test case).
	op.Spec.Restart = nil
	if err := r.Update(logCtx, op); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
```

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
  `OperationSpec.Clusters`'s current single-cluster restriction), `--force`.
- Fetch the `Operation` (same `k8sClient.Get` + namespace/env resolution as
  `NewOperationRunCommand`) — read-only, just to build the patch and to
  check the `--only`-on-`Succeeded` warning below. **No idempotency
  resolution happens here** — that's the point of moving the check server-
  side.
- Patch `spec.restart = {step, only}` (no `acknowledgedNonIdempotent` on the
  first attempt, unless `--force` was already passed — see below) and
  submit.
- **If the webhook denies it**: the denial's `Result.Details.Causes` names
  the unacknowledged non-idempotent steps (a structured field is what makes
  this reliable — parsing a free-text message to find step names would be
  fragile the moment the wording changes). Print them, prompt for an
  interactive confirmation naming them (reuse `cmdutil.IOStreams`'s prompt
  helper if one exists — check `references/cli/util.go`), then resubmit the
  *same* patch with `acknowledgedNonIdempotent` set to exactly that denial's
  list. `--force` skips the prompt and resubmits with the denial's list
  immediately — it does not change what gets sent on the *first* attempt,
  it only skips asking after a denial. This also means `--force` on a
  restart that turns out to need no acknowledgment (nothing non-idempotent
  in scope) is a silent no-op, which is correct.
- A denial for any other reason (Operation currently `Running`, permission
  denied, unknown step) is a real error — surface it plainly, `--force`
  must never swallow a rejection that isn't specifically about
  unacknowledged non-idempotent steps.
- If `--only` is set and the target step's current phase is `Succeeded`
  (not `Failed`), print a separate, CLI-side warning before submitting: its
  output is about to be recomputed but, under `--only`, nothing downstream
  will re-read the new value. This one stays client-side deliberately — it's
  advisory UX, not a safety gate, so there's nothing wrong with it living
  in the CLI the way the idempotency check no longer does.
- Poll until the restart is either processed (`spec.Restart` cleared by the
  controller and phase moved off `Suspended`/terminal) or the Operation
  reaches a new terminal phase — reuse the polling shape `run` already has,
  factored into a shared helper (duplicated three ways otherwise: `run`,
  `restart`, `resume`), but note the loop's exit condition is different here
  than a plain "wait for `IsTerminal()`": right after submitting, the object
  briefly still shows the *previous* run's terminal phase until the
  controller picks up `spec.Restart`, so polling on phase alone risks a
  false-positive exit before the restart even started.

`NewOperationResumeCommand`: same shape, no idempotency gate (resuming isn't
re-executing anything), calls `.Resume(ctx)`.

`NewOperationSuspendCommand`: same shape again, mirrors
`NewWorkflowSuspendCommand` (`references/cli/workflow.go`) almost exactly —
`Use: "suspend <name>"`, optional `--step`, no idempotency gate (pausing
isn't re-executing anything either), calls `.Suspend(ctx)` / `.Suspend(ctx, step)`.
This one is essentially free: `Suspend` is part of the `wfUtils.WorkflowOperator`/
`WorkflowStepOperator` interfaces, so the operator method has to exist the
moment `pkg/workflow/operation/operation_v2alpha1.go` is written regardless
of whether a CLI command calls it — the marginal cost is only the ~20-line
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

## Dependency: the safety gate is only as real as the webhook that enforces it

This POC ships with **no admission webhooks at all** (`POC.md`: "neither of
the required SubjectAccessReview checks is implemented"). GWCP-108269 is
what adds one. Server-side idempotency enforcement as designed above is a
new validation rule on that same webhook, extended to also cover `Operation`
*updates* where `spec.Restart` is being set (today's webhook, once it
exists, would otherwise only need to validate creates). That means:

- If GWCP-108269 hasn't landed yet when this work ships, there is no
  admission path at all, and the controller would execute any
  `spec.Restart` unconditionally — silently defeating the whole point of
  moving the check server-side.
- **Interim fallback, if sequencing forces this to ship first:** have
  `Reconcile` itself run `NonIdempotentSteps` and refuse to execute an
  unacknowledged restart (log + leave `spec.Restart` untouched, or set a
  status condition explaining why). This is still *not bypassable* — the
  controller is the only thing that ever turns `spec.Restart` into an actual
  re-run, so no client can skip it by avoiding the CLI — it just loses the
  KEP's stated preference that "a refusal costs a `kubectl apply` and
  nothing else" (rejection surfaces on the next status read instead of
  immediately). Note this doesn't need the webhook for the *audit* half
  either — Kubernetes audit logging on the `Update` call already captures
  who set `spec.Restart`, webhook or not; only the fail-fast-at-apply-time
  property is what the webhook specifically buys.
- Track this as a real cross-story dependency, not a footnote — coordinate
  sequencing with GWCP-108269 rather than assuming this can land fully
  independently.

## Suggested sequencing (each stage independently buildable/testable)

1. **API only** — type changes + `make manifests` + regenerated CRDs. No
   behavior change yet; `Reconcile` doesn't read the new fields. Safe to land
   alone, unblocks everything else.
2. **`pkg/workflow/operation` additions** — the new operator + snapshot +
   idempotency-check functions, with unit tests against a fake client. Still
   no wiring into the controller or CLI, so still behavior-inert.
   `restartFromStep`'s context-backend ConfigMap handling (mirroring
   `CleanStatusFromStep`/`clearContextVars`) is the highest-risk piece of
   this whole plan to get wrong silently — a bug here doesn't fail loudly,
   it just makes a downstream step read a stale or missing prior-step
   output. Test it explicitly, not just through the happy-path e2e case.
3. **Controller wiring** — `Suspended` phase mapping, TTL sweep,
   `spec.Restart` trigger handling, `core.Args`/flag threading. This is the
   first stage with an observable behavior change (suspend surfaces
   correctly; TTL deletes things if the flag is set — default keeps it off).
   Ship the interim controller-side idempotency check here (see the
   "Dependency" section above) if GWCP-108269's webhook isn't ready yet —
   don't ship `spec.Restart` processing with *no* enforcement at all even
   temporarily.
4. **Admission webhook** — extend GWCP-108269's `Operation` webhook to also
   validate updates that set `spec.Restart`, per the idempotency-gate
   section above. Coordinate timing with that story explicitly; if it lands
   first, stage 3 can skip the interim fallback entirely.
5. **CLI wiring** — `restart`/`resume`/`suspend` commands (restart's denial-
   handling flow depends on stage 4 existing to actually deny anything),
   `status`/`list` output changes.
6. **Tests + docs** — e2e scenarios from `RETRY_PLAN.md`, `POC.md` checkbox.

## Test plan specifics

- **Unit** (`pkg/workflow/operation/operation_v2alpha1_test.go`): snapshot
  correctness (attempt numbers increment, prior message/phase preserved),
  `--step`/`--only` trimming boundaries, `restartFromStep` preserving an
  upstream step's context-backend entry while clearing the restarted step's
  own (and anything positioned after it) — assert directly against the
  ConfigMap contents, not just against `ws.Steps`, `NonIdempotentSteps`
  allow/deny against a fake `WorkflowStepDefinition` fixture with/without
  `Idempotent`, restarting a step regardless of its current phase
  (`Succeeded`/`Skipped`, not just `Failed` — confirming there's no phase
  check left over from `CleanStatusFromStep`), and
  `Suspend`/`Resume` phase-transition correctness against a fake client —
  whole-workflow `Suspend(ctx)` moves every `Running` step to `Suspending`
  (and `SubStepsStatus` entries too), `Suspend(ctx, step)` moves only the
  named step and errors with "can not find step" on an unknown one, and
  `op.Status.Phase`/the persisted object reflect the change after
  `cli.Status().Update`.
- **Unit** (`references/cli/operation_test.go`, extending the existing file):
  `NewOperationSuspendCommand` — invokes `operationWorkflowOperator.Suspend`
  vs. the step-scoped variant depending on whether `--step` is set, polls on
  phase `== Suspended` rather than `IsTerminal()` (a fake client that never
  reaches `Suspended` should time out/not return, proving it isn't using the
  terminal check), and surfaces the operator's error (e.g. unknown step name)
  without swallowing it. `NewOperationRestartCommand` never calls
  `NonIdempotentSteps` itself (assert this directly — e.g. a fake client with
  no `WorkflowStepDefinition` fixtures installed at all must still be able to
  submit a restart without erroring on a missing lookup); on a denial, it
  parses `Result.Details.Causes` (not the message string) to get the step
  list, prompts, and resubmits with exactly that list; `--force` skips the
  prompt but a non-idempotency denial (any other reason) is never swallowed
  by `--force`.
- **Unit** (webhook package, wherever GWCP-108269 lands its validator):
  an `Operation` update setting `spec.Restart` with an incomplete
  `acknowledgedNonIdempotent` is denied with `Causes` naming exactly the
  missing steps, not just a generic message; a create (no prior restart
  history) is unaffected by this new rule; a step-scoped restart only
  evaluates the steps actually in scope for that `--step`/`--only`
  combination, not the whole template's step list.
- **Unit** (`pkg/controller/.../operation/ttl_test.go`): `sweepTTL` table test
  — no TTL set, cluster default only, per-Operation override, already
  expired, not yet expired (asserts `RequeueAfter` equals the remaining
  window, not a fixed constant).
- **e2e** (`test/e2e-test/operation_test.go`, extending the existing
  `ginkgo` suite): restart a step whose Job is made to fail once
  (`test/e2e-test/testdata/operation/vela-system/workflowstepdefinition.yaml`'s
  restart step, or a new fixture step designed to fail on attempt 1) and
  assert `status.attempts == 2` with the first attempt's failure preserved in
  `stepAttempts`; suspend/resume round-trip via a `suspend` step type;
  `restart --step` on a non-idempotent step refused without `--force`; a
  two-step template where step 2 consumes step 1's `outputs` via `inputs`,
  step 2 fails, `restart --step <step2> --only`, and step 2 re-reads step 1's
  original output correctly (the actual scenario this section exists for);
  `restart --step <step1>` on a step that already `Succeeded` is allowed
  (not refused for being non-Failed) and correctly clears step 2's downstream
  result too, forcing it to re-run against step 1's new output;
  `--default-operation-ttl-seconds` causes a terminal `Operation` to
  disappear after the window, and a `Suspended` one does not.
