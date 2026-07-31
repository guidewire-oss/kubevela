# Plan: infer the cache key at build time

Date: 2026-07-31
Branch: `feature/source-definitions`
Status: **plan — agreed, not started**

## What changes

`storage.key` stops being author-supplied and becomes generated. `vela def` reads the
template, infers which `context` fields the resolution depends on, and emits the key
expression into the YAML it produces. Admission recomputes the inference and rejects a
mismatch, so the generated artifact cannot be edited or hand-written around.

```
   author writes CUE                vela def                    applied YAML
┌────────────────────┐        ┌──────────────────┐        ┌────────────────────┐
│ schema: {...}      │        │ infer context    │        │ storage: {         │
│ storage: {         │───────▶│ usage from the   │───────▶│   key: "x-\(...)"  │◀── generated
│   storageTTL: 15m  │        │ template         │        │   storageTTL: 15m  │◀── authored
│ }                  │        └──────────────────┘        │ }                  │
│ template: {...}    │                                    └────────────────────┘
└────────────────────┘                                              │
         ▲                                                          ▼
         │                                              ┌────────────────────┐
    rejected if it                                      │ admission          │
    supplies storage.key                                │ re-infers, rejects │
                                                        │ a mismatch         │
                                                        └────────────────────┘
```

The runtime is unchanged: the resolver evaluates `storage.key` exactly as it does today.
Inference never runs in the cluster.

## Why

Author-written keys made the author responsible for three things the toolchain can do
better, with no signal when any was missed:

| Problem | Under inference |
|---|---|
| Key must be a legal object name | generated, legal by construction |
| Key must interpolate successfully | generated from fields the template already reads |
| Key must discriminate on everything the output depends on | derived from exactly what the template reads |

## Division of responsibility

| Concern | Owner |
|---|---|
| Context dimensions in the key | inferred by `vela def`, generated into `storage.key` |
| Properties | hashed by the resolver, appended at resolve time |
| `storageTTL`, `onStaleFailure` | author, in `storage:` |
| Schema/logic drift | read path: compare the entry's `config.oam.dev/type` label against the current ConfigTemplate, treat a mismatch as a miss |
| Tamper resistance | validating webhook re-infers and compares |

The final cache identity is `<generated storage.key>-<propertiesHash>`. `storage.key` is
the context portion only — worth stating because reading it suggests otherwise.

## Steps

Each step is a commit. Tests come first where the behaviour is assertable.

| # | Step | Test |
|---|---|---|
| 1 | Inference: template → ordered dimensions | unit, table-driven over context usage |
| 2 | Dimensions → key expression | unit, exact generated strings |
| 3 | Hook into the shared CUE→object conversion | unit: converting a demo definition yields the expected key |
| 4 | Regenerate the five in-repo definitions and the e2e fixtures | existing suites stay green |
| — | *(`deployment-namer` needs no rewrite: it reads `context.name`, so inference keys on it)* | |
| 5 | `vela def` rejects an author-supplied `storage.key` | unit: informative message naming the field as computed |
| 6 | Validating webhook: re-infer and reject a mismatch | unit, then live `kubectl apply` of a tampered YAML |
| 7 | e2e coverage | focused suite green against a cluster |

## Two decisions needed before starting

### 1. Step order — the original sequence breaks the repo in the middle

Rejecting an author-supplied key *first* invalidates every definition in the repo,
because all five currently supply one. Everything between that commit and the
regeneration commit has a broken build and a red e2e suite, which defeats the point of
committing at checkpoints.

The order above therefore does inference and generation first, regenerates the
definitions, and only then adds the rejection — so every commit is a working checkpoint.

### 2. Version skew on upgrade

If inference changes in a later release — a newly classified context field, an ordering
fix — every SourceDefinition YAML already committed to git carries a stale key. GitOps
re-applies it continuously, so admission starts rejecting objects that were valid
yesterday and the cluster cannot converge until every definition is regenerated.

| Option | Cost |
|---|---|
| **Strict** — reject on mismatch | an inference change becomes a breaking change needing coordinated regeneration |
| **Warn and correct** — accept, stamp the right value | reintroduces GitOps drift during the skew window |
| **Versioned** — stamp `cacheKeyVersion`, strict within a version, accept across one | more machinery; upgrades stay boring |

Leaning strict with the version stamped, since a stale key costs a cold cache entry
rather than incorrect data — but this is easier to decide now than after the first
upgrade.

## Still to specify (blocks step 1)

- **Ordering table**: which context fields contribute, in what order (broad → narrow), so
  the generated key is deterministic.
- **Keyword classification**: every constant in `pkg/cue/process/keyword.go` is keyed,
  not-key-relevant, or forbidden in a source template. Enforced by a test that fails when
  a new field is added without a decision — the guard that keeps inference honest.
- **Indexed reads**: `context.appLabels["x"]` contributes that label's value; a dynamic
  index is not statically knowable and should be rejected.
- **Overflow**: what happens when the assembled key would exceed 253 characters.

## Interaction with what is already committed

`36ff4886b` made `storage.key` mandatory and validated it. That stays correct for the
*applied object* — the webhook still requires a key and still validates its characters.
What changes is where the key comes from. The CLI gains the opposite rule for the
*authored CUE*: supplying one is an error.

Two artifacts, two rules:

| Artifact | Rule |
|---|---|
| authored `.cue` | `storage.key` must be **absent** |
| applied YAML | `storage.key` must be **present** and match inference |

---

# Specification: inference v1

## Versioning: content-addressed rules

The classification and ordering live in an embedded CUE data file, not in Go. Go scans
the template's AST and assembles the key; the CUE file decides which fields count and in
what order. Policy is the part that needs reviewing and diffing; mechanism is the part
that needs good errors.

**The version identity is the hash of that file.** No integer to bump by hand, and no way
to edit the rules without the identity changing:

```yaml
metadata:
  annotations:
    definition.oam.dev/cache-key-rules: "a3f9c21b"
```

Generation uses the latest rules; **validation loads the rules the object was stamped
with**, so an inference change never invalidates YAML already committed. The registry is
a `go:embed` over a directory of historical rule files, keyed by hash - which makes
"versions are never removed" literal and self-enforcing: to validate an object stamped
`a3f9c21b`, the binary must still contain the file that hashes to it.

The cost is that every historical rules file ships forever. A couple of KB each, so
negligible for years. Retire one only if it is found to be *unsafe*, never for tidiness.

## Context classification

Every constant in `pkg/cue/process/keyword.go` is classified. A field that is not
classified is **rejected** in a source template: failing closed means a context field
added later cannot silently become an unkeyed dependency, which is the one failure that
serves one consumer another's data.

### Keyed — contribute to the key, in this order

Broad to narrow, so keys group by prefix when listed.

| Order | Field | Contributes |
|---|---|---|
| 1 | `cluster` | cluster name |
| 2 | `clusterVersion` | version string |
| 3 | `namespace` | namespace |
| 4 | `appName` | application name |
| 5 | `appRevision` / `appRevisionNum` | revision identity |
| 6 | `publishVersion` | publish version |
| 7 | `workflowName` | workflow name |
| 8 | `appLabels["k"]` | that label's value, entries sorted by key |
| 9 | `appAnnotations["k"]` | that annotation's value, entries sorted by key |
| 10 | `name` | consuming component's name |
| 11 | `componentType` | consuming component's type |
| 12 | `revision` | component revision |
| 13 | `replicaKey` | replica key |

Consumer-level fields sort last, so a source that does not read them keeps a key shared
across components.

### Forbidden — reading these fails admission

*Policy context.* Not populated during component render; reading them yields nothing
useful and implies a surface where `fromSource` does not resolve.

`policyName`, `policyType`, `policyRevisionName`, `policyRevision`, `policyRevisionHash`

*Internal plumbing.* Not stable identifiers, and several are the source machinery itself -
`appSourceCacheStore` is a live Go object rather than data.

`appComponents`, `appWorkflow`, `appPolicies`, `components`, `artifacts`,
`appSources`, `appSourceTypes`, `appSourceTemplates`, `appSourceSensitivePaths`,
`appSourceCacheStore`

### Not context

`output`, `outputs`, `parameter`, `outputSecretName` — `parameter` is hashed separately;
the rest have no meaning in a source template.

## The invariant

An earlier draft forbade consumer identity outright, to stop a template depending on
something the key ignored. Inference removes that failure by construction, so the
prohibition bought nothing and blocked a legitimate pattern - a component name used as
the identifier a source queries by. The invariant is therefore not that output is
independent of the consumer, but:

> A source's output may depend on the consumer, and when it does, the cache key says so.

Which is the better kind: enforced by construction rather than by prohibition.

**Residual, not blocking:** `context.name` is the *application* name in a workflow-step
context and the *component* name in a component or trait render. That is inert today,
since `fromSource` does not resolve in workflow steps and both admission and the parser
reject it there. When that surface is added, either classify `name` as forbidden for it
or introduce an explicit `componentName`.

## Generated key

```
<definitionName>[-<dimension values in the order above>]
```

Emitted as a CUE interpolation, so the runtime evaluates it unchanged:

```cue
storage: {
  key: "cluster-lookup-\(context.cluster)"
}
```

The resolver appends the properties hash, so the full identity is
`<storage.key>-<propertiesHash>`. A source reading no context yields a bare
`<definitionName>`, shared everywhere.

## Rules

- **Indexed reads** contribute the value at a literal index. A dynamic index
  (`context.appLabels[parameter.k]`) is not statically knowable and is rejected.
- **Aliasing is followed**: `_c: context.cluster` used later still counts, because the
  whole template is scanned rather than just `output:`.
- **Over-inclusion is accepted**: a field read only in an `errs:` message still keys. The
  cost is a narrower cache, never a wrong one.
- **Overflow**: if the assembled key would exceed 253 characters, hash the dimension
  portion. Deterministic, still unique, and only degrades readability in the rare case.
