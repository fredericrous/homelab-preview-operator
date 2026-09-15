package controller

import (
	"context"
	"fmt"
	"io"
	"sort"

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

	// TailLines bounds what the API server reads; LimitBytes bounds what crosses
	// the wire. Both are needed: LimitBytes alone truncates from the START of
	// the log, which on a chatty test image is the least useful part.
	tail := int64(50)
	req := t.clientset.CoreV1().Pods(namespace).GetLogs(items[0].Name, &corev1.PodLogOptions{
		TailLines: &tail,
		LimitBytes: func() *int64 {
			// Ask for a little more than we keep, so TruncateLogTail can cut on
			// a line boundary instead of mid-word.
			n := maxBytes + 512
			return &n
		}(),
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs for %s/%s: %w", namespace, items[0].Name, err)
	}
	defer stream.Close() //nolint:errcheck // read-only stream

	data, err := io.ReadAll(io.LimitReader(stream, maxBytes+512))
	if err != nil {
		return "", fmt.Errorf("read logs for %s/%s: %w", namespace, items[0].Name, err)
	}
	return string(data), nil
}
