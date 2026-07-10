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

- `cluster-lookup.sourcedefinition.yaml` — `SourceDefinition` `cluster-lookup`:
  reads the `cluster-info` ConfigMap in `kube-system` and surfaces `region`,
  `zone`, `provider`. Keyed per cluster, so its cache entry is shared across all
  Applications on the cluster.
- `tenant-data.sourcedefinition.yaml` — `SourceDefinition` `tenant-data`: reads
  the current namespace's labels and surfaces tenant `name`, `department`,
  `environment`, and an optional `costCenter`. Keyed per cluster + namespace.
- `tenant-config.application.yaml` — `Application` `tenant-config`: binds both
  sources and writes their values into a ConfigMap via `fromSource`.
- `tenant-config.fixture-namespace.yaml` — the `source-demo` namespace with the
  `tenant.example.com/*` labels.
- `tenant-config.fixture-clusterinfo.yaml` — the `cluster-info` ConfigMap.

## Apply

Fixtures first (the sources have nothing to read otherwise):

```bash
kubectl apply -f examples/source-definition-demo/tenant-config.fixture-namespace.yaml
kubectl apply -f examples/source-definition-demo/tenant-config.fixture-clusterinfo.yaml
kubectl apply -f examples/source-definition-demo/cluster-lookup.sourcedefinition.yaml
kubectl apply -f examples/source-definition-demo/tenant-data.sourcedefinition.yaml
kubectl apply -f examples/source-definition-demo/tenant-config.application.yaml
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
- **Optional field needs a default.** `costCenter` is optional in the
  `tenant-data` schema, so its `fromSource` uses the map form with
  `default: "unassigned"` — required by admission (the webhook rejects an
  optional-field `fromSource` without a default).
- **Required inputs enforced by consumption.** Removing a required label
  (`kubectl label ns source-demo tenant.example.com/name-`) leaves the value
  incomplete, so the source reports `Failed`.
