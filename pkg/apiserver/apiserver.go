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

package apiserver

import (
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/tools/cache"
	v1beta1 "sigs.k8s.io/mcs-api/pkg/apis/v1beta1"

	"github.com/squat/service-reflector/pkg/storage"
)

var (
	// Scheme defines methods for serializing and deserializing API objects.
	Scheme = runtime.NewScheme()
	// Codecs provides methods for retrieving codecs and serializers for specific
	// versions and content types.
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})

	// Required unversioned types for the generic API server.
	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	Scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)

	// Register MCS types.
	if err := v1beta1.Install(Scheme); err != nil {
		panic(err)
	}

	// Register EndpointSlice types.
	if err := discoveryv1.AddToScheme(Scheme); err != nil {
		panic(err)
	}
}

// Config defines the config for the apiserver.
type Config struct {
	GenericConfig *genericapiserver.Config
}

// ServiceEmitter contains state for the emitter API server.
type ServiceEmitter struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	// Informers needed to back storage.
	serviceExportInformer cache.SharedIndexInformer
	endpointSliceInformer cache.SharedIndexInformer
}

// CompletedConfig embeds a private pointer that cannot be instantiated outside of this package.
type CompletedConfig struct {
	*completedConfig
}

// Complete fills in any fields not set that are required to have valid data.
func (cfg *Config) Complete(
	serviceExportInformer cache.SharedIndexInformer,
	endpointSliceInformer cache.SharedIndexInformer,
) CompletedConfig {
	c := completedConfig{
		GenericConfig:         cfg.GenericConfig.Complete(nil),
		serviceExportInformer: serviceExportInformer,
		endpointSliceInformer: endpointSliceInformer,
	}
	return CompletedConfig{&c}
}

// New returns a new instance of ServiceEmitter from the given config.
func (c completedConfig) New() (*ServiceEmitter, error) {
	genericServer, err := c.GenericConfig.New("service-emitter", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	s := &ServiceEmitter{GenericAPIServer: genericServer}

	// Install multicluster.x-k8s.io/v1beta1 group (ServiceExport).
	mcsGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(
		v1beta1.GroupName,
		Scheme,
		metav1.ParameterCodec,
		Codecs,
	)
	seStorage := storage.NewServiceExportStorage(c.serviceExportInformer)
	if _, err := c.serviceExportInformer.AddEventHandler(seStorage.(cache.ResourceEventHandler)); err != nil {
		return nil, err
	}
	mcsGroupInfo.VersionedResourcesStorageMap[v1beta1.GroupVersion.Version] = map[string]rest.Storage{
		"serviceexports": seStorage,
	}
	if err := s.GenericAPIServer.InstallAPIGroup(&mcsGroupInfo); err != nil {
		return nil, err
	}

	// Install discovery.k8s.io/v1 group (EndpointSlice).
	discoveryGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(
		discoveryv1.SchemeGroupVersion.Group,
		Scheme,
		metav1.ParameterCodec,
		Codecs,
	)
	esStorage := storage.NewEndpointSliceStorage(c.endpointSliceInformer, c.serviceExportInformer)
	if _, err := c.endpointSliceInformer.AddEventHandler(esStorage.(cache.ResourceEventHandler)); err != nil {
		return nil, err
	}
	discoveryGroupInfo.VersionedResourcesStorageMap[discoveryv1.SchemeGroupVersion.Version] = map[string]rest.Storage{
		"endpointslices": esStorage,
	}
	if err := s.GenericAPIServer.InstallAPIGroup(&discoveryGroupInfo); err != nil {
		return nil, err
	}

	return s, nil
}
