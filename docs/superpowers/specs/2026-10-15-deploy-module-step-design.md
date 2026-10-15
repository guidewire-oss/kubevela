# Placement-driven `deploy-module` step (spike PoC)

Status: PoC, uncommitted. Evaluates Option B of the module multicluster spike
in the shape agreed with the author: keep the inner `module-<name>` Application
(it owns tracking), and let a new workflow step place its tiers.

## Shape

- User Application: `type: module` component, new optional parameter
  `clusters: [...string]` (runtime cluster names).
- Module files may annotate any auxiliary object
  `modules.oam.dev/placement: hub|runtime`. Absent means `hub`, so existing
  modules render exactly as before.
- `RenderApplication` splits each auxiliary tier into a hub tier (existing name)
  and a runtime tier (`<tier>-runtime`). The next tier depends on both. When no
  object is runtime, the output is unchanged (no workflow, no new tier).
- When a runtime tier exists, the inner Application gets one workflow step
  `deploy-module` with `clusters`. Rendering fails if runtime objects exist and
  `clusters` is empty.
- Definitions always go to the hub; the placement annotation is stripped from
  them during stamping.
- `deploy-module` step: loads components with `oam.#LoadComponetsInOrder`, and
  applies each one with `oam.#ApplyComponent`. A component whose objects carry
  `placement: runtime` is applied once per cluster in `clusters`; every other
  component is applied to the hub (`cluster: "local"`).

## Answers this PoC gives to the spike's questions

1. Annotation survival: `toObjects` and `stampIdentity` deep-copy metadata, so it
   survives; definitions are stripped on purpose.
2. Cluster names: passed as a component parameter (the inner app cannot see the
   user's topology policy).
3. Install order: tiers keep their `dependsOn` chain; `LoadComponetsInOrder`
   preserves it for the step.
4. Definitions: hub only, regardless of annotation.
5. Lifecycle: in an Application workflow `kube`/`oam` applies go through
   `AppHandler.Dispatch` -> resource keeper, so they are tracked per cluster in
   the inner Application's ResourceTracker. GC on a dropped object, and health
   across clusters, still need a cluster run to confirm.
6. Validation/publish: no change in the PoC; see the findings list in the PoC
   write-up.
