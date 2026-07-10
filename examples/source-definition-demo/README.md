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

- `definitions/cluster-lookup.yaml` — `SourceDefinition` `cluster-lookup`: reads
  the `cluster-info` ConfigMap in `kube-system` and surfaces `region`, `zone`,
  `provider`. Keyed per cluster, so its cache entry is shared across all
  Applications on the cluster.
- `definitions/tenant-data.yaml` — `SourceDefinition` `tenant-data`: reads the
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
kubectl apply -f examples/source-definition-demo/definitions/
kubectl apply -f examples/source-definition-demo/apps/
```

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

# SourceDefinition Demo: polling an HTTP service (`get-random`)

This example runs a tiny in-cluster service that returns a random integer in a
range, and a `SourceDefinition` that polls it over HTTP via the CueX `http`
provider. It shows source resolution that performs real network I/O, with the
cache bounding how often the service is hit.

## Files

- `resources/random-service.yaml` — a `python:3-alpine` Deployment + Service
  (and the handler ConfigMap). `GET /?min=&max=` returns `{"value": N, ...}`.
- `definitions/get-random.yaml` — `SourceDefinition` `get-random`: takes `min`
  and `max` properties, polls the service, and surfaces `value` (int),
  `valueString`, `min`, `max`. Cached per range for a short TTL.
- `apps/random-app.yaml` — `Application` `random-app`: binds `get-random` with
  `min: 10, max: 20` and writes the value into a ConfigMap.

## Apply

```bash
kubectl apply -f examples/source-definition-demo/resources/random-service.yaml
kubectl -n source-demo rollout status deploy/random-service
kubectl apply -f examples/source-definition-demo/definitions/get-random.yaml
kubectl apply -f examples/source-definition-demo/apps/random-app.yaml
```

## Verify

```bash
kubectl get configmap random-result -n source-demo -o jsonpath='{.data.value}{"\n"}'
# -> a number between 10 and 20
```

## What to notice

- **Caching a volatile source.** The value is cached per `(min,max)` range for
  `storageTTL: "30s"`. Within a window every consumer sees the same number; it
  re-rolls on the next miss. This is the point of the cache: it bounds load on
  the polled service. Force an immediate re-roll by deleting the cache entry:

  ```bash
  vela config delete get-random-10-20
  ```

- **`onStaleFailure: "fail"`.** If the service is unreachable, this source fails
  the render rather than serving a stale number (a stale random value is
  meaningless). Contrast with the `use-stale` default used elsewhere.
- **int vs string.** The schema exposes both `value` (int) and `valueString`;
  the ConfigMap consumes `valueString` because ConfigMap data must be strings.

---

# SourceDefinition Demo: all sources → one Deployment (`random-deployment`)

This ties every source together. A raw Deployment is created whose:

- **replica count** is randomly 1–5, from `get-random`;
- **name** is `<region>-<zone>-<department>-<tenant>-<component>`, assembled by a
  chained `deployment-namer` source from `cluster-lookup` and `tenant-data`;
- **labels** carry region, zone, department, tenant, and environment, each read
  directly from the relevant source.

## Files

- `definitions/deployment-namer.yaml` — `SourceDefinition` `deployment-namer`: a
  chained source that takes region/zone/department/tenant as inputs and returns
  the joined, lowercased name (with the component name from `context.name`).
- `apps/random-deployment.yaml` — `Application` `random-deployment`, using
  `get-random`, `cluster-lookup`, `tenant-data`, and `deployment-namer`.

Requires the same fixtures as the earlier demos plus the random service:

```bash
kubectl apply -f examples/source-definition-demo/resources/          # namespace, cluster-info, random-service
kubectl -n source-demo rollout status deploy/random-service
kubectl apply -f examples/source-definition-demo/definitions/        # all SourceDefinitions
kubectl apply -f examples/source-definition-demo/apps/random-deployment.yaml
```

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
- **int flows straight through.** `spec.replicas` reads `rng.value` (an int) with
  no string conversion, unlike the ConfigMap demo above.
