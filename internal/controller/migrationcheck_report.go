package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
	"github.com/fredericrous/homelab-preview-operator/internal/githubapp"
)

// checkRunTextBytes caps the log tail sent as the check run's output.text.
// GitHub allows 64 KiB; status.message is already capped far below that.
const checkRunTextBytes = 8 << 10

// CheckRunReporter publishes a MigrationCheck's state to the forge.
//
// It is an interface so tests can record what would have been posted. A nil
// reporter on the reconciler disables reporting altogether.
type CheckRunReporter interface {
	// Report creates the check run when existingID is 0, otherwise updates it.
	// It returns the check run id to remember.
	Report(ctx context.Context, report previewv1.MigrationReport, existingID int64, run githubapp.CheckRun) (int64, error)
}

// GitHubAppReporter posts check runs as a GitHub App.
//
// The App credentials come from a Secret the OPERATOR names (--github-app-secret),
// read at call time so a rotated ExternalSecret is picked up without a restart.
// They are never selected by the CR: a MigrationCheck is created from a label
// on a public pull request, and its spec must not be able to point the operator
// at an arbitrary Secret. RepoPrefix bounds where a CR may post at all.
type GitHubAppReporter struct {
	Reader     client.Reader
	SecretRef  types.NamespacedName
	API        *githubapp.Client
	RepoPrefix string
}

// Report implements CheckRunReporter.
func (g *GitHubAppReporter) Report(ctx context.Context, report previewv1.MigrationReport, existingID int64, run githubapp.CheckRun) (int64, error) {
	if report.Provider != "" && report.Provider != "github" {
		return 0, fmt.Errorf("unsupported report provider %q", report.Provider)
	}
	if g.RepoPrefix != "" && !strings.HasPrefix(report.Repo, g.RepoPrefix) {
		return 0, fmt.Errorf("refusing to report on %q: outside the allowed prefix %q", report.Repo, g.RepoPrefix)
	}

	secret := &corev1.Secret{}
	if err := g.Reader.Get(ctx, g.SecretRef, secret); err != nil {
		return 0, fmt.Errorf("read github app secret %s: %w", g.SecretRef, err)
	}
	creds, err := githubapp.CredentialsFromSecretData(secret.Data)
	if err != nil {
		return 0, fmt.Errorf("github app secret %s: %w", g.SecretRef, err)
	}

	if existingID == 0 {
		return g.API.CreateCheckRun(ctx, creds, report.Repo, run)
	}
	if err := g.API.UpdateCheckRun(ctx, creds, report.Repo, existingID, run); err != nil {
		return existingID, err
	}
	return existingID, nil
}

// checkRunFor maps a MigrationCheck's state to the check run to publish.
//
// The phase decides status and conclusion; the reason and message become the
// title and summary; the log tail travels as fenced text. Expired maps to
// timed_out rather than failure so that an infrastructure miss (clone never
// settled, image never pulled) reads differently on the PR from a migration
// that actually broke.
func checkRunFor(mc *previewv1.MigrationCheck, now time.Time) githubapp.CheckRun {
	rep := mc.Spec.Report
	run := githubapp.CheckRun{HeadSHA: rep.Revision, Name: rep.Name}
	if run.Name == "" {
		run.Name = "migration-check"
	}
	if mc.Status.StartedAt != nil {
		t := mc.Status.StartedAt.Time
		run.StartedAt = &t
	}

	switch mc.Status.Phase {
	case previewv1.MigrationCheckPassed:
		run.Status, run.Conclusion = "completed", "success"
	case previewv1.MigrationCheckFailed:
		run.Status, run.Conclusion = "completed", "failure"
	case previewv1.MigrationCheckExpired:
		run.Status, run.Conclusion = "completed", "timed_out"
	default:
		run.Status = "in_progress"
	}
	if run.Status == "completed" {
		t := now
		if mc.Status.CompletedAt != nil {
			t = mc.Status.CompletedAt.Time
		}
		run.CompletedAt = &t
	}

	title := mc.Status.Reason
	if title == "" {
		title = string(mc.Status.Phase)
	}
	if title == "" {
		title = "Pending"
	}
	run.Title = title

	head, tail := splitDetail(mc.Status.Message)
	var summary strings.Builder
	summary.WriteString(head)
	if summary.Len() == 0 {
		summary.WriteString(string(mc.Status.Phase))
	}
	if mc.Spec.Probe != nil {
		fmt.Fprintf(&summary, "\n\nImage: `%s`", mc.Spec.Probe.Image)
	}
	if mc.Status.CloneNamespace != "" {
		fmt.Fprintf(&summary, "\nClone: `%s`", mc.Status.CloneNamespace)
	}
	run.Summary = summary.String()
	if tail != "" {
		if len(tail) > checkRunTextBytes {
			tail = tail[len(tail)-checkRunTextBytes:]
		}
		run.Text = "```\n" + tail + "\n```"
	}
	return run
}

// splitDetail separates a status message into its first line and the rest
// (the log tail, when one was attached with joinDetail).
func splitDetail(msg string) (head, tail string) {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		return msg[:i], strings.TrimSpace(msg[i+1:])
	}
	return msg, ""
}

// publishReport pushes the current phase to the forge if it has not been
// reported yet, then records the check run id and reported phase on the CR.
// Reporting never changes the verdict: an error is logged and left for the
// next reconcile to retry.
func (r *MigrationCheckReconciler) publishReport(ctx context.Context, mc *previewv1.MigrationCheck) error {
	if r.Reporter == nil || mc.Spec.Report == nil {
		return nil
	}
	if mc.Status.ReportedPhase == mc.Status.Phase {
		return nil
	}
	id, err := r.Reporter.Report(ctx, *mc.Spec.Report, mc.Status.CheckRunID, checkRunFor(mc, r.now()))
	if err != nil {
		return err
	}
	mc.Status.CheckRunID = id
	mc.Status.ReportedPhase = mc.Status.Phase
	return r.Status().Update(ctx, mc)
}

// reportCancelled tells the forge that a check run still in progress will
// never get a verdict (the CR is being deleted: PR closed, superseded by a new
// head, or unlabelled). Best effort; never blocks teardown.
func (r *MigrationCheckReconciler) reportCancelled(ctx context.Context, mc *previewv1.MigrationCheck) {
	if r.Reporter == nil || mc.Spec.Report == nil || mc.Status.CheckRunID == 0 || mc.Status.ReportedPhase.IsTerminal() {
		return
	}
	run := checkRunFor(mc, r.now())
	now := r.now()
	run.Status, run.Conclusion, run.CompletedAt = "completed", "cancelled", &now
	run.Title = "Cancelled"
	run.Summary = "MigrationCheck removed before a verdict (pull request closed, superseded, or unlabelled)."
	if _, err := r.Reporter.Report(ctx, *mc.Spec.Report, mc.Status.CheckRunID, run); err != nil {
		r.Log.V(1).Info("could not cancel check run", "migrationcheck", mc.Name, "error", err.Error())
	}
}
