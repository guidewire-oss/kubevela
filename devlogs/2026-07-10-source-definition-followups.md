# DevLog: SourceDefinition (KEP-2.16) follow-ups

Date: 2026-07-10 (updated 2026-07-13)
Branch: feature/source-definitions

## HANDOFF / STATUS (read this first)

Branch `feature/source-definitions`, working tree CLEAN. My session's commits sit
on top of `a1f26b01a` (first source commit is `779268de6`). Latest: `b042c27ab`.

**Environment note:** pre-existing vendoring drift (go.mod pins kubevela/pkg
v1.11.0 & workflow v0.7.0; vendor has v1.11.1 & v0.7.2). NOT from this branch.
Build/test with `-mod=mod`. Unit test cmd: `CGO_ENABLED=0 go test -mod=mod ./pkg/...`.
Exclude etcd suite with `-run 'Test[^A]|TestA[^P]|TestAP[^I]'` (TestAPIs needs etcd).
User runs the controller locally via `make run` with flags:
`--feature-gates=MultiStageComponentApply=true --feature-gates=EnableCueValidation=false
--application-re-sync-period=30s --use-webhook=true --webhook-port=9445
--webhook-cert-dir=./k8s-webhook-server/serving-certs --leader-elect=false`.
This container CANNOT reach the user's k3d cluster; user runs cluster checks.

**What's DONE & committed this session (all with unit tests, all green):**
1. Admission validation of source contracts (paths vs schema, forward-only order,
   RBAC SAR) + `default:` required only when optional source field feeds a
   REQUIRED target (target-aware, `e672a77f7`).
2. Input-contract validation: source `properties` vs the definition's
   `parameter:` block, per-field errors, + `fromSource` output-type vs
   component/trait target-parameter type, via CUE AST (`680e393a5`, `389de0475`).
3. Shared process-level LRU (Layer 1) keyed by storage.key; TTL capped at
   min(LRU 30s, storageTTL) so short TTLs aren't masked (`0dd69bc12` + `b042c27ab`).
4. Cache Config label = live ConfigTemplateRef name (`8932a496b`).
5. Skip schematic validation for fromSource params at admission (`7bbe53da9`).
6. Source-change re-dispatch, OPT-IN via `app.oam.dev/autoUpdate:"true"` OR
   `app.oam.dev/autoUpdateSources` ("true"/"*"/comma-list of source names).
   Per-source hash stamped on workload (`source.oam.dev/resolved-hash`);
   dispatcher gate re-dispatches when a selected source's hash changed
   (`3803a9155`, `d8b598532`).
7. ROOT-CAUSE fix (`b042c27ab`): a succeeded workflow short-circuits
   (workflow.go allRunnersSucceeded) so the apply-component step never re-runs;
   StateKeep re-applied the stale RT manifest. `refreshSourceDrivenComponents`
   (application_controller.go, in the WorkflowStateSucceeded path, before
   stateKeep) re-invokes applyComponentFunc per placed instance
   (status.Services -> cluster/ns) for opted-in fromSource components; the
   per-source-hash gate makes it a no-op when unchanged. Uses RT upsert-by-key
   (idempotent, confirmed no RT-concept impact).
8. Examples: `examples/source-definition-demo/{definitions,apps,resources}/` +
   README. Sources: cluster-lookup (reads cluster-info CM), tenant-data (ns
   labels), get-random (polls random.org over HTTPS, plain-text int),
   deployment-namer (chained: assembles <region>-<zone>-<dept>-<tenant>-<comp>).
   App random-deployment ties them together, replicas from get-random, opted in
   with autoUpdateSources:"true".

**OPEN / NEXT:**
- [ ] USER to verify live: after rebuild+restart `make run`, the demo
      Deployment replicas should re-roll each ~reconcile window (storageTTL 10s,
      resync 30s). Last root-cause fix (`b042c27ab`) is UNVERIFIED against a live
      cluster. If still stuck, next dlv breakpoint: inside
      refreshSourceDrivenComponents -> does apply() reach generator.go:308 now?
- [x] FOLLOW-UP BUG (DONE, `errs:` resolver support): source resolver now
      evaluates the authored `errs:` field. Extracted a shared
      `extractUserErrors(val, entityType, entityName)` helper (dedups the
      workloadDef/traitDef `Complete()` decode blocks) that reads `errs: []string`,
      warns+skips on malformed, and filters blank entries. In `resolve()`, after
      the source template compiles and before decoding `output`, a non-empty
      `errs:` is a hard failure ("source definition X reported errors: ...") set
      on the per-source status, and is subject to the same stale-cache fallback
      (UseStale policy serves the cached value with a "refresh reported errors"
      message). Tests: TestResolveSourceErrsFieldFails,
      TestResolveSourceErrsFieldEmptyIsIgnored (green). Full pkg/cue/definition
      suite green.
- [ ] Minor: refreshSourceDrivenComponents re-renders all fromSource components
      when enabled; the per-source LIST scoping is enforced only at the dispatch
      gate (correct, but a wasted render for non-selected sources). User said
      "fine as-is."
- [ ] KEP prose still references a `phase` status field and (in a couple of
      spots) may lag the shipped model; direction-change note added, kept per
      user request. Pre-PR: `make manifests generate` sanity check; consider
      squashing the autoUpdate add/remove churn (51a20ede1 vs 3803a9155).

## Objective

Address three gaps found in the KEP-2.16 review:

- **#4 Shared in-memory LRU cache.** Current "Layer 1" is a per-render map on
  `sourceResolver` (recreated every reconcile, no eviction, no cross-Application
  sharing). Replace with a process-level LRU keyed by the resolved `storage.key`,
  sitting in front of the Config/Secret store, so the cache is shared across
  Applications and survives across reconciles.
- **#5 hub/spoke scope.** Decision: documentation only. Investigation showed the
  resolver does NOT force `multicluster.Local` (the review's claim was wrong).
  Source templates compile against the process context, and the vendored CueX
  `kube` provider already honours a `cluster:` field in its params
  (`kube.go:49,74`). So spoke resolution is already achievable by the definition
  author; this is a docs task, not a code change.
- **#6 `default:` admission enforcement.** Reject an Application at admission when
  a `fromSource` path names an OPTIONAL schema field and the target parameter is
  required but no `default:` is supplied. Extend the existing `ValidateSources`
  webhook stage.

Deferred / not in scope this pass: the KEP-referenced "reusable LRU abstraction
from the Helm renderer feature" does not exist in the tree. Per decision, ship a
self-contained LRU now; refactor to the shared abstraction later if it lands.

## Progress

### Investigation
- Confirmed no forced `multicluster.Local` pin in source resolution
  (`template.go` compiles against `r.ctx.GetCtx()` at :922/:994).
- Confirmed `hashicorp/golang-lru` is NOT vendored; `k8s.io/utils/lru` IS.
- Confirmed both Cursor plans (admission validation, cache semantics) are fully
  landed; neither covered #6.

## Decisions Made
- LRU: self-contained now, `k8s.io/utils/lru` (already vendored) with a manual
  short in-memory TTL wrapper. Avoids adding a new dependency.
- Spoke scope: docs only.

### #6 default: enforcement (DONE)
- `validation_sources.go`: track `HasDefault` on each fromSource ref (map form
  with a `default:` key); add `sourceSchemaValidator.IsOptionalPath` (iterates
  parent fields with `cue.Optional(true)` and checks `iter.IsOptional()`).
- Reject when path is optional and no default supplied.
- BONUS FIX: `HasPath` previously used `LookupPath`, which does NOT resolve
  optional fields — so any use of an optional schema field was mis-rejected as
  "not declared". `lookup()` now falls back to field iteration for optionals.
- Tests: added 3 cases (optional-without-default rejected; optional-with-default
  accepted; required-without-default accepted). All `TestValidateSources` pass.

### #4 shared process-level LRU (DONE)
- New `pkg/cue/definition/source_cache_lru.go`: `lruSourceCacheStore` wraps any
  `SourceCacheStore` with a package-level singleton `k8s.io/utils/lru` cache
  (size 512), keyed by the resolved `storage.key`. Fixed 30s in-memory TTL
  (Layer 1), independent of per-source `storageTTL` (Layer 2).
  - Read: Layer 1 hit within TTL served fresh; miss falls to delegate; only
    fresh (non-stale) delegate hits are promoted (stale values keep flowing
    through the resolver's onStaleFailure path).
  - Write: persists via delegate, then populates Layer 1.
- Wired in `pkg/appfile/appfile.go` right after the store fallback, so BOTH the
  controller's Config-API store and the Secret fallback are fronted by the LRU.
- Because the LRU is a process singleton keyed by storage.key, entries are
  shared across Applications and survive across reconciles (KEP requirement).
- Tests: `source_cache_lru_test.go` (5 cases incl. cross-store sharing). Pass.
- Note: the KEP-referenced "reusable LRU abstraction from the Helm renderer"
  still does not exist; this is self-contained per decision. `k8s.io/utils/lru`
  chosen (already vendored) — no new dependency.
- `pkg/appfile/dryrun` BeforeSuite fails on missing etcd binary (pre-existing env
  limitation, same as TestAPIs); unrelated to this change.

### #5 hub/spoke scope + KEP doc reconciliation (DONE)
- Rewrote "Resolution Scope: hub vs spoke" to reflect reality: the CueX kube/
  ex.#Read provider already accepts a `cluster:` field (kube.go:49,74) and routes
  to that cluster via the gateway; empty → hub/local. So spoke resolution is
  author-driven (set `cluster: context.cluster` + key by context.cluster).
  `attributes.scope` documented as advisory only; controller builds no spoke
  client. Added hub and spoke read examples; fixed the full cluster-config-reader
  example to include `cluster: context.cluster`.
- Doc reconciliation with shipped code (user direction: document the change, keep
  the KEP content):
  - Fixed two stale `definition:` source bindings in the status example → `type:`
    (the confirmed KEP mistake).
  - Added an "Implementation note (direction change)" at the top of Application
    Status: `phase` field was dropped in favour of `expiresAt` + `message`;
    phase values in the doc are logical states inferable from those two. Shipped
    status shape documented.
  - Corrected the Layer 1 description: now accurately a shared process-level
    singleton LRU on k8s.io/utils/lru (not the not-yet-existing Helm abstraction).

## TODOs
- [x] #6 default: enforcement + unit tests
- [x] #4 process-level LRU wrapper + wiring + tests
- [x] #5 docs in KEP README (spoke via cluster: param) + phase/binding reconcile

### e2e run: admission-webhook bug (fromSource rejected at apply) — FIXED

First real e2e run against a live cluster (from the user's Mac) surfaced 4
failures. Root cause investigation:

- 3 of 4 failed at ADMISSION with `parameter.image: conflicting values string
  and {fromSource:"img.image"} (mismatched types string and struct)`.
- Traced: webhook `ValidateCUESchematicAppfile` (`validate.go`) →
  `EvalContext` → `workloadDef.Complete()` type-checks the component/trait CUE
  template with the RAW `{fromSource: ...}` node still in params. CUE rejects it
  against the declared type (`image: string`, `replicas: int`).
- This is inherent: the KEP says admission validates fromSource PATHS only and
  must NOT resolve (resolution needs I/O, done at reconcile). But schematic
  validation was type-checking the unresolved node.
- NOT a regression from my 3 commits (they don't touch validate.go). The e2e
  suite is a manual/local gate that had never actually run green on this branch;
  the admission path was simply never exercised before.

Fix (user chose "skip param validation when fromSource present", matching the
existing workflow-supplied-params skip precedent):
- `validate.go`: new `hasFromSourceParams(params)` walks params for a fromSource
  key at any depth. In `ValidateCUESchematicAppfile`:
  - component with fromSource → skip `ValidateComponentParams` + the component
    template eval + its traits (traits need the component's pCtx). `continue`.
  - trait with fromSource → skip that trait's `EvalContext`.
- Paths are still validated structurally by the webhook's ValidateSources stage
  (my #6 work), so coverage is not lost — only the value type-check is deferred.
- Tests: `TestHasFromSourceParams` (6 cases). Existing validation suites green.

The 4th failure (`configTemplateRef not ready`, 60s timeout) is separate and
likely a stale deployed image / SourceDefinition controller not running the new
reconciler in-cluster — NOT reproducible from this container (cluster lives on
the user's Mac). Left for the user to confirm against a freshly built image.

### e2e run 3: wrong cache config-type label — FIXED

After the admission fix, the `storage key policy` test got much further (created
the cache secret) but failed:
`unexpected config type label: "stale-image-source"` — the cache Secret's
`config.oam.dev/type` label was the RAW source type, not the versioned
ConfigTemplate name the test expected.

Root cause: `parseSources` (parser.go:503) wipes `sd.Status` on the appfile copy
(consistent with Component/TraitDefinition, so status doesn't leak into the
ApplicationRevision snapshot). But `sourceTemplateRefsByType` read
`ConfigTemplateRef` FROM that wiped copy, so it was always empty →
`templateName == ""` → the config store labeled the cache with the raw source
type. Deterministic, not a race (the test waits for ConfigTemplateRef before
creating the App, so the ref IS live in-cluster).

Fix: `sourceTemplateRefsByType(ctx, cli, af)` now fetches the LIVE
SourceDefinition via the client (app namespace, falling back to vela-system) to
read `ConfigTemplateRef`, instead of the status-wiped appfile copy. Call site in
application_controller.go passes logCtx + r.Client. The status-wipe convention is
preserved (snapshot stays clean); only the cache-label lookup changed source.
Tests: `TestSourceTemplateRefsByType` (system-ns ref, app-ns ref, no-ref skip,
missing skip) + nil-inputs. Pass.

## TODOs (post-e2e)
- [x] Admission fromSource skip (run 2)
- [x] Live ConfigTemplateRef lookup for cache label (run 3)
- [ ] User: rebuild image + redeploy, re-run e2e to confirm all 4 specs green

### Input-contract validation (CUE-AST) — Step A DONE

User: comparing source properties against the definition's parameter: schema is
essential; use CUE AST to check input type compatibility. Scope agreed:
per-field errors (not raw CUE errors); reject unknown fields; extend to
component/trait targets too (Step B).

Step A (source properties -> source parameter: block), all in validation_sources.go:
- `extractTopLevelBlock(template, name)` generalises schema extraction; new
  `loadSourceParameter` compiles the parameter: block to a `cueStruct`.
- `cueStruct` (lookup/has/kindAt) + `sourceSchemaValidator.KindAt` expose the
  declared CUE kind at a dotted path (optional-aware).
- `kindsCompatible` = kind-intersection (int<:number), permissive on constraints
  to avoid false positives, still catches string-vs-int. `kindName` for messages.
- `validateSourceInputs`: per source, flatten properties to leaf paths
  (`flattenLeafPaths`), and `checkInputLeaf` each: undeclared field -> per-field
  error; type mismatch -> per-field error. Literals use `jsonKind`; fromSource
  leaves take the referenced source schema output field's kind (chained type
  check WITHOUT resolving). Parameterless definition + any property -> error.
- Tests: `TestValidateSourceInputs` (6 cases). Existing `TestValidateSources`
  still green (open param blocks, matching types).

### Input-contract validation — Step B DONE (component/trait target types)

`validateFromSourceTargetTypes`: for each fromSource in a component/trait's
properties, the source's schema OUTPUT field kind must be compatible with the
consuming component/trait PARAMETER field kind at the same property path.
- `loadTargetParameter(ctx, ns, kind, defType)` fetches the Component/Trait
  definition (oamutil.GetDefinition, ns fallback), compiles the template with
  `WorkloadCompiler.CompileStringWithOptions(..., DisableResolveProviderFunctions{})`
  + BaseTemplate (static, no rendering/I/O), and returns the `parameter:` block
  as a cueStruct. FAIL-OPEN: any failure (def missing, template won't compile
  statically, no parameter block) -> nil -> check skipped for that target.
- kind-compatibility reused from Step A (`kindsCompatible`, `KindAt`).
- Schema-less sources skip the check (no output type known) — this is why the
  e2e trait test (scale-source has no schema:) is not falsely flagged.
- Tests: `TestValidateFromSourceTargetTypes` (4: string->string ok, string->int
  component fail, int->int trait ok, string->int trait fail). Full webhook suite
  green (excluding etcd TestAPIs).

Caveat noted: WorkloadCompiler tries to load external CUE packages (network call
to apiserver) during static compile; unreachable => logged, non-fatal, fail-open.
Same compiler admission already uses, so no new dependency profile.

### Negative-path e2e coverage (admission deny path)

The source e2e suite was entirely happy-path (all Should(Succeed())). Added a
`Context("rejects invalid source usage at admission")` with 5 focused negative
cases asserting `HaveOccurred()` + `ContainSubstring(<stable message fragment>)`,
following the existing requiredparam_validation_test.go pattern:
1. fromSource path not in schema
2. optional schema field consumed without default
3. forward source dependency
4. undeclared source property (Step A)
5. incompatible source property type (Step A)

Purpose: prove the webhook is WIRED and the deny path reaches the client
end-to-end — the class of bug that unit tests structurally cannot catch (the
earlier schematic-validation bug only surfaced via e2e). Logic per-branch stays
unit-tested; e2e adds one representative each.

Status: compiles (`go vet ./test/e2e-test` clean). NOT yet run against a live
cluster — requires the user to rebuild+redeploy the image (this container cannot
reach the k3d cluster). Marked unverified-against-live until the user runs it.

FIRST LIVE RUN: 2 passed (bad-path, no-default); 3 failed — but as a TEST bug,
not a product bug. The 3 failing cases used `Components: nil`, and the
Application CRD requires spec.components (json:"components", no omitempty), so the
API server rejected them with `spec.components: Required value` BEFORE the
webhook ran — ValidateSources never fired. Fix: added a shared `minimalComp()`
(a valid webservice component) to the forward-ref, unknown-prop, and bad-type
cases so they clear structural validation and reach ValidateSources. The webhook
accumulates all error groups (no short-circuit, validation.go:483-488) so the
source-level error still surfaces alongside the (passing) component check.

Boundary noted (not acting on it): because spec.components is structurally
required, a sources-only Application (no components) is not expressible today.
Fine for current KEP scope (fromSource consumed by components/traits); relevant
if the future "fromSource in workflow steps/policies" enhancement wants
sources-only apps.

### default-rule made target-aware (matches KEP)

Earlier the optional-field default check (from #6) was UNCONDITIONAL: any
optional source field consumed without a default was rejected, regardless of the
target. The KEP says a default is required only when an optional source field
feeds a REQUIRED target. Reworked to match the KEP:
- New `cueStruct.requiredAt(path)` -> (required, declared): required means the
  target field is declared, non-optional, and has no default.
- Moved the default check out of the main ValidateSources loop (which had no
  target knowledge) into the two target-aware passes:
  - `validateSourceInputs` (target = the source's own parameter: block)
  - `validateFromSourceTargetTypes` (target = component/trait parameter: block)
  Both now: reject only if source field optional AND no default AND
  target.requiredAt(path).required.
- Removed dead `fromSourceReference.HasDefault` field.
- Tests: the 3 old default cases in TestValidateSources relied on a target with
  no ComponentDefinition (fail-open) and were misleading; removed them. Added 4
  target-aware cases to TestValidateFromSourceTargetTypes (optional->required no
  default = reject; optional->required + default = accept; optional->optional no
  default = accept; required->required no default = accept). Full webhook suite
  green.
- Example comments (apps/app.yaml, README) corrected: the demo's costCenter
  feeds a free-form ConfigMap field (not a required parameter), so a default is
  NOT mandatory there; it is supplied as good practice only.

### Example: get-random (HTTP-polling source)

Added a demo where a SourceDefinition polls an in-cluster HTTP service:
- `resources/random-service.yaml`: python:3-alpine Deployment+Service, handler
  from a ConfigMap; GET /?min=&max= -> {"value":N}. No image build.
- `definitions/get-random.yaml`: uses vela/http #Get + encoding/json Unmarshal +
  strconv; min/max as parameters passed as query params; exposes value(int),
  valueString, min, max. storageTTL 30s, onStaleFailure "fail" (stale random is
  meaningless). Verified compiling under WorkloadCompiler.
- `apps/random-app.yaml`: binds get-random {min:10,max:20}, writes valueString
  into a ConfigMap (data must be strings; hence valueString not value).
Demonstrates caching a volatile source (bounds polling; re-roll via
`vela config delete get-random-10-20`).

### Example: random-deployment (all sources -> one Deployment)

Ties every source together into one raw Deployment:
- replicas 1-5 from get-random (int -> spec.replicas, no conversion)
- name <region>-<zone>-<department>-<tenant>-<component> from a new chained
  `deployment-namer` source (definitions/deployment-namer.yaml), because
  fromSource cannot concatenate across sources; namer takes the four values as
  inputs (fed via fromSource from cluster-lookup + tenant-data) and joins them
  with context.name (the component name).
- labels (region/zone/department/tenant/environment) read directly per field.
- apps/random-deployment.yaml; sources ordered rng, cluster, tenant, namer so
  namer's forward-only deps are satisfied.
Verified: namer compiles and yields "us-east-1-us-east-1a-platform-acme-web";
YAML parses; type/ordering/default admission rules satisfied.

Note: source templates see the component name as `context.name` (the KEP table
calls it context.componentName; shipped runtime uses context.name). Examples use
context.name.

### get-random reworked to random.org (dropped in-cluster service)

The in-cluster python service failed when the controller runs locally
(`make run`): dial error resolving random-service.source-demo.svc.cluster.local
(cluster DNS not reachable from a local process). Tried a serviceURL param +
localhost:3030 port-forward; user preferred dropping the service entirely.

Now get-random polls https://www.random.org/integers/?...&format=plain over
HTTPS (external endpoint, no in-cluster DNS problem; needs controller outbound
internet). random.org returns a bare integer with a trailing newline (NOT JSON),
so parse with strings.TrimSpace + strconv.Atoi (not json.Unmarshal). Verified
the endpoint returns e.g. "4\n" and the template compiles under WorkloadCompiler.

Removed: resources/random-service.yaml, apps/random-app.yaml (standalone service
demo). onStaleFailure changed to use-stale (external endpoint may blip; a stale
number beats failing). random-deployment app + README updated; no port-forward.

### FOLLOW-UP BUG: fromSource re-resolution not detected for healthy components

Traced while wiring the random-deployment demo. When a source re-resolves to a
NEW value (e.g. get-random rolls a different number), a HEALTHY component is not
re-dispatched, so the new value never reaches the cluster.

Root cause (pkg/controller/.../application/dispatcher.go):
- Dispatch gate (~:167): requiresDispatch = !isHealth || err || propertiesChanged
  || (!SkipApplyWorkload && isAutoUpdateEnabled).
- componentPropertiesChanged (:272) compares comp.Params against the snapshot's
  raw Properties. But comp.Params still holds the RAW {fromSource: "..."}
  directive: Complete() (template.go:143-149) resolves fromSource into a LOCAL
  paramFile for rendering and never writes resolved values back into comp.Params.
- So the raw directive == snapshot raw directive => propertiesChanged=false, and
  a healthy component only re-dispatches if autoUpdate="true".

Workaround in the demo: app.oam.dev/autoUpdate: "true" (forces dispatch every
reconcile). This is a side effect of autoUpdate (documented as "auto update when
it finds DEFINITION changes"), not a purpose-built trigger.

Proper fix (deferred): make change detection fromSource-aware -- compare the
RESOLVED values (or a hash of them) rather than the raw directive -- so a
re-resolved source value re-dispatches without autoUpdate. Touches the dispatch
path + tests.

Also note: even with autoUpdate, cadence is gated by the reconcile resync (5m
default) and the storageTTL + ~30s LRU floor.

FIXED (this pass): change detection is now fromSource-aware, OPT-IN and per-source.
- New oam.AnnotationSourceResolvedHash = "source.oam.dev/resolved-hash" (stores a
  JSON map source-name -> hash) and oam.AnnotationAutoUpdateSources =
  "app.oam.dev/autoUpdateSources".
- Opt-in: source-change re-dispatch fires only when autoUpdate=="true" OR
  autoUpdateSources is present. autoUpdateSources values: "true"/"*" = any
  consumed source; comma-list of source names = only those; absent/empty = off.
  (sourceAutoUpdateSelector parses this into matchAll/set/enabled.)
- Per-source: resolvedSourceHashes(comp) hashes each consumed source's
  ConsumedFields separately (read from comp.Ctx[SourceResolutionStatus],
  populated by Complete() before the dispatch gate -- confirmed via
  applyComponentFunc rendering the SAME wl passed to dispatcher.run).
  changedSources() diffs current vs the map read back from the live workload
  (liveResolvedSourceHashes); a source missing from live counts as changed.
  stampResolvedSourceHashes writes the JSON map before Dispatch.
- Gate: sourceValuesChanged = any CHANGED source that is also SELECTED (matchAll
  or in set), OR'd into requiresDispatch. Stable (no over-dispatch). The hash is
  stamped whenever the component consumes sources so a baseline exists even
  before opt-in.
- Only active under the MultiStageComponentApply feature gate (gate-off path
  dispatches unconditionally anyway).
- Demo: random-deployment carries app.oam.dev/autoUpdateSources: "true"; README
  documents the values.
- Tests: source_hash_test.go (plain Go, no envtest): per-source stable +
  value-sensitive hashes, changedSources, selector grammar (true/*/list/empty),
  stamp + live round-trip. All pass.

### ROOT CAUSE (debugged live): succeeded workflow never re-applies

Symptom: get-random re-rolls (Config Secret changes, confirmed) but the
Deployment's replicas stay at the first value; autoUpdateSources present.

Debugger (dlv, user): generator.go:307 (applyComponentFunc FACTORY) hits, :308
(the returned closure body) does NOT -> the apply-component workflow runner is
never executed on resync. Cause: workflow.go:128 `if allRunnersSucceeded return
Succeeded` -- a succeeded workflow short-circuits, so the component never
re-renders and resourceKeeper.Dispatch never re-records the manifest into the
ResourceTracker. StateKeep (the continuous drift loop) then faithfully re-applies
the STALE manifest stored in the RT (mr.Data), not a freshly-resolved value.
=> the earlier dispatcher-gate fix sits inside a layer that never runs post-success.

FIX (this pass): re-resolve + refresh the RT before StateKeep, so drift
enforcement carries the current value.
- application_controller.go: refreshSourceDrivenComponents() runs in the
  WorkflowStateSucceeded path, before r.stateKeep. For opted-in
  (autoUpdate/autoUpdateSources) apps with sources, it re-invokes
  handler.applyComponentFunc for each component that consumes fromSource, per
  placed instance from status.Services (svc.Cluster/svc.Namespace -> honours
  topology/override). apply() = render (fresh resolve) + resourceKeeper.Dispatch
  (re-records RT + applies). The per-source-hash gate in the dispatcher makes it
  a no-op when nothing changed. componentConsumesFromSource / rawContainsFromSource
  detect fromSource in component/trait properties.
- LRU masking fix: the shared 30s in-memory LRU could hide a changed Config value
  for up to 30s, defeating a short storageTTL. lruSourceCacheStore.Read now caps
  the in-memory entry lifetime at min(LRU ttl, storageTTL) via effectiveTTL, so a
  10s storageTTL is honoured. Tests: TTL-capped fall-through + TestEffectiveTTL.
- Kept the dispatcher-gate + per-source hashing/stamping (still correct and does
  the work when the workflow DOES run and inside the refresh re-apply).
- Removed the temporary klog debug line.

### 2026-07-13 - `errs:` resolver support (follow-up bug)
- The KEP documents an authored `errs:` block for SourceDefinitions (same idea as
  workloadDef/traitDef `Complete()`), but the source resolver never evaluated it,
  so a definition could declare a resolution invalid and be silently ignored.
- Added `extractUserErrors(val cue.Value, entityType, entityName string) []string`
  (template.go, above `resolve()`): reads `errs: []string`, warns+skips on
  malformed, drops blank entries. Refactored both `Complete()` decode blocks to
  call it (removes duplication; behavior preserved, blanks now filtered).
- `resolve()`: evaluate `errs:` immediately after the source template compiles and
  BEFORE decoding `output`. Non-empty => hard failure
  `source definition <type> reported errors: <joined>`, recorded on the per-source
  status (Phase=Failed). Placed inside the stale-cache-fallback ladder: with
  OnStaleFailure=UseStale + a stale cached value, serve the cache instead
  (message "refresh reported errors; serving stale cached value").
- Tests: TestResolveSourceErrsFieldFails (fails + asserts status message),
  TestResolveSourceErrsFieldEmptyIsIgnored (blank-only errs => success). Full
  pkg/cue/definition suite green (`CGO_ENABLED=0 go test -mod=mod ./pkg/cue/definition/`).

## Lessons Learned
- The review's "forced Local pin at line 77" was a hallucinated citation; line 77
  is the `sourceCacheTTL` const. Always verify agent file:line claims against the
  code (as the memory guidance warns).
