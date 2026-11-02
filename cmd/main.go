package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	homekitv1alpha1 "github.com/harrymitchinson/oikade/api/homekit/v1alpha1"
	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
	pluginsv1alpha1 "github.com/harrymitchinson/oikade/api/plugins/v1alpha1"
	zigbeev1alpha1 "github.com/harrymitchinson/oikade/api/zigbee/v1alpha1"
	homecontroller "github.com/harrymitchinson/oikade/internal/controller/home"
	homekitcontroller "github.com/harrymitchinson/oikade/internal/controller/homekit"
	mattercontroller "github.com/harrymitchinson/oikade/internal/controller/matter"
	pluginscontroller "github.com/harrymitchinson/oikade/internal/controller/plugins"
	zigbeecontroller "github.com/harrymitchinson/oikade/internal/controller/zigbee"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(homev1alpha1.AddToScheme(scheme))
	utilruntime.Must(homekitv1alpha1.AddToScheme(scheme))
	utilruntime.Must(zigbeev1alpha1.AddToScheme(scheme))
	utilruntime.Must(pluginsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(matterv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "runtime":
			os.Exit(runRuntime(os.Args[2:]))
		case "hap":
			os.Exit(runHAP(os.Args[2:]))
		case "matter":
			os.Exit(runMatter(os.Args[2:]))
		}
	}

	var mqttURL, zigbeeImage, hapImage string
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&mqttURL, "mqtt-url", "mqtt://oikade-mosquitto.oikade-system.svc:1883",
		"MQTT broker that Zigbee2MQTT instances connect to.")
	flag.StringVar(&zigbeeImage, "zigbee2mqtt-image", "koenkk/zigbee2mqtt:2.14.2",
		"Default Zigbee2MQTT image, overridable per instance.")
	flag.StringVar(&hapImage, "hap-image", "",
		"Image for HomeKit and Matter bridge pods. Defaults to the operator's own image (via $POD_NAME/$POD_NAMESPACE).")
	var logLevel slog.Level
	flag.TextVar(&logLevel, "log-level", slog.LevelInfo, "Minimum log level: debug, info, warn, or error.")
	showVersion := versionFlag(flag.CommandLine)
	flag.Parse()
	if *showVersion {
		printVersion(os.Stdout)
		return
	}

	configureLogging(logLevel)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c57a2a4a.oikade.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&homecontroller.RuleReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "home-rule")
		os.Exit(1)
	}
	if hapImage == "" {
		if hapImage, err = ownImage(context.Background(), mgr.GetAPIReader()); err != nil {
			setupLog.Error(err, "Failed to determine HAP image; set --hap-image")
			os.Exit(1)
		}
	}
	if err := (&homekitcontroller.BridgeReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Image:   hapImage,
		MQTTURL: mqttURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "homekit-bridge")
		os.Exit(1)
	}
	if err := (&zigbeecontroller.Zigbee2MQTTReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Image:   zigbeeImage,
		MQTTURL: mqttURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "zigbee-zigbee2mqtt")
		os.Exit(1)
	}
	if err := (&pluginscontroller.PluginReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		MQTTURL: mqttURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "plugins-plugin")
		os.Exit(1)
	}
	if err := (&homecontroller.DeviceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "home-device")
		os.Exit(1)
	}
	if err := (&mattercontroller.BridgeReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Image:   hapImage,
		MQTTURL: mqttURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "matter-bridge")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager", "version", version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// ownImage returns the image of the manager container in the operator's own pod.
func ownImage(ctx context.Context, c client.Reader) (string, error) {
	name, ns := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE")
	if name == "" || ns == "" {
		return "", errors.New("$POD_NAME and $POD_NAMESPACE are not set")
	}
	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
		return "", fmt.Errorf("reading own pod: %w", err)
	}
	for _, ctr := range pod.Spec.Containers {
		if ctr.Name == "manager" {
			return ctr.Image, nil
		}
	}
	return "", errors.New("no container named manager in own pod")
}
