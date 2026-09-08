package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Kubernetes object names are not free-form, and RunMesh's identifiers do not
// satisfy them. An AttemptID looks like
//
//	job_06g71af4wx3vdak8enst952h34.fetch.1
//
// which carries an underscore (illegal in a DNS-1123 label), dots, and enough
// length to overflow the budget once Kubernetes appends its own suffixes.
//
// This file is the whole of the translation, and it is a translation rather
// than a rename: the RunMesh identity stays authoritative and lands in labels
// and annotations, where it can be queried and read back. The object name is a
// derived, lossy, collision-resistant handle.

const (
	// namePrefix keeps every object this package creates greppable in a
	// namespace that may hold other things.
	namePrefix = "rm-"

	// maxNameLen is 63 — the DNS-1123 LABEL limit — minus the six characters
	// Kubernetes appends when it names a Job's pod ("-xxxxx").
	//
	// The Job name itself could be a 253-character subdomain. The pod's cannot,
	// and a Job whose pods can never be named is a Job that never runs, with an
	// error message far from this file.
	maxNameLen = 63 - 6

	// hashLen is the suffix that survives truncation. Truncating a long
	// identifier makes two different attempts collide; appending a hash of the
	// UNtruncated value makes that vanishingly unlikely while keeping the
	// readable part readable.
	hashLen = 8
)

// JobName derives the Kubernetes Job name for one attempt.
//
// It is DETERMINISTIC, and that is load-bearing rather than tidy: creating a
// Job is not idempotent by itself, so if the API server accepts a create and
// the response is lost, the retry has to be able to recognise its own work.
// Same attempt, same name, AlreadyExists — which the executor adopts instead
// of starting a second workload. A random name would have started two.
//
// Across attempts the name necessarily differs, because AttemptID does: a
// retry is a NEW execution and gets its own workload. Safety across attempts
// is IdempotencyKey's job, not this one's.
func JobName(attemptID string) string {
	sum := sha256.Sum256([]byte(attemptID))
	suffix := hex.EncodeToString(sum[:])[:hashLen]

	body := sanitiseLabel(attemptID)
	budget := maxNameLen - len(namePrefix) - 1 - hashLen
	if len(body) > budget {
		body = body[:budget]
	}
	body = strings.Trim(body, "-")
	if body == "" {
		// Every character was illegal. The hash alone is still a valid,
		// unique name, and an unreadable name beats a rejected one.
		return namePrefix + suffix
	}
	return namePrefix + body + "-" + suffix
}

// sanitiseLabel maps arbitrary text onto the DNS-1123 label alphabet:
// lowercase alphanumerics and '-', starting and ending alphanumeric.
func sanitiseLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			// Collapse any run of illegal characters to one separator, so
			// "job_x..y" does not become "job-x--y".
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// labelValue makes a string safe to use as a label VALUE, which is a laxer
// alphabet than a name: alphanumerics plus '-', '_' and '.', up to 63
// characters, beginning and ending alphanumeric.
//
// Job ids pass through unchanged — an underscore is legal here even though it
// is not in a name — which is what keeps
// `kubectl get jobs -l runmesh.io/job-id=job_06g7...` a thing an operator can
// actually type.
func labelValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-_.")
	}
	return out
}
