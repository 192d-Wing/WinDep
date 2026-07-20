# WinDep platform (Helm chart)

Deploys the full WinDep control plane — the [telemetry/ZTP API](../api/) (`windep-api`),
the nginx **deploy server** that serves the boot/WIM/config payload tree (`windep-web`), and
the Cloudscape **admin** console (`windep-admin`) — behind Cilium **anycast** VIPs, on either
full Kubernetes or **k3s**.

```
platform/windep/
├─ Chart.yaml
├─ values.yaml            # documented defaults
├─ values-k3s.yaml        # k3s edge site
├─ values-example.yaml    # full Kubernetes site
└─ templates/
   ├─ api/  web/  admin/  # the three workloads (+ Service/PVC/PDB/HPA/NetworkPolicy)
   ├─ cilium/             # LB IP pool + BGP advertisement (L2 announcement alternative)
   └─ tls/                # cert-manager ACME Issuer + Certificates
```

Per-site specifics (VIPs, StorageClasses, admin allow-list, image tags, ACME server) live in a
`values-<site>.yaml`. BGP **peering** (ASNs, neighbors) is **cluster-owned** — a cluster-wide
`CiliumBGPClusterConfig` peers upstream and its `CiliumBGPPeerConfig`s export any
`CiliumBGPAdvertisement` labeled `advertise: <group>`. The chart therefore ships only a
`CiliumBGPAdvertisement` (labeled with `networking.advertiseGroup`), never a peering policy.

```bash
helm template windep platform/windep -f platform/windep/values-k3s.yaml      # preview
helm install  windep platform/windep -n windep --create-namespace \
  -f platform/windep/values-k3s.yaml                                         # apply (k3s)
helm install  windep platform/windep -n windep --create-namespace \
  -f platform/windep/values-example.yaml                                     # apply (full k8s)
helm upgrade  windep platform/windep -n windep -f platform/windep/values-k3s.yaml   # roll an update
```

Copy a values file per environment and edit its VIPs, StorageClass, `admin.allowedCIDRs`,
`images.*.tag`, and the `certManager.issuer.acme` block.

---

## Prerequisites

- **Cilium** as CNI with `bgpControlPlane.enabled=true` (or `l2announcements.enabled=true` for
  `networking.mode: l2`), providing the `CiliumLoadBalancerIPPool` / `CiliumBGPAdvertisement` /
  `CiliumL2AnnouncementPolicy` CRDs.
- **metrics-server** (for the API HPA).
- **cert-manager** installed cluster-wide when `certManager.enabled: true` (the default).
- **RWX StorageClass** for the payload PV (`storage.webRoot`) — NFS, Longhorn share-manager,
  CephFS. `local-path` (k3s default) is RWO and will not bind it.

### Secrets (not created by the chart)

```bash
# Iron Bank pull secret for the windep-web nginx image (Repo One creds):
kubectl -n windep create secret docker-registry repo1-pull \
  --docker-server=registry1.dso.mil \
  --docker-username=<REPO1_USER> --docker-password=<REPO1_TOKEN>

# Optional app secrets. Set CONFIG_KEY once WinPE carries the matching key:
kubectl -n windep create secret generic windep-admin-config-key --from-literal=key="$(openssl rand -base64 32)"
kubectl -n windep create secret generic windep-admin-auth       --from-literal=token="<bearer>"
```

TLS is handled by cert-manager (below). To manage TLS yourself instead, set
`certManager.enabled: false` and pre-create `windep-{api,web,admin}-tls`
(`kubectl create secret tls ... --cert=... --key=...`).

---

## TLS via cert-manager ACME

With `certManager.enabled: true` the chart renders an ACME `Issuer` (or `ClusterIssuer`) and a
`Certificate` per service, writing into the `windep-{api,web,admin}-tls` secrets the workloads
mount. cert-manager issues and auto-renews them — no manual cert rotation.

> **The ACME server MUST be your internal ACME CA** (e.g. step-ca / smallstep), so issued certs
> chain to the internal root **baked into `boot.wim`**. WinPE trusts only that root — a public
> Let's Encrypt cert would fail validation on every WinPE HTTPS fetch. cert-manager supports the
> **DNS-01** and **HTTP-01** solvers only (not TLS-ALPN-01); set `certManager.issuer.acme.solvers`
> to your internal DNS-01 provider/webhook (the values files ship an `rfc2136` stub).

Prereqs the ACME flow needs: the ACME account private-key secret is created by cert-manager on
first use; a DNS-01 solver typically needs a credential secret (e.g. `windep-acme-tsig` for
rfc2136) — create it per your provider.

Check issuance: `kubectl -n windep get issuer,certificate,certificaterequest,order,challenge`.

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

# 3) Secrets (above) + install the chart with the k3s values.
kubectl create namespace windep
helm install windep platform/windep -n windep -f platform/windep/values-k3s.yaml
```

Notes:

- **metrics-server**: k3s bundles it (needed by the HPA). If you added `--disable=metrics-server`,
  install it separately or the HPA will not scale.
- **Single node**: for a one-node k3s, lower `api.hpa.minReplicas` / `api.replicas` if the node
  can't run two API pods. Anycast/ECMP is moot on a single node — BGP still advertises the VIP,
  just from one path.
- **L2 instead of BGP**: on a flat segment with no BGP peer, set `networking.mode: l2` and install
  Cilium with `--set l2announcements.enabled=true`. L2 is failover, not anycast.

---

## Values reference (per-site knobs)

| Value | Into | Notes |
|-------|------|-------|
| `web.vip` / `admin.vip` | Service `io.cilium/lb-ipam-ips` annotation | anycast VIPs |
| `networking.lbPoolCIDR` | `CiliumLoadBalancerIPPool` block | pool the VIPs draw from |
| `networking.advertiseGroup` | `CiliumBGPAdvertisement` `advertise:` label | must match the cluster's peer configs, or the VIP is allocated but never advertised |
| `networking.mode` | `bgp` → advertisement, `l2` → announcement | never both |
| `admin.allowedCIDRs` | `windep-admin` NetworkPolicy ingress | source subnets for the RW VIP |
| `storage.webRoot.storageClass` | payload PVC | must be ReadWriteMany |
| `images.{api,admin,nginx}.tag` | container images | per-site registry/tag |
| `certManager.issuer.acme.*` | cert-manager `Issuer` | internal ACME directory + DNS-01 solver |

---

## Network boot & DHCP

Standing up the payloads and wiring UEFI HTTPS Boot / TFTP PXE (DHCP options 60/66/67) is covered
in [../Server/README.md](../Server/README.md). The chart provisions the `windep-web` deploy server
(which serves `/boot`, `/images`, `/config` and proxies `/api/*`); point your DHCP boot URL at that
VIP once the payload PV is populated via the admin console.
