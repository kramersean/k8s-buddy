# 3. NetworkPolicy ingress open on TCP 8080 rather than podSelector-scoped

## Status

Accepted

## Context

`deploy/kustomize/base/networkpolicy.yaml` establishes a default-deny posture
for the `k8s-buddy` namespace and then opens exactly two holes: DNS egress to
kube-system, and application ingress on TCP 8080 to buddy-api pods.

That second rule has no `from:` selector — it admits port 8080 from any source.
A reviewer's first instinct is that this should be scoped, for example
`from: [podSelector: {}]` to restrict it to in-namespace traffic. That
restriction silently breaks the demo, and the reason is not visible in the
manifest.

The demo's traffic path is NodePort 30080. Verified against this cluster with
kubectl: traffic arriving via a NodePort is SNATed by kube-proxy to the node's
own IP before it reaches the pod, so the source address the NetworkPolicy
evaluates is a node IP, not the original client's and not a pod IP. A
`from: podSelector` rule therefore does not match, and the packet is dropped.
The failure mode is the worst kind: TCP connects and then hangs forever, with
no RST — a black hole that looks like an application bug, not a policy denial.
kind's kindnet CNI does enforce NetworkPolicy in this cluster, confirmed
empirically by observing DNS egress fail under default-deny before the DNS hole
was added, so the unqualified rule is a real, load-bearing hole rather than an
inert label.

Two tighter alternatives were considered:

- **`externalTrafficPolicy: Local` on the NodePort Service.** This preserves
  the client source IP, which would let a `from:` selector work. It is actively
  wrong for this cluster: the 30080 `extraPortMapping` in
  `deploy/kind/kind-config.yaml` lands on the control-plane node, which is
  NoSchedule-tainted and therefore never runs a buddy-api pod. With `Local`,
  that node has no local endpoint to route to and drops the traffic outright.
- **An `ipBlock` covering the node/pod CIDR.** Tighter than "any source", but it
  hardcodes a docker-assigned subnet. This machine's kind network happens to be
  `172.20.0.0/16`; kind commonly assigns `172.18.0.0/16` or another range on a
  clean install. Committing one would reintroduce exactly the "works on my
  machine, fails on a clean checkout" failure mode this project is trying to
  avoid.

## Decision

The `allow-http-ingress` policy opens TCP 8080 with no `from:` selector.

The blast radius is kept small on the axes that can be constrained portably:
the policy's `podSelector` matches only buddy-api pods, only port 8080 is
opened, only the Ingress policy type is declared, and egress from those pods
remains fully default-denied except for DNS.

## Consequences

- The NodePort path that `hack/demo.sh` and a host `curl localhost:30080`
  both use works on any machine, with any docker-assigned CIDR, with no
  per-machine edits.
- Any pod in the cluster can reach buddy-api on 8080. Given that buddy-api is
  deliberately exposed to the host through a NodePort, this widens nothing that
  was not already reachable.
- The unqualified rule reads as sloppy without this context, which is precisely
  why the reasoning is recorded here and referenced from the manifest.
- If K8s Buddy later moves behind an Ingress controller instead of a NodePort,
  this decision should be revisited: with a known ingress-controller namespace
  as the only client, a `namespaceSelector`-scoped rule becomes both correct and
  portable. That would supersede this ADR.
