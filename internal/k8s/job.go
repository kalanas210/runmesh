package k8s

import (
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// Label and annotation keys. The Kubernetes object name is a lossy, derived
// handle (see names.go); THESE carry the real RunMesh identity, which is what
// makes `kubectl get jobs -l runmesh.io/job-id=job_06g7...` work and what lets
// a future reaper find workloads belonging to a step that no longer exists.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelJobID     = "runmesh.io/job-id"
	LabelStepID    = "runmesh.io/step-id"
	LabelAttempt   = "runmesh.io/attempt"
	LabelTool      = "runmesh.io/tool"
	LabelOwner     = "runmesh.io/owner"

	// LabelNetwork is the one label with teeth. The sandbox NetworkPolicy in
	// deploy/kubernetes/20-networkpolicy.yaml selects on it, so this string is
	// a contract between Go code and YAML: change it here without changing it
	// there and every task silently gets the deny-all policy, or — far worse —
	// none at all. TestNetworkPolicySelectorsMatchTheJobLabels reads the
	// manifest and fails if the two ever drift.
	LabelNetwork = "runmesh.io/network"

	// The two values LabelNetwork takes. Every pod carries one of them: a pod
	// with no label at all would match no policy, and "matches no policy" in
	// Kubernetes means "unrestricted", which is the opposite of the intent.
	NetworkDeny  = "deny"
	NetworkAllow = "allow"

	// AnnAttemptID and AnnIdempotencyKey hold the values labels cannot: an
	// annotation has no length limit and no alphabet, so the identity survives
	// intact rather than sanitised.
	AnnAttemptID       = "runmesh.io/attempt-id"
	AnnIdempotencyKey  = "runmesh.io/idempotency-key"
	ManagedByValue     = "runmesh"
	ComponentTaskValue = "task"

	// containerName is fixed, because the log request has to name a container
	// and guessing is how you get "a container name must be specified".
	containerName = "task"
)

// Environment the task container is handed. This IS the container tool
// contract, and it is passed as environment rather than argv so an image's own
// ENTRYPOINT stays in charge of how it runs.
const (
	EnvTool           = "RUNMESH_TOOL"
	EnvParams         = "RUNMESH_PARAMS"
	EnvDeps           = "RUNMESH_DEPS"
	EnvJobID          = "RUNMESH_JOB_ID"
	EnvStepID         = "RUNMESH_STEP_ID"
	EnvAttempt        = "RUNMESH_ATTEMPT"
	EnvAttemptID      = "RUNMESH_ATTEMPT_ID"
	EnvIdempotencyKey = "RUNMESH_IDEMPOTENCY_KEY"
	// EnvResultMarker hands the image the sentinel rather than making it
	// hard-code one, so the parser and the producer cannot drift apart across
	// versions.
	EnvResultMarker = "RUNMESH_RESULT_MARKER"
)

// BuildJob translates one attempt into a batch/v1 Job.
//
// It is a pure function of its inputs — no client, no clock, no IO — so every
// property that matters about the spec is a table test rather than something
// you find out from a cluster.
func BuildJob(cfg Config, in tools.Input, owner string) (*batchv1.Job, error) {
	image := in.Limits.Image
	if image == "" {
		image = cfg.Image
	}
	if image == "" {
		return nil, runmesh.Fatal(runmesh.CodeContractBroken,
			"tool %q runs in a container but names no image, and no default image is configured", in.Tool)
	}

	resources, err := buildResources(in.Limits)
	if err != nil {
		return nil, err
	}

	name := JobName(in.AttemptID)
	network := NetworkDeny
	if in.Limits.Network {
		// Set from the RESOLVED limits, which internal/policy produced. A tool
		// descriptor asking for the network is not enough: if the operator has
		// not switched network tools on, policy refused the step long before
		// this line, and Limits.Network is false.
		network = NetworkAllow
	}
	labels := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelComponent: ComponentTaskValue,
		LabelJobID:     labelValue(in.JobID),
		LabelStepID:    labelValue(in.StepID),
		LabelAttempt:   strconv.Itoa(in.Attempt),
		LabelTool:      labelValue(in.Tool),
		LabelOwner:     labelValue(owner),
		LabelNetwork:   network,
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				AnnAttemptID:      in.AttemptID,
				AnnIdempotencyKey: in.IdempotencyKey,
			},
		},
		Spec: batchv1.JobSpec{
			// The single most important line in this file. RunMesh owns
			// retries: the attempt counter, the backoff, the
			// retryable/terminal classification, and the record of every
			// attempt. If Kubernetes retried too there would be two budgets,
			// two sources of truth for "attempt", and an execution history the
			// dashboard could not reconstruct. See ADR 0003.
			BackoffLimit: ptrTo(int32(0)),
			Completions:  ptrTo(int32(1)),
			Parallelism:  ptrTo(int32(1)),

			// Cleanup that survives RunMesh dying. The executor deletes its own
			// Jobs on the paths it controls; this is what stops a crashed
			// process leaking workloads for ever, which is precisely when
			// nobody is around to notice.
			TTLSecondsAfterFinished: ptrTo(int32(cfg.TTLAfterFinished.Seconds())),

			// A BACKSTOP, not the timeout. RunMesh's own deadline fires first
			// and produces a classified TIMED_OUT with a proper timeline entry;
			// this only matters when RunMesh is not there to enforce anything,
			// so it is deliberately the more generous of the two. Making them
			// equal would race, and Kubernetes winning that race turns a clean
			// RunMesh timeout into an opaque DeadlineExceeded.
			ActiveDeadlineSeconds: ptrTo(int64(deadlineFor(in.Limits.Timeout, cfg.DeadlineGrace))),

			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// Never, because backoffLimit: 0 only bounds Job-level
					// retries. A restarting container would re-run the tool
					// inside one attempt, which is the same two-budget problem
					// one layer down.
					RestartPolicy: corev1.RestartPolicyNever,

					// A task pod has no business talking to the API server, and
					// the default is to mount a token that lets it try. This is
					// free to set and closes a real path: a compromised tool
					// with a projected token is a compromised tool with a
					// Kubernetes client.
					AutomountServiceAccountToken: ptrTo(false),
					ServiceAccountName:           cfg.TaskServiceAccount,

					// Kubernetes injects an environment variable for every
					// Service in the namespace unless you say not to. For a
					// task pod that is a free map of the namespace handed to
					// code chosen by a language model, and it is also how a
					// tool's own configuration gets shadowed by a Service
					// somebody named badly.
					EnableServiceLinks: ptrTo(false),

					// Deleted the moment RunMesh decides the step is over, so
					// a task does not get half a minute of a node it has been
					// told to stop using.
					TerminationGracePeriodSeconds: ptrTo(int64(terminationGrace(cfg.TerminationGrace))),

					// The pod-level half of the hardening. Container-level
					// settings below repeat runAsNonRoot and runAsUser on
					// purpose: a container security context OVERRIDES the pod's
					// rather than merging with it, so relying on inheritance is
					// exactly how one added sidecar silently runs as root.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptrTo(true),
						RunAsUser:    ptrTo(runAsUser(cfg.RunAsUser)),
						RunAsGroup:   ptrTo(runAsGroup(cfg.RunAsGroup)),
						// The primary group of every process, and the group
						// that owns the writable emptyDir below. Without it a
						// non-root uid can find /tmp mounted and not writable.
						FSGroup: ptrTo(runAsGroup(cfg.RunAsGroup)),
						SeccompProfile: &corev1.SeccompProfile{
							// RuntimeDefault is the container runtime's own
							// syscall filter — roughly 60 syscalls blocked
							// including the ones behind most kernel escapes.
							// Unconfined is the DEFAULT if this is omitted,
							// which is the sort of default worth writing a
							// line to opt out of.
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},

					// Writable scratch, and the only writable path in the pod.
					// It exists because readOnlyRootFilesystem is on and a
					// Python interpreter that cannot write anywhere cannot even
					// import a module that touches a temporary file. SizeLimit
					// is what stops a task filling the node's disk through the
					// one door it has: an unbounded emptyDir is memory- or
					// disk-backed storage with no quota at all.
					Volumes: []corev1.Volume{{
						Name: scratchVolume,
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{
								SizeLimit: scratchSize(in.Limits.EphemeralStorage),
							},
						},
					}},

					Containers: []corev1.Container{{
						Name:      containerName,
						Image:     image,
						Env:       buildEnv(in),
						Resources: resources,

						// IfNotPresent, not the Always that a mutable tag like
						// :dev implies. `kind load docker-image` puts the image
						// on the node with no registry behind it, so Always
						// fails with ErrImagePull on a cluster where the image
						// is demonstrably present. A digest-pinned deployment
						// does not care either way.
						ImagePullPolicy: corev1.PullIfNotPresent,

						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot: ptrTo(true),
							RunAsUser:    ptrTo(runAsUser(cfg.RunAsUser)),
							RunAsGroup:   ptrTo(runAsGroup(cfg.RunAsGroup)),

							// setuid binaries and file capabilities cannot
							// raise this process's privileges. This is the one
							// flag that most directly answers "the tool found a
							// local root exploit in the image".
							AllowPrivilegeEscalation: ptrTo(false),

							// Nothing in the image may be modified, so a tool
							// cannot overwrite its own interpreter, drop a
							// binary somewhere on PATH, or persist anything at
							// all between attempts. Everything writable is the
							// emptyDir, which dies with the pod.
							ReadOnlyRootFilesystem: ptrTo(true),

							Privileged: ptrTo(false),

							// ALL, not a list. Enumerating the dangerous
							// capabilities is a list that ages; dropping
							// everything and adding back what a tool proves it
							// needs is a list that does not.
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
							SeccompProfile: &corev1.SeccompProfile{
								Type: corev1.SeccompProfileTypeRuntimeDefault,
							},
						},

						VolumeMounts: []corev1.VolumeMount{{
							Name:      scratchVolume,
							MountPath: scratchPath,
						}},
					}},
				},
			},
		},
	}, nil
}

// buildEnv passes the attempt to the container.
//
// Environment rather than argv or a ConfigMap: argv would put step parameters
// in every `ps` on the node, and a ConfigMap is a second object to create,
// name, and garbage-collect on exactly the paths where cleanup is already
// hardest. The ceiling is the kernel's environment limit, and params are
// already capped far below it by RUNMESH_MAX_PARAMS_BYTES.
func buildEnv(in tools.Input) []corev1.EnvVar {
	env := []corev1.EnvVar{
		// The root filesystem is read-only and /tmp is the only writable
		// mount, so anything that defaults to writing under $HOME or the
		// system temporary directory has to be pointed at it. Without these
		// two, a Python that merely imports a module which touches a cache
		// fails with a permission error that reads like a bug in the tool.
		{Name: "TMPDIR", Value: scratchPath},
		{Name: "HOME", Value: scratchPath},
		{Name: EnvTool, Value: in.Tool},
		{Name: EnvJobID, Value: in.JobID},
		{Name: EnvStepID, Value: in.StepID},
		{Name: EnvAttempt, Value: strconv.Itoa(in.Attempt)},
		{Name: EnvAttemptID, Value: in.AttemptID},
		{Name: EnvResultMarker, Value: ResultMarker},
	}
	// The value a side-effecting tool uses so that a retry does not duplicate
	// the effect. Stable across every attempt of this step, unlike AttemptID.
	if in.IdempotencyKey != "" {
		env = append(env, corev1.EnvVar{Name: EnvIdempotencyKey, Value: in.IdempotencyKey})
	}
	if len(in.Params) > 0 {
		env = append(env, corev1.EnvVar{Name: EnvParams, Value: string(in.Params)})
	}
	if len(in.Deps) > 0 {
		if deps, err := marshalDeps(in.Deps); err == nil {
			env = append(env, corev1.EnvVar{Name: EnvDeps, Value: deps})
		}
	}
	return env
}

// buildResources translates the tool's declared limits.
//
// Requests are set to the limits rather than lower. Bursting is a good default
// for a service and a bad one for a batch task whose duration is being
// measured and shown on a waterfall: a step that runs in 200 ms under no load
// and 3 s under load produces a timeline nobody can interpret, and a retry
// budget that depends on what else the cluster is doing.
func buildResources(l tools.Limits) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}
	if l.CPU == "" && l.Memory == "" && l.EphemeralStorage == "" {
		return out, nil
	}
	out.Limits = corev1.ResourceList{}
	out.Requests = corev1.ResourceList{}

	for name, raw := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    l.CPU,
		corev1.ResourceMemory: l.Memory,
		// The dimension people forget, and the one that takes a NODE down
		// rather than a pod: container logs and writable layers count towards
		// it, so a task looping on print() fills the node's disk and evicts
		// everything scheduled there. A pod over its ephemeral-storage limit is
		// evicted on its own.
		corev1.ResourceEphemeralStorage: l.EphemeralStorage,
	} {
		if raw == "" {
			continue
		}
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			return corev1.ResourceRequirements{}, runmesh.Fatal(runmesh.CodeContractBroken,
				"tool declares %s=%q, which is not a Kubernetes quantity: %v", name, raw, err)
		}
		out.Limits[name] = q
		out.Requests[name] = q
	}
	return out, nil
}

// scratchVolume and scratchPath name the one writable path a task gets. /tmp
// rather than something bespoke because every runtime already writes there:
// Python's tempfile, its bytecode cache when it is not disabled, and anything
// that calls mkstemp. A tool that needs a different path is a tool that needs a
// review, not another mount.
const (
	scratchVolume = "scratch"
	scratchPath   = "/tmp"
)

// scratchSize converts the resolved ephemeral-storage limit into the emptyDir's
// own quota. They are deliberately the same number: ephemeral-storage bounds
// what the kubelet will let the pod consume overall, SizeLimit bounds this
// volume, and setting only the first leaves the medium unbounded on some
// runtimes. A tool with no declared storage gets an unlimited emptyDir, which
// is why internal/policy always supplies a default.
func scratchSize(quantity string) *resource.Quantity {
	if quantity == "" {
		return nil
	}
	q, err := resource.ParseQuantity(quantity)
	if err != nil {
		// buildResources has already rejected an unparseable quantity with a
		// classified error; reaching here means the value was empty-ish. Fail
		// open on the SIZE only, never on the limit itself.
		return nil
	}
	return &q
}

func runAsUser(v int64) int64 {
	if v < 1 {
		return defaultNonRootUID
	}
	return v
}

func runAsGroup(v int64) int64 {
	if v < 1 {
		return defaultNonRootGID
	}
	return v
}

func terminationGrace(d time.Duration) int64 {
	if d <= 0 {
		return int64(defaultTerminationGrace / time.Second)
	}
	return int64((d + time.Second - 1) / time.Second)
}

// The distroless "nonroot" uid and gid, which both shipped images already use.
// They are the fallback when a Config is built by hand — a test, or a caller
// that has not been updated — so that a zero value can never mean root.
const (
	defaultNonRootUID       = 65532
	defaultNonRootGID       = 65532
	defaultTerminationGrace = 5 * time.Second
)

// deadlineFor is the Kubernetes backstop deadline in seconds.
func deadlineFor(timeout, grace time.Duration) int64 {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	if grace <= 0 {
		grace = time.Minute
	}
	// Round up, and never below one second: activeDeadlineSeconds is an
	// integer, and a sub-second step must not become a zero-second deadline
	// that fails the Job before its container starts.
	secs := int64((timeout + grace + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

func ptrTo[T any](v T) *T { return &v }
