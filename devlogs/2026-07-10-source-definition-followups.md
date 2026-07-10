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

## Lessons Learned
- The review's "forced Local pin at line 77" was a hallucinated citation; line 77
  is the `sourceCacheTTL` const. Always verify agent file:line claims against the
  code (as the memory guidance warns).
