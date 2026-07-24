// Command claw-controller is the kube-claw control plane: Kubernetes operator,
// secret authority, embedded Slack router, and workload API (DESIGN.md §4).
//
// Phase 0 wires the controller-runtime manager + scheme + health probes.
// Reconcilers, the store, the secret authority, and the API land in later phases.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/apihttp"
	"github.com/traego/kube-claw/internal/approvals"
	"github.com/traego/kube-claw/internal/artifacts"
	"github.com/traego/kube-claw/internal/controller"
	"github.com/traego/kube-claw/internal/identity"
	"github.com/traego/kube-claw/internal/models"
	"github.com/traego/kube-claw/internal/providersync"
	slackrouter "github.com/traego/kube-claw/internal/router/slack"
	"github.com/traego/kube-claw/internal/runengine"
	"github.com/traego/kube-claw/internal/scheduler"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
	"github.com/traego/kube-claw/internal/upgrade"
	"github.com/traego/kube-claw/internal/version"
)

var scheme = runtime.NewScheme()

// agentsNS is the namespace seeded agents live in (matches the run engine + router).
const agentsNS = "claw-agents"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clawv1alpha1.AddToScheme(scheme))
}

func main() {
	var dataDir, probeAddr, apiAddr, uiAddr, uiBaseURL, runnerImage, selfURL, anthropicSecret, defaultAgent, logFormat, cloudBaseImages string
	var enableRouter bool
	var artifactTTL, artifactMaxTTL time.Duration
	flag.StringVar(&dataDir, "data-dir", "/var/lib/claw", "directory for the SQLite store")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe bind address")
	flag.StringVar(&apiAddr, "api-bind-address", ":8443", "HTTP API bind address")
	flag.StringVar(&uiAddr, "ui-bind-address", ":8090", "secret-intake UI bind address (separate listener)")
	flag.StringVar(&uiBaseURL, "ui-base-url", "http://localhost:8090", "public base URL of the intake UI (for returned links)")
	flag.StringVar(&runnerImage, "runner-image", "kube-claw-runner:dev", "image used for agent run Jobs")
	flag.StringVar(&selfURL, "self-url", "http://claw-controller.claw-system.svc:8443", "in-cluster URL run pods use to reach the controller")
	flag.StringVar(&anthropicSecret, "anthropic-secret", "claw-anthropic-key", "K8s secret (key \"api-key\") injected into run pods for the agent loop")
	flag.StringVar(&defaultAgent, "default-agent", "general", "agent assigned when a Slack channel is onboarded")
	flag.StringVar(&cloudBaseImages, "cloud-base-images", "", "comma-separated name=image overrides for the seeded cloud base images (gcloud/aws/azure); empty = derive from --runner-image")
	flag.DurationVar(&artifactTTL, "artifact-ttl", 24*time.Hour, "default lifetime of published-document share links")
	flag.DurationVar(&artifactMaxTTL, "artifact-max-ttl", 7*24*time.Hour, "cap on per-publish share-link lifetime overrides")
	flag.BoolVar(&enableRouter, "enable-router", true, "run the embedded Slack router")
	flag.StringVar(&logFormat, "log-format", "console", "log output format: \"console\" (human-readable, for local dev) or \"json\" (structured, for cloud log backends)")
	flag.Parse()

	// json => structured logs that Cloud Logging / Azure Monitor / CloudWatch
	// parse into queryable fields; console => human-readable for `kubectl logs`.
	ctrl.SetLogger(zap.New(zap.UseDevMode(logFormat != "json")))
	log := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.AgentReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		RestrictAgentEgress: os.Getenv("CLAW_RESTRICT_AGENT_EGRESS") == "true",
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up AgentReconciler")
		os.Exit(1)
	}

	// Pre-migration snapshot (DESIGN.md §24.5): when this boot's version differs
	// from the one that last migrated, copy the DB first — the PVC-local restore
	// point for a failed migration release. Taken before Open (quiesced file).
	if snap, err := sqlite.SnapshotBeforeMigrate(dataDir, "claw.db", version.Get()); err != nil {
		log.Error(err, "unable to snapshot store before migration")
		os.Exit(1)
	} else if snap != "" {
		log.Info("snapshotted store before migration", "snapshot", snap)
	}

	// Open the SQLite store on the PVC and migrate.
	st, err := sqlite.Open(context.Background(), filepath.Join(dataDir, "claw.db"))
	if err != nil {
		log.Error(err, "unable to open store", "dataDir", dataDir)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		log.Error(err, "unable to migrate store")
		os.Exit(1)
	}
	if err := sqlite.WriteVersionMarker(dataDir, version.Get()); err != nil {
		log.Error(err, "unable to write version marker")
		os.Exit(1)
	}

	// Secret authority: local dev master key on the PVC (prod: KMS-wrapped).
	cipher, err := secrets.NewLocalCipher(filepath.Join(dataDir, "master.keyset"))
	if err != nil {
		log.Error(err, "unable to init cipher")
		os.Exit(1)
	}
	secSvc := &secrets.Service{Store: st, Cipher: cipher}
	modelSvc := &models.Service{Store: st, Cipher: cipher}

	// Agent identity: K8s SA TokenReview verifier + claw session-token signer.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Error(err, "unable to create clientset")
		os.Exit(1)
	}
	signer, err := identity.NewSigner()
	if err != nil {
		log.Error(err, "unable to create token signer")
		os.Exit(1)
	}
	idProvider := &identity.KubernetesSAProvider{Client: clientset, Audience: "claw-controller"}
	approvalSvc := &approvals.Service{Store: st, Secrets: secSvc, Reader: mgr.GetAPIReader()}
	artifactSvc := &artifacts.Service{Store: st, TTL: artifactTTL, MaxTTL: artifactMaxTTL}

	// Slack connector router (channel→agent routing + run creation). Built when
	// routes are configured; usable via the fake event endpoint regardless of
	// tokens. The Socket Mode transport is added separately when tokens exist.
	// Notifier posts replies + approval buttons back to Slack (needs the bot token).
	var slackNotifier *slackrouter.Notifier
	if bot := os.Getenv("CLAW_SLACK_BOT_TOKEN"); bot != "" {
		slackNotifier = slackrouter.NewNotifier(bot)
	}
	// Router handles channel routing, DM secret registration, and approvals. Built
	// when routes are configured OR a bot token is present (DMs work without routes).
	var slackRt *slackrouter.Router
	if routes := parseSlackRoutes(os.Getenv("CLAW_SLACK_ROUTES")); len(routes) > 0 || slackNotifier != nil {
		slackRt = &slackrouter.Router{
			Config: slackrouter.Config{Routes: routes}, Store: st, Approvals: approvalSvc,
			Secrets: secSvc, Notifier: slackNotifier, UIBase: uiBaseURL,
			DefaultAgent: defaultAgent, AgentsNS: "claw-agents",
		}
		// The router lists agent CRDs (each carries its image + prompt) so it can
		// route a request to the best-fit agent.
		reader := mgr.GetAPIReader()
		slackRt.AgentLister = func(ctx context.Context) []slackrouter.AgentChoice {
			var list clawv1alpha1.AgentList
			if err := reader.List(ctx, &list, client.InNamespace("claw-agents")); err != nil {
				return nil
			}
			out := make([]slackrouter.AgentChoice, 0, len(list.Items))
			for i := range list.Items {
				a := &list.Items[i]
				desc := a.Name
				if a.Spec.Model != nil && a.Spec.Model.SystemPrompt != "" {
					desc = a.Spec.Model.SystemPrompt
					if len(desc) > 300 {
						desc = desc[:300]
					}
				}
				out = append(out, slackrouter.AgentChoice{Namespace: a.Namespace, Name: a.Name, Description: desc})
			}
			return out
		}
		// LLM agent router: with an Anthropic key, classify each new request and
		// pick the best-fit agent (which carries its own image + prompt).
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			slackRt.Classifier = slackrouter.NewClassifier(key)
			log.Info("slack agent router enabled (llm classification)")
		}
		log.Info("slack router configured", "routes", len(routes), "defaultAgent", defaultAgent)
	}

	// Self-update coordinator (DESIGN.md §24): runs when the supervisor deployed
	// us with a ControlPlane reference. Writes the startup-confirmed signal,
	// conducts the Slack upgrade conversation, applies approvals.
	var upgradeCoord *upgrade.Coordinator
	if cpName := os.Getenv("CLAW_CONTROLPLANE_NAME"); cpName != "" {
		upgradeCoord = &upgrade.Coordinator{
			Store:     st,
			Reader:    mgr.GetAPIReader(),
			Writer:    mgr.GetClient(),
			Name:      cpName,
			Namespace: envOr("CLAW_CONTROLPLANE_NAMESPACE", "claw-system"),
		}
		if slackNotifier != nil {
			upgradeCoord.Notifier = slackNotifier
		}
		if err := mgr.Add(upgradeCoord); err != nil {
			log.Error(err, "unable to add upgrade coordinator")
			os.Exit(1)
		}
		if slackRt != nil {
			slackRt.Upgrades = upgradeCoord
		}
		log.Info("self-update coordinator enabled", "controlplane", cpName)
	}

	// HTTP API (uncached reader so /v1/agents works without waiting on caches).
	if err := mgr.Add(&apihttp.Server{
		Addr:                  apiAddr,
		Store:                 st,
		Reader:                mgr.GetAPIReader(),
		K8s:                   mgr.GetClient(),
		Secrets:               secSvc,
		UIBase:                uiBaseURL,
		Identity:              idProvider,
		Signer:                signer,
		Approvals:             approvalSvc,
		Artifacts:             artifactSvc,
		Models:                modelSvc,
		Router:                slackRt,
		Notifier:              slackNotifier,
		AdminPassword:         os.Getenv("CLAW_ADMIN_PASSWORD"),
		EnableFakeSlackEvents: os.Getenv("CLAW_ENABLE_FAKE_SLACK") == "true",
		Upgrades:              upgradeOrNil(upgradeCoord),
	}); err != nil {
		log.Error(err, "unable to add HTTP API server")
		os.Exit(1)
	}

	// Public token-gated pages (secret intake + artifact share links) on a
	// SEPARATE listener.
	if err := mgr.Add(&apihttp.UIServer{Addr: uiAddr, Secrets: secSvc, Artifacts: artifactSvc}); err != nil {
		log.Error(err, "unable to add UI server")
		os.Exit(1)
	}

	// Slack Socket Mode transport: only when routes + tokens are present. The
	// fake event endpoint works without tokens for local testing.
	if enableRouter && slackRt != nil {
		appTok, botTok := os.Getenv("CLAW_SLACK_APP_TOKEN"), os.Getenv("CLAW_SLACK_BOT_TOKEN")
		if appTok != "" && botTok != "" {
			if err := mgr.Add(&slackrouter.Runnable{Router: slackRt, AppToken: appTok, BotToken: botTok}); err != nil {
				log.Error(err, "unable to add slack socket mode")
				os.Exit(1)
			}
			log.Info("slack socket mode enabled")
		} else {
			log.Info("slack socket mode disabled (no tokens); fake event endpoint available")
		}
	}

	// Run engine: launches a Job per Pending run (Phase 5 demo slice).
	if err := mgr.Add(&runengine.Engine{
		Store:           st,
		K8s:             mgr.GetClient(),
		RunnerImage:     runnerImage,
		ControllerURL:   selfURL,
		Interval:        2 * time.Second,
		Notifier:        slackNotifier,
		AnthropicSecret: anthropicSecret,
	}); err != nil {
		log.Error(err, "unable to add run engine")
		os.Exit(1)
	}

	if err := mgr.Add(&scheduler.Scheduler{Store: st, Interval: 30 * time.Second}); err != nil {
		log.Error(err, "unable to add scheduler")
		os.Exit(1)
	}
	if err := mgr.Add(&providersync.Syncer{Models: modelSvc, Interval: 6 * time.Hour}); err != nil {
		log.Error(err, "unable to add provider catalog sync")
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

	// Seed the default agent so the control plane works out of the box: Slack
	// channel onboarding assigns DefaultAgent, and the run engine blocks forever
	// if that Agent CR does not exist. Idempotent — created once, never clobbers
	// an operator-edited agent. Runs via a direct client so it doesn't depend on
	// the manager cache being started.
	if err := seedDefaultAgent(context.Background(), mgr.GetConfig(), defaultAgent); err != nil {
		log.Error(err, "unable to seed default agent", "agent", defaultAgent)
		os.Exit(1)
	}

	// Pre-register the cloud base images (gcloud/aws/azure) so an agent can be
	// pointed at one out of the box (e.g. a cloud-ops agent needs the gcloud CLI;
	// the default runner image deliberately has no cloud tooling). Idempotent
	// upsert — refreshes the image ref to the deployed tag on every boot.
	if err := seedCloudBaseImages(context.Background(), st, runnerImage, cloudBaseImages); err != nil {
		log.Error(err, "unable to seed cloud base images")
		os.Exit(1)
	}

	// Seed one agent per cloud provider, each pinned to that provider's base image.
	// With the base images registered above, the LLM router can route a GCP/AWS/
	// Azure request to the matching agent, whose baseImageRef resolves the right CLI.
	if err := seedCloudAgents(context.Background(), mgr.GetConfig()); err != nil {
		log.Error(err, "unable to seed cloud agents")
		os.Exit(1)
	}

	log.Info("starting claw-controller", "version", version.Get())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// parseSlackRoutes parses CLAW_SLACK_ROUTES (a JSON array of routes) into the
// router config. Invalid JSON logs and yields no routes.
func parseSlackRoutes(s string) []slackrouter.Route {
	if s == "" {
		return nil
	}
	var routes []slackrouter.Route
	if err := json.Unmarshal([]byte(s), &routes); err != nil {
		ctrl.Log.WithName("setup").Error(err, "invalid CLAW_SLACK_ROUTES json")
		return nil
	}
	return routes
}

// seedDefaultAgent ensures a usable default Agent exists in claw-agents so the
// control plane works on a fresh install. Without it, the first onboarded Slack
// channel routes to DefaultAgent, the run engine can't load that (nonexistent)
// Agent, and every run for it spins forever on "Agent.claw.run not found".
//
// It's idempotent and create-only: if the agent already exists (operator-edited
// or seeded on a prior boot) it's left untouched. The seed carries no
// baseImageRef/image, so the run engine falls back to the global runner image,
// and no secrets, so it launches without an approval gate.
func seedDefaultAgent(ctx context.Context, cfg *rest.Config, name string) error {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	return seedAgent(ctx, c, &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS},
		Spec: clawv1alpha1.AgentSpec{
			DisplayName: "General Assistant",
			Runtime:     clawv1alpha1.RuntimeSpec{Mode: "scaleToZeroSession", IdleTimeout: "15m"},
			Model: &clawv1alpha1.ModelSpec{
				SystemPrompt: "You are a helpful, general-purpose assistant running in a sandboxed Linux container with a bash tool. Answer questions clearly, do the work when you can, and ask for clarification when a request is ambiguous. For cloud-provider-specific tasks (GCP, AWS, Azure), a more specialized agent with that provider's CLI pre-installed will usually handle the request instead.",
			},
		},
	})
}

// cloudAgentDef is a seeded cloud-ops agent: a name, its base image, and a prompt
// the LLM router matches requests against (so a GCP question routes to the gcloud
// agent, etc.). The baseImageRef must name a registered base image (seeded by
// seedCloudBaseImages) — that's the link that gets the right CLI into the run.
type cloudAgentDef struct{ name, display, baseImageRef, prompt string }

var cloudAgentDefs = []cloudAgentDef{
	{
		name: "gcp-ops", display: "GCP Operations", baseImageRef: "gcloud",
		prompt: "You are a Google Cloud (GCP) operations assistant. You run in a sandboxed Linux container with the gcloud and bq CLIs pre-installed. Use them to investigate and answer questions about GCP: billing and cost analysis, Cloud Logging volume and spend, IAM, GKE, BigQuery, and general resource inspection. Request credentials via the secret tool when you need them. Prefer concrete, numbers-backed answers from live queries over generic advice.",
	},
	{
		name: "aws-ops", display: "AWS Operations", baseImageRef: "aws",
		prompt: "You are an Amazon Web Services (AWS) operations assistant. You run in a sandboxed Linux container with the AWS CLI v2 pre-installed. Use it to investigate and answer questions about AWS: Cost Explorer / billing analysis, CloudWatch, IAM, EC2/EKS, S3, and general resource inspection. Request credentials via the secret tool when you need them. Prefer concrete, numbers-backed answers from live queries over generic advice.",
	},
	{
		name: "azure-ops", display: "Azure Operations", baseImageRef: "azure",
		prompt: "You are a Microsoft Azure operations assistant. You run in a sandboxed Linux container with the az CLI pre-installed. Use it to investigate and answer questions about Azure: cost management / billing analysis, Monitor, RBAC, AKS, storage, and general resource inspection. Request credentials via the secret tool when you need them. Prefer concrete, numbers-backed answers from live queries over generic advice.",
	},
}

// seedCloudAgents pre-registers one agent per cloud provider, each pinned to that
// provider's base image. Combined with seedCloudBaseImages, this gives the LLM
// router three capability-distinct agents to route to: a GCP/AWS/Azure request is
// matched (by these prompts) to the matching agent, whose baseImageRef then
// resolves the right CLI image for the run. Idempotent + create-only.
func seedCloudAgents(ctx context.Context, cfg *rest.Config) error {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	for _, def := range cloudAgentDefs {
		if err := seedAgent(ctx, c, &clawv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: def.name, Namespace: agentsNS},
			Spec: clawv1alpha1.AgentSpec{
				DisplayName:  def.display,
				BaseImageRef: def.baseImageRef,
				Runtime:      clawv1alpha1.RuntimeSpec{Mode: "scaleToZeroSession", IdleTimeout: "15m"},
				Model:        &clawv1alpha1.ModelSpec{SystemPrompt: def.prompt},
			},
		}); err != nil {
			return fmt.Errorf("seed agent %q: %w", def.name, err)
		}
	}
	return nil
}

// seedAgent creates an Agent if it doesn't already exist. Create-only: an existing
// agent (operator-edited or seeded on a prior boot) is left untouched, so edits
// survive restarts. Safe under racing replicas (AlreadyExists is not an error).
func seedAgent(ctx context.Context, c client.Client, agent *clawv1alpha1.Agent) error {
	var existing clawv1alpha1.Agent
	err := c.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.Name}, &existing)
	if err == nil {
		return nil // already present — don't clobber operator edits
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if err := c.Create(ctx, agent); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	ctrl.Log.WithName("setup").Info("seeded agent", "namespace", agent.Namespace, "agent", agent.Name)
	return nil
}

// cloudBaseImage names the seeded cloud base images and a short description each.
type cloudBaseImage struct{ name, desc string }

var cloudBaseImageDefs = []cloudBaseImage{
	{"gcloud", "Google Cloud SDK (gcloud, bq) — GCP cost/billing/logging queries"},
	{"aws", "AWS CLI v2 (sts, ce) — AWS cost/usage queries"},
	{"azure", "Azure CLI (az) — Azure cost/usage queries"},
}

// seedCloudBaseImages registers the gcloud/aws/azure base images so agents can
// reference them without an operator running `claw baseimage create` by hand.
//
// Each image ref is derived from runnerImage by swapping the "kube-claw-runner"
// component for "kube-claw-<name>" (the naming scripts/build-push-gke.sh uses),
// so they share the deployed registry + tag. The override flag (name=image,...)
// wins per-name for anyone whose images don't follow that convention. Registration
// is an idempotent upsert, so a redeploy refreshes the ref to the current tag.
func seedCloudBaseImages(ctx context.Context, st store.Store, runnerImage, override string) error {
	overrides := parseImageOverrides(override)
	lg := ctrl.Log.WithName("setup")
	for _, def := range cloudBaseImageDefs {
		img := overrides[def.name]
		if img == "" {
			img = deriveCloudImage(runnerImage, def.name)
		}
		if img == "" {
			lg.Info("skipping cloud base image (could not derive ref)", "name", def.name, "runnerImage", runnerImage)
			continue
		}
		if err := st.Tx(ctx, func(tx store.Tx) error {
			return tx.CreateBaseImage(store.BaseImage{Name: def.name, Image: img, Description: def.desc})
		}); err != nil {
			return fmt.Errorf("register base image %q: %w", def.name, err)
		}
		lg.Info("registered cloud base image", "name", def.name, "image", img)
	}
	return nil
}

// deriveCloudImage turns a runner image ref into the sibling cloud image ref by
// replacing the "kube-claw-runner" name component, preserving registry and tag.
// Returns "" if the ref doesn't contain "kube-claw-runner".
func deriveCloudImage(runnerImage, cloud string) string {
	const marker = "kube-claw-runner"
	if !strings.Contains(runnerImage, marker) {
		return ""
	}
	return strings.Replace(runnerImage, marker, "kube-claw-"+cloud, 1)
}

// parseImageOverrides parses "gcloud=ref,aws=ref" into a name→image map.
func parseImageOverrides(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// envOr returns the env var's value, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// upgradeOrNil avoids handing the API server a typed-nil interface.
func upgradeOrNil(c *upgrade.Coordinator) apihttp.UpgradeAPI {
	if c == nil {
		return nil
	}
	return c
}
