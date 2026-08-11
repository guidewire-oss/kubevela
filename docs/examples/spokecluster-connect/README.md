# SpokeCluster examples

Generated 2026-08-11 against `feat/cluster-kep-infrastructure` @ `e4f581f34`.

## The thing to understand first: there are two Secrets

They are easy to conflate and they have opposite ownership.

| | Credential Secret | Gateway Secret |
|---|---|---|
| Who creates it | You, or `vela cluster spokes create` | The controller |
| Where | The SpokeCluster's own namespace (`secretRef.namespace` must match if set) | Always `vela-system` |
| Name | Whatever you choose | Same as the SpokeCluster's name |
| Contents | A kubeconfig under key `kubeconfig` | `endpoint`, `ca.crt`, and either `token` or `tls.crt`/`tls.key` |
| Applies to | `credential.type: kubeconfig` only | Every credential type |
| Example file | `00-credential-secret-kubeconfig.yaml` | `99-gateway-secret-reference.yaml` (reference only, do not apply) |

You create the kubeconfig Secret yourself, and only for `credential.type: kubeconfig`. That Secret is the input, not the cluster-gateway registration. The controller reads your Secret, materializes the credential, and writes a second Secret in `vela-system` that cluster-gateway actually consumes. That output Secret is deliberately bit-compatible with what `vela cluster join` writes, so existing consumers cannot tell the two apart.

For `credential.type: aws` you create **no Secret at all**. The hub uses its ambient Pod Identity or IRSA identity, assumes the per-cluster role, calls `eks:DescribeCluster`, and mints a short-lived EKS bearer token (`k8s-aws-v1.`). The real token lifetime is **15 minutes** (STS GetCallerIdentity presign window); the controller schedules remint at **+13 minutes** (`Materialized.NextRefresh`, a 2-minute lead) and `nextRequeue` is `min(probeIntervalSeconds, time until NextRefresh)`. Cached credentials are reused until that deadline. Keep the probe interval under about 13 minutes if you want continuous Connected probes between remints.

## What the kubeconfig must look like

The provider is stricter than `kubectl`. From `pkg/spokecluster/credential/kubeconfig.go`:

- The `current-context` is used. It must exist, and its cluster and user must resolve.
- `certificate-authority-data` must be **inline**. A file-path `certificate-authority` is rejected, because the path refers to the machine that produced the kubeconfig, not the hub controller.
- Auth must be an **embedded** `token`, or an embedded `client-certificate-data` plus `client-key-data`. Exec plugins (`aws eks get-token`, `gke-gcloud-auth-plugin`) and file-path credentials are rejected.
- `tls-server-name`, if set, must equal the endpoint host. cluster-gateway always derives the verified ServerName from the endpoint, so a differing value is rejected at registration rather than producing a spoke that registers and then fails every handshake.
- The cluster `server` must be `https` and must not target hub-internal DNS (`*.svc`, `*.cluster.local`, `kubernetes.default…`) or cloud metadata/link-local addresses (`169.254.0.0/16`, loopback, Azure `168.63.129.16`, AWS IMDS IPv6). RFC1918 endpoints (for example k3d Docker IPs) and public cloud API hostnames (for example `*.eks.amazonaws.com`) are allowed.
- `insecure-skip-tls-verify: true` is rejected. cluster-gateway treats a missing `ca.crt` as skip-verify, so connect requires inline `certificate-authority-data` and refuses to drop the trust anchor.
- A kubeconfig with neither `certificate-authority-data` nor a usable CA path is likewise rejected (file-path CAs are already refused separately).
- Prefer `credential.type: aws` (short-lived EKS tokens) over a static kubeconfig. When a kubeconfig is unavoidable, use a projected ServiceAccount token or a short-lived client cert. The provider reads JWT `exp` and client-cert `NotAfter` and sets `NextRefresh` two minutes earlier so a rotated source Secret is re-read before the credential dies. If the credential is already inside that lead window, `NextRefresh` is the hard expiry so a long probe interval cannot outlive the token. If it is already expired, `NextRefresh` stays unset and the next pass follows the probe interval instead of a 5s rematerialize loop. Opaque long-lived tokens still rematerialize every pass.
- Bind only the Role in `09-spoke-least-privilege-rbac.yaml` on the spoke. Do not use cluster-admin kubeconfigs.

## Files

| File | What it shows |
|---|---|
| `00-credential-secret-kubeconfig.yaml` | The input Secret you create by hand |
| `01-spokecluster-kubeconfig-minimal.yaml` | Smallest valid SpokeCluster |
| `02-spokecluster-kubeconfig-full.yaml` | Every optional field set explicitly |
| `03-spokecluster-aws-podidentity.yaml` | EKS via Pod Identity, no Secret needed |
| `04-spokecluster-aws-irsa.yaml` | Same, IRSA variant |
| `05-spokecluster-tenant-namespace.yaml` | `detach` outside `vela-system` |
| `06-spokecluster-orphan.yaml` | `deletionPolicy: orphan` |
| `07-spokecluster-phase2-stubs.yaml` | `blueprintRef` / `rolloutStrategyRef` accepted and ignored; `infraProvisioning` is webhook-denied (see 08) |
| `08-invalid-examples.yaml` | Cases that should be rejected, with the expected error |
| `spokecluster-azure.yaml` | AKS shape. Do not apply: webhook rejects `type: azure` |
| `spokecluster-gcp.yaml` | GKE shape. Do not apply: webhook rejects `type: gcp` |
| `09-spoke-least-privilege-rbac.yaml` | Spoke-side ServiceAccount, ClusterRole, and binding for connect probes (not cluster-admin) |
| `10-networkpolicy.yaml` | Hub NetworkPolicy example for cluster-core (webhook, metrics, egress via cluster-gateway). Apply by hand when the CNI enforces NetworkPolicy |
| `99-gateway-secret-reference.yaml` | What the controller produces, for reading only |

## Applying them

The feature gate must be on, or nothing reconciles and status stays empty. The SpokeCluster admission webhook is enabled by default whenever the gate is on (`clusterCore.webhook.enabled=true`); set it to `false` only if you intentionally want CRD-schema-only admission. `clusterCore.replicaCount` defaults to `2` with preferred pod anti-affinity, soft topology spread, and a PodDisruptionBudget so the webhook and leader-elected controller survive a node drain; use `--set clusterCore.replicaCount=1` on single-node sandboxes if you need a lighter footprint:

```
helm install vela-core charts/vela-core -n vela-system --create-namespace \
  --set featureGates.enableClusterInfrastructure=true
```

For production, prefer cert-manager so the validating webhook can use `failurePolicy: Fail` from the first apply (job-patch installs briefly with `Ignore` until the patch Job rewrites the CA and policy):

```
helm install vela-core charts/vela-core -n vela-system --create-namespace \
  --set featureGates.enableClusterInfrastructure=true \
  --set admissionWebhooks.certManager.enabled=true
```

Note the gate does **not** control whether the CRD exists. Helm applies `crds/` unconditionally, so `kubectl get spokeclusters` works either way; with the gate off the objects simply never get a status.

Apply `10-networkpolicy.yaml` when the CNI enforces NetworkPolicy. The policy opens the webhook port as to-source (API server IPs vary), restricts `/metrics` to Prometheus, and lets cluster-core egress to DNS, the API server, cluster-gateway, and HTTPS (AWS). It does not lock cluster-gateway ingress; the aggregated APIService and vela-core also dial it.

## CLI

Prefer `vela cluster spokes` over `vela cluster join` for new clusters.

```
vela cluster spokes create my-spoke --kubeconfig ./spoke.kubeconfig
vela cluster spokes create prod-east --aws --aws-region us-west-2 \
  --aws-role-arn arn:aws:iam::111122223333:role/spokecluster-prod-east
vela cluster spokes list
vela cluster spokes show my-spoke
vela cluster spokes detach my-spoke
```

`create --kubeconfig` writes a Secret (`<name>-kubeconfig` by default, or `--secret`) and a SpokeCluster. `create --aws` writes no Secret; pass `--aws-region` and `--aws-role-arn` (optional `--aws-cluster-name`, `--aws-auth-mode`, `--aws-external-id`). `detach` deletes the SpokeCluster; the controller then removes the gateway Secret unless `deletionPolicy` is `orphan`. A kubeconfig source Secret is left in place. Use `--secret existing-name` when the kubeconfig Secret already exists.

## Migrating from `vela cluster join`

`vela cluster join` writes the gateway Secret itself. A SpokeCluster with the same name is refused: the controller will not adopt a Secret it did not create.

To convert a joined cluster:

1. `vela cluster detach <name>` (this drops the join registration)
2. `vela cluster spokes create <name> --kubeconfig ./spoke.kubeconfig`

Or keep the join registration and create a SpokeCluster under a different name. Existing join clusters keep working without any change.
