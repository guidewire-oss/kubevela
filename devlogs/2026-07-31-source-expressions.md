# Spike: CUE expressions in component properties instead of `fromSource`

**Branch:** `wip/source-expressions`, off `feature/source-definitions` at `f9e6edfc0`.
**Status:** mechanism proven in `pkg/definition/sourceexpr/`, nothing wired into resolution.

## The gap this addresses

`fromSource` yields a whole value or nothing. There is no way to combine a resolved
value with anything else, so an author who needs `"<region>-cluster"` has to add a
parameter to the `SourceDefinition` and build the string there — putting a
consumer's formatting concern inside a shared platform artifact.

The proposal:

```yaml
properties:
  cluster: '$(source.clusterInfo.region + "-cluster")'
```

## What was actually verified

The open question was validation: `fromSource: "img.image"` types cleanly because
it is a bare path that can be looked up in the source's `schema:`. An arbitrary
expression has no such path.

**The obvious approach does not work.** Substituting the schema itself and
evaluating leaves CUE with a non-concrete operand:

```
source: {"my-source": {region: string}}
out: source["my-source"].region + "-cluster"
=> non-concrete value string in operand to +
```

CUE will not compute on a type.

**What does work:** materialise the schema into concrete *sentinel* values of the
right kind, evaluate against those, take the result's kind. The schema still
supplies the types; the sentinel only makes the evaluator willing to run.

| Expression | Inferred | Caught at admission |
|---|---|---|
| `source["s"].region` | `string` | |
| `source["s"].region + "-cluster"` | `string` | |
| `source["s"].count * 2` | `int` | |
| `source["s"].count / 2` | `float` | CUE's `/` is float division |
| `source["s"].count + "-cluster"` | — | ✅ invalid operands |
| `source["s"].undeclared` | — | ✅ not in schema |

So the same expression is evaluated twice: against sentinels at admission to get a
**type**, against resolved values at render to get a **value**.
`TestTypeOfAgreesWithEval` asserts the two agree, because a type check that can
disagree with the render is theatre.

## Three findings worth keeping

**1. Sentinel choice is load-bearing.** An `int` sentinel of `0` makes
`100 / source["s"].count` fail admission with a division-by-zero that could never
happen at render. The sentinel is `1`. Strings are `"x"` rather than `""` for the
same reason — typing should not sit on a degenerate input.

**2. Sentinel typing is unsound for value-dependent branching.**

```
[if source["s"].count > 5 {"big"}, "small"][0]
```

types against the sentinel, not against reality. Contained by a grammar gate that
rejects conditionals, comparisons, disjunctions, slicing, and non-constant
indexing. Constant *string* indexing is allowed — it is field access by another
name, and it is required (see below).

**3. A hyphenated source name cannot be reached with dots.** This is the sharpest
edge and it caught the original sketch:

```
source.my-source.region   →  (source.my) - (source.region)
```

Both halves are legal CUE, so nothing complains until the evaluator says
"undefined field: my", which tells the author nothing. `Validate` detects a
subtraction whose operands are both rooted at `source` and says what to write
instead. Ordinary subtraction (`count - 1`) is unaffected.

**But the dotted form is still the right default.** `source.<name>` names the
*binding* (`spec.sources[].name`), not the SourceDefinition, and real binding
names in this repo are `clusterInfo`, `tenant`, `img`, `first`, `second`, `s`,
`scaleData` - 7 of the 8 present are legal CUE identifiers. Only
`random-deployment-replicas` needs brackets.

So: dots are the documented form, brackets are the escape hatch for a name that
is not an identifier, and `Validate` names the fix when someone hits it. Both
spellings produce the same `Reference`, so schema validation, dependency ordering
and sensitive taint never have to care which was used.

## Folding in `context`

`$(context.appLabels["cluster-name"])` now works alongside `source`.

**This was an explicit KEP Non-Goal**, and the rationale deserves stating before
it is overridden:

> `fromContext`: OAM context fields needed in properties should be exposed via a
> `SourceDefinition` authored by the platform engineer, keeping the resolution
> model consistent

Two things weaken it. The consistency argument is already spent once expressions
exist in properties at all - that ship sails with `source`. And the cost of the
alternative is steep: surfacing `context.appLabels["cluster-name"]` through a
SourceDefinition means a definition, a ConfigTemplate, a cache entry and an
admission path, to relay a value that is already sitting in the render context.

There is also a structural reason context is *safer* here than in a
SourceDefinition. A source's context reads are restricted because they determine
cache identity - reading an unkeyed value would silently break sharing. A property
expression feeds no cache and is evaluated per render, so that constraint does not
apply at all.

**Membership follows the definition being fed.** An expression sees what the
definition it feeds sees, at the moment that definition is rendered — a property
expression is substituted immediately before the ComponentDefinition's template
runs, so the readable set is *that template's* context.

An earlier version reused the cache-key rules list. That was wrong: those rules are
policy about a SourceDefinition's cache identity and curate a different set for a
different purpose. Probing a real render context showed 18 fields where the
cache-key list has 11 — `replicaKey`, `revision` and the `appSource*` internals
among the difference.

The rule also settles `context.name`, which had been ambiguous. In a
SourceDefinition it is the binding entry (amendment A4); in a property expression
it is the component, because that is what a ComponentDefinition's `context.name`
is. Each scope is internally consistent with the definition it belongs to, which
is the only property that survives more surfaces being added.

`TestContextTypesMatchTheRenderContext` builds a real context and requires every
field in it to be classified — readable with a type, or in `notReadable` with a
reason. A field added upstream then forces a decision instead of silently becoming
unavailable, which an author could not distinguish from a typo. Verified
non-vacuous: removing `replicaKey` from the table fails the test.

### Two more CUE mechanics that did not work

**A pattern constraint cannot type an open map.** The obvious way to allow any
label key is `appLabels: [string]: "x"`, but `context.appLabels["anything"]` still
reports `undefined field: anything`. So the sentinel scope materialises exactly the
keys the expression references - which `References()` already knows. That is why
`References` had to be fixed to record the *maximal* chain: it was logging
`context.appLabels` and dropping the index.

**`|` is not a fallback.** `context.appLabels["a"] | "default"` yields `"default"`
even when `a` is present - it is a disjunction, not a default operator. So the
"allow same-type disjunctions for defaulting" idea is dead.

### Defaults — closed, and it was parity not ergonomics

This was first written up as an ergonomic gap. It was worse than that: `fromSource`
already supports a fallback —

```yaml
image:
  fromSource: {name: img, path: image, default: "nginx:latest"}
```

`evaluateFromSourceSelector` returns `default` when resolution fails *or* the path
is missing. So without an equivalent, expressions were **strictly less capable**
than the thing they would replace.

CUE's default marker supplies it, with one non-obvious detail: **the marker goes on
the value, not the fallback.**

```
*source.img.image | "nginx:latest"    → the value when present, the fallback when absent  ✅
source.img.image | *"nginx:latest"    → the fallback ALWAYS                                ✗
source.img.image | "nginx:latest"     → ambiguous when present                             ✗
```

Both wrong forms fail silently, which is why the grammar gate checks the shape
rather than trusting it. `*<read> | <literal>` is the only disjunction allowed; a
general one is still refused because its result type would depend on which branch
a value selected.

It stays soundly typeable because neither branch is chosen by a value: the result
is the value's type when present and the default's when not. Where those differ
the kind comes back as a composite - `*source.s.count | "none"` types as
`(int|string)` - and is rejected at admission, since at render only one branch is
taken and the mismatch might not show for months.

The same mechanism closes the absent-label case:

```yaml
region: '$(*context.appLabels["region"] | "unknown")'
```

### A default is only needed where a value can actually be missing

A source's `schema:` is a contract the resolver enforces - output is validated
against it before anything is cached - so a **required** field is guaranteed
present and demanding a fallback for it would be noise. Only an optional field, or
a context value with no schema at all, can go missing.

That is the rule `fromSource` already follows, and it is target-aware: a default is
mandatory exactly when an *optional* source field feeds a *required* parameter
(`validation_sources.go:251`). The target is not this package's business, so
`UndefendedReads` returns the reads that could be absent and carry no default;
admission pairs them with the parameter it is filling.

| Read | Needs a default |
|---|---|
| `source.info.region` (schema: `region: string`) | no |
| `source.info.vpcId` (schema: `vpcId?: string`) | yes |
| `source.info.network.subnet` (schema: `network?: {subnet: string}`) | yes — an optional ancestor makes it absent too |
| `context.cluster` | no — always supplied, even when empty |
| `context.appLabels["x"]` | yes — no schema, any key may be missing |

A path read both with and without a default is still reported: the unprotected
read is the one that fails.

## Getting a typed (non-string) value into a parameter

The property value is a YAML string either way, so the rule that decides the
substituted type is `Whole()`: a value that is *only* an expression is replaced by
the typed result, while one embedded in surrounding text can only be a string.

```yaml
replicas: '$(source["scale"].replicas)'        # int 3
replicas: '$(source["scale"].replicas * 2)'    # int 6
replicas: 'count-$(source["scale"].replicas)'  # string "count-3"
```

One trap: **CUE's `/` is float division**, so `replicas / 2` types as `float` and
an `int` parameter would refuse it. CUE's integer operators are infix, not
functions:

| Expression | Type | Value (replicas = 7) |
|---|---|---|
| `replicas * 2` | `int` | `14` |
| `replicas div 2` | `int` | `3` |
| `replicas mod 2` | `int` | `1` |
| `replicas / 2` | `float` | `3.5` |

`div`, `mod`, `quo` and `rem` pass the grammar gate as ordinary binary operators.
The function forms (`div(a, b)`) do not - the sandbox rejects any identifier other
than `source` and `context`, which is working as intended but does mean the infix
form is the only one available.

Because the type is known at admission, an `int` parameter fed a `float`
expression is refused before the Application is admitted rather than failing at
render.

## Several expressions in one value

A property value may hold more than one:

```yaml
endpoint: '$(source.app.name).$(context.namespace).svc'
```

Each is evaluated independently and the results are concatenated, so the value is
a **string** regardless of what the parts produce - and that is knowable from the
shape alone, without evaluating anything. An `int` parameter can therefore be told
at admission that `'$(a) $(b)'` will never satisfy it.

**Each expression carries its own default.** There is no outer one, and there
should not be: a default on the whole value could not know which part was absent,
and would mask the rest.

```yaml
endpoint: '$(source.app.name) $(*source.b.region | "unknown")'
```

`ValueType` is what admission compares against the parameter, and it covers the
whole value rather than one expression:

| Property value | Type |
|---|---|
| `nginx:1.25` | `string` — a literal |
| `$(source.scale.replicas)` | `int` — the expression's own type |
| `$(a.x) $(b.y)` | `string` — concatenation |
| `port-$(source.scale.replicas)` | `string` |

It also closes a hole `TypeOf` alone left open. A fragment with no text form - a
struct or a list - cannot be concatenated, and that was previously only caught at
render, *after* the Application had been admitted. On its own the same expression
is fine, because nothing is being concatenated:

```yaml
config:   '$(source.a.obj)'            # struct, fine
broken:   '$(source.a.obj) suffix'     # rejected at admission
```

`TestValueTypeAgreesWithEval` asserts the two stay in step.

## Scopes are values, not text

The scope an expression evaluates against is built as a `cue.Value` from Go data
and supplied via `cue.Scope`, never assembled as CUE source.

That is correctness, not style. Binding names and label keys come from the
Application spec, so a hostile author controls them, and a name like
`a": {pwned: "yes"}, "b` concatenated into CUE text would inject fields into the
scope - silently, because the injected fields would simply be there. Encoding the
data means a name can only ever be a *key*: there is no syntax for it to escape
into. The earlier text-building version happened to be safe because `%q` and
`json.Marshal` escape correctly, but that made safety a property of remembering
to quote rather than of the design.

One concatenation remains and cannot be removed: the expression itself is
compiled as `out: <expr>`. A value like `"x\nevil: 1"` would add a field there, so
`evalIn` re-parses with `parser.ParseExpr` - which demands a single expression -
before compiling. `Validate` already does this, but relying on call order would
put the guarantee one refactor from gone.

Both properties have regression tests, because both fail silently.

## Which surfaces this would apply to

`fromSource` resolves on component and trait properties, plus `spec.sources[]`
chaining. Policy and workflow-step properties are **rejected at admission** —
review finding #10 was that admission validated the directive there while nothing
resolved it, so the consumer received a literal `{"fromSource": ...}` map. The fix
was to reject, not to add resolution.

Expressions would inherit exactly that set, because the substitution point
(`resolveFromSourceParams`) is called from `workloadDef.Complete` and
`traitDef.Complete` and nowhere else.

**On extending to policy — I got this wrong first time, and the cluster corrected
me.** I read `application_policies.go:534` (`CompName: app.Name`), concluded that
`context.name` is the application name in a policy render, and wrote it up as a
trap. It is not.

There are two policy paths, and that is the one I had not distinguished:

| Path | Used for | `context.name` |
|---|---|---|
| `appfile.generatePolicyUnstructuredFromCUEModule` | policies that render resources | the **policy** name |
| `application_policies.go` `renderPolicyCUETemplate` | `scope: Application` transforms | the application name |

Verified on a live cluster: the `legacy-cue-policy` PolicyDefinition writes a
ConfigMap named `context.name`, and in app `app-with-legacy-cue` the ConfigMap
comes out as **`legacy-cue-policy`** — the policy name. So in the path that
actually renders resources, `context.name` behaves exactly as it does for a
component: the name of the instance being rendered.

The second path does set `context.name` to the application name, and also pushes
`context.policyName` (which does not exist in the first path — a probe reading it
failed with `undefined field: policyName`). That inconsistency is real: the same
PolicyDefinition CUE sees a different `context.name` depending on which path
evaluates it. But it is a wart in the Application-scoped transform path, not the
blanket hazard I described, and that path does not dispatch resources at all.

The lesson for this spike is the method, not the conclusion: reading one code path
and generalising produced a confident, wrong claim. The readable-context rule
still holds — an expression sees what its definition sees — but *what* a policy
sees has to be measured per path rather than inferred.

## Can this be rigged into policy and workflow rendering?

Measured on a live cluster rather than reasoned about, and the two halves came out
differently.

**Policies: already work.** With the two guards relaxed, a policy consuming
`fromSource: "tiers.tier"` resolved to `"gold"` and wrote its ConfigMap; the app
reached `running`. No plumbing was needed, for two reasons found by tracing:

- policies are built by `makeComponent(..., types.TypePolicy, ...)` →
  `convertTemplate2Component` → **`NewWorkloadAbstractEngine`**, so
  `workloadDef.Complete` - and therefore `resolveFromSourceParams` - already runs
  for policy properties;
- the policy render context is built by the same
  `GenerateContextDataFromAppFile`, so it already carries `Sources`,
  `SourceTypes`, `SourceTemplates` and the cache store.

So the restriction on policies is a **policy decision, not a technical limit**.
The comment in `source_surfaces.go` - "substitution is wired into
workloadDef.Complete and traitDef.Complete and nowhere else" - is inaccurate:
policies inherit it by sharing the workload engine.

**Workflow steps: they work too, and I was wrong that they could not.** The first
attempt gave

```
notify: failed ... msg.$params.message: conflicting values string and
        {fromSource:"tiers.tier"} (mismatched types string and struct)
```

- the literal directive reaching the consumer, review finding #10's symptom. I
concluded that supporting steps meant changing the `kubevela/workflow` module.
That was wrong: the substitution does not have to happen *inside* the engine, only
*before* it.

`generateWorkflowInstance` is where KubeVela hands steps over
(`Steps: af.WorkflowSteps`), and it already rewrites step properties there via
`convertStepProperties`. A pre-pass in the same place resolves the directives, and
the engine receives ordinary data - it never learns that sources exist. With that,
the step succeeded and printed `silver`, the resolved value.

The pre-pass needed one thing the codebase did not have: an exported entry point
to the resolver. `ResolveFromSourceParams` is that - and it is exactly the
"reusable internal API, callable from any rendering context without duplicating
the cache lookup, key computation, or CueX execution paths" that KEP-2.16 names as
the critical requirement for extending surfaces.

**Caveat worth chasing before this is real.** The pre-pass mutates
`RawExtension.Raw` on the steps in place, and `instance.Steps` shares its backing
array with `af.WorkflowSteps`. That is fine while the appfile is rebuilt per
reconcile, but it is a mutation of shared state and would bite if an appfile were
ever cached across reconciles.

**Two guards, and they disagreed.** `validation_sources.go:134` consults
`SurfaceResolvesFromSource`; `parser.go:826` did not - it rejected policy and
workflow-step directives unconditionally. Relaxing only the predicate left the
parser still refusing. That duplication is now removed: the parser consults the
same predicate, so the set of resolving surfaces is stated once. Behaviour is
unchanged - the list still reads component, trait, source.

### Why policy was *not* then enabled

Enabling it was attempted and reverted, because "policy" is not one surface.

| Policy kind | Path | Resolves? |
|---|---|---|
| renders resources (default) | `appfile.generatePolicyUnstructuredFromCUEModule` → workload engine | **yes** |
| `scope: Application` | `application_policies.go` `renderPolicyCUETemplate` | **no** |

The scoped path cannot resolve, and not for want of plumbing:
`application_controller.go` calls `ApplyApplicationScopeTransforms` at line 166
and `GenerateAppFile` at line 181. **Scoped policies render before the appfile
exists**, so there is no parsed `spec.sources[]`, no templates and no cache store
to resolve against. Its context is built by hand and carries none of them.

`SurfaceResolvesFromSource` is one boolean per surface, so it cannot say "policy,
but only the kind that renders resources". Adding `SurfacePolicy` would therefore
let an author write `fromSource` in an Application-scoped policy where it stays
silently inert - review finding #10 exactly, which is the thing the guard exists
to prevent.

Enabling policy properly needs the check to be scope-aware: admission would look
up the PolicyDefinition (it already looks up SourceDefinitions) and reject only
`scope: Application`. Supporting scoped policies at all is a bigger question,
because moving source resolution before the transforms raises an ordering
problem - a scoped policy can rewrite the Application, including `spec.sources[]`,
so values resolved beforehand would come from the pre-transform spec.

Enabling any surface also needs admission coverage and tests. `ConsumableSurfaces`
no longer needs a separate edit - it is derived.

## What the spike does not address

| | |
|---|---|
| **Trust boundary** | KEP-2.16 says the app author "cannot alter its resolution logic… This separation is load-bearing; the feature's security properties depend on it." Expressions move them from *selecting* a declared field to *computing*. The sandbox (only `source` and `context`, no imports, no providers, grammar-gated) is implemented and tested, but widening that boundary should be a deliberate amendment, not a side effect. |
| **`+sensitive` taint** | Overstated in earlier drafts — see below. Not a blocker under the natural design. |
| **Wiring** | Nothing calls this. The substitution point is where `fromSource` is resolved today (`resolveFromSourceParams`), and dependency ordering would come from `References()`. |
| **Cache identity** | Expressions in a *source's* properties would feed the cache key. Properties are resolved before hashing, so this is probably already correct — unverified. |
| **`vela def` / dry-run** | Untouched. |

## `+sensitive`: less of a problem than first written

Earlier drafts called taint propagation a blocker. That was wrong, and the mistake
was not reading what `+sensitive` does.

Redaction lives at `apply.go:513`, is keyed on the recorded **path**, and applies
only to `status.services[].sources[].properties`. It masks the echo in Application
status. It has never stopped the value reaching the rendered resource -
`image: {fromSource: "creds.token"}` puts the token in the Deployment today, and
`+sensitive` does not change that. The protection is against re-publishing a value
somewhere with different RBAC, not against the value being used.

Because the match is on the path, an expression pass that records **the inputs it
read** - exactly what `References()` returns - is covered by the existing
mechanism with nothing added:

```go
recordConsumedValue(name, type, "creds.token", v)   // masked, as today
```

Taint is needed only if status also records the *computed result*, since
`"prefix-<token>"` sits at no sensitive path. That is a real choice with a real
trade:

| Status records | Cost |
|---|---|
| inputs only | no taint logic; status shows what was read, not what was produced |
| inputs + result | better debugging; needs a rule that masks the result when any referenced path is sensitive |

The second is worth having eventually - with `fromSource` the consumed value *is*
the property value, so recording only inputs loses something that exists today -
but it is an enhancement, not a prerequisite.

## Recommendation

Additive, not a replacement. `fromSource: "img.image"` becomes exactly sugar for
`$(source["img"].image)`: same validation path, no migration, and the structured
form stays the well-trodden case. The grammar gate then only has to be right for
the new form.

This is KEP-sized — it touches the trust model, redaction, and validation — so it
wants its own amendment (A6) or its own KEP rather than folding into the current
branch.
