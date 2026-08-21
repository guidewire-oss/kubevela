# Operations POC (KEP 2.15)

Proof of concept for [KEP 2.15 "Operations"](https://github.com/guidewire-oss/kubevela/blob/design/kep-2.15-operations/design/vela-core/keps/2.15-operations/README.md): letting a workflow step act on an existing Component/Application (restart, scale, rotate secrets, etc.) through a new `OperationTemplate`/`Operation` pair of CRDs, instead of a whole new Application revision.

This replaces the old `04-poc-thin-slice.md` and `RUNBOOK.md`. It's a living doc: check items off and add rows as the POC grows, don't let it drift from the code.

## ⚠️ Not for release

This POC ships without the KEP's permissions model. Any RBAC principal that can `create` an `Operation` can invoke *any* `OperationTemplate` against *any* target in its namespace, since neither of the required `SubjectAccessReview` checks is implemented. Only run this in a disposable namespace, against non-destructive templates. Don't promote this code path until the permission model lands.

## What's implemented

- [x] `OperationTemplate` / `Operation` CRDs, `core.oam.dev/v2alpha1`
- [x] Operation controller runs the template's workflow once via the existing `github.com/kubevela/workflow` engine (no changes to that module)
- [x] Two-tier template resolution: invoker's own namespace, then `vela-system` — same convention as `ComponentDefinition`
- [x] Context injection, [Option 1](https://github.com/guidewire-oss/kubevela/blob/design/kep-2.15-operations/design/vela-core/keps/2.15-operations/README.md#option-1-static-template-context-read-by-the-step-definition) only: a step reads its target through `context.output`/`context.outputs`/`context.namespace`, the same way `healthPolicy` does today
- [x] `context.operationParams`, populated from `Operation.spec.parameters` — **unvalidated, undefaulted**, since the schema-unify step the KEP assigns to admission never runs here; it's the raw JSON the caller sent
- [x] `attach.scope: Component`, filtered by `allowedComponentTypes`
- [x] `vela operation list` / `run` / `status` CLI, modeled on `vela def`
- [x] Job-based step cleanup via `ttlSecondsAfterFinished` (the KEP intentionally doesn't GC step-created resources itself — see its "Resource ownership and cleanup" section — so a template author has to opt into TTLs or an `if: always` cleanup step)
- [x] Concurrency control: a `coordination.k8s.io/Lease` per `(namespace, target, cluster)` serializes Operations racing for the same target; a losing Operation just retries on the next requeue, and a crashed holder's lock expires after `operationLockDuration`
- [ ] Permissions: the two `SubjectAccessReview`s, `spec.runAs` (`Platform`/`Invoker`), service-account `use` grants, `requireDirectGrant`
- [ ] Parameter schema validation/defaulting (needs admission, see above)
- [ ] `Application` attach scope, [Composition and Fan-out](https://github.com/guidewire-oss/kubevela/blob/design/kep-2.15-operations/design/vela-core/keps/2.15-operations/README.md#composition-and-fan-out) (`dispatch-operations`, child Operations, `status.children[]`)
- [ ] Permission-filtered discovery (`vela operation list` currently assumes admin/cluster-admin)
- [ ] Re-execution: `status.steps[].attempts[]`, restart-by-step, idempotency contract — right now one `Operation` is one run to completion, no retry, and a resolution race right after creating the `OperationTemplate` is fatal
- [ ] Multi-cluster: `spec.clusters`, per-cluster dispatch (context is built to take a resolved cluster as a parameter already, so this is meant to be a loop over that call later, not a signature change)
- [ ] Option 3 context injection (`$( )` expressions, `spec.sources[]`)

See the [KEP](https://github.com/guidewire-oss/kubevela/blob/design/kep-2.15-operations/design/vela-core/keps/2.15-operations/README.md) for the rationale and full model behind each row above.

## Running the tests

The e2e test lives at `test/e2e-test/operation_test.go` and runs against a real cluster, same as the rest of that suite. Prereqs:

- a cluster (we use k3d) with this branch's KubeVela installed
- your kubeconfig pointed at it

```bash
ginkgo -v --focus "Operation" ./test/e2e-test/...
```

It restarts a `webservice` Component's Deployment through an `Operation`, and checks that:

- the `OperationTemplate` in `vela-system` is resolvable from the app's own namespace
- the Operation only reaches `Succeeded` once the step's Job actually completes
- `Operation.spec.parameters.reason` reaches the Deployment (via `context.operationParams`)
- the step read the Deployment's live status (via `context.output`)

vela-system fixtures (the `WorkflowStepDefinition`, `OperationTemplate`, and the restart Job's RBAC) are created fresh and torn down in `AfterEach`, so the suite is safe to re-run without manual cleanup.

## Manual CLI walkthrough

Reproduces the same scenario by hand with `kubectl` and the `vela operation` CLI. Run from the repo root. Prereqs:

- a cluster (we use k3d) with this branch's KubeVela installed
- a `vela` binary built from this branch

```bash
export VELA=/path/to/your/local/vela  # binary built from this branch
export NS=operation-poc-manual
kubectl create namespace "$NS"

# template + RBAC, packaged in vela-system
kubectl apply -f test/e2e-test/testdata/operation/vela-system/

kubectl apply -n "$NS" -f test/e2e-test/testdata/operation/app.yaml
kubectl rollout status -n "$NS" deployment/webservice

# discover, then invoke and wait for completion
$VELA operation list -c operation-app/webservice -n "$NS"
$VELA operation run restart-webservice -c operation-app/webservice -n "$NS" \
  --param reason=manual-runbook-restart
```

### Verify

```bash
# --param reason reached the Deployment via context.operationParams
kubectl get deployment -n "$NS" webservice -o jsonpath='{.metadata.annotations.operation\.oam\.dev/restart-reason}{"\n"}'

# the Deployment was actually restarted
kubectl get deployment -n "$NS" webservice -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}{"\n"}'

# the step read the Deployment's live status via context.output
kubectl get pods -n vela-system -l workflow.oam.dev/step-name=webservice-restart \
  -o jsonpath='{.items[*].spec.containers[0].env[?(@.name=="READY_REPLICAS")].value}{"\n"}'
```

### Cleanup

```bash
kubectl delete namespace "$NS"
kubectl delete -f test/e2e-test/testdata/operation/vela-system/
```
