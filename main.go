package main

import (
    "flag"
    "os"

    clientgoscheme "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/healthz"
    "sigs.k8s.io/controller-runtime/pkg/log/zap"

    v1alpha1 "github.com/myorg/nodepatcher/api/v1alpha1"
    "github.com/myorg/nodepatcher/controllers"
)

func main() {
    var metricsAddr string
    var probeAddr string
    var enableLeaderElection bool
    flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
    flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
    flag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election for controller manager.")
    opts := zap.Options{Development: true}
    opts.BindFlags(flag.CommandLine)
    flag.Parse()

    ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

    mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
        Scheme:                 mustScheme(),
        MetricsBindAddress:     metricsAddr,
        HealthProbeBindAddress: probeAddr,
        LeaderElection:         enableLeaderElection,
        LeaderElectionID:       "nodepatcher.myorg.io",
    })
    if err != nil {
        os.Exit(1)
    }

    lockNS := os.Getenv("LOCK_NAMESPACE")
    if lockNS == "" {
        lockNS = "nodepatcher-system"
    }

    pf := controllers.NewProviderFactory()

    if err := (&controllers.NodePatchRunReconciler{
        Client:          mgr.GetClient(),
        Scheme:          mgr.GetScheme(),
        ProviderFactory: pf,
        LockNamespace:   lockNS,
    }).SetupWithManager(mgr); err != nil {
        os.Exit(1)
    }

    if err := (&controllers.NodePatchTaskReconciler{
        Client:          mgr.GetClient(),
        Scheme:          mgr.GetScheme(),
        ProviderFactory: pf,
        LockNamespace:   lockNS,
    }).SetupWithManager(mgr, mgr.GetConfig()); err != nil {
        os.Exit(1)
    }

    if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
        os.Exit(1)
    }
    if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
        os.Exit(1)
    }

    if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
        os.Exit(1)
    }
}

func mustScheme() *runtime.Scheme {
    s := runtime.NewScheme()
    _ = clientgoscheme.AddToScheme(s)
    _ = v1alpha1.AddToScheme(s)
    return s
}
