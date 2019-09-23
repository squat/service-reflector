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

package controller

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	resyncPeriod           = 5 * time.Minute
	reflectedAnnotationKey = "service-reflector.squat.ai/reflected"
	sourceAnnotationKey    = "service-reflector.squat.ai/source"
)

type informerPair struct {
	endpoints cache.SharedIndexInformer
	service   cache.SharedIndexInformer
}

type informerSet struct {
	endpoints coreinformers.EndpointsInformer
	service   coreinformers.ServiceInformer
	namespace coreinformers.NamespaceInformer
}

// Controller is able to reconcile local services against
// services exposed by remote APIs.
type Controller struct {
	client         kubernetes.Interface
	informers      map[string]informerPair
	localInformers informerSet
	logger         log.Logger
	queue          workqueue.RateLimitingInterface

	reconcileAttempts prometheus.Counter
	reconcileErrors   prometheus.Counter
}

// NamedClient is a Kubernetes client with an associated string.
type NamedClient struct {
	Client kubernetes.Interface
	Name   string
}

// New creates a new Controller.
func New(client kubernetes.Interface, factory informers.SharedInformerFactory, clients []*NamedClient, namespace, selector string, logger log.Logger) *Controller {
	if logger == nil {
		logger = log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	}
	c := &Controller{
		client:    client,
		informers: make(map[string]informerPair),
		logger:    logger,
		queue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "service-reflector"),

		reconcileAttempts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "service_reflector_reconcile_attempts_total",
			Help: "Number of attempts to reconcile services",
		}),

		reconcileErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "service_reflector_reconcile_errors_total",
			Help: "Number of errors that occurred while reconciling services",
		}),
	}

	// Ensure the informers are added to the factory list.
	factory.Core().V1().Endpoints().Informer()
	factory.Core().V1().Services().Informer()
	factory.Core().V1().Namespaces().Informer()
	c.localInformers = informerSet{
		endpoints: factory.Core().V1().Endpoints(),
		service:   factory.Core().V1().Services(),
		namespace: factory.Core().V1().Namespaces(),
	}
	for i := range clients {
		c.informers[clients[i].Name] = informerPair{
			endpoints: coreinformers.NewFilteredEndpointsInformer(clients[i].Client, namespace, resyncPeriod, nil, func(options *metav1.ListOptions) { options.LabelSelector = selector }),
			service:   coreinformers.NewFilteredServiceInformer(clients[i].Client, namespace, resyncPeriod, nil, func(options *metav1.ListOptions) { options.LabelSelector = selector }),
		}
	}

	return c
}

// RegisterMetrics registers the controller's metrics with the given registerer.
func (c *Controller) RegisterMetrics(r prometheus.Registerer) {
	r.MustRegister(
		c.reconcileAttempts,
		c.reconcileErrors,
	)
}

// Run the controller.
func (c *Controller) Run(stop <-chan struct{}) error {
	defer c.queue.ShutDown()

	errChan := make(chan error)
	go func() {
		v, err := c.client.Discovery().ServerVersion()
		if err != nil {
			errChan <- errors.Wrap(err, "communicating with server failed")
			return
		}
		level.Info(c.logger).Log("msg", "connection established", "cluster-version", v)
		errChan <- nil
	}()

	select {
	case err := <-errChan:
		if err != nil {
			return err
		}
	case <-stop:
		return nil
	}

	go c.worker()

	for _, inf := range c.informers {
		go inf.endpoints.Run(stop)
		go inf.service.Run(stop)
	}
	if err := c.waitForCacheSync(stop); err != nil {
		return err
	}
	c.addHandlers()

	<-stop
	return nil
}

func (c *Controller) addHandlers() {
	c.localInformers.endpoints.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.handleEvent(""),
		UpdateFunc: func(_, obj interface{}) { c.handleEvent("")(obj) },
		DeleteFunc: c.handleEvent(""),
	})

	c.localInformers.service.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.handleEvent(""),
		UpdateFunc: func(_, obj interface{}) { c.handleEvent("")(obj) },
		DeleteFunc: c.handleEvent(""),
	})

	for api, inf := range c.informers {
		inf.endpoints.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleEvent(api),
			UpdateFunc: func(_, obj interface{}) { c.handleEvent(api)(obj) },
			DeleteFunc: c.handleEvent(api),
		})
		inf.service.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleEvent(api),
			UpdateFunc: func(_, obj interface{}) { c.handleEvent(api)(obj) },
			DeleteFunc: c.handleEvent(api),
		})
	}
}

// waitForCacheSync waits for the informers' caches to be synced.
func (c *Controller) waitForCacheSync(stop <-chan struct{}) error {
	sync := func(inf cache.SharedIndexInformer, api string, t string) bool {
		if !cache.WaitForCacheSync(stop, inf.HasSynced) {
			level.Error(c.logger).Log("msg", fmt.Sprintf("failed to sync %s cache for %s", t, api))
			return false
		}
		level.Debug(c.logger).Log("msg", fmt.Sprintf("successfully synced %s cache for %s", t, api))
		return true
	}
	syncPair := func(inf informerPair, api string) bool {
		ok1 := sync(inf.endpoints, api, "Endpoints")
		ok2 := sync(inf.service, api, "Service")
		return ok1 && ok2
	}

	ok1 := sync(c.localInformers.endpoints.Informer(), "Kubernetes", "Endpoints")
	ok2 := sync(c.localInformers.service.Informer(), "Kubernetes", "Service")
	ok3 := sync(c.localInformers.namespace.Informer(), "Kubernetes", "namespace")
	ok := ok1 && ok2 && ok3
	for api, inf := range c.informers {
		if !syncPair(inf, api) {
			ok = false
		}
	}

	if !ok {
		return errors.New("failed to sync caches")
	}
	level.Info(c.logger).Log("msg", "successfully synced all caches")
	return nil
}

func (c *Controller) handleEvent(api string) func(interface{}) {
	return func(obj interface{}) {
		if api == "" {
			mo := obj.(metav1.Object)
			if v, ok := mo.GetAnnotations()[reflectedAnnotationKey]; !ok || v != "true" {
				level.Debug(c.logger).Log("msg", "ignoring service that is not managed by this controller", "name", mo.GetName(), "namespace", mo.GetNamespace())
				return
			}
			source, ok := mo.GetAnnotations()[sourceAnnotationKey]
			if !ok {
				level.Warn(c.logger).Log("msg", "service is managed by this controller but has no source annotation; skipping", "name", mo.GetName(), "namespace", mo.GetNamespace())
				return
			}
			api = source
		}
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err != nil {
			level.Error(c.logger).Log("msg", "failed to generate key", "api", api)
			return
		}

		key = key + "/" + api
		c.queue.Add(key)
	}
}

func (c *Controller) splitKey(key string) (namespace, name, api string) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 {
		// Should never occur.
		panic(fmt.Errorf("%s should have three parts", key))
	}
	return parts[0], parts[1], parts[2]
}

func (c *Controller) worker() {
	level.Debug(c.logger).Log("msg", "starting worker")
	for c.processNextWorkItem() {
	}
	level.Debug(c.logger).Log("msg", "stopping worker")
}

func (c *Controller) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	level.Debug(c.logger).Log("msg", "processing queue item", "key", key)
	defer c.queue.Done(key)

	c.reconcileAttempts.Inc()
	err := c.sync(key.(string))
	if err == nil {
		c.queue.Forget(key)
		return true
	}

	c.reconcileErrors.Inc()
	utilruntime.HandleError(errors.Wrap(err, fmt.Sprintf("sync %q failed", key)))
	c.queue.AddRateLimited(key)

	return true
}

func (c *Controller) sync(key string) error {
	logger := log.With(c.logger, "key", key)
	ns, name, api := c.splitKey(key)

	inf, ok := c.informers[api]
	if !ok {
		return fmt.Errorf("failed to find informer for %s", api)
	}

	_, exists, err := inf.service.GetStore().GetByKey(ns + "/" + name)
	if err != nil {
		return fmt.Errorf("failed to get Service %s in namespace %s from API %s: %v", name, ns, api, err)
	}

	// Does not exist on remote, so we need to delete locally.
	if !exists {
		return errors.Wrap(c.deleteLocal(ns, name, api), "failed to delete local objects")
	}

	svc, end, namespace, err := c.generate(ns, name, api)
	if err != nil {
		return fmt.Errorf("failed to generate objects: %v", err)
	}

	// First create the namespace if necessary.
	_, exists, err = c.localInformers.namespace.Informer().GetStore().GetByKey(ns)
	if err != nil {
		return fmt.Errorf("failed to get namespace %s locally: %v", ns, err)
	}
	if !exists {
		_, err = c.client.CoreV1().Namespaces().Create(namespace)
		if err != nil {
			return fmt.Errorf("failed to create namespace %s: %v", ns, err)
		}
	}
	// Next take care of creating the Service.
	obj, exists, err := c.localInformers.service.Informer().GetStore().GetByKey(ns + "/" + name)
	if err != nil {
		return fmt.Errorf("failed to get Service %s in namespace %s for API %s locally: %v", name, ns, api, err)
	}
	if exists {
		local := obj.(*v1.Service)
		if v, ok := local.Annotations[reflectedAnnotationKey]; !ok || v != "true" {
			level.Info(logger).Log("msg", "refusing to overwrite Service that is not managed by this controller", "name", name, "namespace", ns)
			return nil
		}
		if v, ok := local.Annotations[sourceAnnotationKey]; ok && v != api {
			level.Info(logger).Log("msg", "refusing to overwrite Service that is reflected from another API", "name", name, "namespace", ns)
			return nil
		}
		if c.servicesEquivalent(svc, local) {
			level.Debug(logger).Log("msg", "Services are already equivalent", "name", name, "namespace", ns)
			return nil
		}
		_, err := c.client.CoreV1().Services(ns).Update(svc)
		return errors.Wrap(err, fmt.Sprintf("failed to update Service %s in namespace %s for API %s", name, ns, api))
	}
	if _, err := c.client.CoreV1().Services(ns).Create(svc); err != nil {
		return fmt.Errorf("failed to create Service %s in namespace %s for API %s: %v", name, ns, api, err)
	}

	// Now take care of the Endpoints.
	obj, exists, err = c.localInformers.endpoints.Informer().GetStore().GetByKey(ns + "/" + name)
	if err != nil {
		return fmt.Errorf("failed to get Endpoints %s in namespace %s for API %s locally: %v", name, ns, api, err)
	}
	// There won't always be an endpoint; in that case return early.
	if end == nil {
		if !exists {
			return nil
		}
		// An Endpoints should not exist, so delete it.
		err = c.client.CoreV1().Endpoints(ns).Delete(name, &metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete Endpoints %s in namespace %s for API %s: %v", name, ns, api, err)
		}
		return nil
	}

	if exists {
		local := obj.(*v1.Endpoints)
		if v, ok := local.Annotations[reflectedAnnotationKey]; !ok || v != "true" {
			level.Info(logger).Log("msg", "refusing to overwrite Endpoints that is not managed by this controller", "name", name, "namespace", ns)
			return nil
		}
		if c.endpointsEquivalent(end, local) {
			level.Debug(logger).Log("msg", "Endpoints are already equivalent", "name", name, "namespace", ns)
			return nil
		}
		_, err := c.client.CoreV1().Endpoints(ns).Update(end)
		return errors.Wrap(err, fmt.Sprintf("failed to update Endpoints %s in namespace %s for API %s", name, ns, api))
	}
	_, err = c.client.CoreV1().Endpoints(ns).Create(end)
	return errors.Wrap(err, fmt.Sprintf("failed to create Endpoints %s in namespace %s for API %s", name, ns, api))
}

func (c *Controller) generate(ns, name, api string) (*v1.Service, *v1.Endpoints, *v1.Namespace, error) {
	obj, exists, err := c.informers[api].service.GetStore().GetByKey(ns + "/" + name)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get Service %s in namespace %s from API %s", name, ns, api)
	}
	// We just verified that the object exists and that there is no error, so this
	// should never occur.
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to find Service: %v", err)
	}
	if !exists {
		return nil, nil, nil, errors.New("the specified Service does not exist")
	}
	sremote := obj.(*v1.Service)

	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Annotations: map[string]string{
				reflectedAnnotationKey: "true",
			},
		},
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: copyAnnotations(sremote.Annotations),
			Labels:      sremote.Labels,
		},
		Spec: v1.ServiceSpec{
			ExternalName: sremote.Spec.ExternalName,
			Type:         sremote.Spec.Type,
			Ports:        sremote.Spec.Ports,
		},
	}
	svc.Annotations[reflectedAnnotationKey] = "true"
	svc.Annotations[sourceAnnotationKey] = api
	if sremote.Spec.ClusterIP == "None" {
		svc.Spec.ClusterIP = "None"
	}

	// ExternalName Services are easy.
	if svc.Spec.Type == v1.ServiceTypeExternalName {
		return svc, nil, namespace, nil
	}

	// For consistency, always try to dereference the Endpoints.
	obj, exists, err = c.informers[api].endpoints.GetStore().GetByKey(ns + "/" + name)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get Endpoints %s in namespace %s from API %s", name, ns, api)
	}
	if !exists {
		return nil, nil, nil, fmt.Errorf("could not find matching Endpoints for Service %s in namespace %s", name, ns)
	}
	eremote := obj.(*v1.Endpoints)

	end := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: copyAnnotations(eremote.Annotations),
			Labels:      eremote.Labels,
		},
	}
	end.Annotations[reflectedAnnotationKey] = "true"
	end.Annotations[sourceAnnotationKey] = api
	for i := range eremote.Subsets {
		if len(eremote.Subsets[i].Addresses) == 0 {
			continue
		}
		s := v1.EndpointSubset{
			Ports: eremote.Subsets[i].Ports,
		}
		for j := range eremote.Subsets[i].Addresses {
			s.Addresses = append(s.Addresses, v1.EndpointAddress{
				IP: eremote.Subsets[i].Addresses[j].IP,
			})
		}
		end.Subsets = append(end.Subsets, s)
	}

	return svc, end, namespace, nil
}

// endpointsEquivalent ensures that b has all of the important
// data from a, not vice-versa.
func (c *Controller) endpointsEquivalent(a, b *v1.Endpoints) bool {
	if (a != nil) != (b != nil) {
		return false
	}
	for k := range a.Annotations {
		if a.Annotations[k] != b.Annotations[k] {
			return false
		}
	}
	return a.Name == b.Name && a.Namespace == b.Namespace && reflect.DeepEqual(a.Subsets, b.Subsets)
}

// servicesEquivalent ensures that b has all of the important
// data from a, not vice-versa.
func (c *Controller) servicesEquivalent(a, b *v1.Service) bool {
	if (a != nil) != (b != nil) {
		return false
	}
	for k := range a.Annotations {
		if a.Annotations[k] != b.Annotations[k] {
			return false
		}
	}
	return a.Name == b.Name && a.Namespace == b.Namespace && a.Spec.ExternalName == b.Spec.ExternalName && a.Spec.Type == b.Spec.Type && (a.Spec.ClusterIP == "None") == (b.Spec.ClusterIP == "None") && reflect.DeepEqual(a.Spec.Ports, b.Spec.Ports)
}

// deleteLocal will delete the Endpoints and Services matching the given
// parameters against the local API.
func (c *Controller) deleteLocal(ns, name, api string) error {
	// First try to delete the Service.
	obj, exists, err := c.localInformers.service.Informer().GetStore().GetByKey(ns + "/" + name)
	if err != nil {
		return fmt.Errorf("failed to get Service %s in namespace %s for API %s locally", name, ns, api)
	}
	if !exists {
		// It's already gone so do nothing.
		level.Info(c.logger).Log("msg", "Service already does not exist locally", "name", name, "namespace", ns)
		return nil
	}
	slocal := obj.(*v1.Service)
	if v, ok := slocal.Annotations[reflectedAnnotationKey]; !ok || v != "true" {
		level.Info(c.logger).Log("msg", "refusing to delete Service that is not managed by this controller", "name", name, "namespace", ns)
		return nil
	}
	// Check if we can delete the namespace entirely.
	obj, exists, err = c.localInformers.namespace.Informer().GetStore().GetByKey(ns)
	if !exists {
		return fmt.Errorf("expected to find namespace %s", ns)
	}
	if err != nil {
		return fmt.Errorf("failed to get namespace: %s locally", ns)
	}
	nslocal := obj.(*v1.Namespace)
	if v := nslocal.Annotations[reflectedAnnotationKey]; v == "true" {
		ss, err := c.localInformers.service.Lister().Services(ns).List(labels.Everything())
		if err != nil {
			return fmt.Errorf("failed to list Services in namespace %s: %v", ns, err)
		}
		// We are going to delete the whole namespace
		// so we don't need to delete anything else.
		if len(ss) == 0 || len(ss) == 1 && ss[0].Name == name {
			return c.client.CoreV1().Namespaces().Delete(ns, &metav1.DeleteOptions{})
		}
	}
	// Try to delete the Service.
	err = c.client.CoreV1().Services(ns).Delete(name, &metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Service %s in namespace %s for API %s", name, ns, api)
	}
	// Next try to delete the Endpoints.
	obj, exists, err = c.localInformers.endpoints.Informer().GetStore().GetByKey(ns + "/" + name)
	if err != nil {
		return fmt.Errorf("failed to get Endpounts %s in namespace %s for API %s locally", name, ns, api)
	}
	if !exists {
		// It's already gone so do nothing.
		level.Info(c.logger).Log("msg", "Endpoints already does not exist locally", "name", name, "namespace", ns)
		return nil
	}
	elocal := obj.(*v1.Endpoints)
	if v, ok := elocal.Annotations[reflectedAnnotationKey]; !ok || v != "true" {
		level.Info(c.logger).Log("msg", "refusing to delete Endpoints that is not managed by this controller", "name", name, "namespace", ns)
		return nil
	}
	err = c.client.CoreV1().Endpoints(ns).Delete(name, &metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Endpoints %s in namespace %s for API %s", name, ns, api)
	}
	return nil
}

func copyAnnotations(annotations map[string]string) map[string]string {
	a := make(map[string]string)
	for k, v := range annotations {
		a[k] = v
	}
	return a
}
