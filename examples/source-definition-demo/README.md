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
