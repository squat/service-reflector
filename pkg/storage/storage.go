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

package storage

import (
	"context"
	"errors"
	"sync"

	discoveryv1 "k8s.io/api/discovery/v1"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/client-go/tools/cache"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"
)

// EmitterStorage is the interface implemented by both storage types.
type EmitterStorage interface {
	rest.Storage
	rest.Scoper
	rest.KindProvider
	rest.Lister
	rest.Watcher
	cache.ResourceEventHandler
}

// -----------------------------------------------------------------------
// ServiceExportStorage
// -----------------------------------------------------------------------

type serviceExportStorage struct {
	rest.TableConvertor
	informer cache.SharedIndexInformer
	sync.Mutex
	ws []*watcher
}

// NewServiceExportStorage creates a new EmitterStorage backed by a
// ServiceExport informer.
func NewServiceExportStorage(informer cache.SharedIndexInformer) EmitterStorage {
	return &serviceExportStorage{
		TableConvertor: rest.NewDefaultTableConvertor(schema.GroupResource{
			Group:    v1beta1.GroupName,
			Resource: "serviceexports",
		}),
		informer: informer,
	}
}

func (s *serviceExportStorage) New() runtime.Object {
	return &v1beta1.ServiceExport{}
}

func (s *serviceExportStorage) Destroy() {}

func (s *serviceExportStorage) Kind() string { return "ServiceExport" }

func (s *serviceExportStorage) NamespaceScoped() bool { return true }

func (s *serviceExportStorage) NewList() runtime.Object {
	return &v1beta1.ServiceExportList{}
}

func (s *serviceExportStorage) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	if !s.informer.HasSynced() {
		return nil, errors.New("backend is not ready")
	}
	ls := mergeSelectors(labels.Everything(), options)
	fs := defaultFieldSelector(options)
	ns := genericapirequest.NamespaceValue(ctx)

	list := &v1beta1.ServiceExportList{}
	for _, item := range s.informer.GetStore().List() {
		se := item.(*v1beta1.ServiceExport)
		if !ls.Matches(labels.Set(se.Labels)) {
			continue
		}
		if !fs.Matches(generic.ObjectMetaFieldsSet(&se.ObjectMeta, true)) {
			continue
		}
		if !matchNamespace(ns, &se.ObjectMeta) {
			continue
		}
		list.Items = append(list.Items, *se)
	}
	return list, nil
}

func (s *serviceExportStorage) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	ls := mergeSelectors(labels.Everything(), options)
	fs := defaultFieldSelector(options)
	ns := genericapirequest.NamespaceValue(ctx)
	return addWatcher(s, &s.ws, ls, fs, ns), nil
}

func (s *serviceExportStorage) OnAdd(obj any, _ bool) {
	handler(s, s.ws, watch.Added, obj)
}
func (s *serviceExportStorage) OnDelete(obj any) {
	handler(s, s.ws, watch.Deleted, obj)
}
func (s *serviceExportStorage) OnUpdate(_, obj any) {
	handler(s, s.ws, watch.Modified, obj)
}

// -----------------------------------------------------------------------
// EndpointSliceStorage
// -----------------------------------------------------------------------

type endpointSliceStorage struct {
	rest.TableConvertor
	sliceInformer  cache.SharedIndexInformer
	exportInformer cache.SharedIndexInformer
	sync.Mutex
	ws []*watcher
}

// NewEndpointSliceStorage creates a new EmitterStorage backed by an
// EndpointSlice informer, filtered to slices that back a service which
// has a ServiceExport in the same namespace.
func NewEndpointSliceStorage(sliceInformer, exportInformer cache.SharedIndexInformer) EmitterStorage {
	return &endpointSliceStorage{
		TableConvertor: rest.NewDefaultTableConvertor(schema.GroupResource{
			Group:    discoveryv1.SchemeGroupVersion.Group,
			Resource: "endpointslices",
		}),
		sliceInformer:  sliceInformer,
		exportInformer: exportInformer,
	}
}

func (s *endpointSliceStorage) New() runtime.Object {
	return &discoveryv1.EndpointSlice{}
}

func (s *endpointSliceStorage) Destroy() {}

func (s *endpointSliceStorage) Kind() string { return "EndpointSlice" }

func (s *endpointSliceStorage) NamespaceScoped() bool { return true }

func (s *endpointSliceStorage) NewList() runtime.Object {
	return &discoveryv1.EndpointSliceList{}
}

func (s *endpointSliceStorage) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	if !s.sliceInformer.HasSynced() || !s.exportInformer.HasSynced() {
		return nil, errors.New("backend is not ready")
	}
	ls := mergeSelectors(labels.Everything(), options)
	fs := defaultFieldSelector(options)
	ns := genericapirequest.NamespaceValue(ctx)

	list := &discoveryv1.EndpointSliceList{}
	for _, item := range s.sliceInformer.GetStore().List() {
		es := item.(*discoveryv1.EndpointSlice)
		if !ls.Matches(labels.Set(es.Labels)) {
			continue
		}
		if !fs.Matches(generic.ObjectMetaFieldsSet(&es.ObjectMeta, true)) {
			continue
		}
		if !matchNamespace(ns, &es.ObjectMeta) {
			continue
		}
		if !s.hasExport(es) {
			continue
		}
		list.Items = append(list.Items, *es)
	}
	return list, nil
}

func (s *endpointSliceStorage) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	ls := mergeSelectors(labels.Everything(), options)
	fs := defaultFieldSelector(options)
	ns := genericapirequest.NamespaceValue(ctx)
	return addWatcher(s, &s.ws, ls, fs, ns), nil
}

// hasExport returns true if a ServiceExport exists for the service that owns es.
func (s *endpointSliceStorage) hasExport(es *discoveryv1.EndpointSlice) bool {
	svcName := es.Labels[discoveryv1.LabelServiceName]
	if svcName == "" {
		return false
	}
	key := es.Namespace + "/" + svcName
	_, exists, _ := s.exportInformer.GetStore().GetByKey(key)
	return exists
}

func (s *endpointSliceStorage) OnAdd(obj any, _ bool) {
	es, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok || !s.hasExport(es) {
		return
	}
	handler(s, s.ws, watch.Added, obj)
}

func (s *endpointSliceStorage) OnDelete(obj any) {
	es, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		// Tombstone
		if d, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
			es, ok = d.Obj.(*discoveryv1.EndpointSlice)
		}
	}
	if !ok || es == nil || !s.hasExport(es) {
		return
	}
	handler(s, s.ws, watch.Deleted, obj)
}

func (s *endpointSliceStorage) OnUpdate(_, obj any) {
	es, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok || !s.hasExport(es) {
		return
	}
	handler(s, s.ws, watch.Modified, obj)
}

// -----------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------

func handler(mu sync.Locker, ws []*watcher, et watch.EventType, obj any) {
	mo, ok := obj.(metav1.Object)
	if !ok {
		return
	}
	ro, ok := obj.(runtime.Object)
	if !ok {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for _, w := range ws {
		if !w.ls.Matches(labels.Set(mo.GetLabels())) {
			continue
		}
		objMeta := &metav1.ObjectMeta{
			Name:      mo.GetName(),
			Namespace: mo.GetNamespace(),
			Labels:    mo.GetLabels(),
		}
		if !w.fs.Matches(generic.ObjectMetaFieldsSet(objMeta, true)) {
			continue
		}
		if !matchNamespace(w.ns, objMeta) {
			continue
		}
		nonBlockingSend(w.ch, watch.Event{
			Type:   et,
			Object: ro,
		})
	}
}

type watcher struct {
	ch   chan watch.Event
	ls   labels.Selector
	fs   fields.Selector
	ns   string
	stop func()
}

func addWatcher(mu sync.Locker, ws *[]*watcher, ls labels.Selector, fs fields.Selector, ns string) *watcher {
	mu.Lock()
	i := len(*ws)
	w := &watcher{
		ch: make(chan watch.Event, 100),
		ls: ls,
		fs: fs,
		ns: ns,
	}
	w.stop = func() {
		mu.Lock()
		defer mu.Unlock()
		(*ws)[i] = (*ws)[len(*ws)-1]
		(*ws)[len(*ws)-1] = nil
		*ws = (*ws)[:len(*ws)-1]
	}
	*ws = append(*ws, w)
	mu.Unlock()
	return w
}

func (w *watcher) ResultChan() <-chan watch.Event {
	return w.ch
}

func (w *watcher) Stop() {
	w.stop()
	close(w.ch)
}

func nonBlockingSend(ch chan watch.Event, e watch.Event) {
	select {
	case ch <- e:
	default:
	}
}

func mergeSelectors(s labels.Selector, options *metainternalversion.ListOptions) labels.Selector {
	ls := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		ls = options.LabelSelector
	}
	if r, ok := s.Requirements(); ok {
		ls = ls.Add(r...)
	}
	return ls
}

func defaultFieldSelector(options *metainternalversion.ListOptions) fields.Selector {
	fs := fields.Everything()
	if options != nil && options.FieldSelector != nil {
		fs = options.FieldSelector
	}
	return fs
}

func matchNamespace(ns string, obj *metav1.ObjectMeta) bool {
	return ns == "" || obj.Namespace == ns
}
