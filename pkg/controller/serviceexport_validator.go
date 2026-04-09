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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"
)

// ServiceExportValidator watches local ServiceExport + Service objects.
// When a ServiceExport is created/updated it:
//  1. Verifies a matching Service exists → sets Valid condition.
//  2. Writes service metadata as annotations on the ServiceExport so
//     importing clusters can reconstruct the ServiceImport without
//     reading the raw Service.
//
// It also manages the Ready condition (set to false until the service is valid).
type ServiceExportValidator struct {
	client client.Client
	log    logr.Logger
}

// NewServiceExportValidator creates a new ServiceExportValidator.
func NewServiceExportValidator(c client.Client, log logr.Logger) *ServiceExportValidator {
	return &ServiceExportValidator{client: c, log: log}
}

// SetupWithManager registers the validator with the controller manager.
func (r *ServiceExportValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.ServiceExport{}).
		Named("serviceexport-validator").
		Complete(r)
}

// Reconcile implements reconcile.Reconciler.
func (r *ServiceExportValidator) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("serviceexport", req.NamespacedName)

	se := &v1beta1.ServiceExport{}
	if err := r.client.Get(ctx, req.NamespacedName, se); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting ServiceExport: %w", err)
	}

	svc := &corev1.Service{}
	err := r.client.Get(ctx, req.NamespacedName, svc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, r.setInvalid(ctx, se, v1beta1.ServiceExportReasonNoService,
				fmt.Sprintf("service %q not found", req.NamespacedName))
		}
		return reconcile.Result{}, fmt.Errorf("getting Service: %w", err)
	}

	// Service exists — write metadata annotations and set Valid.
	patch := se.DeepCopy()
	if patch.Annotations == nil {
		patch.Annotations = make(map[string]string)
	}

	// Encode service ports.
	ports := localServicePorts(svc)
	portsJSON, err := json.Marshal(ports)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("marshalling service ports: %w", err)
	}
	patch.Annotations[AnnotationServicePorts] = string(portsJSON)
	patch.Annotations[AnnotationServiceType] = string(localServiceImportType(svc))
	patch.Annotations[AnnotationSessionAffinity] = string(svc.Spec.SessionAffinity)

	if svc.Spec.SessionAffinityConfig != nil {
		sacJSON, err := json.Marshal(svc.Spec.SessionAffinityConfig)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("marshalling session affinity config: %w", err)
		}
		patch.Annotations[AnnotationSessionAffinityConfig] = string(sacJSON)
	} else {
		delete(patch.Annotations, AnnotationSessionAffinityConfig)
	}

	ipFamiliesJSON, err := json.Marshal(svc.Spec.IPFamilies)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("marshalling ip families: %w", err)
	}
	patch.Annotations[AnnotationIPFamilies] = string(ipFamiliesJSON)

	log.Info("ServiceExport is valid, annotations written")
	if err := r.client.Patch(ctx, patch, client.MergeFrom(se)); err != nil {
		return reconcile.Result{}, fmt.Errorf("patching ServiceExport annotations: %w", err)
	}

	// Patch status separately.
	sePatch := se.DeepCopy()
	setCondition(&sePatch.Status.Conditions, newCondition(
		v1beta1.ServiceExportConditionValid,
		metav1.ConditionTrue,
		v1beta1.ServiceExportReasonValid,
		fmt.Sprintf("service %q found and exported", req.NamespacedName),
	))
	if err := r.client.Status().Patch(ctx, sePatch, client.MergeFrom(se)); err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, fmt.Errorf("patching ServiceExport status: %w", err)
	}

	return reconcile.Result{}, nil
}

func (r *ServiceExportValidator) setInvalid(ctx context.Context, se *v1beta1.ServiceExport, reason v1beta1.ServiceExportConditionReason, msg string) error {
	patch := se.DeepCopy()
	setCondition(&patch.Status.Conditions, newCondition(
		v1beta1.ServiceExportConditionValid,
		metav1.ConditionFalse,
		reason,
		msg,
	))
	r.log.Info("ServiceExport is invalid", "reason", reason, "message", msg)
	if err := r.client.Status().Patch(ctx, patch, client.MergeFrom(se)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("patching ServiceExport status: %w", err)
	}
	return nil
}

var _ reconcile.Reconciler = &ServiceExportValidator{}
