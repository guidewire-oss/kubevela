> ⚠️ **Early concept draft.** This KEP is an early-stage exploration. It is **incomplete and may be inaccurate**, its direction is unsettled, and it should not be relied upon for implementation or as a description of committed behaviour. Expect substantial change.

# KEP-2.25: `@uses`, CUE Imports from a Registry

**Status:** Drafting (Not ready for consumption)
**Parent:** [vNext Roadmap](../README.md)
**Depends on:** [KEP-2.13](../2.13-addons/README.md) (registries), [KEP-2.20](../2.20-module-versioning/README.md) (locks in `ApplicationRevision`)
**Related:** [KEP-2.16](../2.16-source-definition/README.md) (the `registry` CueX provider), [KEP-2.11](../2.11-definition-testing/README.md)

A definition template is a single CUE string in a CRD. There is no filesystem behind it, so `import` only reaches packages compiled into the binary or registered as `Package` CRs. This KEP adds a resolution pass that runs before compilation: a definition declares what it needs with a file-level `@uses` attribute, KubeVela fetches those files from a configured addon registry, builds them into real CUE packages for that one compilation, and synthesises the `import` that binds them. The author writes one line per dependency and no import statements.

## Problem

CUE has an import system. KubeVela cannot use it.

A `ComponentDefinition` carries `spec.schematic.cue.template`, one string, compiled by `cuex.Compiler.CompileString`. The compiler builds a single anonymous `build.Instance` whose `Imports` come from the `PackageManager`, which holds two things: packages compiled into the binary (`vela/kube`, `vela/http`, `base64`, and friends) and `cue.oam.dev/v1alpha1` `Package` CRs loaded from the cluster. Anything else fails with `builtin package "x" undefined`.

What that costs, in practice:

- **Reuse is copy-paste.** A shared `#Labels`, a `#ProbeSpec`, an internal naming convention, a set of resource presets: every definition that wants them carries its own copy. When the convention changes, someone edits fifty definitions.
- **`Package` CRs are not a comfortable home for library CUE.** They work, and for provider registration they are the right thing. As a way to share plain CUE they have four problems, and each one bites:
  - the CUE lives inside a YAML string in a cluster object, so the source of truth leaves git and has to be re-applied to every cluster;
  - every `Package` in the cluster is loaded into the compiler for every render, whether anything imports it or not, so one bad package is a fleet-wide blast radius;
  - the import path namespace is global and flat, with no versioning and no way to run two versions side by side;
  - nothing records where the CUE came from. There is no commit, no digest, no provenance.
- **Addons cannot share CUE with themselves.** An addon shipping twelve definitions has twelve copies of its own helpers.
- **defkit answers this in Go, not in CUE.** `vela-go-definitions` gets reuse from the Go type system, which is the right answer for that repo and no answer at all for a definition hand-authored in a cluster.

Meanwhile the fetch half of the problem is already solved. KubeVela has a registry system (`vela addon registry add`, backed by a ConfigMap, with GitHub, Gitee, GitLab and OSS readers) where credentials are a platform concern, and KEP-2.16 added a `registry` CueX provider with a `FileReader` interface wired at startup precisely so a template can read a file out of a named registry without carrying a URL and a token of its own. What is missing is not the ability to fetch a file. It is the ability to turn a fetched file into something CUE will let you `import`.

## Goals

- Let a definition template import CUE that lives in a git repository, by name and version, with no filesystem and no `Package` CR.
- Reuse the existing addon registry system for location and credentials, so a definition names a registry and never a URL or a token.
- Make resolution identical across the three compile paths (controller render, admission, CLI), because a feature that only works in one of them is worse than no feature.
- Make the resolved set immutable for the life of an `ApplicationRevision`, so a floating version cannot silently change what a stored revision renders.
- Cache fetched CUE in the cluster, readable with `kubectl`, keyed by content, shared across Applications.
- Declare a dependency in one line and one place, with nothing for the author to keep in sync, and with `cue fmt` leaving it alone.

## Non-Goals

- **Not a module system for definitions.** KEP-2.20's `_imports.cue` pulls whole published *modules* (definitions plus their auxiliary resources) into an addon. This KEP pulls *CUE fragments* into a single template. Different layer, different unit. The boundary is spelled out in [Relationship to KEP-2.20](#relationship-to-kep-220).
- **Not CUE's own module system.** No `cue.mod`, no central registry, no `cue mod get`. See [Alternatives](#alternatives-considered) for why, and for the deliberate decision not to foreclose it.
- **Not a package manager.** No dependency solving, no version ranges, no lockfile that a human edits.
- **Not transitive.** A fetched file that itself carries `@uses` is an error, not a recursion. See [No transitive resolution](#no-transitive-resolution).
- **Not a change to the `Package` CRD.** `cue.oam.dev/v1alpha1` `Package` lives in `kubevela/pkg` and four repos consume it. Nothing here alters it. Two ways of routing this design through it were considered and are in [Alternatives](#alternatives-considered).

## Mental Model

```
  definition template              registry (git)              compilation
  ───────────────────              ──────────────              ───────────
  @uses(ingress,                   networking/
    "catalog:net/ingress.cue         ingress.cue     ──►   build.Instance
     @v1.2.0")                                             ImportPath:
          │                                                catalog/net/ingress-8f3a1c9e
          │  resolution pass                                     │
          ▼                                                      │
  import ingress "catalog/net/ingress-8f3a1c9e"  ◄── synthesised ─┘
          │        (injected into the AST, never written by hand)
          ▼
  ingress.#Ingress & {host: parameter.host}
```

`@uses` says *where it comes from*. `import` says *what it binds to*. The resolution pass is the only thing in between, and it runs once per compilation, for that compilation only.

## What CUE Actually Allows

The syntax below is not a guess. It was probed against the `cuelang.org/go` version KubeVela pins, and one of the results changed the design.

| Probe | Result |
|---|---|
| `@uses(catalog:networking/ingress@v1.2.0)`, unquoted | **Parse error:** `invalid attribute: expected '('`. A bare `@` cannot appear in an attribute body. |
| `@uses("catalog:networking/ingress.cue@v1.2.0")`, quoted | Parses. Attribute text preserved verbatim. |
| Several `@uses` attributes in one file | Parse fine, in any order, before or after the import block. |
| `format.Node` round trip | Byte-identical. `cue fmt` neither reorders nor rewrites them. |
| Import path with a `@v1` suffix | Binds, with the identifier taken from the last element minus the version. Available, and unnecessary once paths are content-addressed. |
| Two majors of the same path in one file | Work side by side with import aliases. |
| Import path with no dot in the first element | Works. A plain registry name is a legal first element. |
| Fetched file with **no** package clause | Builds and imports cleanly. |
| Fetched file whose package clause disagrees with the path | Builds and imports cleanly. The identifier comes from the import path, not the clause. |
| Two fetched files with **different** package clauses in one import | **Error:** `package name "other" conflicts with previous package name "ingress"`. |
| A three-file package whose files reference each other's definitions | Builds and renders concretely through one alias. |
| A consumer reaching a hidden field of an imported package | **Error:** `undefined field: _managed`. Package scoping holds across the boundary. |
| Package clause disagreeing with the directory name | Fine. `AddSyntax` overwrites the pre-set `PkgName` with the first file's clause, and the aliased import binds regardless. |
| One file with a clause, one without, in the same package | Both resolve. A clauseless file joins the package rather than being dropped. |
| A fetched library importing `vela/base64` | **Error:** `builtin package "vela/base64" undefined`, unless the fetched instance's own `Imports` are populated too. |
| A provider call inside a library, reached as a sub-expression | Never executes, and does not error. The value simply stays non-concrete. |
| An `*ast.ImportDecl` inserted into a parsed file before `AddSyntax` | Resolves. No `astutil.Sanitize` needed, and it coexists with import declarations the author wrote. |
| The same, with two aliases pointing at two builds of one library | Both bind. `a: old.#Ingress` and `b: new.#Ingress` evaluate independently. |
| A reference to an alias with no import | **Error:** `output: reference "ingress" not found`, naming the alias. |

The first row is why `@uses` takes a quoted string. The last row about package clauses is why a directory reference has a rule about them. The injection rows are why the author never writes an `import` at all: see [The import is synthesised](#the-import-is-synthesised).

## Syntax

```cue
@uses(<alias>, "<registry>:<path>[@<version>]")
```

| Part | Meaning |
|---|---|
| `<alias>` | The identifier the library binds to in this template. Optional: it defaults to the file's base name, which is the same rule CUE itself uses to derive an identifier from an import path. |
| `<registry>` | The name of a registry configured in the cluster, as listed by `vela addon registry list`. Not a URL. Whatever credential that registry was registered with is what the fetch uses. |
| `<path>` | What to pull, relative to the registry's own root path. Ending in `.cue` means one file. Anything else means a directory, whose `*.cue` files are pulled as one package. |
| `<version>` | Optional git ref: tag, branch or commit SHA. Absent means the registry's default ref, which is what "latest" means here. |

That is the whole declaration. There is no import line to write.

| `@uses` | Binds as |
|---|---|
| `@uses("catalog:networking/ingress.cue@v1.2.0")` | `ingress` |
| `@uses(ing, "catalog:networking/ingress.cue@v1.2.0")` | `ing` |
| `@uses("catalog:common/labels.cue")` | `labels` |
| `@uses("catalog:networking")` (a directory) | `networking` |
| `@uses(v1, "catalog:networking/ingress.cue@v1.2.0")` and `@uses(v2, "catalog:networking/ingress.cue@v2.0.1")` | `v1` and `v2`, independently |

### The import is synthesised

The author declares a dependency and a name for it. The resolver writes the `import`, into the AST, after the file-level attributes, immediately before the value is built. The path it writes is `<registry>/<path>-<digest>`, where the digest is the first 16 hex characters of the content hash.

Nobody types that path, nobody reads it, and it never appears in a source file. It exists to be unique, and so it is built to be unique rather than argued into uniqueness:

- **Two versions of one library coexist**, because two contents give two paths. Probed: `a: old.#Ingress` and `b: new.#Ingress` evaluate independently in one template. The alternative designs all had to forbid this and explain why.
- **A collision with a globally-registered `Package` is not possible.** `bi.Imports` holds the `PackageManager`'s packages and the resolved `@uses` in one list, and CUE resolves a duplicate import path first-wins with no error and no stable ordering. A hand-authored `Package.spec.path` cannot collide with a content digest, so the hazard is designed out rather than detected.
- **The cache key, the lock digest and the import path are one identity.** The ConfigMap name, the `usesLocks` entry and the import path all derive from the same hash, so there is one thing to reason about instead of three that have to agree.

The registry and path stay in front of the digest purely so that a CUE error, a stack trace or a `kubectl` listing says something a human recognises. They carry no meaning the resolver depends on.

**The cost is that the template is not standalone-evaluable CUE.** Written as authored, `ingress.#Ingress` is an unresolved reference, and CUE says so plainly: `output: reference "ingress" not found`. This is not a new property. A KubeVela template already refers to `context.name`, which is only defined because `cuex.WithExtraData` appends `context: {...}` to the string before parsing. Injecting an import is the same pre-processing done on the AST rather than the text, and `vela def vet` is the tool that evaluates a template properly in either case.

**Aliases must be unique within a template**, which CUE enforces anyway, and an alias that no `@uses` declares is the error above with a rewritten message naming the attribute the author probably meant to write.

### Worked example

The library, in a git repo registered as `catalog`:

```cue
// networking/ingress.cue
package ingress

#Ingress: {
	host:    string
	service: string
	port:    *80 | int

	manifest: {
		apiVersion: "networking.k8s.io/v1"
		kind:       "Ingress"
		spec: rules: [{
			host: #Ingress.host
			http: paths: [{
				path:     "/"
				pathType: "Prefix"
				backend: service: {
					name: #Ingress.service
					port: number: #Ingress.port
				}
			}]
		}]
	}
}
```

The definition that consumes it:

```cue
@uses("catalog:networking/ingress.cue@v1.2.0")

parameter: {
	host: string
	port: *80 | int
}

outputs: ing: (ingress.#Ingress & {
	host:    parameter.host
	service: context.name
	port:    parameter.port
}).manifest
```

### A file or a whole package

A CUE package is a directory, and real libraries outgrow one file, so `@uses` pulls either. The two are told apart by the extension, which needs no marker and reads the way an import path already reads:

```cue
@uses("catalog:networking/ingress.cue@v1.2.0")   // one file
@uses("catalog:networking@v1.2.0")               // every *.cue in networking/, as one package
```

Directory references are **not recursive**. One directory is one CUE package, which is CUE's own rule; a subdirectory is a different package and needs its own `@uses`. So `catalog:networking` picks up `networking/ingress.cue` and `networking/tls.cue`, and does not reach `networking/internal/helpers.cue`.

The rule about package clauses is narrower than it first looks: **no two files may declare different clauses.** Files with no clause at all are fine alongside one that has a clause, and their contents join the package normally. Probed: with `a.cue` declaring `package net` and `b.cue` declaring nothing, both `#A` and `#B` resolve. A genuine disagreement fails with `package name "other" conflicts with previous package name "ingress"`, which says nothing about where the files came from, so resolution rejects it first with the registry, path, version and both clause names.

#### When the package clause does not match the directory

It can differ, it is allowed, and under this design it essentially always differs. The synthetic import path carries a digest, so a directory called `networking/` becomes `catalog/networking-8f3a1c9e2b7d4056` and no package clause is ever going to match that.

Nothing depends on the match, because **the alias governs the binding and the clause does not.** `cue/util.BuildImport` pre-sets `PkgName` from the path's base element, and `AddSyntax` then overwrites it with the first file's clause: probed, a package built at `catalog/networking-8f3a1c9e2b7d4056` from files declaring `package net` ends up with `PkgName: "net"`, and only a package whose files declare nothing keeps the pre-set value. Either way the import is aliased, so the consumer binds `networking.#Ingress` regardless of what the clause says.

That leaves one decision worth making deliberately: **the default alias is the base name of the path, never the package clause.** A library in `networking/` whose files say `package net` still binds as `networking` unless the `@uses` names something else.

The reason is a principle worth stating on its own, because other parts of this design lean on it too: **nothing about how a template binds may depend on content that has to be fetched.** Defaulting the alias to the clause would mean you cannot tell which identifier a definition binds without a network round trip, so `vela def vet` could not check an import offline, admission could not validate the reference without fetching, and a library changing its own package clause would silently break every consumer that relied on the default. Deriving it from the `@uses` line keeps all three cheap.

`vela def vet` can still say something useful when the clause and the alias differ, as a readability note rather than an error, because a library named `net` living in a directory named `networking` is legal and mildly confusing and the author may not have meant it.

The digest is taken over the file names and contents in sorted order, so it is stable across fetches and across registries that store the same package.

#### Using a pulled package

Three files in the registry under `networking/`, all `package networking`:

```
catalog/
  networking/
    labels.cue
    tls.cue
    ingress.cue
```

```cue
// networking/labels.cue
package networking

// package-private: a definition that imports this package cannot reach it
_managed: "app.kubernetes.io/managed-by": "kubevela"

#Labels: {
	appName: string
	out:     _managed & {"app.kubernetes.io/name": appName}
}
```

```cue
// networking/tls.cue
package networking

#TLS: {
	host:   string
	secret: string | *"\(host)-tls"
	out: [{hosts: [host], secretName: secret}]
}
```

```cue
// networking/ingress.cue
package networking

#Ingress: {
	hostname: string
	svc:      string
	portNum:  *80 | int
	useTLS:   *true | bool

	// #Labels and #TLS live in sibling files in the same package
	_labels: (#Labels & {appName: svc}).out
	_tls:    (#TLS & {host: hostname}).out

	manifest: {
		apiVersion: "networking.k8s.io/v1"
		kind:       "Ingress"
		metadata: labels: _labels
		spec: {
			if useTLS {tls: _tls}
			rules: [{
				host: hostname
				http: paths: [{
					path:     "/"
					pathType: "Prefix"
					backend: service: {name: svc, port: number: portNum}
				}]
			}]
		}
	}
}
```

The definition that consumes all three:

```cue
@uses("catalog:networking@v1.2.0")

parameter: {
	host: string
	tls:  *true | bool
}

outputs: ing: (networking.#Ingress & {
	hostname: parameter.host
	svc:      context.name
	useTLS:   parameter.tls
}).manifest
```

One line, three files, one alias. `#Ingress` reaching `#Labels` and `#TLS` across file boundaries is ordinary CUE within a package, and it is the thing a single-file reference cannot do: with three separate `@uses` they would be three packages and could not see each other at all.

Rendered, with `context.name: "frontend"` and `parameter.host: "shop.example.com"`:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  labels:
    app.kubernetes.io/managed-by: kubevela
    app.kubernetes.io/name: frontend
spec:
  rules:
    - host: shop.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: frontend
                port:
                  number: 80
  tls:
    - hosts: [shop.example.com]
      secretName: shop.example.com-tls
```

**Encapsulation comes with it.** `_managed` is a hidden field, so it is package-scoped: `#Labels` uses it and the rendered output contains its value, but a definition writing `networking._managed` fails with `undefined field: _managed`. A library can therefore have an inside and an outside, which is the other thing a pile of single files cannot give you.

This is the one new registry capability the KEP needs: a `ListFiles(path)` on the reader. `AsyncReader` today has `ReadFile(path)` and `ListAddonMeta()`, and the latter is addon-shaped rather than a general listing. Every backend already walks a tree to implement it, so the addition is small.

## Resolution Model

Resolution is a pass over the parsed file, before the value is built:

1. **Parse** the template. Collect every file-level `@uses` attribute.
2. **Resolve each reference.** Registry name to registry entry (ConfigMap), version to concrete ref, then fetch. Every fetch goes through the same `FileReader` the `registry` CueX provider uses, so it inherits the credential handling and the not-found semantics already built for KEP-2.16.
3. **Build** each result into a `*build.Instance` with `cue/util.BuildImport(syntheticPath, files)`, where `syntheticPath` carries the content digest.
4. **Inject** one `*ast.ImportDecl` into the parsed file, binding each alias to its synthetic path, inserted after any file-level attributes.
5. **Attach** the instances to that compilation's `bi.Imports`, alongside the `PackageManager`'s own.
6. **Compile** as normal.

Steps 2 and 3 are skipped whenever the cache can answer, which after warm-up is nearly always.

Step 4 is the same kind of pre-processing KubeVela already does to every template: `cuex.WithExtraData` appends `context: {...}` as text before parsing, which is why a definition referring to `context.name` is not standalone-evaluable CUE either. Injecting a declaration is that, done on the AST instead of the string, and it is the reason the author never writes an `import` line.

### Per-compilation, never global

The compilers are singletons. `WorkloadCompiler`, the workflow providers compiler and `cuex.DefaultCompiler` are each created once and shared by every render in the process. Resolved `@uses` imports must therefore be attached to the `build.Instance` for one compilation and never registered with the `PackageManager`.

This is the load-bearing constraint of the whole design. Register them globally and definition A's imports become visible inside definition B, renders become order-dependent, and a definition that compiles in a warm controller fails on a cold one. It is the same class of bug as a CUE provider package registered with one compiler and not the other, which has already bitten `vela/helm`, `vela/registry` and `vela/velaconfig`.

Concretely, in `kubevela/pkg`:

```go
// cuex: attach extra imports to a single compilation.
func WithExtraImports(imports ...*build.Instance) CompileOption
```

`CompileStringWithOptions` currently sets `bi.Imports` before parsing. It moves to after, so a resolver can read the parsed file's attributes, and appends rather than replaces.

### Every compile path, or none

"Three paths" would be a comfortable simplification. The real shape is worse and has to be designed for: **four compilers, and a set of call sites that bypass all of them.**

| Compiler | Defined in | Internal packages | Reached by |
|---|---|---|---|
| `cuex.DefaultCompiler` | `kubevela/pkg` `cue/cuex/compiler.go` | upstream only | `GetCUExParameterValue` in `pkg/utils/common`, `vela cuex`, `adopt` |
| `WorkloadCompiler` | `pkg/cue/cuex/compiler.go` | config, helm, base64, http, kube, cue, registry, velaconfig | controller render, appfile validate, admission |
| `ConfigCompiler` | `pkg/cue/cuex/compiler.go` | config | `pkg/cue/script` |
| `providers.DefaultCompiler` | `pkg/workflow/providers/compiler.go` | workflow providers | `vela def`, SDK gen, schema, docgen, VelaQL |

The call sites that compile a definition template, and would silently not resolve `@uses` if each were expected to opt in:

| Where | Entry point | What breaks |
|---|---|---|
| Controller render | `pkg/cue/definition/template.go` | Definition does not render. Application stuck. |
| Appfile parse and validate | `pkg/appfile/validate.go`, `pkg/cue/convert.go` | Admission and dry-run both fail |
| Admission | `pkg/webhook/utils/utils.go`, `.../application/validation_sources_load.go` | Every definition using `@uses` rejected at apply |
| `vela def` (`vet`, `render`, `apply`, `edit`, `upgrade`) | `Definition.FromCUEString`, `pkg/definition/definition.go` | Works in cluster, fails on the laptop |
| `vela dry-run`, `vela live-diff` | `pkg/appfile/dryrun` via the appfile parser | Dry-run disagrees with what the controller will do, which is the one thing dry-run exists to prevent |
| SDK generation | `pkg/definition/gen_sdk/gen_sdk.go` | Generated SDKs miss or misreport parameters |
| Parameter schema | `pkg/schema/schema.go`, `ParsePropertiesToSchema` | UI and `vela def doc-gen` schemas wrong |
| Docs generation | `references/docgen/parser.go` | Reference docs wrong |

**So resolution belongs inside the compiler, not at the call sites.** The alternative, a `CompileOption` each caller passes, was the first sketch and it is wrong at this scale. Eight call sites across four packages, three binaries and two repos, each of which compiles today and would keep compiling if it forgot, is a defect waiting on whoever adds the ninth. It is the same failure CLAUDE.md already records for provider packages registered with one compiler and not the other, which has bitten `vela/helm`, `vela/registry` and `vela/velaconfig` in turn. That was two compilers. This is four.

#### Exactly where the pass runs

One function, in `kubevela/pkg`: `cue/cuex/compiler.go`, `Compiler.CompileStringWithOptions`. It already parses the template and builds an instance; the pass sits between those two steps.

```go
bi := build.NewContext().NewInstance("", nil)
// bi.Imports = in.PackageManager.GetImports()   <- moves down, so resolved
//                                                  instances can be appended
for _, mutator := range cfg.PreResolveMutators { ... }

f, err := parser.ParseFile("-", src, parser.ParseComments)

// ── the @uses pass ──────────────────────────────────────────────
//   1. walk f.Decls for *ast.Attribute with key "uses"
//   2. resolve each: cache, else fetch, then BuildImport(digestPath, files)
//   3. give each resolved instance the compiler's own packages, so a
//      library may import vela/* at all
//   4. inject one *ast.ImportDecl after the file-level attributes
// ────────────────────────────────────────────────────────────────

bi.Imports = append(in.PackageManager.GetImports(), resolved...)
if err = bi.AddSyntax(f); err != nil { ... }
val := cuecontext.New().BuildInstance(bi)
```

Step 3 is the one that is easy to leave out and hard to notice: without it a library containing `import "vela/kube"` fails with `builtin package "vela/kube" undefined`, as covered under [CueX providers inside a library](#cuex-providers-inside-a-library).

#### How the resolver reaches it

Not as a field on `Compiler`. There are four compilers, so a field is four wiring points and four chances to forget, which is the failure being designed out. A process has one resolver, so it is process-scoped:

```go
// kubevela/pkg, cue/cuex
var UsesResolver = singleton.NewSingleton[Resolver](func() Resolver { return unconfigured{} })
```

`util/singleton` is already how that repo carries process-wide dependencies: `singleton.KubeClient` and `singleton.DynamicClient` work exactly this way, with `.Set()` called once at startup (`references/cli/env.go` does it for the CLI). One `Set` per binary, and every compiler in that process picks it up, including any added later.

| Binary | What it sets |
|---|---|
| `vela-core` | Registry-backed with the ConfigMap cache, in `cmd/core/app/bootstrap.go`, beside the `cuexregistry.FileReader` registration that is already there |
| `vela` CLI | Registry-backed when there is a kubeconfig, falling back to a local cache under `~/.vela/uses/` |
| CLI, no cluster | Cache-only. A reference that is not cached is an error naming it, never a silent skip |

**The default must fail loudly.** A binary that links the library and never calls `Set` gets `unconfigured{}`, which returns an error naming the fix, in the manner the registry provider already uses (`no registry file reader is registered; cmd/core/app/bootstrap.go should register one`). Silently ignoring `@uses` would compile the template without the import and surface as `reference "ingress" not found`, which points at the template rather than at the missing wiring.

**Opting out is per compilation, not per compiler**, via a `DisableUsesResolution{}` option mirroring the `DisableResolveProviderFunctions{}` that already exists. That is what settles the open question about VelaQL views and Config templates without needing a second resolver or a second compiler.

### What `@uses` deliberately does not reach

Some CUE in KubeVela is compiled with a bare `cuecontext.New()` and never touches cuex at all. Those paths cannot resolve `@uses` and are not made to:

| Where | Why it is out |
|---|---|
| `pkg/cue/definition/health/health.go` | A definition's `healthPolicy` and `customStatus` are compiled separately, on a bare context. **This is a real seam, not a tidy one:** a template can use an imported helper and its own health CUE cannot. Either health moves onto a cuex compiler, or the limitation is documented and enforced with an error rather than left to be discovered. |
| `pkg/addon/render.go`, `pkg/addon/addon.go` | Addon metadata and notes, not definition templates. KEP-2.20's `_imports.cue` is the mechanism at that layer. |
| `pkg/definition/celexpr`, `propexpr`, `goloader/hooks.go` | Expression evaluation over a value, not template compilation. Nothing to import into. |

VelaQL views (`pkg/velaql/view.go`) and Config templates (`pkg/cue/script/template.go`) are a different case again: they are cuex-compiled, so they would inherit `@uses` for free once their compilers carry a resolver. Whether they should is an open question rather than an accident to stumble into.

### No transitive resolution

`@uses` inside a fetched file is an error:

```
resolving @uses in definition "my-ingress": file "networking/ingress.cue" from
registry "catalog" at v1.2.0 declares its own @uses("catalog:common/labels.cue").
Transitive @uses is not supported. Declare it in the definition instead.
```

Ignoring it silently would be worse: the file would compile, `labels` would be undefined, and the error would surface as an unrelated CUE evaluation failure somewhere deep in the template. Erroring keeps the dependency set of a definition flat, complete and visible in the definition itself, which is also what makes the lock in `ApplicationRevision` a full record.

Depth-limited transitive resolution is a reasonable thing to want later. It is not free: it needs cycle detection, a resolution order, and a lock format that records a graph rather than a list. Left as an open question.

### CueX providers inside a library

A fetched library is CUE, and KubeVela's CUE is CueX, so the question arises immediately: may a library call `vela/kube`, `vela/http`, `vela/base64`? It cannot be verified in its own repository, since `vela/kube` does not exist outside a KubeVela binary, so any such library is written on the presumption that it will work once the controller compiles it.

It can be made to work. By default it does not work at all, and when it half works it fails silently, so the position has to be a deliberate one.

**A library's own imports are not wired by default.** `cue/util.BuildImport` builds an instance from files and leaves its `Imports` empty, so a library containing `import "vela/base64"` fails with `builtin package "vela/base64" undefined`. The resolver has to populate the fetched instance's `Imports` from the compiler's `PackageManager`, not just the consuming instance's. One line, and without it no library can use any CueX package at all.

**Resolution only reaches provider calls that surface as fields.** `Resolve` walks the built value with `util.Iterate`, which visits fields (hidden and optional included) and skips any path containing `#`. A `#do` struct that exists only inside an expression is never visited, so the function never runs. Probed with `base64.#Encode` in a library, checking the result for concreteness:

| Library writes the call as | Consumer writes | Executes |
|---|---|---|
| an expression, `enc: (base64.#Encode & {...}).$returns` | `out: (secrets.#Encoded & {...}).enc` | No |
| an expression | `x: secrets.#Encoded & {...}` | No |
| a field, `call: base64.#Encode & {...}` | `out: (secrets.#Encoded & {...}).enc` | No |
| a field | `x: secrets.#Encoded & {...}` | **Yes** |
| a hidden field, `_call: ...` | `x: secrets.#Encoded & {...}` | **Yes** |

Two conditions, both necessary: the library must bind the call to a field rather than leave it in an expression, and the consumer must bind the instantiated struct to a field rather than select a sub-path out of it. The second is a constraint on the consumer imposed by the library's internals, which the library author cannot enforce and the consumer has no way to know about.

The failure is not an error. It is an unresolved value, `concrete=false`, which surfaces much later as an incomplete-value complaint about a field with no obvious connection to the cause.

**The provider set is an implicit, unversioned contract.** A library is compiled by whichever binary pulls it, with whatever packages that binary registered. `WorkloadCompiler` carries `helm`, `registry` and `velaconfig`; `providers.DefaultCompiler` does not. So a library calling `vela/registry` would resolve under the controller and fail under `vela def vet`, which is precisely the split this KEP set out to close, reintroduced one level down.

**So: pure CUE by default.** A library may declare types, defaults, constraints and transformations. All of that is verifiable in its own repository with plain `cue vet`, and none of it has any of the problems above. Provider calls need the registry to set `allowProviders`, and a library containing `#do` or `#provider` without it is rejected at resolution rather than compiled and quietly half-evaluated.

That is also the honest reading of the risk. Anything a library could do with a provider, a definition author can already do by writing the same CUE inline, where it is visible in the definition and evaluated in the consumer's own tree. Moving it into a library buys nothing except distance from whoever reviews it.

## Caching

Three layers, each solving a different problem.

| Layer | Scope | Keyed by | Solves |
|---|---|---|---|
| In-memory | One reconcile | `registry/path@ref` | An Application with 20 components using the same helper fetches once, not 20 times. |
| ConfigMap | Cluster | content digest | Renders survive a registry outage, and a cold controller does not re-fetch the world. |
| Lock in `ApplicationRevision` | One revision | definition plus reference | A stored revision renders the same bytes next year as it did today. |

### The ConfigMap cache

One ConfigMap per resolved reference, in `vela-system`, holding the fetched CUE verbatim so `kubectl get cm -o yaml` shows readable source:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: uses-8f3a1c9e2b7d4056
  namespace: vela-system
  labels:
    uses.oam.dev/registry: catalog
    uses.oam.dev/version: v1.2.0
    uses.oam.dev/pinned: "true"
  annotations:
    uses.oam.dev/path: networking/ingress.cue
    uses.oam.dev/import-path: catalog/networking/ingress-8f3a1c9e2b7d4056
    uses.oam.dev/resolved-ref: 4f2c8a1b9e3d7c5081a6f4b2e9d8c7a3b1f5e0d2
    uses.oam.dev/digest: sha256:8f3a1c9e2b7d40563f81b2c9e4a7d6058b3f1c2e9a4d7b60582f1c3e9b4a7d60
    uses.oam.dev/fetched-at: "2026-08-21T10:14:02Z"
data:
  ingress.cue: |
    package ingress
    #Ingress: {...}
```

The name is `uses-` plus the first 16 hex characters of the **content** digest, `sha256` over the fetched files. Keying on content rather than on `registry|path|ref` is what makes the cache name, the lock digest and the synthetic import path the same string, so there are not three identities to keep in agreement. It also deduplicates for free: a tag and the commit it points at fetch to one entry, and a library vendored into two registries is stored once.

The registry, path and version are still recorded, as labels and annotations, because that is what an operator searches on. They are provenance, not identity.

The path is an annotation rather than a label because a path with `/` in it is not a legal label value. Registry, version and pinnedness are labels because those are what you select on.

Refresh depends on pinning:

| Reference | On a cache hit | On registry unreachable |
|---|---|---|
| Pinned (semver tag or commit SHA) | Served from cache. Never re-fetched. The content cannot have changed. | Irrelevant, no fetch happens. |
| Floating (branch, or no version) | Served from cache until TTL expires, then re-resolved. | Stale content served, and a condition raised on the consuming Application. Refusing to render because a git host is down is the wrong failure. |

That mirrors KEP-2.16's `onStaleFailure` handling, deliberately: an operator should not have to learn two stories about stale data.

**Why not `Package` CRs as the cache.** A `Package` CR is not storage, it is registration: everything in one is loaded into every compiler in the process and importable by every definition. That is the wrong property for a cache, for reasons set out in full under [Materialise the fetched CUE as `Package` CRs](#materialise-the-fetched-cue-as-package-crs). The cache has to be inert bytes that only a resolver reads, and a ConfigMap is inert.

**Size.** A ConfigMap caps at roughly 1 MiB. A directory reference makes that reachable. Resolution rejects a reference whose fetched content exceeds a configurable limit (default 512 KiB) with a message naming the reference and the size, rather than letting the API server reject the write with something opaque.

**Garbage collection.** Cache entries are not owned by any Application, so ownerReferences will not do. A periodic sweep removes entries not referenced by any `ApplicationRevision` lock and not touched within a retention window (default 30 days).

## Pinning: `@uses` locks

KEP-2.20 established the pattern: resolve once, store the canonical result in `ApplicationRevision`, and read the stored result from then on. `@uses` needs the same treatment for the same reason. Without it, a definition that says `@uses("catalog:common/labels.cue")` with no version renders one thing today and another thing after someone pushes to the default branch, and the `ApplicationRevision` that exists to make a revision reproducible would be quietly lying.

`ApplicationRevision.spec.usesLocks` records, per definition, per reference:

```yaml
spec:
  usesLocks:
    - definition: my-ingress
      kind: ComponentDefinition
      references:
        - registry: catalog
          path: networking/ingress.cue
          requestedVersion: v1.2.0
          resolvedRef: 4f2c8a1b9e3d7c5081a6f4b2e9d8c7a3b1f5e0d2
          digest: sha256:8f3a1c9e...     # names the cache entry, uses-8f3a1c9e2b7d4056,
                                         # and the import path, catalog/networking/ingress-8f3a1c9e
```

Lifecycle:

- **First admission** resolves each reference and writes the lock.
- **Every later render of that revision** reads `resolvedRef` and `digest` from the lock and resolves from cache. The registry is not consulted. A floating reference floats exactly once, at the moment the revision is created, and is frozen after.
- **A digest mismatch** between the lock and the cache entry is an error, not a silent re-fetch. Something rewrote a tag, or the cache was tampered with, and both are worth stopping for.
- **A new revision** re-resolves from scratch, which is where a floating reference picks up new content. That is the right place for it: a new revision is a visible event with a diff.

The alternative, nesting `references` inside KEP-2.20's `ComponentDefinitionLock`, is tidier if every definition is module-backed, and wrong for legacy definitions and for trait, policy and workflow-step definitions that have no component lock. A parallel list is the less clever option and covers all of them. Open question either way.

## Failure Modes

Every one of these should name the definition, the reference and the registry. A CUE error that says only `builtin package "catalog/networking/ingress-8f3a1c9e" undefined` sends people looking in the wrong place entirely.

| Failure | When caught | Behaviour |
|---|---|---|
| Unknown registry name | Admission | Reject. `registry "catalog" is not configured; vela addon registry list` |
| File not in registry | Admission | Reject, naming path and ref. `found: false` from the reader is a hard error here, unlike in `#ReadFile` where an optional file is a legitimate thing to want. |
| Fetched CUE does not parse | Admission | Reject, with the parse error rewritten to point at the registry path and line, not at `-`. |
| Reference to an alias no `@uses` declares | Admission | Reject. CUE says `reference "ingress" not found`; the message is rewritten to name the `@uses` line that would fix it. |
| Two `@uses` declaring the same alias | Admission | Reject, naming both references. |
| `@uses` declared but never referenced | Admission | Warn. Harmless, usually a leftover, and it still costs a fetch, so `vela def vet` flags it. |
| Package clause differs from the alias | `vela def vet` only | Note. Legal, and quite possibly deliberate. Mentioned once so the author can see it, and never anything more than that. |
| Transitive `@uses` in a fetched file | Admission | Reject. See above. |
| Two `@uses` naming the same library at different versions | Render | Not an error. Two contents give two digests, so they are two packages and both bind. |
| Package clause conflict within a directory reference | Admission | Reject, naming both clauses and the files they came from. |
| Content over the size limit | Admission | Reject, naming the size and the limit. |
| Library contains `#do` or `#provider`, registry has no `allowProviders` | Admission | Reject, naming the file and the construct. |
| Library calls a provider the compiling binary does not register | Admission | Reject, naming the package. Better than resolving under the controller and failing under `vela def vet`. |
| Registry unreachable, reference pinned and cached | Render | Render normally. No fetch was needed. |
| Registry unreachable, reference floating and cached | Render | Render from stale cache, raise a condition on the Application. |
| Registry unreachable, nothing cached | Render | Fail the render with a retryable condition. There is nothing to render with. |
| Lock digest does not match cache | Render | Fail. Do not re-fetch. |

## Security Considerations

**Imported CUE is not inert.** This is the single most important thing in this document. CUE by itself is data, but KubeVela's compiler is CueX, and a file containing a `#do` / `#provider` struct executes a provider function with the controller's identity. A fetched file can call `vela/kube` and read any resource the controller can read, or call `vela/http` and post it somewhere. "It's only CUE" is not a defence.

The mitigations, in order of how much they matter:

1. **Only a platform admin can add a registry.** `@uses` names a registry, never a URL. An application author cannot point a definition at a repository the platform has not already trusted, and the credential used is the registry's, not theirs. This is the same separation KEP-2.16 rests on and it does most of the work.
2. **Provider calls in imported CUE are denied by default.** Resolution walks the fetched AST and rejects a file containing `#do` or `#provider` unless the registry entry sets `allowProviders: true`. Shared helpers are overwhelmingly pure CUE. A library that genuinely needs to read cluster state is a deliberate decision by whoever configured the registry, made once, visibly.
3. **Path allowlist per registry.** An optional `usesPaths` prefix list on the registry entry, so a registry that also serves addons can expose only its library subtree.
4. **Pinning is the reviewable form.** A pinned reference plus a digest in the lock means the content that rendered a revision is identified exactly. Floating references are supported because "latest" is genuinely what you want while iterating, and they are marked as such in the cache labels and in `vela def vet` output so the difference is visible.

Resource limits, all configurable: maximum `@uses` per definition (default 16), maximum fetched size per reference (default 512 KiB), maximum files per directory reference (default 32), fetch timeout (default 3s), and total resolution timeout per definition (default 5s).

Those two timeouts are set against a budget rather than picked: `admissionWebhookTimeout` in the chart is 10 seconds and the definition webhooks run `failurePolicy: Fail`, so resolution has to finish inside a window it shares with everything else validation does, and blowing it rejects the apply. A cold cache on an admission request means a live git fetch inside that window, which is the one place this design puts a network call on a latency-sensitive path.

**RBAC.** The resolver writes to ConfigMaps in `vela-system`, which the controller can already do. The CLI needs read access to the cache and to the registry ConfigMap, and falls back to fetching directly when it has neither.

## Scope and Sequencing

The core of this is genuinely small, and most of it is already probed: collect the attributes, fetch, `BuildImport`, inject an `ImportDecl`, attach to `bi.Imports`. Perhaps a couple of hundred lines. Three things around it are not small, and none of them are the interesting part:

**It is a cross-repo change with a wait in the middle.** `cuex.Compiler` lives in `kubevela/pkg`, so resolution moving inside `CompileStringWithOptions` is a PR there, merged and tagged, then a `go.mod` bump in `kubevela/`. There are no local `replace` directives by convention, so the second PR cannot start until the first has landed. `workflow/`, `prism/`, `kube-trigger/` and `velaux/` all pin `kubevela/pkg` too, at four different pseudo-versions, but none of them need to move: they pick it up whenever they next bump.

**`ListFiles` is six implementations, not one.** `AsyncReader` is implemented by `reader_github.go`, `reader_gitee.go`, `reader_gitlab.go`, `reader_oss.go`, `reader_local.go` and `reader_memory.go`. The last two are trivial. The first four each have their own tree, pagination and rate-limit behaviour, and each needs a test. This is the whole cost of directory mode.

**`usesLocks` is a versioned API change.** A new field on `ApplicationRevision` means CRD regeneration and deepcopy, and `kubevela-core-api` picks it up on its own release-aligned cadence rather than with the feature. The lock lifecycle is also the most intricate correctness surface in the document, and it overlaps machinery KEP-2.20 has designed and not built.

### The security delta is smaller than it first appears

Worth stating plainly, because it changes what has to be solved before anything ships. A definition author can already write a `#do` / `#provider` struct by hand in a template, and it already executes with the controller's identity. `@uses` does not add a capability. It adds a *source* for one.

What it genuinely changes is **mutability**: with a floating ref, the CUE the controller compiles can change without the definition changing, which is new and is the part that deserves care. Pinning removes almost all of it.

### A first cut that is actually easy

| In | Out, and what that saves |
|---|---|
| Single-file references | Directory mode, so no `ListFiles` across six readers |
| Pinned refs only: a tag or a commit SHA, no floating, no "latest" | The mutability half of the security question, and definition-update storms |
| ConfigMap cache, content-addressed | |
| Resolution inside the compiler, so every path gets it | |
| `vela def vet` and `vela def render` parity with the controller | |
| | `usesLocks`, because pinned content cannot change, so reproducibility is free without a lock |

That drops both of the expensive items and the API change, and it is still the entire point of the feature: reusable CUE, pinned, from a registry the platform already trusts. Phase two adds floating refs, which is what pulls in the lock, and directory mode, which is what pulls in `ListFiles`.

### Still unchecked

Things that have not been probed and could each cost a day:

- **Rate limits.** A fleet with a cold cache all fetching from the same GitHub registry at once. The cache makes this a cold-start problem rather than a steady-state one, but cold starts are exactly when a controller is restarting.
- **A definition applied before its registry exists.** GitOps applies in no particular order. The failure should be a clear retryable condition, not a rejection that needs a human to re-apply.
- **`vela def vet` with neither a kubeconfig nor a cache.** It has to say something better than a resolution error, because this is the first thing a new user will hit.


## CLI

```bash
# What does this definition actually pull in, and is any of it floating?
vela def uses ./my-ingress.cue
vela def uses componentdefinition/my-ingress

# Resolve and compile without applying. Same resolver as the controller.
vela def vet ./my-ingress.cue
vela def render ./my-ingress.cue

# Cache inspection and maintenance
vela addon registry cache list
vela addon registry cache show uses-8f3a1c9e2b7d4056
vela addon registry cache purge --registry catalog
```

`vela def vet` resolving `@uses` is the point at which this feature is usable. Without it, the authoring loop is "apply and find out", which is exactly the loop KEP-2.11 is trying to close.

It reports at three levels, and the distinction is the exit code rather than the wording:

| Level | Example | Exit code |
|---|---|---|
| Error | An alias no `@uses` declares; a reference that will not resolve | Non-zero |
| Warning | An `@uses` that nothing references, which still costs a fetch | Zero, unless `--strict` |
| Note | A package clause that differs from the alias it binds to | Zero, always |

Notes exist to be read once and ignored thereafter. A library named `net` living in a directory named `networking` is legal, mildly confusing, and may well be what the author intended, so it earns a line of output and nothing else. Keeping it out of the exit code is the part that matters: a CI job running `vela def vet` over a tree of definitions must not go red over a naming preference, and `--strict` is there for the teams who want it to.

## Alternatives Considered

**Splice the fetched CUE into the template as text.** The obvious cheap implementation, and it fails on scoping. Two libraries defining `#Config` collide. Hidden fields from the library leak into the definition's namespace. Line numbers in errors point into a synthesised string that exists nowhere on disk. Real imports give namespacing, shadowing and error attribution for free, and the probes above confirm CUE handles the versioned-path and mismatched-clause cases without complaint. There is no reason to hand-roll a worse version of a mechanism the language already has.

**Keep using `Package` CRs, and improve the ergonomics.** This is today's answer. The four problems in [Problem](#problem) are not ergonomic, they are structural: source of truth outside git, global namespace, no versioning, no provenance, and a per-render blast radius. `Package` CRs remain the right mechanism for what they were built for, which is registering an external provider endpoint together with its CUE schema. They are the wrong mechanism for a shared `#Labels`.

### Materialise the fetched CUE as `Package` CRs

Tempting, and worth taking seriously rather than dismissing, because it is the existing extension point and it would delete most of the new machinery. The resolver fetches, writes a `Package` with `spec.path` set to the import path and `spec.templates` to the files, and the `PackageManager` informer does the rest. No `WithExtraImports`, no change to `CompileStringWithOptions`, nothing new in `kubevela/pkg` at all. `NewExternalPackage` already calls `util.BuildImport(pkg.Spec.Path, pkg.Spec.Templates)`, which is the exact call this design would otherwise make by hand. `kubectl get packages` would list what is importable, which is better observability than a wall of ConfigMaps.

Two things kill it in that form.

**`spec.path` is a cluster-global key, and duplicates resolve silently.** Different definitions pinning different versions of one library is not an edge case, it is the point of pinning. Two `Package` CRs may both declare `spec.path: catalog/networking/ingress`, because they are keyed internally by namespace and name; `GetImports()` flattens both into `bi.Imports`, and CUE takes the first and discards the second without an error. Probed:

```
imports [v1, v2] → x: {port: 80,  v: "v1"}    err=<nil>
imports [v2, v1] → x: {port: 443, v: "v2"}    err=<nil>
```

The order comes from a `SyncMap`, so it is not stable, which means the same cluster can render a definition differently on different reconciles with nothing in any log to say why.

Content-addressed paths, which [The import is synthesised](#the-import-is-synthesised) adopts for its own reasons, would fix this half of the objection: a `Package` per digest cannot collide with another `Package` per digest. Worth saying plainly, because it means the first-wins hazard is an argument against *sloppy* paths rather than against `Package` as a container. What it does not fix is the second objection, which is the one that stands on its own.

**Global registration makes `@uses` stop being the dependency record.** A `Package` fetched on behalf of definition A is importable by definition B, which never declared it. B would compile in a warm controller that had already rendered A, and fail in a cold one, or after a restart, or on a shard that happens to reconcile in a different order. This is the same failure described in [Per-compilation, never global](#per-compilation-never-global), arrived at from the storage end instead of the compiler end.

**The variant that survives** is to use `Package` as the *storage format* while keeping per-compilation selection: version-qualified `spec.path`, a label marking resolver-owned packages, and `GetImports()` skipping labelled ones so the resolver picks the ones a definition actually declares. That keeps `kubectl get packages`, reuses a typed schema instead of an untyped `data:` map, and keeps the isolation. It costs a behaviour change to `PackageManager` in `kubevela/pkg`, which four repos consume, and it still needs `WithExtraImports`, so it does not buy the simplicity that made the idea attractive. It also leaves `spec.provider` sitting in the schema meaning nothing, which has to be rejected on write. Worth revisiting if the ConfigMap cache turns out to be the awkward part; see the open questions.

**Adopt CUE's own module system.** Upstream is building exactly this, with `cue.mod`, semver, and a central registry. It is where the ecosystem is going and this KEP should not stand in its way. It cannot be adopted now: it wants a filesystem and a module context per definition, and KubeVela's unit of authoring is one string in a CR. The mitigation is to borrow its shape where the shape is doing something, and not where it is only decoration. Import paths look like CUE module paths and the resolver is a single pass in one place; the `@vN` suffix is skipped, because its whole job upstream is to let several majors coexist in a build and this design has ruled that out. When CUE modules become usable in this context, `@uses` becomes a shim in front of them rather than a thing to unpick.

**Vendor shared CUE at build time.** What `vela-go-definitions` effectively does through defkit, and it is a good answer for repos with a build step. It is not an answer for a definition authored by hand and applied to a cluster, and it makes a change to a shared helper into a rebuild and redeploy of every consumer. Worth keeping as the recommended path for generated definitions; not a substitute for imports.

**Put the reference in the definition spec instead of in the CUE.** A `spec.uses: []` field on `ComponentDefinition`, rather than an attribute in the template. Structurally cleaner: no attribute parsing, validated by the CRD schema. Rejected because the reference belongs to the template, not the object. `vela def vet` on a `.cue` file on a laptop, a defkit-generated template, and a definition embedded in an addon source tree all have CUE and no CR wrapper. The attribute travels with the code that uses it, which is also why Go put imports in the file and not in `go.mod`.

### Give `Package` a git source instead

A different question from materialising Packages above, and a better one. Rather than the CUE living in a YAML string, `Package` grows a `spec.source` naming a registry, a path and a ref; a controller fetches and populates the templates. The import stays what it is today, no new attribute, nothing to learn.

It fixes three of the problems this KEP raises against `Package` CRs, and they are real ones: the source of truth returns to git, what gets applied to every cluster is a small pointer rather than the content, and a resolved commit and digest give the provenance that is missing today. If the choice were between a git-sourced `Package` and a hand-maintained one, it would not be a choice.

It fixes none of the other three, and those are the ones that decide it for a shared library:

| | Git-sourced `Package` | `@uses` |
|---|---|---|
| Two definitions on different versions of one library | Not possible. `spec.path` is a cluster-global key | Each definition pins its own, and one definition can hold both |
| Reading a definition tells you what it depends on | No. `import` is a dangling reference to cluster state | Yes, the `@uses` line is right there |
| A stored `ApplicationRevision` renders the same bytes later | No revision boundary to pin to | `usesLocks` |
| Applying a definition before its dependency exists | Fails, so installs need ordering | Resolved as part of admitting the definition |
| Blast radius of one bad library | Every render in the process | Only definitions that declare it |

The framing that settles it: a globally-registered library is a **special case** of `@uses`, namely one that every definition happens to pin to the same version. The reverse does not hold. `@uses` can express a platform-blessed stdlib, at the cost of one declaration line per definition; global `Package` registration cannot express per-definition pinning at any cost.

Shipping both would be worse than either. Two ways to get CUE into a definition means a rule about which to reach for, and the place they meet fails quietly: an `@uses` import path that collides with a globally-registered `spec.path` hits the first-wins behaviour probed above, with no error and no stable ordering.

The cost is not small either. `templates` is a required field on the CRD and would have to be relaxed; there is no `status` subresource to record a resolved ref, a digest or a fetch error, so one would have to be added; there is no `Package` controller anywhere, only an informer in `PackageManager`; and `kubevela/pkg` has no git dependency, so the fetch itself would need the interface-in-`pkg`, implementation-in-`kubevela` pattern that `FileReader` already uses. That is a real change to a CRD four repos consume, in exchange for a mechanism that does less.

**Where the idea does belong.** A **provider** `Package` carries the CUE schema for an external endpoint, the `mysql` example in `pkg/cue/cuex/README.md` being the canonical one, and that schema has exactly the same "CUE in a YAML string" problem. `@uses` cannot help there, because a provider `Package` is registration rather than a library: the schema and the endpoint have to arrive together. Giving `Package` a git source is a clean and independently valuable change for that case, worth proposing to `kubevela/pkg` on its own merit, where it would serve every CueX consumer and not only KubeVela. It is out of scope here, and it does not compete with this KEP.

## Relationship to KEP-2.20

Both fetch CUE from a registry. They are not the same thing and should not be merged.

| | KEP-2.20 `_imports.cue` | KEP-2.25 `@uses` |
|---|---|---|
| Unit | A module: definitions plus auxiliary resources | A CUE file or directory |
| Declared in | The addon source tree, one file per addon | The definition template, at the point of use |
| Consumed by | The addon controller, at reconcile | The CUE compiler, at compile |
| Produces | Definitions installed into the cluster | An import binding inside one compilation |
| Versioning | Semver ranges against a registry index | A git ref, pinned into the revision lock |

A module imported by `_imports.cue` may well contain definitions that use `@uses`. That composes: the module is installed, its definitions are stored, and their `@uses` resolve when they compile. The reverse does not, and should not.

## Cross-KEP References

- **KEP-2.13** Declarative addon lifecycle, and the registry model `@uses` names.
- **KEP-2.16** SourceDefinition, whose `registry` CueX provider and `FileReader` interface this KEP reuses for fetching, and whose stale-data policy it mirrors.
- **KEP-2.20** Module and API line versioning; the lock-in-`ApplicationRevision` pattern, and the `_imports.cue` boundary above.
- **KEP-2.11** Definition testing; `vela def vet` resolving `@uses` is what makes a definition with imports testable before it is applied.

## Open Questions

1. **Where does the lock live?** A parallel `spec.usesLocks` list, or `references` nested inside KEP-2.20's `ComponentDefinitionLock`. Nesting is tidier for module-backed component definitions and does not cover traits, policies, workflow steps or legacy definitions.
2. **Should a directory reference be able to opt into recursion?** Non-recursive matches CUE, where one directory is one package. A library laid out as `networking/` plus `networking/internal/` would need two `@uses`, one of which exists only to satisfy the other, which is the case worth watching for.
3. **Should the alias be required rather than defaulted?** Defaulting to the base name keeps the common case to one argument and matches how CUE derives an identifier from an import path. Requiring it means every bound identifier appears literally in the file, which is friendlier to grep and to a reader who does not know the rule.
4. **Transitive `@uses`, later?** Erroring is right for v1. If it is relaxed, it needs cycle detection, a defined resolution order, and a lock format that records a graph.
5. **What is "latest" for an OSS registry?** OSS has no refs. Probably: reject `@version` on an OSS-backed registry rather than pretend.
6. **Should VelaQL views and Config templates get `@uses` too?** Both are cuex-compiled, so they inherit it for free the moment their compiler carries a resolver. That is an argument for deciding deliberately rather than by omission, in both directions.
7. **Does health CUE need it?** `healthPolicy` and `customStatus` compile on a bare `cuecontext`, so a definition's template could import a helper that its own health block cannot see. Moving health onto a cuex compiler is the fix; refusing `@uses`-derived identifiers in health with a clear error is the cheap alternative.
8. **Is a per-registry index worth having?** Something like `vela addon registry ls-cue catalog` for discovery. Nice, and it needs listing support the readers do not uniformly have.
9. **Should `@uses` be allowed in an Application's inline CUE**, not only in definitions? Same resolver, much wider trust boundary, since an application author would then be choosing what the controller compiles.
10. **`Package` CRs as the cache storage format?** Not as registration, which does not work, but as a typed store the resolver selects from per compilation. Better observability and a real schema, for a behaviour change in `kubevela/pkg`'s `PackageManager` and a meaningless `spec.provider` to reject.
11. **Non-CUE files?** A definition sometimes wants a JSON schema or a YAML fragment from the same repo. `registry.#ReadFile` already covers that at value level. Whether `@uses` should also cover it at import level is a separate question.
