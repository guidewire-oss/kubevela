# SourceDefinition Demo: reading cluster state into a ConfigMap

This example shows two `SourceDefinition`s that read **real cluster state** and an
`Application` that writes the resolved values into a ConfigMap:

- **`namespace-metadata`** — reads the `tenant` and `department` labels off the
  Application's own namespace.
- **`cluster-info`** — reads the `cluster-info` ConfigMap in the `default`
  namespace and exposes its `region` and `environment` keys.

Both read via the CueX `kube` provider (`kube.#Get`) at resolution time, cache
the result (see each `storage:` block), and the Application substitutes the
resolved values into a `k8s-objects` ConfigMap component via `fromSource`.

## Files

- `read-cluster-config-fixtures.yaml` — prerequisites: the `source-demo`
  namespace (labelled `tenant`/`department`) and the `cluster-info` ConfigMap in
  `default`.
- `read-cluster-config-app.yaml` — the two `SourceDefinition`s and the
  `Application` `resolved-metadata`.

## Apply

Fixtures first (the sources have nothing to read otherwise):

```bash
kubectl apply -f examples/source-definition-demo/read-cluster-config-fixtures.yaml
kubectl apply -f examples/source-definition-demo/read-cluster-config-app.yaml
```

## Verify

```bash
# App should reach running
kubectl get application resolved-metadata -n source-demo -w

# The ConfigMap the component wrote, populated from the two sources
kubectl get configmap resolved-metadata -n source-demo -o yaml
```

Expected `data`:

```yaml
data:
  tenant:      acme
  department:  platform
  region:      us-east-1
  environment: production
```

Inspect resolution status and the shared cache entries:

```bash
# Per-source resolution status on the component
kubectl get application resolved-metadata -n source-demo -o jsonpath='{.status.services}'

# Cache entries (labelled Secrets in vela-system). cluster-info-<cluster> is
# shared across every Application on the cluster that reads it.
vela config list | grep -E 'namespace-metadata|cluster-info'
```

## What to notice

- **No properties needed.** Both sources have `parameter: {}` — resolution is
  driven entirely by `context` (namespace, cluster), so the Application just
  names the sources and consumes fields via `fromSource`.
- **Cache scoping via the key.** `cluster-info` keys on `\(context.cluster)`
  only, so a single backing Config is shared across all Applications on the
  cluster. `namespace-metadata` keys on cluster + namespace, so it is per
  namespace.
- **Required inputs are enforced by consumption.** Each source references the
  labels / ConfigMap keys directly in `output:`. If one is absent the reference
  is incomplete, resolution fails, and the source reports `Failed` on the
  Application status. (The KEP describes an `errs:` block for custom messages;
  the current runtime does not yet evaluate `errs:` for SourceDefinitions, so
  these examples rely on the incomplete-value behaviour instead.)

## Try the failure paths

```bash
# Remove a required label -> namespace-metadata can no longer resolve and the
# source reports Failed on the Application status.
kubectl label namespace source-demo tenant-
kubectl get application resolved-metadata -n source-demo -o jsonpath='{.status.services}'

# Delete the cache entry to force an immediate re-resolution
vela config delete cluster-info-<cluster>
```
