// Copyright 2019 the Service Reflector authors
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

	v1 "k8s.io/api/core/v1"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// EmitterStorage implements the interfaces needed for
// emission, namely lister and watcher.
type EmitterStorage interface {
	rest.Storage
	rest.Scoper
	rest.KindProvider
	rest.Lister
	rest.Watcher
	cache.ResourceEventHandler
}

type endpointsStorage struct {
	coreinformers.EndpointsInformer
	selector labels.Selector
	sync.Mutex
	ws []*watcher
}

// NewEndpointsStorage creates a new EmitterStorage for Endpoints.
func NewEndpointsStorage(informer coreinformers.EndpointsInformer, selector labels.Selector) EmitterStorage {
	return &endpointsStorage{EndpointsInformer: informer, selector: selector}
}

func (s *endpointsStorage) New() runtime.Object {
	return &v1.Endpoints{}
}

func (s *endpointsStorage) Kind() string {
	return "Endpoints"
}

func (s *endpointsStorage) NamespaceScoped() bool {
	return true
}
func (s *endpointsStorage) NewList() runtime.Object {
	return &v1.EndpointsList{}
}

func (s *endpointsStorage) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	if !s.Informer().HasSynced() {
		return nil, errors.New("backend is not ready")
	}
	el := &v1.EndpointsList{}
	es, err := s.Lister().List(mergeSelectors(s.selector, options))
	if err != nil {
		return el, err
	}
	fs := defaultFieldSelector(options)
	for _, end := range es {
		if !fs.Matches(generic.ObjectMetaFieldsSet(&end.ObjectMeta, true)) {
			continue
		}
		if !matchNamespace(genericapirequest.NamespaceValue(ctx), &end.ObjectMeta) {
			continue
		}
		if end.ResourceVersion < options.ResourceVersion {
			continue
		}
		el.Items = append(el.Items, *end)
	}
	return el, nil
}

func (s *endpointsStorage) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	return addWatcher(s, &s.ws, mergeSelectors(s.selector, options), defaultFieldSelector(options), genericapirequest.NamespaceValue(ctx)), nil
}

func (s *endpointsStorage) OnAdd(obj interface{})       { handler(s, s.ws, watch.Added, obj) }
func (s *endpointsStorage) OnDelete(obj interface{})    { handler(s, s.ws, watch.Deleted, obj) }
func (s *endpointsStorage) OnUpdate(_, obj interface{}) { handler(s, s.ws, watch.Modified, obj) }

type serviceStorage struct {
	coreinformers.ServiceInformer
	selector labels.Selector
	sync.Mutex
	ws []*watcher
}

// NewServiceStorage creates a new EmitterStorage for Services.
func NewServiceStorage(informer coreinformers.ServiceInformer, selector labels.Selector) EmitterStorage {
	return &serviceStorage{ServiceInformer: informer, selector: selector}
}

func (s *serviceStorage) New() runtime.Object {
	return &v1.Service{}
}

func (s *serviceStorage) Kind() string {
	return "Service"
}

func (s *serviceStorage) NamespaceScoped() bool {
	return true
}
func (s *serviceStorage) NewList() runtime.Object {
	return &v1.ServiceList{}
}

func (s *serviceStorage) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	if !s.Informer().HasSynced() {
		return nil, errors.New("backend is not ready")
	}
	sl := &v1.ServiceList{}
	ss, err := s.Lister().List(mergeSelectors(s.selector, options))
	if err != nil {
		return sl, err
	}
	fs := defaultFieldSelector(options)
	for _, svc := range ss {
		if !fs.Matches(generic.ObjectMetaFieldsSet(&svc.ObjectMeta, true)) {
			continue
		}
		if !matchNamespace(genericapirequest.NamespaceValue(ctx), &svc.ObjectMeta) {
			continue
		}
		if svc.ResourceVersion < options.ResourceVersion {
			continue
		}
		sl.Items = append(sl.Items, *svc)
	}
	return sl, nil
}

func (s *serviceStorage) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	return addWatcher(s, &s.ws, mergeSelectors(s.selector, options), defaultFieldSelector(options), genericapirequest.NamespaceValue(ctx)), nil
}

func (s *serviceStorage) OnAdd(obj interface{})       { handler(s, s.ws, watch.Added, obj) }
func (s *serviceStorage) OnDelete(obj interface{})    { handler(s, s.ws, watch.Deleted, obj) }
func (s *serviceStorage) OnUpdate(_, obj interface{}) { handler(s, s.ws, watch.Modified, obj) }

func handler(mu sync.Locker, ws []*watcher, et watch.EventType, obj interface{}) {
	mu.Lock()
	defer mu.Unlock()
	for _, w := range ws {
		if !w.ls.Matches(labels.Set(obj.(metav1.Object).GetLabels())) {
			return
		}
		if !w.fs.Matches(generic.ObjectMetaFieldsSet(obj.(*metav1.ObjectMeta), true)) {
			return
		}
		if !matchNamespace(w.ns, obj.(*metav1.ObjectMeta)) {
			return
		}
		nonBlockingSend(w.ch, watch.Event{
			Type:   et,
			Object: obj.(runtime.Object),
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
		ch: make(chan watch.Event),
		ls: ls,
		fs: fs,
		ns: ns,
		stop: func() {
			mu.Lock()
			defer mu.Unlock()
			(*ws)[i] = (*ws)[len(*ws)-1]
			(*ws)[len(*ws)-1] = nil
			*ws = (*ws)[:len(*ws)-1]
		},
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
