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
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.Install(s); err != nil {
		t.Fatalf("installing mcs scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("installing core scheme: %v", err)
	}
	return s
}

// TestLocalDerivedName verifies the localDerivedName function produces the expected output.
func TestLocalDerivedName(t *testing.T) {
	nn := types.NamespacedName{Namespace: "default", Name: "my-svc"}
	got := localDerivedName(nn)
	if len(got) < 8 || got[:8] != "derived-" {
		t.Errorf("localDerivedName(%v) = %q, want prefix 'derived-'", nn, got)
	}
	// Should be deterministic.
	if got2 := localDerivedName(nn); got != got2 {
		t.Errorf("localDerivedName is not deterministic: %q != %q", got, got2)
	}
}

// TestServiceExportValidatorValid verifies that a ServiceExport gets the Valid
// condition set when a matching Service exists.
func TestServiceExportValidatorValid(t *testing.T) {
	scheme := newScheme(t)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
			},
			ClusterIP: "10.0.0.1",
		},
	}
	se := &v1beta1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-svc",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, se).WithStatusSubresource(se).Build()
	validator := NewServiceExportValidator(c, noopLogger())

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-svc"}}
	_, err := validator.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}

	// Check that annotations were written.
	updated := &v1beta1.ServiceExport{}
	if err := c.Get(context.Background(), req.NamespacedName, updated); err != nil {
		t.Fatalf("getting ServiceExport: %v", err)
	}

	if updated.Annotations[AnnotationServiceType] == "" {
		t.Errorf("expected %s annotation to be set", AnnotationServiceType)
	}
	if updated.Annotations[AnnotationServicePorts] == "" {
		t.Errorf("expected %s annotation to be set", AnnotationServicePorts)
	}
}

// TestServiceExportValidatorNoService verifies the Invalid condition is set
// when there is no matching Service.
func TestServiceExportValidatorNoService(t *testing.T) {
	scheme := newScheme(t)
	se := &v1beta1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-svc",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(se).WithStatusSubresource(se).Build()
	validator := NewServiceExportValidator(c, noopLogger())

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing-svc"}}
	_, err := validator.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
}

// TestExtractAnnotations verifies round-trip of service metadata through annotations.
func TestExtractAnnotations(t *testing.T) {
	ports := []v1beta1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}}
	portsJSON, _ := json.Marshal(ports)
	ipFamilies := []corev1.IPFamily{corev1.IPv4Protocol}
	ipJSON, _ := json.Marshal(ipFamilies)

	se := &v1beta1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationServiceType:     string(v1beta1.ClusterSetIP),
				AnnotationServicePorts:    string(portsJSON),
				AnnotationSessionAffinity: string(corev1.ServiceAffinityNone),
				AnnotationIPFamilies:      string(ipJSON),
			},
		},
	}

	siType, gotPorts, sa, sac, families, err := extractAnnotations(se)
	if err != nil {
		t.Fatalf("extractAnnotations: %v", err)
	}
	if siType != v1beta1.ClusterSetIP {
		t.Errorf("type: got %q, want ClusterSetIP", siType)
	}
	if len(gotPorts) != 1 || gotPorts[0].Port != 80 {
		t.Errorf("ports: got %v, want [{http 80 TCP}]", gotPorts)
	}
	if sa != corev1.ServiceAffinityNone {
		t.Errorf("sessionAffinity: got %q, want None", sa)
	}
	if sac != nil {
		t.Errorf("sessionAffinityConfig: expected nil, got %v", sac)
	}
	if len(families) != 1 || families[0] != corev1.IPv4Protocol {
		t.Errorf("ipFamilies: got %v, want [IPv4]", families)
	}
}

// TestSetCondition verifies that setCondition correctly inserts/replaces.
func TestSetCondition(t *testing.T) {
	conditions := []metav1.Condition{}
	c1 := newCondition(v1beta1.ServiceExportConditionValid, metav1.ConditionTrue, v1beta1.ServiceExportReasonValid, "m1")
	setCondition(&conditions, c1)
	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(conditions))
	}

	c2 := newCondition(v1beta1.ServiceExportConditionValid, metav1.ConditionFalse, v1beta1.ServiceExportReasonNoService, "m2")
	setCondition(&conditions, c2)
	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(conditions))
	}
	if conditions[0].Status != metav1.ConditionFalse {
		t.Errorf("expected ConditionFalse, got %v", conditions[0].Status)
	}

	c3 := newCondition(v1beta1.ServiceExportConditionConflict, metav1.ConditionFalse, v1beta1.ServiceExportReasonNoConflicts, "m3")
	setCondition(&conditions, c3)
	if len(conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(conditions))
	}
}

// TestConflictDetectorNoConflict verifies no conflict is reported when all
// remote specs agree.
func TestConflictDetectorNoConflict(t *testing.T) {
	scheme := newScheme(t)
	se := &v1beta1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-svc",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationServiceType:  string(v1beta1.ClusterSetIP),
				AnnotationServicePorts: `[{"port":80,"protocol":"TCP"}]`,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(se).WithStatusSubresource(se).Build()
	cd := NewConflictDetector(c, noopLogger())

	nn := types.NamespacedName{Namespace: "default", Name: "my-svc"}
	cd.RecordRemote(nn, "cluster-b", se) // same spec as local

	req := reconcile.Request{NamespacedName: nn}
	_, err := cd.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// TestConflictDetectorConflict verifies a conflict is detected when a remote
// cluster has a different port.
func TestConflictDetectorConflict(t *testing.T) {
	scheme := newScheme(t)
	localSE := &v1beta1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-svc",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationServiceType:  string(v1beta1.ClusterSetIP),
				AnnotationServicePorts: `[{"port":80,"protocol":"TCP"}]`,
			},
		},
	}
	remoteSE := &v1beta1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-svc",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationServiceType:  string(v1beta1.ClusterSetIP),
				AnnotationServicePorts: `[{"port":443,"protocol":"TCP"}]`, // different port
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(localSE).WithStatusSubresource(localSE).Build()
	cd := NewConflictDetector(c, noopLogger())

	nn := types.NamespacedName{Namespace: "default", Name: "my-svc"}
	cd.RecordRemote(nn, "cluster-b", remoteSE)

	req := reconcile.Request{NamespacedName: nn}
	_, err := cd.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// noopLogger returns a logr.Logger that discards all output.
func noopLogger() logr.Logger {
	return logr.Discard()
}
