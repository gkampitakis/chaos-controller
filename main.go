// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	chaosv1beta1 "github.com/DataDog/chaos-controller/api/v1beta1"
	"github.com/DataDog/chaos-controller/cloudservice"
	"github.com/DataDog/chaos-controller/config"
	"github.com/DataDog/chaos-controller/controllers"
	"github.com/DataDog/chaos-controller/ddmark"
	"github.com/DataDog/chaos-controller/eventbroadcaster"
	"github.com/DataDog/chaos-controller/log"
	"github.com/DataDog/chaos-controller/o11y/metrics"
	metricstypes "github.com/DataDog/chaos-controller/o11y/metrics/types"
	"github.com/DataDog/chaos-controller/o11y/profiler"
	profilertypes "github.com/DataDog/chaos-controller/o11y/profiler/types"
	"github.com/DataDog/chaos-controller/targetselector"
	"github.com/DataDog/chaos-controller/utils"
	"github.com/DataDog/chaos-controller/watchers"
	chaoswebhook "github.com/DataDog/chaos-controller/webhook"

	"k8s.io/apimachinery/pkg/runtime"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	// +kubebuilder:scaffold:imports
)

//go:generate mockery  --config .local.mockery.yaml
//go:generate mockery  --config .vendor.mockery.yaml

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// +kubebuilder:scaffold:scheme
	_ = clientgoscheme.AddToScheme(scheme)
	_ = chaosv1beta1.AddToScheme(scheme)
}

func main() {
	logger, err := log.NewZapLogger()
	if err != nil {
		setupLog.Error(err, "error creating controller logger")
		os.Exit(1)
	}

	// get controller node name
	controllerNodeName, exists := os.LookupEnv("CONTROLLER_NODE_NAME")
	if !exists {
		logger.Fatal("missing required CONTROLLER_NODE_NAME environment variable")
	}

	cfg, err := config.New(logger, os.Args[1:])
	if err != nil {
		logger.Fatalw("unable to create a valid configuration", "error", err)
	}

	broadcaster := eventbroadcaster.EventBroadcaster()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: cfg.Controller.MetricsBindAddr,
		LeaderElection:     cfg.Controller.LeaderElection,
		LeaderElectionID:   "75ec2fa4.datadoghq.com",
		EventBroadcaster:   broadcaster,
		Host:               cfg.Controller.Webhook.Host,
		Port:               cfg.Controller.Webhook.Port,
		CertDir:            cfg.Controller.Webhook.CertDir,
	})
	if err != nil {
		logger.Errorw("unable to start manager", "error", err)
		os.Exit(1)
	}

	// event notifiers
	err = eventbroadcaster.RegisterNotifierSinks(mgr, broadcaster, cfg.Controller.Notifiers, logger)
	if err != nil {
		logger.Errorw("error(s) while creating notifiers", "error", err)
	}

	// metrics sink
	ms, err := metrics.GetSink(logger, metricstypes.SinkDriver(cfg.Controller.MetricsSink), metricstypes.SinkAppController)
	if err != nil {
		logger.Errorw("error while creating metric sink, switching to noop", "error", err)

		ms, err = metrics.GetSink(logger, metricstypes.SinkDriverNoop, metricstypes.SinkAppController)

		if err != nil {
			logger.Fatalw("error creating noop metrics sink", "error", err)
		}
	}

	// handle metrics sink client close on exit
	defer func() {
		logger.Infow("closing metrics sink client before exiting", "sink", ms.GetSinkName())

		if err := ms.Close(); err != nil {
			logger.Errorw("error closing metrics sink client", "sink", ms.GetSinkName(), "error", err)
		}
	}()

	if ms.MetricRestart() != nil {
		logger.Errorw("error sending MetricRestart", "sink", ms.GetSinkName())
	}

	// profiler sink
	prfl, err := profiler.GetSink(logger, profilertypes.SinkDriver(cfg.Controller.ProfilerSink))
	if err != nil {
		logger.Errorw("error while creating profiler sink, switching to noop", "error", err)

		prfl, err = profiler.GetSink(logger, profilertypes.SinkDriverNoop)

		if err != nil {
			logger.Errorw("error while creating noop profiler sink", "error", err)
		}
	}
	// handle profiler sink close on exit
	defer prfl.Stop()

	// target selector
	targetSelector := targetselector.NewRunningTargetSelector(cfg.Controller.EnableSafeguards, controllerNodeName)

	var gcPtr *time.Duration
	if cfg.Controller.ExpiredDisruptionGCDelay >= 0 {
		gcPtr = &cfg.Controller.ExpiredDisruptionGCDelay
	}

	// initialize the cloud provider manager which will handle ip ranges files updates
	cloudProviderManager, err := cloudservice.New(logger, cfg.Controller.CloudProviders)
	if err != nil {
		handleFatalError(fmt.Errorf("error initializing CloudProviderManager: %s", err.Error()))
	}

	cloudProviderManager.StartPeriodicPull()

	// create reconciler
	r := &controllers.DisruptionReconciler{
		Client:                                mgr.GetClient(),
		BaseLog:                               logger,
		Scheme:                                mgr.GetScheme(),
		Recorder:                              mgr.GetEventRecorderFor(chaosv1beta1.SourceDisruptionComponent),
		MetricsSink:                           ms,
		TargetSelector:                        targetSelector,
		InjectorAnnotations:                   cfg.Injector.Annotations,
		InjectorLabels:                        cfg.Injector.Labels,
		InjectorServiceAccount:                cfg.Injector.ServiceAccount,
		InjectorImage:                         cfg.Injector.Image,
		ChaosNamespace:                        cfg.Injector.ChaosNamespace,
		InjectorDNSDisruptionDNSServer:        cfg.Injector.DNSDisruption.DNSServer,
		InjectorDNSDisruptionKubeDNS:          cfg.Injector.DNSDisruption.KubeDNS,
		InjectorNetworkDisruptionAllowedHosts: cfg.Injector.NetworkDisruption.AllowedHosts,
		ImagePullSecrets:                      cfg.Injector.ImagePullSecrets,
		ExpiredDisruptionGCDelay:              gcPtr,
		CacheContextStore:                     make(map[string]controllers.CtxTuple),
		Reader:                                mgr.GetAPIReader(),
		EnableObserver:                        cfg.Controller.EnableObserver,
		CloudServicesProvidersManager:         cloudProviderManager,
	}

	informerClient := kubernetes.NewForConfigOrDie(ctrl.GetConfigOrDie())
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(informerClient, time.Hour*24, kubeinformers.WithNamespace(cfg.Injector.ChaosNamespace))

	cont, err := r.SetupWithManager(mgr, kubeInformerFactory)
	if err != nil {
		logger.Errorw("unable to create controller", "controller", chaosv1beta1.DisruptionKind, "error", err)
		os.Exit(1) //nolint:gocritic
	}

	r.Controller = cont

	watcherFactory := watchers.NewWatcherFactory(logger, ms, r.Client, r.Recorder)
	r.DisruptionsWatchersManager = watchers.NewDisruptionsWatchersManager(cont, watcherFactory, r.Reader, logger)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				logger.Debugw("Initiate the removal of all expired watchers.")
				r.DisruptionsWatchersManager.RemoveAllExpiredWatchers()

			case <-ctx.Done():
				// Context canceled, terminate the goroutine
				return
			}
		}
	}()

	defer cancel()

	stopCh := make(chan struct{})
	kubeInformerFactory.Start(stopCh)

	go r.ReportMetrics()

	// register disruption validating webhook
	setupWebhookConfig := utils.SetupWebhookWithManagerConfig{
		Manager:                       mgr,
		Logger:                        logger,
		MetricsSink:                   ms,
		Recorder:                      r.Recorder,
		NamespaceThresholdFlag:        cfg.Controller.SafeMode.NamespaceThreshold,
		ClusterThresholdFlag:          cfg.Controller.SafeMode.ClusterThreshold,
		EnableSafemodeFlag:            cfg.Controller.SafeMode.Enable,
		DeleteOnlyFlag:                cfg.Controller.DeleteOnly,
		HandlerEnabledFlag:            cfg.Handler.Enabled,
		DefaultDurationFlag:           cfg.Controller.DefaultDuration,
		ChaosNamespace:                cfg.Injector.ChaosNamespace,
		CloudServicesProvidersManager: cloudProviderManager,
		Environment:                   cfg.Controller.SafeMode.Environment,
	}
	if err = (&chaosv1beta1.Disruption{}).SetupWebhookWithManager(setupWebhookConfig); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", chaosv1beta1.DisruptionKind)
		os.Exit(1) //nolint:gocritic
	}

	if cfg.Handler.Enabled {
		// register chaos handler init container mutating webhook
		mgr.GetWebhookServer().Register("/mutate-v1-pod-chaos-handler-init-container", &webhook.Admission{
			Handler: &chaoswebhook.ChaosHandlerMutator{
				Client:  mgr.GetClient(),
				Log:     logger,
				Image:   cfg.Handler.Image,
				Timeout: cfg.Handler.Timeout,
			},
		})
	}

	if cfg.Controller.UserInfoHook {
		// register user info mutating webhook
		mgr.GetWebhookServer().Register("/mutate-chaos-datadoghq-com-v1beta1-disruption-user-info", &webhook.Admission{
			Handler: &chaoswebhook.UserInfoMutator{
				Client: mgr.GetClient(),
				Log:    logger,
			},
		})
	}

	// erase/close caches contexts
	defer func() {
		for _, contextTuple := range r.CacheContextStore {
			contextTuple.CancelFunc()
		}

		if err := ddmark.CleanupAllLibraries(); err != nil {
			logger.Error(err)
		}
	}()

	// +kubebuilder:scaffold:builder

	logger.Infow("starting chaos-controller")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		stopCh <- struct{}{} // stop the informer

		logger.Errorw("problem running manager", "error", err)
		os.Exit(1) //nolint:gocritic
	}
}

// handleFatalError logs the given error and exits if err is not nil
func handleFatalError(err error) {
	if err != nil {
		setupLog.Error(err, "fatal error occurred on setup")
		os.Exit(1)
	}
}
