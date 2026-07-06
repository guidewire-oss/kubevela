# k3d + OCM + KubeVela Dispatcher Demo

This is a quick local demo flow to run the dispatcher examples with OCM on k3d.

## Prerequisites

- `k3d`
- `kubectl`
- `clusteradm` (from OCM)
- KubeVela controller running against the hub cluster (for local dev, `make run`)

## 1) Create 2 k3d clusters (shared network + reachable hub API)

Create a shared Docker network so hub/spoke containers can communicate:

```bash
docker network create vela-net
HOST_IP=$(ipconfig getifaddr en0)
```

If `en0` is not your active interface, use the correct interface/IP for your machine.

Create the hub with explicit API port and TLS SANs so OCM agents can reach it via host endpoints:

```bash
k3d cluster create kubevela \
  --network vela-net \
  --api-port 50934 \
  --k3s-arg "--tls-san=${HOST_IP}@server:0" \
  --k3s-arg "--tls-san=host.docker.internal@server:0" \
  --k3s-arg "--tls-san=0.0.0.0@server:0" \
  --servers 1 --agents 1

# Spoke TLS SANs are recommended if you also want direct push dispatch
# from KubeVela to the spoke (non-OCM path).
k3d cluster create spoke1 \
  --network vela-net \
  --api-port 50935 \
  --k3s-arg "--tls-san=${HOST_IP}@server:0" \
  --k3s-arg "--tls-san=host.docker.internal@server:0" \
  --k3s-arg "--tls-san=0.0.0.0@server:0" \
  --servers 1 --agents 1
```

Optional context aliases:

```bash
kubectl config rename-context k3d-kubevela hub
kubectl config rename-context k3d-spoke1 spoke1
```

If you skip aliases, replace `hub`/`spoke1` below with `k3d-kubevela`/`k3d-spoke1`.

## 2) Install OCM hub components

```bash
clusteradm init --context hub --wait
```

## 3) Generate join command and register spoke to OCM

```bash
clusteradm get token --context hub
```

Copy the printed `clusteradm join ...` command and run it against the spoke context.
Use a hub apiserver endpoint that the spoke can reach from inside k3d (commonly `host.k3d.internal:50934`), for example:

```bash
clusteradm join \
  --hub-token <token> \
  --hub-apiserver https://host.k3d.internal:50934 \
  --cluster-name spoke1 \
  --context spoke1 \
  --wait
```

Accept the spoke on hub:

```bash
clusteradm accept --clusters spoke1 --context hub
```

Verify:

```bash
kubectl --context hub get managedclusters
```

## 4) Register clusters to KubeVela (cluster-gateway)

Register local hub cluster:

```bash
vela cluster join local --in-cluster-bootstrap --yes
```

Register the spoke cluster using kubeconfig context:

```bash
vela cluster join spoke1 --cluster-kubeconfig-context spoke1 --yes
```

Verify:

```bash
vela cluster ls
```

## 5) Apply dispatcher examples

```bash
kubectl apply -f examples/workflow/dispatcher/cluster-gateway-dispatcher.yaml
kubectl apply -f examples/workflow/dispatcher/internal-cluster-gateway-dispatcher.yaml
kubectl apply -f examples/workflow/dispatcher/ocm-manifestwork-dispatcher.yaml
```

## 6) Apply demo applications

```bash
kubectl apply -f examples/workflow/dispatcher/deploy-with-dispatcher.yaml
kubectl apply -f examples/workflow/dispatcher/deploy-with-internal-dispatcher.yaml
kubectl apply -f examples/workflow/dispatcher/deploy-with-ocm-dispatcher.yaml
```

## 7) Quick verification

```bash
kubectl -n vela-system get dispatcher
kubectl -n default get application
kubectl --context hub -n ocm-spoke get manifestwork
```

Notes:

- The default dispatcher example is named `default`.
- If `deploy` step omits `dispatcher`, controller-level default dispatcher is used (`default` unless overridden by `--default-dispatcher`).
