// Command claw-supervisor is the self-update plane (DESIGN.md §24): a tiny,
// always-running reconciler that owns the controller StatefulSet from the
// ControlPlane CR, polls the release manifest, applies approved updates,
// health-watches rollouts, and rolls back failures. It deliberately carries
// none of the interesting machinery — its value is being too boring to break.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/supervisor"
	"github.com/traego/kube-claw/internal/version"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clawv1alpha1.AddToScheme(scheme))
}

func main() {
	var namespace, probeAddr, logFormat string
	var checkInterval time.Duration
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace the ControlPlane + controller live in")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe bind address")
	flag.StringVar(&logFormat, "log-format", "console", `log output format: "console" or "json"`)
	flag.DurationVar(&checkInterval, "default-check-interval", 6*time.Hour, "release-manifest poll interval when the CR doesn't set one")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(logFormat != "json")))
	log := ctrl.Log.WithName("setup")
	if namespace == "" {
		namespace = "claw-system"
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		// The supervisor's world is one namespace: the ControlPlane CR and the
		// controller StatefulSet. Scoping the cache keeps RBAC namespaced too.
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Failure notifications: one bare chat.postMessage with the bot token (no
	// socket mode, no Slack framework surface). Optional.
	var notify supervisor.Notifier
	if tok := os.Getenv("CLAW_SLACK_BOT_TOKEN"); tok != "" {
		notify = supervisor.NewSlackNotifier(tok)
		log.Info("slack failure notifications enabled")
	}

	if err := (&supervisor.Reconciler{Client: mgr.GetClient(), Notify: notify}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up ControlPlane reconciler")
		os.Exit(1)
	}
	// Manifest signing (T-9): configured PEM public keys (one or more — a
	// rotation ring) make signature verification mandatory (fail closed);
	// absent = unsigned mode.
	pubKeys, err := supervisor.ParseManifestPublicKeys(os.Getenv("CLAW_MANIFEST_PUBKEY"))
	if err != nil {
		log.Error(err, "invalid CLAW_MANIFEST_PUBKEY")
		os.Exit(1)
	}
	if len(pubKeys) > 0 {
		log.Info("manifest signature verification enabled", "keys", len(pubKeys))
	}
	if err := mgr.Add(&supervisor.Poller{
		Client:          mgr.GetClient(),
		Namespace:       namespace,
		DefaultInterval: checkInterval,
		PubKeys:         pubKeys,
	}); err != nil {
		log.Error(err, "unable to add release poller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to add healthz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to add readyz check")
		os.Exit(1)
	}

	log.Info("starting claw-supervisor", "version", version.Get(), "namespace", namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
