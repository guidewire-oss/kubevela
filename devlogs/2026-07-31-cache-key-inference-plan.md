# Plan: infer the cache key at build time

Date: 2026-07-31
Branch: `feature/source-definitions`
Status: **DONE** — steps 1-7 implemented and verified against a live cluster
(9 of 9 source e2e specs, then 13 of 13 once the derived-key specs were added).
Superseded in part by the amendment at the end of this file.

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
| 5 | `vela def` accepts a matching `storage.key`, rejects a mismatch | unit: message names both the wrong value and the expected one |
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

One rule, both artifacts:

> `storage.key` equals what inference produces, and is written for you when absent.

An earlier draft had the authored CUE reject any key and the applied object require
one. That breaks the round-trip: `vela def get` emits the stored template, key
included, so `get`, edit, `apply` would fail on a file the tool itself produced.
Accepting a matching key costs nothing - a wrong one is still rejected, naming the
value that was expected - and collapses two rules into one.

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

---

# Amendment: the source context is built from the rules

Date: 2026-07-31
Status: **plan — agreed, not started**

Three changes that turned out to be one change.

## What changes

### 1. `context.name` becomes the binding entry

Everywhere else in KubeVela, `context.name` is the *instance* being rendered - `api`,
not `webservice`. For a source it is currently the **consuming component's** name,
because the resolver compiles against the component's process context wholesale.

```yaml
sources:
  - name: backstage            # <- context.name should be this
    type: backstage-component  #    (the definition name is already the key prefix)
```

Verified against a live cluster before proposing this: a source reading `context.name`,
bound as `BINDING-ALIAS` and consumed by `component-called-web`, produced the cache entry
`whoami-component-called-web`.

### 2. Consumer identity is not readable; pass it as a property

`componentType`, `revision` and `replicaKey` leave the keyed set, and no `componentName`
is added. A source that needs to know its consumer takes it as a property:

```yaml
sources:
  - {name: namer-web, type: deployment-namer, properties: {component: web}}
  - {name: namer-api, type: deployment-namer, properties: {component: api}}
```

Bindings are Application-level, so a source whose output varies per component needs one
binding per component regardless. Making it a property says so at the binding site rather
than hiding it in the template, and the properties hash gives each its own entry.

What this buys is worth more than the convenience it costs:

> A source's output depends only on Application context and its properties - never on
> which consumer asked.

Sources become portable across surfaces. When `fromSource` extends to workflow steps and
policies there is nothing to reclassify, no absent-versus-empty question for a component
field, and no per-surface rules - because nothing was ever consumer-scoped.

This reverses a call made earlier the same day. Consumer identity was un-forbidden on the
grounds that inference made reading it *safe*, which was true and beside the point: safe
is not the same as warranted, and the Application-level binding is what tips it.

### 3. One list, not two

`forbidden` is dropped. Both categories rejected; the only difference was the message, and
under fail-closed an unclassified field is already refused.

The exhaustiveness test goes with it. Its stated justification - that an unclassified
field could "silently become an unkeyed dependency" - was wrong: an unlisted field is
rejected, so a template cannot read it, so it cannot become a dependency. It protected
usability, not correctness, and it imposed friction on anyone adding a context field for
unrelated reasons.

A `notes` map survives for message quality, with no completeness obligation:

```cue
keyed: {cluster: order: 1, ...}

notes: {
  policyName: "not populated during a component render"
}
```

The policy hash then covers `keyed` alone.

## Why these are one change

Point 1 requires the resolver to stop reusing the component's context, since `name` has to
mean something different. Once a source-specific context is being built, constructing it
**from the keyed list** is nearly free - and it makes points 2 and 3 structural rather
than merely validated:

| Layer | Before | After |
|---|---|---|
| Admission | rejects a template reading a non-keyed field | unchanged |
| Runtime | the field is present and readable; a bypassed webhook means an unkeyed dependency | the field is not in the context at all - the template cannot compile |

So inference and the runtime cannot disagree, because both are derived from one list. That
closes the same gap the `fromSource` surface backstop closed, by construction rather than
by a second check.

## Steps

| # | Step | Test |
|---|---|---|
| 1 | Rules: single `keyed` list, `notes`, consumer identity removed, hash covers `keyed` | unit; existing inference tests updated |
| 2 | Inference: unlisted is unsupported, message uses `notes` | unit |
| 3 | Resolver builds the source context from the keyed list, `name` = binding entry | unit, then a live check that the entry alias appears in the key |
| 4 | `deployment-namer` takes a `component` property; demo app updated | e2e |
| 5 | Regenerate definitions and fixtures | existing suites stay green |

## Consequences to expect

- **The policy hash moves.** Every definition needs re-applying; the annotation is what
  makes that a clear rejection rather than a silent mismatch.
- **`deployment-namer` is the worked example** of the per-consumer pattern, so it is worth
  getting right rather than merely working.
- **`context.name` changes meaning** for any definition already using it. The only ones
  are in this repo.
