# The local Kubernetes cluster

```bash
kind create cluster --config deploy/kind/cluster.yaml
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
kubectl wait --for=condition=Ready node --all --timeout=300s

kubectl apply -f deploy/kubernetes/00-namespaces.yaml -f deploy/kubernetes/10-rbac.yaml

docker build -f deploy/docker/task.Dockerfile -t runmesh/task:dev .
kind load docker-image runmesh/task:dev --name runmesh
```

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
`kubectl get networkpolicy` lists them, and they do nothing. Week 4 puts the
tool sandbox behind one, and a security control whose every observable signal
says "in place" while it permits everything is the worst thing to discover late.

Verify enforcement rather than believing it:

```bash
kubectl -n runmesh-tasks run probe --image=busybox:1.36 --restart=Never -- \
    sh -c 'wget -qO- --timeout=3 https://example.com >/dev/null && echo REACHABLE || echo BLOCKED'
kubectl -n runmesh-tasks logs probe
```

With no policy it prints `REACHABLE`. Once Week 4's deny-all policy is applied,
the same command must print `BLOCKED`. If it still says `REACHABLE`, the CNI is
not enforcing and nothing built on top of that policy is real.

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
