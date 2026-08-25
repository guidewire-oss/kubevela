# The Context Registry

`context.cue` is the single declaration of every field KubeVela's render context
can carry, its type, and which call sites offer it. Go **reads** this file — it
does not restate it.

That is the point rather than a convenience. When admission checked one
hand-written Go table and render used another, the two disagreed in both
directions at once: `context.appRevisionNum` passed the check and failed at
render, while `context.policyName` was supplied at render and refused by the
check. One declaration cannot do that.

## Two files, two scopes

Contributors regularly conflate these. They answer different questions:

| | `sourceexpr/context.cue` (this package) | `cachekey/rules/a-v1.cue` |
|---|---|---|
| Governs | `$(context.x)` in an **Application's** properties | `context.x` inside a **SourceDefinition's** CUE template |
| Declares | the field's type, and which surfaces offer it | whether a source may read it, plus key position |
| Adding a field here lets you | write it in a component/trait/step/policy property | — |

A field in the registry but not in the rules is readable in an Application
expression and **not** readable inside a source template. `context.policyRevisionName`
is an example: it is in the registry, but only on `policy-app`, a surface that
renders before the appfile exists and so resolves no sources.

See [`../cachekey/README.md`](../cachekey/README.md) for the other half.

## Architecture

- **`context.cue`** — the registry. Field groups, composed into one type per surface.
- **`registry.go`** — loads it (`//go:embed`), exposes `ContextFor`, `SurfaceOffers`,
  `SurfaceDeclared`, `SurfacePlural`, `SurfaceNames`.
- **`context.go`** — `ContextSchema`, a view over one surface's `cue.Value`.
- **`typecheck.go`, `eval.go`, `scope.go`** — type-check and evaluate expressions
  against a `ContextSchema`.

## The shape

Fields are declared once in a group, and groups are composed into a type per call
site. The surface types **are** CUE types — there is no enum and no translation
layer, so a field's type is its CUE type.

```cue
#ComponentIdentity: {componentName: string, componentType: string, ...}
#TraitIdentity:     {traitType: string}

surfaces: {
	component:    {#AppIdentity, #DeliveryIdentity, #ClusterIdentity, #ComponentIdentity, name: string}
	trait:        {surfaces.component, #TraitIdentity}
	workflowstep: {#AppIdentity, #DeliveryIdentity, #ClusterIdentity, #StepIdentity, name: string}
	...
}
```

### Surfaces

| Surface | Is |
|---|---|
| `component` | a ComponentDefinition template, and the properties substituted before it |
| `trait` | a TraitDefinition template — the component's context plus its own type |
| `workflowstep` | a workflow step's properties, substituted before the engine sees them |
| `policy-rendered` | a PolicyDefinition with a CUE template — renders through the component engine |
| `policy-default` | a built-in policy (`topology`, `override`, …) — read off the appfile, never rendered |
| `policy-app` | an Application-scoped policy — renders before the appfile exists |

Only the first four resolve sources. The root list is `sourceReadingSurfaces` in
`pkg/cue/definition/source_surfaces.go`; `ConsumableSurfaces` derives from it, and
everything else about surfaces derives from that in turn — the second list exists
so a definition cannot be refused a surface the controller supports.

`sourceReadingSurfaces` also contains `source`, which is chaining — one source
reading another. It is not a registry surface and not in `ConsumableSurfaces`: a
chained source resolves inside whichever render triggered the outer binding, so it
introduces no context of its own and is not somewhere an Application consumes a
value.

## Adding a context field

1. **Put it in the group that describes when it exists.** It then appears on every
   surface embedding that group. A field present in the render context but readable
   nowhere goes in `excluded:` with a `+reason=` — the loader *requires* the reason
   and panics at startup without one.

2. **Make sure the render path actually supplies it.** Declaring a field the render
   omits type-checks at admission and fails at render. Worse, a field the render
   supplies as a permanent empty string type-checks *and* renders, and tells the
   author nothing — `context.cluster` was declared on `policy-rendered` while that
   path never assigned it, so in one reconcile a component read `"local"` and the
   policy beside it read `""`.

3. **Run the tests.** They are the enforcement, not a formality:

   | Test | Enforces |
   |---|---|
   | `TestContextTypesMatchTheRenderContext` | every field the render context carries is in a group or in `excluded` |
   | `TestRenderedPolicySurfaceMatchesTheRender` (`pkg/appfile`) | every field a surface declares renders non-empty |
   | `TestKeyedFieldsExistInTheContextRegistry` (`pkg/definition/cachekey`) | every keyed field exists here |

4. **If a source template should be able to read it too**, add it to the cache-key
   rules as well — see [`../cachekey/README.md`](../cachekey/README.md). Adding it
   here alone only makes it available to Application expressions.

## Why declared type against real type is unification

The tests build a genuine render context per surface and unify it with the
declared type, rather than comparing field kinds one at a time:

```
surfaces.component & <a real render context>
→ appRevisionNum: conflicting values "3" and int (mismatched types string and int)
```

A hand-written comparison is a check somebody has to remember to extend.
Unification cannot be forgotten.

## Surface compatibility

A field on *some* surfaces and not others is normal and useful: it is what lets a
source key on `componentName` and be restricted to components and traits, rather
than every source being confined to the intersection of every call site.

Where such a source may be consumed is enforced per binding at Application
admission, and reported by `vela def show` before anything is applied:

```
| components     | ✔ |                             |
| workflow steps | ✘ | reads context.componentName |
```

Error wording is derived, never prose kept true by hand — `SurfacePlural` and
`SurfacesOffering` build "unavailable in workflow steps" and "available on:
component, trait" from the registry itself.

## Notes

- **`context.custom` is `_` by construction.** It is whatever an Application-scoped
  policy published, and absent unless one did. Reading it requires both a type
  assertion (`& string`) and a default (`*… | fallback`). Do not try to type it
  further.
- **`context.name` means something different on every surface** — the component,
  the step, the binding inside a source. That is why each definition kind also gets
  its own `{definitionType}Name` / `{definitionType}Type` pair.
- **Unknown surfaces fail open.** A caller not yet taught to name its surface gets
  the component's context rather than nothing, so it behaves as it did before
  surfaces existed instead of rejecting valid expressions.
