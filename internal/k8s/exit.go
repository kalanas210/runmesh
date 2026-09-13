package k8s

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// maxLogTailBytes bounds the log tail one failed attempt carries. The kubelet
// already keeps no more than 2 KiB of it (FallbackToLogsOnError), so this only
// matters if a runtime keeps more; it is stored with every failed attempt, and
// the store is not a log archive.
const maxLogTailBytes = 4 << 10

// containerExit turns the task container's terminated state into a classified
// failure, or returns nil when the pod recorded no failed exit for it.
//
// Both answers are terminal. An exit code is the task's own verdict on its
// input, and an OOM kill is the operator's memory grant being met: another
// attempt runs the same code, on the same input, under the same limit.
func containerExit(pod *corev1.Pod) *runmesh.ToolError {
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.State.Terminated
		if cs.Name != containerName || t == nil {
			continue
		}
		oom := t.Reason == "OOMKilled"
		if t.ExitCode == 0 && !oom {
			// A clean exit is not an explanation of a failed Job; let the Job's
			// own verdict stand.
			return nil
		}

		var failure *runmesh.ToolError
		if oom {
			failure = runmesh.Fatal(runmesh.CodeTaskOOMKilled,
				"the task was killed at its memory limit%s", memoryLimit(pod))
		} else {
			msg := fmt.Sprintf("the task exited with code %d", t.ExitCode)
			if t.Reason != "" && t.Reason != "Error" {
				// "Error" is the kubelet's reason for every non-zero exit, and
				// says nothing the exit code does not.
				msg += " (" + t.Reason + ")"
			}
			failure = runmesh.Fatal(runmesh.CodeTaskExited, "%s", msg)
		}
		failure.Exit = &runmesh.ExitInfo{
			Code:    t.ExitCode,
			Reason:  t.Reason,
			LogTail: logTail(t.Message, maxLogTailBytes),
		}
		return failure
	}
	return nil
}

// memoryLimit names the task container's memory limit for an OOM message, or
// returns "" when the pod spec does not carry one.
func memoryLimit(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			return " of " + q.String()
		}
	}
	return ""
}

// logTail returns at most max bytes from the end of s. When it has to cut, it
// starts on a line boundary if there is one, and never mid-character.
func logTail(s string, max int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	end := s[cut:]
	if s[cut-1] == '\n' {
		return end // the cut already falls on a line boundary
	}
	if i := strings.IndexByte(end, '\n'); i >= 0 && i < len(end)-1 {
		return end[i+1:]
	}
	return strings.ToValidUTF8(end, "")
}
