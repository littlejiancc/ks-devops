/*
Copyright 2019 The KubeSphere Authors.

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

package app

import (
	"fmt"
	"kubesphere.io/devops/cmd/controller/app/options"
	"kubesphere.io/devops/pkg/apis"
	"kubesphere.io/devops/pkg/client/devops"
	"kubesphere.io/devops/pkg/client/devops/jenkins"
	"kubesphere.io/devops/pkg/client/s3"
	"kubesphere.io/devops/pkg/config"
	"kubesphere.io/devops/pkg/informers"
	"kubesphere.io/devops/pkg/k8s"
	"kubesphere.io/devops/pkg/utils/term"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog"
	"k8s.io/klog/klogr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

func NewControllerManagerCommand() *cobra.Command {
	s := options.NewDevOpsControllerManagerOptions()
	conf, err := config.TryLoadFromDisk()
	if err == nil {
		// make sure LeaderElection is not nil
		s = &options.DevOpsControllerManagerOptions{
			KubernetesOptions: conf.KubernetesOptions,
			JenkinsOptions:    conf.DevopsOptions,
			S3Options:         conf.S3Options,
			LeaderElection:    s.LeaderElection,
			LeaderElect:       s.LeaderElect,
			WebhookCertDir:    s.WebhookCertDir,
		}
	} else {
		klog.Fatal("Failed to load configuration from disk", err)
	}

	cmd := &cobra.Command{
		Use:   "controller-manager",
		Short: `KubeSphere DevOps controller manager`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if errs := s.Validate(); len(errs) == 0 {
				err = run(s, signals.SetupSignalHandler())
			}
			return
		},
		SilenceUsage: true,
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags()

	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of KubeSphere DevOps controller",
		Run: func(cmd *cobra.Command, args []string) {
			//cmd.Println(version.Get())
		},
	}

	cmd.AddCommand(versionCmd)

	return cmd
}

func run(s *options.DevOpsControllerManagerOptions, stopCh <-chan struct{}) error {
	kubernetesClient, err := k8s.NewKubernetesClient(s.KubernetesOptions)
	if err != nil {
		klog.Errorf("Failed to create kubernetes clientset %v", err)
		return err
	}

	var devopsClient devops.Interface
	if s.JenkinsOptions != nil && len(s.JenkinsOptions.Host) != 0 {
		devopsClient, err = jenkins.NewDevopsClient(s.JenkinsOptions)
		if err != nil {
			return fmt.Errorf("failed to connect jenkins, please check jenkins status, error: %v", err)
		}
	}

	informerFactory := informers.NewInformerFactories(
		kubernetesClient.Kubernetes(),
		kubernetesClient.KubeSphere(),
		kubernetesClient.ApiExtensions())

	mgrOptions := manager.Options{
		CertDir: s.WebhookCertDir,
		Port:    8443,
	}

	if s.LeaderElect {
		mgrOptions = manager.Options{
			CertDir:                 s.WebhookCertDir,
			Port:                    8443,
			LeaderElection:          s.LeaderElect,
			LeaderElectionNamespace: "kubesphere-devops-system",
			LeaderElectionID:        "ks-devops-controller-manager-leader-election",
			LeaseDuration:           &s.LeaderElection.LeaseDuration,
			RetryPeriod:             &s.LeaderElection.RetryPeriod,
			RenewDeadline:           &s.LeaderElection.RenewDeadline,
		}
	}

	klog.V(0).Info("setting up manager")
	ctrl.SetLogger(klogr.New())
	// Use 8443 instead of 443 cause we need root permission to bind port 443
	mgr, err := manager.New(kubernetesClient.Config(), mgrOptions)
	if err != nil {
		klog.Fatalf("unable to set up overall controller manager: %v", err)
	}

	if err = apis.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatalf("unable add APIs to scheme: %v", err)
	}

	var s3Client s3.Interface
	if s.S3Options != nil && len(s.S3Options.Endpoint) != 0 {
		s3Client, err = s3.NewS3Client(s.S3Options)
		if err != nil {
			return fmt.Errorf("failed to connect to s3, please check s3 service status, error: %v", err)
		}
	}

	// register common meta types into schemas.
	metav1.AddToGroupVersion(mgr.GetScheme(), metav1.SchemeGroupVersion)

	if err = addControllers(mgr,
		kubernetesClient,
		informerFactory,
		devopsClient,
		s3Client,
		s.KubernetesOptions, stopCh); err != nil {
		klog.Fatalf("unable to register controllers to the manager: %v", err)
	}

	// Start cache data after all informer is registered
	klog.V(0).Info("Starting cache resource from apiserver...")
	informerFactory.Start(stopCh)

	klog.V(0).Info("Starting the controllers.")
	if err = mgr.Start(stopCh); err != nil {
		klog.Fatalf("unable to run the manager: %v", err)
	}

	return nil
}