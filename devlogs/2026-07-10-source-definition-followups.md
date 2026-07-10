# DevLog: SourceDefinition (KEP-2.16) follow-ups

Date: 2026-07-10
Branch: feature/source-definitions

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

## Lessons Learned
- The review's "forced Local pin at line 77" was a hallucinated citation; line 77
  is the `sourceCacheTTL` const. Always verify agent file:line claims against the
  code (as the memory guidance warns).
