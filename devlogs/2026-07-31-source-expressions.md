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

**Membership comes from the cache-key rules.** `contextTypes` declares each field's
type, and `TestContextTypesMatchTheKeyRules` asserts its keys match
`cachekey.Rules.Fields()` exactly, so the two cannot drift on *what* is readable.
That keeps one curated set, one error message, and keeps `context.output` and
`context.status` unreachable by construction.

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

## What the spike does not address

| | |
|---|---|
| **Trust boundary** | KEP-2.16 says the app author "cannot alter its resolution logic… This separation is load-bearing; the feature's security properties depend on it." Expressions move them from *selecting* a declared field to *computing*. The sandbox (only `source` and `context`, no imports, no providers, grammar-gated) is implemented and tested, but widening that boundary should be a deliberate amendment, not a side effect. |
| **`+sensitive` taint** | Today redaction knows a property came from a sensitive path. `"prefix-" + secret` is not recognisably the secret. `References()` returns the paths an expression reads, which is the input a taint rule needs — but the rule itself is not written. Getting this wrong leaks. |
| **Wiring** | Nothing calls this. The substitution point is where `fromSource` is resolved today (`resolveFromSourceParams`), and dependency ordering would come from `References()`. |
| **Cache identity** | Expressions in a *source's* properties would feed the cache key. Properties are resolved before hashing, so this is probably already correct — unverified. |
| **`vela def` / dry-run** | Untouched. |

## Recommendation

Additive, not a replacement. `fromSource: "img.image"` becomes exactly sugar for
`$(source["img"].image)`: same validation path, no migration, and the structured
form stays the well-trodden case. The grammar gate then only has to be right for
the new form.

This is KEP-sized — it touches the trust model, redaction, and validation — so it
wants its own amendment (A6) or its own KEP rather than folding into the current
branch.
