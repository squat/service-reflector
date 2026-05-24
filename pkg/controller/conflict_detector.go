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
	"encoding/json"
	"fmt"

	"sync"

	"github.com/go-logr/logr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"
)

// ConflictDetector watches local ServiceExport objects and compares the
// service metadata annotations written by the validator against those seen
// across all remote clusters (via Watchers). When a conflict is detected it
// sets the ServiceExportConditionConflict condition on the local ServiceExport.
//
// The design is simple: when a watcher receives a remote ServiceExport, it
// calls the ConflictDetector.RecordRemote method. The ConflictDetector stores
// the last-known spec from each remote cluster and re-evaluates conflicts for
// the affected service whenever any remote spec changes.
type ConflictDetector struct {
	client client.Client
	log    logr.Logger

	// remoteSpecs maps "namespace/name" -> clusterID -> serialised spec summary.
	// Protected by mu.
	mu          sync.RWMutex
	remoteSpecs map[types.NamespacedName]map[string]string

	// queue is used by Watchers via RecordRemote/FogetRemote
	// to trigger conflict re-evaluation.
	queue workqueue.TypedRateLimitingInterface[reconcile.Request]
}

// NewConflictDetector creates a ConflictDetector.
func NewConflictDetector(c client.Client, log logr.Logger) *ConflictDetector {
	return &ConflictDetector{
		client:      c,
		log:         log,
		remoteSpecs: make(map[types.NamespacedName]map[string]string),
	}
}

// SetupWithManager registers the conflict detector with the manager.
func (r *ConflictDetector) SetupWithManager(mgr ctrl.Manager) error {
	r.queue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.ServiceExport{}).
		Named("conflict-detector").
		Complete(r); err != nil {
		return err
	}

	return mgr.Add(r)
}

func (r *ConflictDetector) Start(ctx context.Context) error {
	// When the manager cancels ctx, shut down the queue so Get() returns.
	go func() {
		<-ctx.Done()
		r.queue.ShutDown()
	}()

	for {
		req, shutdown := r.queue.Get()
		if shutdown {
			return nil
		}
		if _, err := r.Reconcile(ctx, req); err != nil {
			r.log.Error(err, "conflict reconcile error")
		}
		r.queue.Done(req)
	}
}

// Reconcile re-evaluates conflicts for a ServiceExport.
func (r *ConflictDetector) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	se := &v1beta1.ServiceExport{}
	if err := r.client.Get(ctx, req.NamespacedName, se); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	conflict, reason, msg := r.detectConflict(req.NamespacedName, se)
	return reconcile.Result{}, r.updateConflictCondition(ctx, se, conflict, reason, msg)
}

// RecordRemote is called by Watchers when a remote ServiceExport is seen.
// It updates the stored remote spec for the given cluster. The next reconcile
// triggered by the controller-runtime watch will pick up the updated state.
func (r *ConflictDetector) RecordRemote(nn types.NamespacedName, clusterID string, se *v1beta1.ServiceExport) {
	r.mu.Lock()
	if r.remoteSpecs[nn] == nil {
		r.remoteSpecs[nn] = make(map[string]string)
	}
	r.remoteSpecs[nn][clusterID] = specSummary(se)
	r.mu.Unlock()
	if r.queue != nil {
		r.queue.Add(reconcile.Request{NamespacedName: nn})
	}
}

// ForgetRemote is called by Watchers when a remote ServiceExport is deleted.
func (r *ConflictDetector) ForgetRemote(nn types.NamespacedName, clusterID string) {
	r.mu.Lock()
	if m, ok := r.remoteSpecs[nn]; ok {
		delete(m, clusterID)
		if len(m) == 0 {
			delete(r.remoteSpecs, nn)
		}
	}
	r.mu.Unlock()
	if r.queue != nil {
		r.queue.Add(reconcile.Request{NamespacedName: nn})
	}
}

// detectConflict checks whether any remote cluster disagrees on port/type for
// the given service. Returns (conflict bool, reason, message).
func (r *ConflictDetector) detectConflict(nn types.NamespacedName, localSE *v1beta1.ServiceExport) (bool, v1beta1.ServiceExportConditionReason, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	remotes, ok := r.remoteSpecs[nn]
	if !ok || len(remotes) == 0 {
		return false, v1beta1.ServiceExportReasonNoConflicts, "no remote exports observed"
	}

	localSummary := specSummary(localSE)
	for clusterID, remoteSummary := range remotes {
		if remoteSummary != localSummary {
			return true, v1beta1.ServiceExportReasonPortConflict,
				fmt.Sprintf("port/type conflict with cluster %s", clusterID)
		}
	}
	return false, v1beta1.ServiceExportReasonNoConflicts, "all exports are consistent"
}

// updateConflictCondition patches the ServiceExport's Conflict condition.
func (r *ConflictDetector) updateConflictCondition(ctx context.Context, se *v1beta1.ServiceExport, conflict bool, reason v1beta1.ServiceExportConditionReason, msg string) error {
	status := metav1.ConditionFalse
	if conflict {
		status = metav1.ConditionTrue
		r.log.Info("conflict detected", "serviceexport", types.NamespacedName{Namespace: se.Namespace, Name: se.Name}, "reason", reason)
	}

	patch := se.DeepCopy()
	setCondition(&patch.Status.Conditions, newCondition(v1beta1.ServiceExportConditionConflict, status, reason, msg))

	if err := r.client.Status().Patch(ctx, patch, client.MergeFrom(se)); err != nil {
		return fmt.Errorf("patching ServiceExport conflict status: %w", err)
	}
	return nil
}

// specSummary returns a stable string summarising the exported service spec
// stored in a ServiceExport's annotations. Used for equality checks.
func specSummary(se *v1beta1.ServiceExport) string {
	if se == nil || se.Annotations == nil {
		return ""
	}
	summary := struct {
		Type  string `json:"type"`
		Ports string `json:"ports"`
	}{
		Type:  se.Annotations[AnnotationServiceType],
		Ports: se.Annotations[AnnotationServicePorts],
	}
	b, _ := json.Marshal(summary)
	return string(b)
}

var _ reconcile.Reconciler = &ConflictDetector{}
var _ manager.Runnable = &ConflictDetector{}
