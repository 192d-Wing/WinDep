# WinDep telemetry API

A small, **stateless** Go ([Fiber](https://gofiber.io/)) service that receives deployment
**status**, streamed **logs**, and **inventory** from WinPE agents over HTTPS. It runs on
Kubernetes with Cilium, fronted by an **anycast** LoadBalancer VIP, and scales horizontally.

## Why stateless

Reports and logs are written as **structured JSON to stdout** (for your cluster log pipeline —
Loki/ELK/etc.) and surfaced as **Prometheus metrics** at `/metrics`. No authoritative per-machine
state lives in the pod, so:

- any replica can serve any machine's POST → an **anycast VIP + ECMP** is correct;
- pods scale up/down (and get rescheduled) with **no data loss**;
- the "source of truth" dashboard reads from your logs/metrics backend, not from the API.

`GET /api/machines` returns a small **per-pod** ring buffer for quick debugging only — it is
explicitly non-authoritative (a given machine's reports may have landed on other replicas).

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/api/report` | Deployment status (`state`, `percent`, `message`, ...) |
| POST | `/api/log` | Batched deploy log lines |
| POST | `/api/inventory` | Full hardware inventory (optional) |
| GET | `/api/machines` | Per-pod debug snapshot (non-authoritative) |
| GET | `/metrics` | Prometheus metrics |
| GET | `/healthz`, `/readyz` | Liveness / readiness probes |

## Configuration (env)

| Var | Default | Notes |
|-----|---------|-------|
| `LISTEN_ADDR` | `:8443` | Listen address |
| `TLS_CERT` / `TLS_KEY` | _(unset)_ | PEM paths. If set, serves HTTPS; if unset, plaintext (expects upstream TLS termination). |

WinPE trusts the internal CA, so the mounted cert must chain to it.

## Build & run locally

```bash
cd api
go mod tidy
go run .                    # plaintext on :8443
# or with TLS:
TLS_CERT=api.crt TLS_KEY=api.key go run .
```

Container:

```bash
docker build -t ghcr.io/192d-wing/windep-api:0.1.0 api/
docker push ghcr.io/192d-wing/windep-api:0.1.0
```

## Deploy (Kubernetes + Cilium)

Prereqs: Cilium with the **BGP control plane** enabled (for anycast) and metrics-server
(for the HPA).

Manifests are managed with **Kustomize** (`platform/`): a `base/` plus an `overlays/example/`
whose single [`vars.yaml`](../platform/overlays/example/vars.yaml) sets the per-environment BGP
ASNs and anycast addresses (see "Per-environment variables" below).

```bash
# 1) TLS cert (chains to your internal CA)
kubectl create namespace windep
kubectl -n windep create secret tls windep-api-tls --cert=api.crt --key=api.key

# 2) Edit platform/overlays/example/vars.yaml (ASNs, anycast IP, LB CIDR, peer)
#    and the image in platform/overlays/example/kustomization.yaml, then apply
#    everything (app + Cilium LB IPAM + BGP) in one shot:
kubectl apply -k platform/overlays/example

#    Preview without applying:
kubectl kustomize platform/overlays/example
```

For a flat L2 segment without BGP (NOT anycast), swap `cilium/bgp-peering.yaml` for
`cilium/l2announcement.yaml` in an overlay.

### Per-environment variables

[`platform/overlays/example/vars.yaml`](../platform/overlays/example/vars.yaml) is the one file
you edit per site. Kustomize `replacements` inject it into the manifests:

| Variable | Injected into | Type |
|----------|---------------|------|
| `anycastIP` | Service `io.cilium/lb-ipam-ips` annotation | string |
| `lbPoolCIDR` | `CiliumLoadBalancerIPPool` block | string |
| `localASN` / `peerASN` | `CiliumBGPPeeringPolicy` router / neighbor | **integer** |
| `peerAddress` | BGP neighbor address | string |

ASNs are kept as integers in a typed YAML resource (not a stringly-typed ConfigMap/`.env`) so the
Cilium CRD's integer schema is satisfied. Copy the `example` overlay per environment.

Point the agents at the VIP by setting `apiUrl` in
[`Deploy/ztp.config.json`](../Deploy/ztp.config.json), e.g.
`https://windep-api.jhics.org` resolving to `10.0.100.10`.

## Anycast, in one paragraph

The Service gets VIP `10.0.100.10` from the Cilium LB IP pool. Every node advertises that VIP as
a `/32` over BGP, so the upstream router ECMP-hashes client flows across all nodes — one address,
many paths. Scale the Deployment and new pods simply start serving; drain a node and its BGP path
withdraws and flows re-hash. The HPA adjusts replica count on CPU (swap in the Prometheus RPS
metric in `hpa.yaml` for request-rate scaling).

## Scaling knobs

- **HPA**: `minReplicas`/`maxReplicas` and the CPU target in `platform/base/k8s/hpa.yaml`.
- **Request-rate scaling**: fiberprometheus exposes `http_requests_total`; wire prometheus-adapter
  and uncomment the Pods metric in the HPA.
- **Resources**: tune `requests`/`limits` in `platform/base/k8s/deployment.yaml` (HPA math uses requests).
