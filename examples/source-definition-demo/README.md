# SourceDefinition Chaining Demo

This example shows:

- source-to-component value resolution via `fromSource`
- nested path resolution
- chained source resolution where a second source consumes the first
- trait property resolution via `fromSource`
- source status reporting with redacted consumed values (`properties`)

## Files

- `source-chain-app.yaml`
  - `SourceDefinition` `cluster-source`
  - `SourceDefinition` `render-source` (depends on `cluster-source` output)
  - `Application` `source-chain-app`

## Apply

```bash
kubectl apply -f examples/source-definition-demo/source-chain-app.yaml
```

## Verify

```bash
kubectl get application source-chain-app -n default -w
kubectl get deploy web-chain -n default -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl get deploy web-chain -n default -o jsonpath='{.spec.replicas}{"\n"}'
kubectl get application source-chain-app -n default -o yaml
```

Expected:

- app phase reaches `running`
- image resolves to `nginx:1.25.2`
- replicas resolves to `3`
- `status.services[].sources[]` transitions to `Resolved`
- `clusterInfo` source writes masked consumed values (image tag path redacted as `***`)

---

# SourceDefinition Demo: reading real cluster state into a ConfigMap

This example uses two purpose-built `SourceDefinition`s that read live cluster
state via the CueX `kube` provider, and an `Application` that writes the resolved
values into a ConfigMap. One manifest per file.

## Files

- `definitions/cluster-lookup.cue` — `SourceDefinition` `cluster-lookup`: reads
  the `cluster-info` ConfigMap in `kube-system` and surfaces `region`, `zone`,
  `provider`. Keyed per cluster, so its cache entry is shared across all
  Applications on the cluster.
- `definitions/tenant-data.cue` — `SourceDefinition` `tenant-data`: reads the
  current namespace's labels and surfaces tenant `name`, `department`,
  `environment`, and an optional `costCenter`. Keyed per cluster + namespace.
- `apps/app.yaml` — `Application` `tenant-config`: binds both sources and writes
  their values into a ConfigMap via `fromSource`.
- `resources/namespace.yaml` — the `source-demo` namespace with the
  `tenant.example.com/*` labels.
- `resources/cluster-info.yaml` — the `cluster-info` ConfigMap.

## Apply

Resources (fixtures) first — the sources have nothing to read otherwise — then
the definitions, then the app:

```bash
kubectl apply -f examples/source-definition-demo/resources/
vela def apply examples/source-definition-demo/definitions/   # applies every .cue to vela-system
kubectl apply -f examples/source-definition-demo/apps/
```

The definitions are authored in the `vela def` CUE format, so they are applied
with `vela def apply` rather than `kubectl apply`. With no `-n` flag they install
to the `vela-system` namespace (the standard location for X-Definitions); the
controller reads them cluster-wide regardless of the Application's namespace.

## Verify

```bash
kubectl get application tenant-config -n source-demo -w
kubectl get configmap tenant-config -n source-demo -o yaml
```

Expected `data`:

```yaml
data:
  tenantName:  acme
  department:  platform
  environment: production
  costCenter:  cc-4127        # from the optional label; "unassigned" if absent
  region:      us-east-1
  zone:        us-east-1a
  provider:    aws
```

## What to notice

- **Two scopes.** `cluster-lookup` keys on `\(context.cluster)` (one shared
  entry per cluster); `tenant-data` keys on cluster + namespace (one per
  namespace). Inspect with `vela config list | grep -E 'cluster-lookup|tenant-data'`.
- **Optional field and defaults.** `costCenter` is optional in the `tenant-data`
  schema. Admission requires a `default:` only when an optional source field
  feeds a *required* target parameter. Here the target is a free-form ConfigMap
  field, so a default is not mandatory — but the map form supplies
  `default: "unassigned"` so an absent `costCenter` still produces a value
  rather than omitting the key.
- **Required inputs enforced by consumption.** Removing a required label
  (`kubectl label ns source-demo tenant.example.com/name-`) leaves the value
  incomplete, so the source reports `Failed`.

---

# SourceDefinition Demo: all sources → one Deployment (`random-deployment`)

This ties every source together. A raw Deployment is created whose:

- **replica count** is a random 1–5, from `get-random` (which polls
  [random.org](https://www.random.org) over HTTPS);
- **name** is `<region>-<zone>-<department>-<tenant>-<component>`, assembled by a
  chained `deployment-namer` source from `cluster-lookup` and `tenant-data`;
- **labels** carry region, zone, department, tenant, and environment, each read
  directly from the relevant source.

## Files

- `definitions/get-random.cue` — `SourceDefinition` `get-random`: takes `min`
  and `max`, GETs a single integer from random.org via the CueX `http` provider,
  and surfaces `value` (int) and `valueString`. No in-cluster service required.
- `definitions/deployment-namer.cue` — `SourceDefinition` `deployment-namer`: a
  chained source that takes region/zone/department/tenant as inputs and returns
  the joined, lowercased name (with the component name from `context.name`).
- `apps/random-deployment.yaml` — `Application` `random-deployment`, using
  `get-random`, `cluster-lookup`, `tenant-data`, and `deployment-namer`.

Uses the same fixtures as the earlier demos (namespace + cluster-info):

```bash
kubectl apply -f examples/source-definition-demo/resources/          # namespace, cluster-info
vela def apply examples/source-definition-demo/definitions/          # all SourceDefinitions -> vela-system
kubectl apply -f examples/source-definition-demo/apps/random-deployment.yaml
```

> **Outbound network required.** `get-random` polls `https://www.random.org` from
> the controller process, so the controller needs outbound internet access. This
> avoids the in-cluster-DNS problem entirely — there is no service to run or port
> to forward.

## Verify

```bash
# The created Deployment (name assembled from the sources)
kubectl get deploy -n source-demo -l example.com/tenant=acme \
  -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas,REGION:.metadata.labels.example\\.com/region
# e.g. us-east-1-us-east-1a-platform-acme-web   3   us-east-1
```

## What to notice

- **fromSource cannot concatenate.** A single `fromSource` replaces one node
  with one source field; it cannot join values from several sources into one
  string. Assembling the name therefore uses a **chained source**
  (`deployment-namer`) whose inputs are fed via `fromSource` from earlier
  sources — the KEP's source-chaining pattern. Labels, by contrast, are each a
  single field, so they are read directly.
- **Forward-only ordering.** `deployment-namer` is declared after `cluster` and
  `tenant` in `spec.sources[]`; admission rejects a source that depends on one
  declared at or after its own position.
- **int flows straight through.** `spec.replicas` reads `rng.value` (an int)
  directly; `get-random` exposes `value` (int) and `valueString` for consumers
  that need a string.
- **Caching a volatile source.** `get-random` caches per `(min,max)` for its
  `storageTTL` (10s), so the replica count is stable within a window and re-rolls
  on the next miss — bounding calls to random.org. A fixed ~30s in-memory LRU
  sits in front of the store, so the effective re-roll floor is `storageTTL + ~30s`
  in a running controller. Force an immediate re-roll:

  ```bash
  vela config delete get-random-1-5
  ```

- **Re-resolved values are picked up (opt-in).** The app sets
  `app.oam.dev/autoUpdateSources: "true"`. With it, when a source re-resolves to
  a new value the component is re-dispatched even though its raw spec is
  unchanged: the controller stamps per-source hashes of the consumed values on
  the workload (`source.oam.dev/resolved-hash`) and re-dispatches when a selected
  source's hash differs from the live one. Values: `"true"`/`"*"` for any source,
  or a comma list of source names (e.g. `"rng"`) to scope it. `app.oam.dev/autoUpdate: "true"`
  also enables it. Without any of these, a healthy component keeps its
  last-applied resolved values.
- **Reconcile cadence still applies.** Re-dispatch only happens when the
  Application reconciles — every ~5m by default (the controller's resync
  period), unless nudged (e.g. re-apply, or
  `kubectl annotate app random-deployment demo/nudge=$(date +%s) --overwrite`).
