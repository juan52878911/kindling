# Kubernetes operator

`kindling-operator` lets you ask for sandboxes with `kubectl apply` instead of the CLI: a
`Sandbox` object declares what you want, and the operator creates, keeps and deletes it by
talking to the sandbox frontal over HTTP. Design, scope and status fields:
[`docs/kubernetes.md`](../kubernetes.md).

## What it is, and is not

- A **declarative control plane**: Kubernetes stores the intent (`spec`) and the last thing
  observed (`status`); the frontal is what actually creates and destroys microVMs.
- A **thin operator**: one binary, no client-go or controller-runtime, idempotent and
  restart-safe (it never creates twice a sandbox that already has `status.id`; the finalizer
  stays until the frontal confirms the deletion).
- **Not a pod runtime**: microVMs never run inside the cluster; no Pod per sandbox, no CNI,
  no CSI. **Not the data plane**: `exec`, `files` and `shell` go from the client straight to
  the frontal with the tenant's token. **Not tenant isolation**: quotas and ownership come
  from the frontal's token, not the namespace. **Not HA**: one replica, no leader lease;
  do not set `replicas > 1`.

## 1. Prerequisites

- A kindling daemon on a host with KVM, and the sandbox frontal in front of it
  (`kling plugin install sandbox`, then `cd ext/sandbox && make deploy HOST=ssh://…`, or the
  `kindling-sandbox-host.tar.gz` of the release). See [`ext/sandbox/README.md`](../../ext/sandbox/README.md).
- A tenant token for the operator (`KLING_SANDBOX_TENANTS="ops:tok:20:5"` on the frontal).
- A template on the frontal (`kling sbx template apply -f node.json`).

## 2. Install

The image is `ghcr.io/juan52878911/kindling-operator:<kindling version>`; the manifests are
in `ext/sandbox/deploy/` and in `kindling-operator-deploy.tar.gz` of every release.

```sh
kubectl apply -f ext/sandbox/deploy/namespace.yaml
kubectl apply -f ext/sandbox/deploy/crd.yaml
kubectl apply -f ext/sandbox/deploy/rbac.yaml

# a tenant token of the frontal, not a Kubernetes credential
kubectl create secret generic kindling-operator-frontal \
  --namespace kindling-system \
  --from-literal=token='<tenant token>'

# edit ext/sandbox/deploy/deployment.yaml: the image tag and KLING_SANDBOX_URL
kubectl apply -f ext/sandbox/deploy/deployment.yaml
kubectl -n kindling-system get deploy kindling-operator
```

Outside a cluster (against `kubectl proxy`, for a quick try):

```sh
kubectl proxy --port=8001 &
KLING_SANDBOX_URL=https://sandbox.example.internal KLING_SANDBOX_TOKEN=... \
  kindling-operator -kube-url http://127.0.0.1:8001
```

## 3. Ask for a sandbox

```yaml
apiVersion: sandbox.kindling.dev/v1alpha1
kind: Sandbox
metadata:
  name: agent-pr-123
  namespace: team-a
spec:
  template: node        # or image: toolchain, not both
  ttlSeconds: 1800
  onTTL: freeze          # default: freeze at zero cost, resume in ms
  egress: none            # default: no network
```

```sh
kubectl apply -f sandbox.yaml
kubectl get sandbox agent-pr-123 -n team-a
# NAME           TEMPLATE   STATE     HOST    EXPIRES                AGE
# agent-pr-123   node       running   host1   2026-09-22T18:30:00Z   4s

kubectl get sandbox agent-pr-123 -n team-a -o jsonpath='{.status.id}'
# host1/3f9a2b71...
```

That `status.id` is what the data plane uses:

```sh
kling sbx exec host1/3f9a2b71 -- node --version
kling sbx shell host1/3f9a2b71
kubectl delete sandbox agent-pr-123 -n team-a
```

Changing `spec.ttlSeconds` on a live `Sandbox` renews its TTL. Changing any other field
does nothing (the frontal fixes network, memory and TTL policy at creation): delete and
recreate.

## 4. Verify and operate

```sh
kubectl -n kindling-system logs deploy/kindling-operator
kubectl get sandbox -A
curl -H "Authorization: Bearer $TOK" https://sandbox.example.internal/v1/metrics   # Prometheus, on the frontal
```

After a host reboot, kindling's golden snapshots on that host are invalid; the frontal
rebuilds the template in the background from the recipe annotated in the snapshot and,
meanwhile, serves from any other healthy host. The operator does not need to know.
