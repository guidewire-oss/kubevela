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

## TODOs
- [x] #6 default: enforcement + unit tests
- [ ] #4 process-level LRU wrapper + wiring + tests
- [ ] #5 docs in KEP README (spoke via cluster: param)

## Lessons Learned
- The review's "forced Local pin at line 77" was a hallucinated citation; line 77
  is the `sourceCacheTTL` const. Always verify agent file:line claims against the
  code (as the memory guidance warns).
