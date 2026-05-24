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
	"crypto/sha256"
	"encoding/base32"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"
)

const (
	// AnnotationServicePorts is the JSON-encoded []v1beta1.ServicePort for the exported service.
	AnnotationServicePorts = "service-reflector.squat.ai/service-ports"
	// AnnotationServiceType is the ServiceImportType ("ClusterSetIP" or "Headless").
	AnnotationServiceType = "service-reflector.squat.ai/service-type"
	// AnnotationSessionAffinity holds the session affinity value.
	AnnotationSessionAffinity = "service-reflector.squat.ai/session-affinity"
	// AnnotationSessionAffinityConfig holds the JSON-encoded SessionAffinityConfig (omitted if nil).
	AnnotationSessionAffinityConfig = "service-reflector.squat.ai/session-affinity-config"
	// AnnotationIPFamilies holds the JSON-encoded []corev1.IPFamily.
	AnnotationIPFamilies = "service-reflector.squat.ai/ip-families"
)

// localDerivedName returns the deterministic name for the derived Service that the
// upstream MCS controllers create for a ServiceImport with ClusterSetIP type.
// It mirrors the unexported derivedName() in sigs.k8s.io/mcs-api/controllers.
func localDerivedName(nn types.NamespacedName) string {
	sum := sha256.Sum256([]byte(nn.String()))
	encoded := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "derived-" + strings.ToLower(encoded[:10])
}

// localServicePorts converts a Kubernetes Service's port list to the MCS ServicePort slice.
func localServicePorts(svc *corev1.Service) []v1beta1.ServicePort {
	ports := make([]v1beta1.ServicePort, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		ports[i] = v1beta1.ServicePort{
			Name:        p.Name,
			Protocol:    p.Protocol,
			AppProtocol: p.AppProtocol,
			Port:        p.Port,
		}
	}
	return ports
}

// localServiceImportType returns the MCS ServiceImportType for a given Service.
func localServiceImportType(svc *corev1.Service) v1beta1.ServiceImportType {
	if svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return v1beta1.Headless
	}
	return v1beta1.ClusterSetIP
}

// localShouldIgnoreEndpointSlice returns true if the EndpointSlice is managed by
// the upstream derived-service machinery and should not be processed here.
func localShouldIgnoreEndpointSlice(es *discoveryv1.EndpointSlice) bool {
	mgr, ok := es.Labels[discoveryv1.LabelManagedBy]
	return ok && mgr == "endpointslice-controller.k8s.io"
}

// localServiceImportOwner returns the NamespacedName of the ServiceImport that owns
// an EndpointSlice, based on the standard label.
func localServiceImportOwner(es *discoveryv1.EndpointSlice) *types.NamespacedName {
	svcName, ok := es.Labels[discoveryv1.LabelServiceName]
	if !ok {
		return nil
	}
	return &types.NamespacedName{Namespace: es.Namespace, Name: svcName}
}

// newCondition creates a metav1.Condition with the given type/status/reason/message.
func newCondition(t v1beta1.ServiceExportConditionType, status metav1.ConditionStatus, reason v1beta1.ServiceExportConditionReason, message string) metav1.Condition {
	return v1beta1.NewServiceExportCondition(t, status, reason, message)
}

// setCondition updates or appends a metav1.Condition in the slice, keyed by Type.
func setCondition(conditions *[]metav1.Condition, c metav1.Condition) {
	for i, existing := range *conditions {
		if existing.Type == c.Type {
			(*conditions)[i] = c
			return
		}
	}
	*conditions = append(*conditions, c)
}
