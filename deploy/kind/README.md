# The local Kubernetes cluster

```bash
kind create cluster --config deploy/kind/cluster.yaml
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
kubectl wait --for=condition=Ready node --all --timeout=300s

kubectl apply -f deploy/kubernetes/

docker build -f deploy/docker/task.Dockerfile   -t runmesh/task:dev   .
docker build -f deploy/docker/python.Dockerfile -t runmesh/python:dev .
kind load docker-image runmesh/task:dev runmesh/python:dev --name runmesh

./deploy/kind/verify-networkpolicy.sh
```

That last line is not optional. See below.

The Calico manifest is referenced by pinned tag rather than vendored: it is a
quarter of a megabyte of generated YAML that nothing in this repository edits,
and a pinned URL is reproducible without putting it in every `git log -p`.

## Why the node comes up NotReady

`deploy/kind/cluster.yaml` sets `disableDefaultCNI: true`, so there is no
networking until Calico is applied. A `NotReady` node between those two commands
is the expected state, not a failure — and `kind create cluster --wait` will
time out there, which is a confusing way to learn it.

The reason for disabling it is in the cluster config: kind's default CNI
(kindnet) **does not enforce NetworkPolicy**. Policies apply without error,
`kubectl get networkpolicy` lists them, and they do nothing. The tool sandbox is
behind one, and a security control whose every observable signal says "in place"
while it permits everything is the worst thing to discover late.

## Verify the sandbox, do not believe it

```bash
./deploy/kind/verify-networkpolicy.sh
```

Four probes, about a minute:

| Probe | Expected | What a wrong answer means |
|---|---|---|
| a `network=deny` pod fetches a URL | blocked | the default-deny policy is not enforced |
| a `network=deny` pod fetches an IP | blocked | as above, and not merely DNS failing |
| a `network=allow` pod fetches a URL | reached | the grant path is broken; `http_request` cannot work |
| a `network=allow` pod fetches the cluster API | blocked | egress is `0.0.0.0/0` with no exclusions — the instance metadata service is reachable |

The fourth is the one worth having. A policy that permits egress to everything
passes the first three and is wide open to `169.254.169.254`, which on EC2, GCE
and Azure hands out the node's cloud credentials to anything that asks.

Run it after creating the cluster, after upgrading the CNI, and before believing
any sentence in this repository containing the word "isolated".

Two things back it up from the Go side, and neither replaces it:

- `TestNetworkPolicySelectorsMatchTheJobLabels` reads the shipped manifests and
  fails if the `runmesh.io/network` label the code sets stops matching the
  selector the YAML uses — a drift that would leave task pods matching *no*
  policy, which Kubernetes treats as unrestricted.
- `cmd/task` refuses to connect to any non-public address itself, checked in the
  dialer after DNS resolution. That is the control that holds when the CNI turns
  out not to enforce anything.

## Requirements, and the one that bites on Windows

**Kubernetes needs cgroup v2.** Docker Desktop on WSL2 can be configured for
cgroup v1, and on such a machine `kubeadm init` fails part-way through with

```
error execution phase wait-control-plane: cannot obtain client without bootstrap:
could not bootstrap the admin user in file admin.conf: unable to create ClusterRoleBinding:
context deadline exceeded
```

which says nothing about cgroups at all. Check with `docker info --format
'{{.CgroupVersion}}'`. To fix it, add to `%USERPROFILE%\.wslconfig`:

```ini
[wsl2]
kernelCommandLine = cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1
```

then `wsl --shutdown` and restart Docker Desktop. Note that this restarts every
container on the machine.

Without that change, current node images will not boot and an older Kubernetes
is the workaround:

```bash
kind create cluster --config deploy/kind/cluster.yaml --image kindest/node:v1.30.0
```

That is a local development accommodation, not a supported configuration: the
repository targets whatever `kind create cluster` gives by default, and
`client-go` is pinned to match it.
