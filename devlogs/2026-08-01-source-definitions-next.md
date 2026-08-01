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

### 2. `git-file` — fetch a file from a repo

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

### 3. `http-get` — call a URL

- The `http` provider takes `{method, url, request: {body, header}}` and returns
  `{body, header, statusCode}` — so headers are already supported natively.
- **`headersFromSecret` is out of scope** — someone else is building that. Take
  headers as a plain parameter for now and pick their work up when it lands.
- `statusCode` should be checked in the template and turned into a clear failure
  via `errs:`, or a 404's body silently becomes the resolved value.
- Caching: this is the source that most needs a sensible `storageTTL`, and the one
  where `onStaleFailure: use-stale` earns its keep.

### 4. `configmap` — read a ConfigMap

- `kube.#Get`, straightforward. The demo's `platform-registry` is already 90% of
  this; generalising means parameters for name/namespace and a `data` passthrough.
- Note the type trap found this session: ConfigMap `data` values are always
  **strings**, so a schema declaring `replicas: int` will not match without an
  explicit conversion in the template.

### 5. `secret` — read a Secret

**Decided: not generic.** A source that returns any key of any Secret is too broad
a capability to hand out — it would let any binding read any Secret the controller
can reach, which is the opposite of the trust boundary the whole design rests on.

So this is narrow by construction: the definition names what it exposes, and the
Secret is an implementation detail behind the `schema:`. A platform authors
`registry-credentials` or `database-password` — each declaring exactly its fields,
each marked `// +sensitive` — rather than one `secret` source parameterised by
name and key.

That is more definitions, and it is the right trade: the schema stays a contract
instead of a passthrough, and `consumableFrom` can then say something meaningful
about each one.

Still uses base64 (`base64` provider) and `// +sensitive` on every output field.

### 6. `vela-config` — read a KubeVela Config

**Decided: build it.** The `config` provider lives in
`pkg/cue/cuex/providers/config`.

It is self-referential — source cache entries *are* Config objects, so a source
reading Configs can in principle read other sources' cache entries. That overhead
is accepted; it is not a reason to withhold the capability.

Worth being aware of rather than designing around: a chain that reads its own
cache entry would be odd but not harmful, since entries are content-addressed and
resolution is per-render.

### Cross-cutting for all five

- Each should ship as a `.cue` definition applied with `vela def apply`, so the
  `$internal` block is generated rather than hand-written.
- Each wants an e2e spec; `test/e2e-test/source_expression_test.go` is the pattern.
- Check the inferred cache key for each and make sure the sharing boundary is what
  you would have chosen — that is the whole point of the inference, and it is the
  first thing to look at when one of these behaves oddly.
