# Handoff: SourceDefinitions — where we are, what's next

Written at the end of the session of 2026-07-31/08-01. Nothing is half-applied:
both branches are committed, all unit suites green, `go build ./...` clean.

## Branch state

| Branch | Commits ahead | What it is |
|---|---|---|
| `feature/source-definitions` | 32 unpushed (vs `org/`) | The real work: cache-key inference, `$internal`, `keyInputs` validation, cache-entry metadata, KEP amendments A1–A5 |
| `wip/source-expressions` | 28 beyond that | Spike: `pkg/definition/sourceexpr`, wiring into four surfaces, admission type-checking, e2e suite, two demos |

`wip/source-expressions` branches off `feature/source-definitions` at `f9e6edfc0`.

## 1. Outstanding from last session

### On `feature/source-definitions`

- **Full e2e never run.** Only the 14 SourceDefinition specs have been exercised.
  A local run of all 188 hit 39 failures that could not be attributed — the local
  controller runs a hand-picked flag set that cannot replicate CI. Needs CI, not
  more local poking.
- **`TestMain` / `logf.SetLogger`.** A missing kubeconfig makes packages exit(1)
  silently, because controller-runtime discards logs until a logger is set. Agreed
  as a separate upstream PR; not started.
- **envtest now runs locally** (was recorded as unavailable). `make envtest`
  installs `setup-envtest` into `$(go env GOBIN)` - which mise sets to its own go
  bin, *not* `./bin` - so the Makefile's `$(LOCALBIN)/setup-envtest` is never
  found. Fetch the assets and pass them explicitly:

  ```
  setup-envtest use 1.31.0 -p path
  KUBEBUILDER_ASSETS="<that path>" go test ./pkg/webhook/...
  ```

  All 7 webhook packages pass. This does not cover `test/e2e-test/`, which still
  needs a real cluster and CI's flag set.
- **`design/draft-keps`** never reconciled against the branch KEP.

### On `wip/source-expressions`

- **No KEP amendment for expressions.** There is a devlog
  (`2026-07-31-source-expressions.md`) but no A6. The KEP still describes
  `fromSource` as the only mechanism.
- **Policy surface still gated.** Resource-rendering policies *would* resolve
  today — they use `NewWorkloadAbstractEngine`, so `resolveFromSourceParams`
  already runs, and their context already carries the source plumbing. Blocked
  only because `SurfaceResolvesFromSource` is one boolean covering two paths, and
  Application-scoped policies genuinely cannot resolve sources. Needs a
  scope-aware check: look up the PolicyDefinition (admission already looks up
  SourceDefinitions) and reject only `scope: Application`.
- **Scoped policies need `EnableApplicationScopedPolicies=true`**, off by default.
  That gate silently passed two tests for the wrong reason during the session —
  worth a guard or a skip rather than a spec that can pass vacuously.
- **Workflow pre-pass mutates shared state.** `resolveWorkflowStepSources` rewrites
  `RawExtension.Raw` in place, and `instance.Steps` shares its backing array with
  `af.WorkflowSteps`. Correct while the appfile is rebuilt per reconcile; wrong the
  day one is cached.
- **Trust-boundary amendment.** The sandbox is implemented and tested, but
  KEP-2.16 calls the platform/author separation load-bearing and should say
  explicitly that expressions widen it, rather than have that arrive sideways.

### Worth doing first

`cueStruct.lookup`'s open-list fix (`cue.AnyIndex`) is a genuine **`fromSource`**
bug — a list-valued source property was rejected as undeclared, reproducible with
no expressions anywhere. It is currently only on the spike branch. It should be
cherry-picked onto `feature/source-definitions` on its own merit.

## 2–6. Generic SourceDefinitions to build

The idea: ship a small library of platform-authored sources so a team does not
write CueX by hand for the common cases. Each needs a `schema:` that is the
contract, and each gets its cache key inferred — worth checking the inferred key
for each, since it decides the sharing boundary.

**Providers actually available** (verified, `kubevela/pkg@v1.11.1-...`):
`http`, `kube`, `base64`, `cue`, plus KubeVela's own `config` and `helm`.

### 2. `git-file` — fetch a file from a repo  ✅ DONE

- Parameters: repo, path, ref/branch, and a credential reference.
- "Use the existing registry system": KubeVela's definition registry
  (`references/cli/def.go`, `addon-registry.go`) already speaks GitHub/Gitee and
  handles auth — worth reading before reaching for raw HTTP, so a platform's
  existing registry credentials are reused rather than duplicated.
- Caching matters most here: a git fetch per reconcile would be rude. Key should
  include repo+path+ref so a pinned ref is shared cluster-wide.
- Open question: return the file as an opaque string, or parse it? A `schema:`
  that says `content: string` is honest; one that parses YAML into a typed struct
  is far more useful and much harder to make generic.

### 3. `http-get` — call a URL  ✅ DONE (headers blocked upstream)

- The `http` provider takes `{method, url, request: {body, header}}` and returns
  `{body, header, statusCode}` — so headers are already supported natively.
- **`headersFromSecret` is out of scope** — someone else is building that. Take
  headers as a plain parameter for now and pick their work up when it lands.
- `statusCode` should be checked in the template and turned into a clear failure
  via `errs:`, or a 404's body silently becomes the resolved value.
- Caching: this is the source that most needs a sensible `storageTTL`, and the one
  where `onStaleFailure: use-stale` earns its keep.

### 4. `configmap` — read a ConfigMap  ✅ DONE

Built with no Go at all - `kube.#Get` already exists, so this is a definition and
nothing else. Which is the point: the library only needs a provider where the
transport is genuinely new.

`schema: {data: [string]: string}` - honest about the fact that Kubernetes stores
every ConfigMap value as a string. A schema promising `replicas: int` would type
at admission and fail at render; this way a string into an int parameter is caught
up front (*"type mismatch: expression ... "*), and a consumer wanting a number
converts at the point of use.

Namespace defaults to the Application's own; cluster comes from context, so the
generated key is `configmap-\(context.cluster)-\(context.namespace)` and each
cluster picks up its own copy.

Because `data` is an open map, a key read carries the usual obligation - which is
target-aware, so it only bites feeding a *required* parameter. Both verified on a
cluster.

An optional `cluster` parameter reads someone else's copy - a control-plane
ConfigMap consumed by workloads on a spoke. **The parameter path is exercised but
genuine cross-cluster is untested**: there is only one cluster here, so passing
`cluster: local` proves the plumbing and nothing about multi-cluster routing.
Worth a real two-cluster check before relying on it.

One wrinkle to know: the generated key's readable prefix names the *rendering*
cluster, because only context is inlined there. The parameter is in the hash, so
entries stay distinct, but an operator grepping should read the prefix as "who
asked", not "where it came from".

### 5. `secret` — dropped

Not building this. A source returning any key of any Secret is too broad a
capability to hand out, and the narrow alternative — a definition per credential,
each declaring its own fields — is real work with no demand behind it yet.

If it comes back, it comes back as named definitions (`registry-credentials`,
`database-password`) with `// +sensitive` on every field, not as one source
parameterised by name and key. The schema should stay a contract rather than
become a passthrough.

### 6. `vela-config` — read a KubeVela Config  ✅ DONE (extended)

**Decided: build it.** The `config` provider lives in
`pkg/cue/cuex/providers/config`.

It is self-referential — source cache entries *are* Config objects, so a source
reading Configs can in principle read other sources' cache entries. That overhead
is accepted; it is not a reason to withhold the capability.

Worth being aware of rather than designing around: a chain that reads its own
cache entry would be odd but not harmful, since entries are content-addressed and
resolution is per-render.

**Returns `properties`, `template` and `outputs`.** A Config names the
ConfigTemplate whose contract its properties satisfy, and records what that
template produced; both were being dropped. `outputs` is **references only** -
apiVersion/kind/name/namespace.

`pkg/config` splits this awkwardly and the order matters: `ReadConfig` returns
only properties but is the call that raises `ErrSensitiveConfig`; `GetConfig`
carries the template and references but merely *blanks* a sensitive Config. So
ReadConfig is called first as the gate. `OutputObjects` is always empty on read -
`convertSecret2Config` populates it "only on config render stage".

#### Output values are never returned — decided

`vela-config-outputs`, which fetched the referenced objects with `kube.#Get`, was
built and then **deleted**. A source will not return the contents of a Config's
output, at all: not as an opt-in definition, not behind `consumableFrom`.

Two reasons, and the second is the one that settled it:

1. Those outputs are routinely Secrets, so returning them hands a workload's
   properties a credential it was never granted - the same capability that
   stopped us shipping a generic `secret` source (§5).
2. **Every resolved value is copied into the source's own cache entry**, a Secret
   in `vela-system` keyed cluster-wide. Confirmed by reading one: it held the
   full `data` of the fetched Secret. So the credential outlives the render and
   is shared by every consumer resolving to the same key - a second copy with a
   completely different blast radius from the original.

Anything needing the value should reach it by a mechanism built for that: a
mounted Secret, a CSI driver, `secretKeyRef`.

Worth keeping even though the definition is gone: **a provider call inside a
comprehension over another provider's output resolves in the same pass**,
verified directly. Chained fetches need no staging.

#### What a reference is actually for

Wiring by name, which keeps the value out of the Application, out of the rendered
workload and out of the cache, and lets Kubernetes resolve it at pod start:

```yaml
- name: API_TOKEN
  valueFrom:
    secretKeyRef:
      name: '$(*source.cfg.outputs.creds.name | "missing-creds")'
      key: token
```

Verified on a cluster: with the credential wired that way, neither the
Deployment, the Application nor the cache entry contains it.

#### `properties` is the real exposure

Not the outputs - that was the wrong thing to be guarding first. `properties` are
the **inputs** a template took, which for a credential-bearing template *are* the
credential. The shipped `image-registry` template declares `sensitive: false` and
takes `auth.password` among its parameters, so `ReadConfig`'s guard never fires
for it. A `vela-config` cache entry was found holding `"token":"s3cr3t"`.

It is now marked `+sensitive`. Values still render - that is the point of a
source - but no longer appear in status.

That needed a fix to masking itself: `maskSet` was matched *exactly*, so a marker
on `properties` covered a read of `properties` and published `properties.token`
beside it. Masks now cover what sits under them. The marker can only be written
where a schema declares a field, and `properties: _` declares none, so the struct
is the only place to put one - exact matching made it useless in precisely the
case it exists for.

Mitigating context, not an excuse: the `read-config` workflow step already
exposes the same properties, so this is not a new capability in KubeVela. What is
new is the cached copy.

**Shape follows a ConfigTemplate's own**: `output` is the Secret rendered for the
Config itself, `outputs` are the extra objects under the names the template gave
them.

Naming is not a preference. `pkg/config` builds the stored reference list with
`for k := range objects` over a Go map, so its **order is arbitrary** and can
change when the Config is updated - `outputs[0]` would silently begin meaning a
different object. The names are the only stable handle.

Getting them back costs a re-render. `OutputObjects` is keyed by name in memory
but only a flat list is serialised, so `ParseConfig` is the one route - and it
recompiles the template, resolving every provider call including a `validation:`
block that may reach the network. Bounded by `storageTTL` (a cache miss, not a
reconcile), skipped entirely when a Config has no `outputs:`, and a failure falls
back to `Kind/name` keys rather than failing the read. Stored references remain
the authority on identity; the render only supplies the label. `output` needs no
render - a Config's Secret has the Config's own identity.

Worth a decision later: fixing this at the root, by having `pkg/config` store the
name alongside each reference. Would need a compat path for Configs already
written, and would let the provider drop the re-render entirely.

Verified end to end against a `demo-endpoint` Config producing one ConfigMap and
one Secret: properties, template name, `outputs[0].kind`, `outputs[1].name`, an
out-of-range index falling to its default, and a real ConfigMap value read
through the looped source.

### 6b. `environment` — the env an Application is deployed into  ✅ DONE

A vela env is a Namespace with two labels: `usage.oam.dev/control-plane: env`
marks it as one, `namespace.oam.dev/env: <name>` names it (`pkg/utils/env`). That
is the entire mechanism, so the source is one `kube.#Get` and no Go.

**Named `environment`, not `namespace`, on purpose.** The environment is the
concept a platform team and an application team share; the namespace is where it
happens to live. `source.env.name` says what a value *means*; reading a label key
off a namespace says how it is *stored*, and ties every Application to that
storage choice so the platform can never change it. This was the correction to a
`namespace` source proposed earlier - right data, wrong abstraction.

`name` is optional: a namespace that was never `vela env init`-ed carries no env
label, so a consumer supplies a default. Verified both ways on a cluster, plus
admission refusing an undefended `name` into a required parameter.

Key is `environment-\(context.namespace)` with no cluster component - correct,
since the read is always from the hub and does not vary by where a workload
lands.

`kube.#List` supports `matchingLabels`, so looking an env up *by name* (rather
than by its namespace) is reachable if it is ever wanted. Not built: an
Application reading a different environment's configuration is a use case worth
seeing before enabling.

### 7. List indices in property expressions  ✅ DONE

Found by building §6: `source.cfg.outputs[0].kind` was refused, and the recorded
reason was circular - the grammar rejected indexing because `sentinelFor` did not
pin a list's element type, and `sentinelFor` returned an empty list *because*
indexing was rejected. A schema declaring `[...{kind: string}]` pins the element
type exactly as a struct field does.

Constant integer indices are now accepted (computed ones are still refused - the
result would depend on data that does not exist at admission). The length is
*not* pinned by a schema, so an indexed read is possibly-absent and needs a
default when it feeds a required parameter, exactly as `context.appLabels["x"]`
does; a schema that pins the position (`[string, ...]`) needs none.

`Reference.String()` now renders indices and non-identifier segments in bracket
form. Not cosmetic: those references are quoted back at the author as the fix to
apply, and `*source.cfg.outputs.0.name | <fallback>` does not parse. Same change
makes a hyphenated binding render as `source["my-source"].region` rather than as
the subtraction CUE would actually read.

### 8. Open-map reads were typed against the wrong thing  ✅ FIXED

Also found by building §6, and the more serious of the two. `PathIsOpen` walked a
path by declared field name, so a key read out of `outputs: [string]: _` reported
"not open" - the map declares no key, so the lookup just failed:

| Written | Was | Now |
|---|---|---|
| `outputs.settings.data.region & string` | *undefined field: data* | `string` |
| `*outputs.settings.data.region \| "x"` | accepted as `string` | demands an assertion |

The second row is the bug that mattered. With the read erroring under the default
marker, CUE took the fallback - so the type reported was **the default's, not the
value's**. An int default over a struct read would have passed admission and
rendered a struct into an int parameter. Anything relying on that acceptance is
now correctly refused.

Making assertions mandatory there exposed that assertion and default did not
compose (`isDefaultedRead` only accepted a bare read under the marker). They
answer independent questions - what type is this, and what if it is missing - and
an open, possibly-absent field needs both, so this now works:

```
*(source.cfg.outputs.settings.data.region & string) | "unknown"
```

Verified on a cluster in both directions, including the absent key falling to its
default.

### 9. Candidates not built

- **`terraform-output`** — the strongest remaining one. Cloud resources are why
  terraform-controller exists, and an Application has no typed way to ask "is my
  RDS ready, and which Secret holds its connection details". A `Configuration`
  carries `spec.writeConnectionSecretToRef` (a reference - wire it with
  `envFrom`, exactly the pattern §6 settled on) and `status.apply.state`. The
  trap is `status.apply.outputs`: `map[string]Property{Value string}` with **no**
  sensitivity marking, and terraform outputs routinely include passwords. Same
  call as `properties` - omit, or expose only `+sensitive`. Open question.
- **`service`** — a Service's cluster DNS name and ports, for wiring a component
  to a shared platform service. Cheap `kube.#Get`, no trust issue, low value.
- **`cluster`** — which cluster, its labels and region, for config-by-region.
  Needs the cluster-gateway secret or prism, so it is the only candidate with
  real plumbing. Hold until asked for.
- **Deliberately not building**, consistent with §6: anything returning Secret
  data - a generic `secret`, raw terraform outputs, Config output values. The
  reference-plus-`secretKeyRef` route covers the need without a cached copy.

Worth keeping in mind when judging the next candidate: the library is the
*transport*, not the contract. A platform's real answer is still a named
definition per contract (`tenant-profile`, `platform-registry`) with a typed
schema. These exist so nobody writes CueX to fetch a ConfigMap - not so every
Application reads raw label keys.

### Cross-cutting

- Each should ship as a `.cue` definition applied with `vela def apply`, so the
  `$internal` block is generated rather than hand-written.
- Each wants an e2e spec; `test/e2e-test/source_expression_test.go` is the pattern.
- Check the inferred cache key for each and make sure the sharing boundary is what
  you would have chosen — that is the whole point of the inference, and it is the
  first thing to look at when one of these behaves oddly.
