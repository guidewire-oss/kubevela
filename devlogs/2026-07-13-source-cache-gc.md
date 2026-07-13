# DevLog: Source cache & ConfigTemplate garbage collection
Date: 2026-07-13

## Objective
Auto-delete the two long-lived objects the SourceDefinition feature (KEP-2.16) leaves behind
and never cleans up:
1. Source **cache data Secrets** (`source-cache-*` in `vela-system`) — rendered source data that can be
   served stale.
2. Auto-generated schema **ConfigTemplates** (`config-template-source-<sd>-<hash>` ConfigMaps).

Requirement (from the user): delete stale cache entries only after no usage for **3× the TTL**, where TTL
comes from the source; only delete a ConfigTemplate when **no** SourceDefinition revision still uses it.
ConfigTemplates are shared when `name + schemaHash` collide (e.g. a schema reverted to a previous shape).

## Design decision: TTL-driven mark-and-sweep (not eager finalizer refcount)

We rejected eager per-delete cleanup (finalizer + bi-directional refcount + cluster-wide
ApplicationRevision scan) as fragile; its worst case (deleting a template an old ApplicationRevision still
needs) is worse than leaving inert junk. Instead:

- Periodic sweep (a manager `Runnable` ticker in the SourceDefinition controller, default 10m).
- The sweep is **context-free**: every decision is made from metadata stamped on the objects, plus the set
  of live SourceDefinition `Status.ConfigTemplateRef` names. No CUE re-evaluation, no ApplicationRevision
  scan.

Key facts that made this safe:
- TTL lives in the source CUE `storage.storageTTL` (default 15m), resolved only at render time
  (`pkg/cue/definition/template.go` `resolveCachePolicy`). A sweeper has no render context, so **TTL is
  stamped onto each cache Secret at write time** (`config.oam.dev/ttl`).
- The live render path carries the schema **inside the ApplicationRevision snapshot**
  (`pkg/appfile/template.go` `GetTemplateOfDefinition`), NOT via the ConfigTemplate ConfigMap. The
  ConfigTemplate is only linked from cache Secrets. => a ConfigTemplate is collectible once no surviving
  cache Secret and no live SD references it. No revision scan required for correctness.
- Stale data is served on four branches of `sourceResolver.resolve()` (all guarded by
  `found && stale && OnStaleFailure == use-stale`). `Write` only re-stamps `last-sync-at` on successful
  refresh, so `last-accessed` is a **separate** stamp on the stale-serve path.

## GC predicates
- Cache Secret: `now > last-sync + TTL` (stale) AND `now > effectiveLastAccess + 3×TTL`, where
  `effectiveLastAccess = last-accessed || last-sync-at`.
- ConfigTemplate: after the Secret sweep, delete a `config-template-source-*` ConfigMap not referenced by
  any surviving cache Secret (`config.oam.dev/template`) nor any live SD `Status.ConfigTemplateRef.Name`,
  and older than a grace window (`3×default TTL`) to avoid racing a just-created template.

## Metadata contract added
Constants in `apis/types/types.go`: `config.oam.dev/last-sync-at` (promoted from literals),
`config.oam.dev/last-accessed`, `config.oam.dev/ttl`, `config.oam.dev/template`,
`sourcedefinition.oam.dev/name`, `sourcedefinition.oam.dev/namespace`.
- Cache Secrets (both persistent stores) now stamp ttl/template/sd-name/sd-namespace + last-sync-at, and
  last-accessed on the stale-serve path (throttled to ≥ TTL/2 to bound write amplification).
- ConfigTemplate ConfigMaps stamp `sourcedefinition.oam.dev/name|namespace` labels.

## Changes
- `pkg/cue/process/handle.go`: `SourceCacheStore.Write` now takes `SourceCacheWriteMeta`; added optional
  `SourceCacheToucher` interface.
- `pkg/cue/definition/template.go`: thread meta through `writeSourceCache`; `touchSourceCache` on the four
  stale-serve branches; shared exported helpers `ApplySourceCacheMetadata`, `ShouldTouchSourceCache`,
  plus `sourceCacheTemplateName` (mirrors the controller's `buildSchemaTemplateName`).
- `pkg/cue/definition/source_cache_store_secret.go`, `.../application/source_cache_store_config.go`:
  stamp metadata; implement `Touch`.
- `pkg/cue/definition/source_cache_lru.go`: forward meta; `Touch` delegates.
- `.../core/sources/sourcedefinition/sourcedefinition_controller.go`: stamp SD identity on template;
  register `cacheGCRunnable`; RBAC for secrets + configmaps delete.
- `.../core/sources/sourcedefinition/cache_gc.go`: NEW sweeper.

## Tests
- `cache_gc_test.go`: `TestShouldCollectCacheSecret` (predicate table), `TestSweepSourceCache` (fake-client
  end-to-end: deletes stale/unreferenced, keeps fresh, keeps SD-referenced, keeps young orphan, ignores
  non-source velacore-config secrets). ✅
- `source_cache_metadata_test.go`: metadata stamping, throttled Touch, template-name stability. ✅
- Existing `pkg/cue/definition`, `sourcedefinition`, `config` suites: ✅ (re-run after signature change).

## Correction: do NOT clobber config.oam.dev/type (found via e2e)
The first cut of `ApplySourceCacheMetadata` set `config.oam.dev/type = sourceType` unconditionally,
"un-overloading" the label per the plan. This BROKE the existing contract: the config-API store's
`ParseConfig` sets `config.oam.dev/type = <ConfigTemplate name>`, and both the config factory's
`ErrChangeTemplate` guard and the e2e assertion (`source_definition_test.go` ~line 324) depend on it.
The e2e "creates source cache using storage key policy" failed with
`unexpected config type label: "stale-image-source"`. Fix: `ApplySourceCacheMetadata` is now strictly
additive — it only sets `config.oam.dev/type` when empty (the Secret-store path, which has no template),
preserving the template name on the config-API path. The dedicated `config.oam.dev/template` annotation
still carries the template name for the sweep; `type` is left as the original design intended.

## E2E cleanup
`test/e2e-test/source_definition_test.go` `AfterEach` now (1) deletes the test namespace FIRST — so its
Applications stop and cannot re-write cache entries mid-cleanup — then (2) deletes, from `vela-system`,
every ConfigMap and Secret labelled `sourcedefinition.oam.dev/namespace = <test ns>`, wrapped in an
`Eventually` that re-lists until none remain. Previously only the namespace was deleted, leaking
`config-template-source-*` ConfigMaps and `source-cache-*` Secrets into `vela-system` every run
(confirmed via `vela config list`, which surfaces the config-API-backed Secrets that `kubectl get
secrets` name-greps missed). The label-based sweep is reliable because `CreateOrUpdateConfig` applies
`cfg.Secret` verbatim (labels preserved) and the SD reconciler stamps the same label on ConfigTemplates.
Note: the label only lands via the new controller binary, so pre-change stragglers need a one-time manual
purge (`vela config delete` / delete the labelled objects).

## TODOs / follow-ups
- Sweep interval/enable are reconciler options with sane defaults; expose as controller flags on
  `oamctrl.Args` if operators need to tune them.
- DefinitionRevision generation for SourceDefinition is still unimplemented; template GC keys off live SD
  refs + surviving Secrets, so it stays correct regardless.

## Lessons
- The ConfigTemplate ConfigMap is not on the live render path — that observation is what let us drop the
  expensive ApplicationRevision scan entirely.
- LSP `UndeclaredName` diagnostics after an sed-based export rename were stale; `go build`/`go test` were
  the source of truth and passed.
