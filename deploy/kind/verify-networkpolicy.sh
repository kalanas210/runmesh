#!/usr/bin/env bash
# Prove the sandbox NetworkPolicy is ENFORCED, not merely applied.
#
#   ./deploy/kind/verify-networkpolicy.sh
#
# Run this after creating the cluster, after installing or upgrading the CNI,
# and before believing any sentence in this repository that contains the word
# "isolated". It takes about a minute.
#
# Why it exists: kind's default CNI accepts NetworkPolicy objects and ignores
# them completely. `kubectl get networkpolicy` lists them, `describe` shows the
# rules, no event is emitted, and every packet flows. The only way to know the
# policy is real is to send a packet and watch it fail — which is what this
# does, with four probes:
#
#   1. a deny-labelled pod cannot reach the internet          (default-deny works)
#   2. a deny-labelled pod cannot resolve DNS                 (default-deny is total)
#   3. an allow-labelled pod CAN reach the internet           (the grant works)
#   4. an allow-labelled pod cannot reach a cluster-internal
#      address                                                (the except list works)
#
# Probe 4 is the one worth having. A policy that permits egress to 0.0.0.0/0
# passes probes 1-3 and is wide open to the instance metadata service.

set -euo pipefail

NS="${RUNMESH_TASK_NAMESPACE:-runmesh-tasks}"
IMAGE="${PROBE_IMAGE:-curlimages/curl:8.11.1}"
# Any public address works. example.com is stable, small, and not somebody's
# production API being hit by everybody who runs this script.
TARGET="${PROBE_TARGET:-https://example.com}"
TIMEOUT="${PROBE_TIMEOUT:-8}"

pass=0
fail=0

cleanup() {
  kubectl -n "$NS" delete pod -l runmesh.io/probe=networkpolicy \
    --ignore-not-found --now >/dev/null 2>&1 || true
}
trap cleanup EXIT

# probe runs one curl in a pod carrying the given network label and reports
# whether it connected. `kubectl run --rm -i` blocks until the pod exits and
# propagates its exit status, so curl's own timeout is the whole mechanism.
probe() {
  local name="$1" label="$2" url="$3"
  kubectl -n "$NS" run "$name" \
    --image="$IMAGE" \
    --restart=Never \
    --labels="runmesh.io/network=${label},runmesh.io/probe=networkpolicy" \
    --command --quiet --rm --attach \
    -- curl --silent --show-error --fail --max-time "$TIMEOUT" --output /dev/null "$url" \
    >/dev/null 2>&1
}

expect() {
  local description="$1" want="$2" name="$3" label="$4" url="$5"
  local got="reached"
  probe "$name" "$label" "$url" || got="blocked"

  if [ "$got" = "$want" ]; then
    printf '  \033[32mPASS\033[0m  %-58s (%s)\n' "$description" "$got"
    pass=$((pass + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-58s (want %s, got %s)\n' "$description" "$want" "$got"
    fail=$((fail + 1))
  fi
}

echo
echo "Verifying NetworkPolicy enforcement in namespace ${NS}"
echo

if ! kubectl -n "$NS" get networkpolicy task-default-deny >/dev/null 2>&1; then
  echo "  the policies are not installed: kubectl apply -f deploy/kubernetes/20-networkpolicy.yaml" >&2
  exit 2
fi

expect "a denied pod cannot reach the internet"      blocked probe-deny-net  deny  "$TARGET"
# By IP, so a pass cannot be DNS failing for an unrelated reason. 1.1.1.1 is
# Cloudflare's resolver and answers HTTPS.
expect "a denied pod cannot reach a public IP"       blocked probe-deny-ip   deny  "https://1.1.1.1"
expect "an allowed pod can reach the internet"       reached probe-allow-net allow "$TARGET"
# The Kubernetes API service, always at the first address of the service CIDR,
# inside 10.0.0.0/8 — which the except list removes. A pod that reaches this has
# egress to the whole cluster.
expect "an allowed pod cannot reach the cluster API" blocked probe-allow-api allow "https://kubernetes.default.svc"

echo
if [ "$fail" -ne 0 ]; then
  cat >&2 <<'EOF'

  NETWORK POLICY IS NOT DOING WHAT THE MANIFESTS SAY.

  The usual cause is a CNI that does not implement NetworkPolicy. kind's
  default (kindnet) does not. Check which one is installed:

      kubectl -n kube-system get pods -l k8s-app=calico-node
      kubectl -n kube-system get daemonset

  If Calico is absent, recreate the cluster from deploy/kind/cluster.yaml —
  which sets disableDefaultCNI: true — and install Calico before applying
  anything else. See deploy/kind/README.md.

  Until every probe above passes, no claim about task isolation in this
  repository is true.
EOF
  exit 1
fi

echo "  ${pass}/${pass} probes behaved as the policy says they should."
echo
