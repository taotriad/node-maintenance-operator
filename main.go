/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/medik8s/common/pkg/lease"
	"go.uber.org/zap/zapcore"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	nodemaintenancev1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"
	"github.com/medik8s/node-maintenance-operator/controllers"
	"github.com/medik8s/node-maintenance-operator/pkg/utils"
	"github.com/medik8s/node-maintenance-operator/version"
	//+kubebuilder:scaffold:imports
)

const (
	WebhookCertDir  = "/apiserver.local.config/certificates"
	WebhookCertName = "apiserver.crt"
	WebhookKeyName  = "apiserver.key"
)

var (
	scheme   = k8sruntime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(nodemaintenancev1beta1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr, probeAddr string
		enableLeaderElection, enableHTTP2 bool
		webhookOpts          webhook.Options
	) 
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false, "If HTTP/2 should be enabled for the metrics and webhook servers.")

	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.RFC3339NanoTimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	printVersion()

	configureWebhookOpts(&webhookOpts, enableHTTP2)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		WebhookServer:          webhook.NewServer(webhookOpts),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "135b1886.medik8s.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}


	cl := mgr.GetClient()
	leaseManagerInitializer := &leaseManagerInitializer{cl: cl}
	if err := mgr.Add(leaseManagerInitializer); err != nil {
		setupLog.Error(err, "unable to set up lease Manager", "lease", "NodeMaintenance")
		os.Exit(1)
	}
	
	openshiftCheck,err := utils.NewOpenshiftValidator(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "failed to check if we run on Openshift")
		os.Exit(1)
	}
	isOpenShift := openshiftCheck.IsOpenshiftSupported()
	if isOpenShift{
		setupLog.Info("NMO was installed on Openshift cluster")
	}
	

	if err = (&controllers.NodeMaintenanceReconciler{
		Client:       cl,
		Scheme:       mgr.GetScheme(),
		LeaseManager: leaseManagerInitializer,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NodeMaintenance")
		os.Exit(1)
	}
	if err = (&nodemaintenancev1beta1.NodeMaintenance{}).SetupWebhookWithManager(isOpenShift, mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "NodeMaintenance")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

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

func printVersion() {
	setupLog.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	setupLog.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
	setupLog.Info(fmt.Sprintf("Operator Version: %s", version.Version))
	setupLog.Info(fmt.Sprintf("Git Commit: %s", version.GitCommit))
	setupLog.Info(fmt.Sprintf("Build Date: %s", version.BuildDate))
}

type leaseManagerInitializer struct {
	cl client.Client
	lease.Manager
}

func (ls *leaseManagerInitializer) Start(context.Context) error {
	var err error
	ls.Manager, err = lease.NewManager(ls.cl, controllers.LeaseHolderIdentity)
	return err
}

func configureWebhookOpts(webhookOpts *webhook.Options, enableHTTP2 bool) {

	certs := []string{filepath.Join(WebhookCertDir, WebhookCertName), filepath.Join(WebhookCertDir, WebhookKeyName)}
	certsInjected := true
	for _, fname := range certs {
		if _, err := os.Stat(fname); err != nil {
			certsInjected = false
			break
		}
	}
	if certsInjected {
		webhookOpts.CertDir = WebhookCertDir
		webhookOpts.CertName = WebhookCertName
		webhookOpts.KeyName = WebhookKeyName
		webhookOpts.TLSOpts = []func(*tls.Config){}
	} else {
		setupLog.Info("OLM injected certs for webhooks not found")
	}
	// disable http/2 for mitigating relevant CVEs
	if !enableHTTP2 {
		webhookOpts.TLSOpts = append(webhookOpts.TLSOpts,
			func(c *tls.Config) {
				c.NextProtos = []string{"http/1.1"}
			},
		)
		setupLog.Info("HTTP/2 for webhooks disabled")
	} else {
		setupLog.Info("HTTP/2 for webhooks enabled")
	}

}
