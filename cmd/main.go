package main

import (
	"flag"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
	"github.com/fredericrous/homelab-preview-operator/internal/controller"
	"github.com/fredericrous/homelab-preview-operator/internal/enrichment"
	"github.com/fredericrous/homelab-preview-operator/internal/githubapp"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kustomizev1.AddToScheme(scheme))
	utilruntime.Must(sourcev1.AddToScheme(scheme))
	utilruntime.Must(previewv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var previewDomain string
	var gitRepo string
	var gitProvider string
	var gitAPIBaseURL string
	var probeImage string
	var cveEnrichmentURL string
	var githubAppSecret string
	var githubAPIBaseURL string
	var checkRunRepoPrefix string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&previewDomain, "preview-domain", "daddyshome.fr", "The domain for preview URLs.")
	flag.StringVar(&gitRepo, "github-repo", "fredericrous/homelab", "The repository (owner/repo) for posting PR comments.")
	flag.StringVar(&gitProvider, "git-provider", controller.ProviderGitHub,
		"The forge API flavour to post PR comments with: github or gitea (Forgejo speaks the Gitea API).")
	flag.StringVar(&gitAPIBaseURL, "git-api-base-url", "",
		"The forge API base URL. Defaults to https://api.github.com for github, "+
			"and for gitea to the host of the Flux GitRepository the preview syncs from.")
	flag.StringVar(&probeImage, "probe-image", "curlimages/curl:8.11.1",
		"The image the PreviewCheck http probe Job runs. It only needs curl and a POSIX shell.")
	flag.StringVar(&cveEnrichmentURL, "cve-enrichment-url", "",
		"KEV/EPSS enrichment endpoint (cluster-vision POST /api/cve/enrichment). "+
			"Empty makes the PreviewCheck trivy check fail closed rather than read an unenriched scan as clean.")
	flag.StringVar(&githubAppSecret, "github-app-secret", "",
		"namespace/name of the Secret holding the GitHub App credentials (githubAppID, "+
			"githubAppInstallationID, githubAppPrivateKey) used to publish MigrationCheck verdicts as check runs. "+
			"Empty disables reporting. The Secret is chosen here, never by a CR.")
	flag.StringVar(&githubAPIBaseURL, "github-api-base-url", githubapp.DefaultBaseURL,
		"GitHub API base URL for check runs.")
	flag.StringVar(&checkRunRepoPrefix, "check-run-repo-prefix", "",
		"Only MigrationChecks whose spec.report.repo starts with this prefix (e.g. \"owner/\") may publish check runs. "+
			"Empty allows any repository the App is installed on.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "preview-operator.homelab.io",
		HealthProbeBindAddress: probeAddr,
		// Previously the flag was parsed and then ignored, so the metrics
		// endpoint always bound controller-runtime's default. Nothing scraped
		// it, so nothing noticed.
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
		Cache: cache.Options{
			// Strip managedFields from everything the cache holds. On a cluster
			// with hundreds of Flux-managed objects they are a large fraction of
			// the cached bytes and nothing here ever reads them.
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		Client: client.Options{
			Cache: &client.CacheOptions{
				// Structural enforcement of "no Job/Pod informers": the
				// PreviewCheck reconciler polls the handful of Jobs it creates
				// by RequeueAfter, and caching every Job and Pod in the cluster
				// to watch four of them would cost far more memory than the
				// polling costs API calls.
				DisableFor: []client.Object{&batchv1.Job{}, &corev1.Pod{}},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.KustomizationReconciler{
		Client:        mgr.GetClient(),
		Log:           ctrl.Log.WithName("controllers").WithName("Kustomization"),
		Scheme:        mgr.GetScheme(),
		PreviewDomain: previewDomain,
		GitRepo:       gitRepo,
		GitProvider:   gitProvider,
		GitAPIBaseURL: gitAPIBaseURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Kustomization")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to build a clientset for reading check job logs")
		os.Exit(1)
	}

	// A nil reporter is meaningful: MigrationCheck verdicts stay in status only.
	var reporter controller.CheckRunReporter
	if githubAppSecret != "" {
		ns, name, ok := strings.Cut(githubAppSecret, "/")
		if !ok || ns == "" || name == "" {
			setupLog.Error(nil, "--github-app-secret must be namespace/name", "value", githubAppSecret)
			os.Exit(1)
		}
		reporter = &controller.GitHubAppReporter{
			Reader:     mgr.GetClient(),
			SecretRef:  types.NamespacedName{Namespace: ns, Name: name},
			API:        githubapp.NewClient(githubAPIBaseURL, nil, nil),
			RepoPrefix: checkRunRepoPrefix,
		}
	} else {
		setupLog.Info("no --github-app-secret configured; MigrationCheck verdicts will not be reported as check runs")
	}

	if err = (&controller.MigrationCheckReconciler{
		Client:     mgr.GetClient(),
		Log:        ctrl.Log.WithName("controllers").WithName("MigrationCheck"),
		Scheme:     mgr.GetScheme(),
		Clock:      clock.RealClock{},
		Logs:       controller.NewPodLogTailer(clientset),
		ProbeImage: probeImage,
		Reporter:   reporter,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MigrationCheck")
		os.Exit(1)
	}

	// A nil enricher is meaningful, not an error: the trivy check fails closed
	// rather than treating an unenriched scan as clean.
	var enricher enrichment.CVEEnricher
	if c := enrichment.New(cveEnrichmentURL); c != nil {
		enricher = c
	} else {
		setupLog.Info("no --cve-enrichment-url configured; PreviewCheck trivy checks will be inconclusive")
	}

	if err = (&controller.PreviewCheckReconciler{
		Client:        mgr.GetClient(),
		Log:           ctrl.Log.WithName("controllers").WithName("PreviewCheck"),
		Logs:          controller.NewPodLogTailer(clientset),
		Clock:         clock.RealClock{},
		Recorder:      mgr.GetEventRecorderFor("previewcheck"),
		PreviewDomain: previewDomain,
		ProbeImage:    probeImage,
		Enricher:      enricher,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PreviewCheck")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
