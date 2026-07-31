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
  cluster: '$(source["my-source"].region + "-cluster")'
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

Given how many KubeVela names are hyphenated, this is an argument for making the
bracket form the documented one rather than an escape hatch.

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

### The open ergonomic gap

Admission cannot know whether a label exists, so an absent one fails at render.
There is currently **no way to default it**: conditionals are barred by the grammar
gate (they would make typing unsound), and `|` does not mean what it looks like.

`TestAbsentLabelFailsAtRenderNotAdmission` pins the behaviour so it is not
forgotten. Failing is the right default - silently substituting `""` would let a
missing label flow into a parameter as an empty string - but a `default()` builtin
is probably needed before this is usable. That is the main thing standing between
the spike and a proposal.

## What the spike does not address

| | |
|---|---|
| **Trust boundary** | KEP-2.16 says the app author "cannot alter its resolution logic… This separation is load-bearing; the feature's security properties depend on it." Expressions move them from *selecting* a declared field to *computing*. The sandbox (only `source` and `context`, no imports, no providers, grammar-gated) is implemented and tested, but widening that boundary should be a deliberate amendment, not a side effect. |
| **Defaulting an absent context value** | See above. The blocking ergonomic gap. |
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
