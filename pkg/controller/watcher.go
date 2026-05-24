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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"
)

const (
	resyncPeriod = 5 * time.Minute
)

// Watcher connects to a remote cluster's Emitter URL and reconciles local
// ServiceImport + EndpointSlice objects based on what is exported remotely.
type Watcher struct {
	// clusterID is the name of the local cluster.
	clusterID string
	// remoteID is a human-readable identifier for the remote cluster.
	remoteID string
	// remoteConfig is the REST config pointing at the remote Emitter.
	remoteConfig *rest.Config
	// localClient is the controller-runtime client for the local cluster.
	localClient client.Client
	// namespace restricts which namespaces are watched (empty = all).
	namespace string
	// conflictDetector tracks remote ServiceExport specs for cross-cluster conflict detection.
	conflictDetector *ConflictDetector
	log              logr.Logger
}

// NewWatcher creates a Watcher for one remote Emitter.
func NewWatcher(
	clusterID string,
	remoteID string,
	remoteConfig *rest.Config,
	localClient client.Client,
	namespace string,
	conflictDetector *ConflictDetector,
	log logr.Logger,
) *Watcher {
	return &Watcher{
		clusterID:        clusterID,
		remoteID:         remoteID,
		remoteConfig:     remoteConfig,
		localClient:      localClient,
		namespace:        namespace,
		conflictDetector: conflictDetector,
		log:              log.WithValues("remote", remoteID),
	}
}

// Run starts the watcher and blocks until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	// Build a dynamic client against the remote Emitter.
	dynClient, err := dynamic.NewForConfig(w.remoteConfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client for %s: %w", w.remoteID, err)
	}

	seGVR := schema.GroupVersionResource{
		Group:    v1beta1.GroupVersion.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: "serviceexports",
	}
	esGVR := schema.GroupVersionResource{
		Group:    discoveryv1.SchemeGroupVersion.Group,
		Version:  discoveryv1.SchemeGroupVersion.Version,
		Resource: "endpointslices",
	}

	listSE := func(opts metav1.ListOptions) (runtime.Object, error) {
		if w.namespace != "" {
			return dynClient.Resource(seGVR).Namespace(w.namespace).List(ctx, opts)
		}
		return dynClient.Resource(seGVR).List(ctx, opts)
	}
	watchSE := func(opts metav1.ListOptions) (watch.Interface, error) {
		opts.Watch = true
		if w.namespace != "" {
			return dynClient.Resource(seGVR).Namespace(w.namespace).Watch(ctx, opts)
		}
		return dynClient.Resource(seGVR).Watch(ctx, opts)
	}
	listES := func(opts metav1.ListOptions) (runtime.Object, error) {
		if w.namespace != "" {
			return dynClient.Resource(esGVR).Namespace(w.namespace).List(ctx, opts)
		}
		return dynClient.Resource(esGVR).List(ctx, opts)
	}
	watchES := func(opts metav1.ListOptions) (watch.Interface, error) {
		opts.Watch = true
		if w.namespace != "" {
			return dynClient.Resource(esGVR).Namespace(w.namespace).Watch(ctx, opts)
		}
		return dynClient.Resource(esGVR).Watch(ctx, opts)
	}

	// The dynamic client returns *unstructured.Unstructured objects; use a
	// placeholder type so the informer stores them correctly.
	seInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{ListFunc: listSE, WatchFunc: watchSE},
		&unstructured.Unstructured{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	esInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{ListFunc: listES, WatchFunc: watchES},
		&unstructured.Unstructured{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	if _, err := seInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.onServiceExportAdd(ctx, obj) },
		UpdateFunc: func(_, obj any) { w.onServiceExportAdd(ctx, obj) },
		DeleteFunc: func(obj any) { w.onServiceExportDelete(ctx, obj) },
	}); err != nil {
		return err
	}

	if _, err := esInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.onEndpointSliceAdd(ctx, obj) },
		UpdateFunc: func(_, obj any) { w.onEndpointSliceAdd(ctx, obj) },
		DeleteFunc: func(obj any) { w.onEndpointSliceDelete(ctx, obj) },
	}); err != nil {
		return err
	}

	go seInformer.Run(ctx.Done())
	go esInformer.Run(ctx.Done())

	<-ctx.Done()
	return nil
}

// onServiceExportAdd reconciles a ServiceImport locally when a remote ServiceExport appears.
func (w *Watcher) onServiceExportAdd(ctx context.Context, obj any) {
	se, ok := toServiceExport(obj)
	if !ok {
		return
	}
	nn := types.NamespacedName{Namespace: se.Namespace, Name: se.Name}
	log := w.log.WithValues("serviceexport", nn)

	if w.conflictDetector != nil {
		w.conflictDetector.RecordRemote(nn, w.remoteID, se)
	}

	// Read service metadata from annotations.
	siType, ports, sessionAffinity, sessionAffinityConfig, ipFamilies, err := extractAnnotations(se)
	if err != nil {
		log.Error(err, "failed to extract service metadata from ServiceExport annotations; skipping")
		return
	}

	si := &v1beta1.ServiceImport{}
	err = w.localClient.Get(ctx, nn, si)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "failed to get local ServiceImport")
		return
	}

	if apierrors.IsNotFound(err) {
		// Create.
		si = &v1beta1.ServiceImport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      se.Name,
				Namespace: se.Namespace,
			},
			Spec: v1beta1.ServiceImportSpec{
				Type:                  siType,
				Ports:                 ports,
				SessionAffinity:       sessionAffinity,
				SessionAffinityConfig: sessionAffinityConfig,
				IPFamilies:            ipFamilies,
			},
		}
		if createErr := w.localClient.Create(ctx, si); createErr != nil {
			log.Error(createErr, "failed to create ServiceImport")
		} else {
			log.Info("created ServiceImport")
		}
		return
	}

	// Update.
	patch := si.DeepCopy()
	patch.Spec.Type = siType
	patch.Spec.Ports = ports
	patch.Spec.SessionAffinity = sessionAffinity
	patch.Spec.SessionAffinityConfig = sessionAffinityConfig
	patch.Spec.IPFamilies = ipFamilies
	if patchErr := w.localClient.Patch(ctx, patch, client.MergeFrom(si)); patchErr != nil {
		log.Error(patchErr, "failed to patch ServiceImport")
	} else {
		log.Info("updated ServiceImport")
	}
}

// onServiceExportDelete removes the local ServiceImport when the remote ServiceExport disappears.
func (w *Watcher) onServiceExportDelete(ctx context.Context, obj any) {
	se, ok := toServiceExport(obj)
	if !ok {
		if d, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
			se, ok = toServiceExport(d.Obj)
		}
		if !ok {
			return
		}
	}
	nn := types.NamespacedName{Namespace: se.Namespace, Name: se.Name}
	log := w.log.WithValues("serviceexport", nn)

	if w.conflictDetector != nil {
		w.conflictDetector.ForgetRemote(nn, w.remoteID)
	}

	si := &v1beta1.ServiceImport{}
	if err := w.localClient.Get(ctx, nn, si); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		log.Error(err, "failed to get local ServiceImport for deletion")
		return
	}
	if err := w.localClient.Delete(ctx, si); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "failed to delete local ServiceImport")
	} else {
		log.Info("deleted ServiceImport")
	}
}

// onEndpointSliceAdd reconciles a local EndpointSlice when a remote one appears.
func (w *Watcher) onEndpointSliceAdd(ctx context.Context, obj any) {
	remoteES, ok := toEndpointSlice(obj)
	if !ok {
		return
	}
	if localShouldIgnoreEndpointSlice(remoteES) {
		return
	}

	// Determine the target service name from the label.
	owner := localServiceImportOwner(remoteES)
	if owner == nil {
		return
	}

	log := w.log.WithValues("endpointslice", types.NamespacedName{Namespace: remoteES.Namespace, Name: remoteES.Name})

	// Local EndpointSlice name: use a stable name derived from remote cluster+name.
	localName := localEndpointSliceName(w.remoteID, remoteES.Name)

	localES := &discoveryv1.EndpointSlice{}
	localNN := types.NamespacedName{Namespace: remoteES.Namespace, Name: localName}
	err := w.localClient.Get(ctx, localNN, localES)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "failed to get local EndpointSlice")
		return
	}

	desired := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      localName,
			Namespace: remoteES.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: owner.Name,
				discoveryv1.LabelManagedBy:   "service-reflector",
				v1beta1.LabelSourceCluster:   w.remoteID,
			},
		},
		AddressType: remoteES.AddressType,
		Endpoints:   remoteES.Endpoints,
		Ports:       remoteES.Ports,
	}

	if apierrors.IsNotFound(err) {
		if createErr := w.localClient.Create(ctx, desired); createErr != nil {
			log.Error(createErr, "failed to create local EndpointSlice")
		} else {
			log.Info("created local EndpointSlice")
		}
		return
	}

	patch := localES.DeepCopy()
	patch.AddressType = desired.AddressType
	patch.Endpoints = desired.Endpoints
	patch.Ports = desired.Ports
	patch.Labels = desired.Labels
	if patchErr := w.localClient.Patch(ctx, patch, client.MergeFrom(localES)); patchErr != nil {
		log.Error(patchErr, "failed to patch local EndpointSlice")
	} else {
		log.Info("updated local EndpointSlice")
	}
}

// onEndpointSliceDelete removes the local EndpointSlice mirroring a remote one.
func (w *Watcher) onEndpointSliceDelete(ctx context.Context, obj any) {
	remoteES, ok := toEndpointSlice(obj)
	if !ok {
		if d, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
			remoteES, ok = toEndpointSlice(d.Obj)
		}
		if !ok {
			return
		}
	}
	localName := localEndpointSliceName(w.remoteID, remoteES.Name)
	localNN := types.NamespacedName{Namespace: remoteES.Namespace, Name: localName}
	log := w.log.WithValues("endpointslice", localNN)

	localES := &discoveryv1.EndpointSlice{}
	if err := w.localClient.Get(ctx, localNN, localES); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		log.Error(err, "failed to get local EndpointSlice for deletion")
		return
	}
	if err := w.localClient.Delete(ctx, localES); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "failed to delete local EndpointSlice")
	} else {
		log.Info("deleted local EndpointSlice")
	}
}

// localEndpointSliceName returns a stable, DNS-label-safe name for the local
// mirror of a remote EndpointSlice. It is derived from the remote cluster ID
// and the remote slice name, truncated to the 63-character Kubernetes limit.
func localEndpointSliceName(remoteID, remoteName string) string {
	name := fmt.Sprintf("%s-%s", remoteID, remoteName)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// toServiceExport converts an unstructured object from the dynamic informer
// into a *v1beta1.ServiceExport.
func toServiceExport(obj any) (*v1beta1.ServiceExport, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	se := &v1beta1.ServiceExport{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, se); err != nil {
		return nil, false
	}
	return se, true
}

// toEndpointSlice converts an unstructured object from the dynamic informer
// into a *discoveryv1.EndpointSlice.
func toEndpointSlice(obj any) (*discoveryv1.EndpointSlice, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	es := &discoveryv1.EndpointSlice{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, es); err != nil {
		return nil, false
	}
	return es, true
}

// extractAnnotations reads service metadata from a ServiceExport's annotations.
func extractAnnotations(se *v1beta1.ServiceExport) (
	siType v1beta1.ServiceImportType,
	ports []v1beta1.ServicePort,
	sessionAffinity corev1.ServiceAffinity,
	sessionAffinityConfig *corev1.SessionAffinityConfig,
	ipFamilies []corev1.IPFamily,
	err error,
) {
	ann := se.Annotations
	if ann == nil {
		err = fmt.Errorf("ServiceExport %s/%s has no annotations; not yet validated", se.Namespace, se.Name)
		return
	}

	siType = v1beta1.ServiceImportType(ann[AnnotationServiceType])
	if siType == "" {
		err = fmt.Errorf("ServiceExport %s/%s missing %s annotation", se.Namespace, se.Name, AnnotationServiceType)
		return
	}

	if raw, ok := ann[AnnotationServicePorts]; ok {
		if jsonErr := json.Unmarshal([]byte(raw), &ports); jsonErr != nil {
			err = fmt.Errorf("unmarshalling service ports: %w", jsonErr)
			return
		}
	}

	sessionAffinity = corev1.ServiceAffinity(ann[AnnotationSessionAffinity])

	if raw, ok := ann[AnnotationSessionAffinityConfig]; ok && raw != "" {
		sessionAffinityConfig = &corev1.SessionAffinityConfig{}
		if jsonErr := json.Unmarshal([]byte(raw), sessionAffinityConfig); jsonErr != nil {
			err = fmt.Errorf("unmarshalling session affinity config: %w", jsonErr)
			return
		}
	}

	if raw, ok := ann[AnnotationIPFamilies]; ok {
		if jsonErr := json.Unmarshal([]byte(raw), &ipFamilies); jsonErr != nil {
			err = fmt.Errorf("unmarshalling ip families: %w", jsonErr)
			return
		}
	}

	return
}
