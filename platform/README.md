# WinDep platform manifests (Kustomize)

Deploys the [telemetry API](../api/) with a Cilium **anycast** VIP, on either full
Kubernetes or **k3s**.

```
platform/
├─ base/                     # environment-agnostic manifests
│  ├─ k8s/                   # Deployment, Service, HPA, PDB, Namespace
│  └─ cilium/                # LB IP pool, BGP peering (L2 announcement alt)
├─ components/
│  └─ anycast/               # shared replacements: vars.yaml -> manifests
└─ overlays/
   ├─ example/               # full Kubernetes cluster
   └─ k3s/                   # k3s edge cluster
```

Each overlay supplies a single **`vars.yaml`** (the variable file) with that site's BGP ASNs and
anycast addresses; the `anycast` component injects them via Kustomize `replacements`. ASNs stay
**integers** (a typed YAML resource, not a stringly-typed ConfigMap/`.env`).

```bash
kubectl kustomize platform/overlays/example   # preview
kubectl apply   -k platform/overlays/example  # apply (full k8s)
kubectl apply   -k platform/overlays/k3s      # apply (k3s)
```

Copy an overlay per environment and edit its `vars.yaml` + the image tag in `kustomization.yaml`.

---

## Full Kubernetes

Prereqs: Cilium as CNI with `bgpControlPlane.enabled=true`, and metrics-server (for the HPA).

```bash
kubectl create namespace windep
kubectl -n windep create secret tls windep-api-tls --cert=api.crt --key=api.key
kubectl apply -k platform/overlays/example
```

---

## k3s

k3s bundles **Klipper ServiceLB** and **flannel**, which both fight Cilium's LoadBalancer IPAM +
BGP. Disable them at install so Cilium owns the VIPs.

```bash
# 1) Install k3s WITHOUT servicelb, traefik, flannel, or kube-proxy.
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="\
  --flannel-backend=none \
  --disable-network-policy \
  --disable-kube-proxy \
  --disable=servicelb \
  --disable=traefik" sh -

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# 2) Install Cilium as the CNI, replacing kube-proxy, with the BGP control plane.
API_SERVER_IP=<control-plane-ip>
cilium install \
  --set kubeProxyReplacement=true \
  --set k8sServiceHost=${API_SERVER_IP} --set k8sServicePort=6443 \
  --set bgpControlPlane.enabled=true \
  --set operator.replicas=1
cilium status --wait

# 3) TLS + apply the k3s overlay.
kubectl create namespace windep
kubectl -n windep create secret tls windep-api-tls --cert=api.crt --key=api.key
kubectl apply -k platform/overlays/k3s
```

Notes:

- **metrics-server**: k3s bundles it (needed by the HPA). If you added `--disable=metrics-server`,
  install it separately or the HPA will not scale.
- **Single node**: for a one-node k3s, keep `minReplicas: 2` only if the node can run 2 pods;
  otherwise lower it in an overlay patch. Anycast/ECMP is moot on a single node — BGP still
  advertises the VIP, just from one path.
- **L2 instead of BGP**: on a flat segment with no BGP peer, swap `cilium/bgp-peering.yaml` for
  `cilium/l2announcement.yaml` (via an overlay) and set `--set l2announcements.enabled=true` on the
  Cilium install. L2 is failover, not anycast.

---

## The variable file

`overlays/<env>/vars.yaml`:

| Field | Injected into | Type |
|-------|---------------|------|
| `anycastIP` | Service `io.cilium/lb-ipam-ips` annotation | string |
| `lbPoolCIDR` | `CiliumLoadBalancerIPPool` block | string |
| `localASN` / `peerASN` | `CiliumBGPPeeringPolicy` router / neighbor | integer |
| `peerAddress` | BGP neighbor address | string |

The image (registry/tag) is set with the native Kustomize `images:` transformer in each overlay's
`kustomization.yaml`.
