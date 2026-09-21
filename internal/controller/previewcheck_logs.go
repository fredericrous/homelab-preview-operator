package controller

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodLogTailer reads check Job logs through a raw clientset.
//
// The controller-runtime client cannot read a subresource like pods/log at all,
// and this operator deliberately runs no Pod informer, so the tailer keeps its
// own uncached clientset. Everything it does is best-effort: a log tail is
// detail attached to a verdict already decided by the Job's exit code.
type PodLogTailer struct {
	clientset kubernetes.Interface
}

// NewPodLogTailer returns a tailer over a clientset.
func NewPodLogTailer(cs kubernetes.Interface) *PodLogTailer {
	return &PodLogTailer{clientset: cs}
}

// TailJobLogs returns at most maxBytes of the newest pod's log for a Job.
//
// A single-container pod is read as-is. A multi-container pod (the migration
// probe runs the app as a sidecar next to the curl poller) is read one
// container at a time — the API refuses an unqualified request there — and
// the tails are concatenated under `--- <container> ---` headers, app first,
// so the migration output lands above the probe's verdict.
func (t *PodLogTailer) TailJobLogs(ctx context.Context, namespace, jobName string, maxBytes int64) (string, error) {
	if t == nil || t.clientset == nil {
		return "", fmt.Errorf("no log tailer configured")
	}

	pods, err := t.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", fmt.Errorf("list pods for job %s/%s: %w", namespace, jobName, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods for job %s/%s", namespace, jobName)
	}

	// Newest first: a Job with backoffLimit 0 has one pod, but a pod that was
	// evicted and recreated would otherwise hand back the wrong one's logs.
	items := pods.Items
	sort.Slice(items, func(i, j int) bool {
		return items[j].CreationTimestamp.Before(&items[i].CreationTimestamp)
	})
	pod := &items[0]

	var containers []string
	for _, c := range pod.Spec.InitContainers {
		containers = append(containers, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		containers = append(containers, c.Name)
	}
	if len(containers) <= 1 {
		return t.readContainer(ctx, namespace, pod.Name, "", maxBytes)
	}

	per := maxBytes / int64(len(containers))
	if per < 512 {
		per = 512
	}
	var out strings.Builder
	var firstErr error
	for _, name := range containers {
		text, err := t.readContainer(ctx, namespace, pod.Name, name, per)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			text = "(no log: " + err.Error() + ")"
		}
		fmt.Fprintf(&out, "--- %s ---\n%s\n", name, strings.TrimRight(text, "\n"))
	}
	if out.Len() == 0 && firstErr != nil {
		return "", firstErr
	}
	return out.String(), nil
}

// readContainer streams the tail of one container's log (any container when
// name is empty, which only the API accepts for single-container pods).
func (t *PodLogTailer) readContainer(ctx context.Context, namespace, podName, container string, maxBytes int64) (string, error) {
	// TailLines bounds what the API server reads; LimitBytes bounds what crosses
	// the wire. Both are needed: LimitBytes alone truncates from the START of
	// the log, which on a chatty test image is the least useful part.
	tail := int64(50)
	limit := maxBytes + 512 // a little more than we keep, so the cut lands on a line boundary
	req := t.clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container:  container,
		TailLines:  &tail,
		LimitBytes: &limit,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs for %s/%s: %w", namespace, podName, err)
	}
	defer stream.Close() //nolint:errcheck // read-only stream

	data, err := io.ReadAll(io.LimitReader(stream, limit))
	if err != nil {
		return "", fmt.Errorf("read logs for %s/%s: %w", namespace, podName, err)
	}
	return string(data), nil
}
