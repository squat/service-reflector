// Copyright 2026 the Service Reflector authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcscontrollers "sigs.k8s.io/mcs-api/controllers"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"
)

// ManagerOptions configures the controller manager.
type ManagerOptions struct {
	// KubeConfig is the REST config for the local cluster.
	KubeConfig *rest.Config
	// ClusterID is the name of this cluster.
	ClusterID string
	// Namespace to restrict controllers to (empty = all).
	Namespace string
	// RemoteConfigs maps remoteID -> REST config for each remote Emitter.
	RemoteConfigs map[string]*rest.Config
	// ReflectorSelector filters which remote ServiceExports are processed.
	ReflectorSelector labels.Selector
	// Log is the base logger.
	Log logr.Logger
}

// Start builds and runs the controller manager. It blocks until ctx is done.
func Start(ctx context.Context, opts ManagerOptions) error {
	scheme := runtime.NewScheme()
	if err := v1beta1.Install(scheme); err != nil {
		return fmt.Errorf("installing mcs-api scheme: %w", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("installing discovery scheme: %w", err)
	}

	cacheOpts := cache.Options{}
	if opts.Namespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{
			opts.Namespace: {},
		}
	}

	mgr, err := ctrl.NewManager(opts.KubeConfig, ctrl.Options{
		Scheme: scheme,
		Cache:  cacheOpts,
		// Disable metrics and health endpoints — main.go owns those.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	// ---- Upstream MCS reconcilers ----

	siReconciler := &mcscontrollers.ServiceImportReconciler{
		Client: mgr.GetClient(),
		Log:    opts.Log.WithName("ServiceImportReconciler"),
	}
	if err := siReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up ServiceImportReconciler: %w", err)
	}

	svcReconciler := &mcscontrollers.ServiceReconciler{
		Client: mgr.GetClient(),
		Log:    opts.Log.WithName("ServiceReconciler"),
	}
	if err := svcReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up ServiceReconciler: %w", err)
	}

	esReconciler := &mcscontrollers.EndpointSliceReconciler{
		Client: mgr.GetClient(),
		Log:    opts.Log.WithName("EndpointSliceReconciler"),
	}
	if err := esReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up EndpointSliceReconciler: %w", err)
	}

	// ---- Local reconcilers ----

	validator := NewServiceExportValidator(mgr.GetClient(), opts.Log.WithName("ServiceExportValidator"))
	if err := validator.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up ServiceExportValidator: %w", err)
	}

	conflictDetector := NewConflictDetector(mgr.GetClient(), opts.Log.WithName("ConflictDetector"))
	if err := conflictDetector.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up ConflictDetector: %w", err)
	}

	// ---- Watchers (one per remote cluster) ----

	for remoteID, remoteConfig := range opts.RemoteConfigs {
		w := NewWatcher(
			opts.ClusterID,
			remoteID,
			remoteConfig,
			mgr.GetClient(),
			opts.Namespace,
			opts.ReflectorSelector,
			conflictDetector,
			opts.Log.WithName("Watcher"),
		)
		// Run each watcher as a goroutine managed by the manager's lifecycle.
		wCopy := w
		if err := mgr.Add(runnableFunc(func(ctx context.Context) error {
			return wCopy.Run(ctx)
		})); err != nil {
			return fmt.Errorf("adding watcher for %s: %w", remoteID, err)
		}
	}

	return mgr.Start(ctx)
}

// runnableFunc adapts a function to the manager.Runnable interface.
type runnableFunc func(ctx context.Context) error

func (f runnableFunc) Start(ctx context.Context) error { return f(ctx) }
