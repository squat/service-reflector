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
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

type event func(t *testing.T, clients map[string]kubernetes.Interface)

func createEvent(api string, obj metav1.Object) event {
	return func(t *testing.T, clients map[string]kubernetes.Interface) {
		client := clients[api]
		switch v := obj.(type) {
		case *v1.Endpoints:
			if _, err := client.CoreV1().Endpoints(v.Namespace).Create(v); err != nil {
				t.Fatalf("failed to create Endpoints: %v", err)
			}
		case *v1.Service:
			if _, err := client.CoreV1().Services(v.Namespace).Create(v); err != nil {
				t.Fatalf("failed to create Service: %v", err)
			}
		case *v1.Namespace:
			if _, err := client.CoreV1().Namespaces().Create(v); err != nil {
				t.Fatalf("failed to create namespace: %v", err)
			}
		default:
			t.Fatalf("got unexpected type %T", v)
		}
	}
}

func deleteEvent(api string, obj metav1.Object) event {
	return func(t *testing.T, clients map[string]kubernetes.Interface) {
		client := clients[api]
		switch v := obj.(type) {
		case *v1.Endpoints:
			if err := client.CoreV1().Endpoints(v.Namespace).Delete(v.Name, &metav1.DeleteOptions{}); err != nil {
				t.Fatalf("failed to delete Endpoints: %v", err)
			}
		case *v1.Service:
			if err := client.CoreV1().Services(v.Namespace).Delete(v.Name, &metav1.DeleteOptions{}); err != nil {
				t.Fatalf("failed to delete Service: %v", err)
			}
		case *v1.Namespace:
			if err := client.CoreV1().Namespaces().Delete(v.Name, &metav1.DeleteOptions{}); err != nil {
				t.Fatalf("failed to delete namespace: %v", err)
			}
		default:
			t.Fatalf("got unexpected type %T", v)
		}
	}
}

func TestController(t *testing.T) {
	for _, tt := range []struct {
		name     string
		clients  []*NamedClient
		events   []event
		expected []metav1.Object
		selector string
	}{
		{
			name: "Empty",
		},
		{
			name: "Simple",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
			},
			expected: []metav1.Object{
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				},
				&v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				},
				&v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
						},
					},
				},
			},
		},
		{
			name: "Simple and Delete",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
				deleteEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
			},
		},
		{
			name:     "Simple and Selector",
			selector: "foo=bar",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							"foo": "bar",
						},
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
			},
			expected: []metav1.Object{
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							"foo":             "bar",
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				},
				&v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							"foo":             "bar",
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				},
				&v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
						},
					},
				},
			},
		},
		{
			name: "Mismatched",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foob",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
			},
		},
		{
			name: "Mismatched API",
			clients: []*NamedClient{
				{
					Name:   "bar",
					Client: fake.NewSimpleClientset(),
				},
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("bar", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
			},
		},
		{
			name: "ExternalName",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
						Type: v1.ServiceTypeExternalName,
					},
				}),
			},
			expected: []metav1.Object{
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
						Type: v1.ServiceTypeExternalName,
					},
				},
				&v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
						},
					},
				},
			},
		},
		{
			name: "Duplicate Service",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
				{
					Name:   "bar",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
				createEvent("bar", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 90,
							},
						},
					},
				}),
				createEvent("bar", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 9090,
								},
							},
						},
					},
				}),
			},
			expected: []metav1.Object{
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				},
				&v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				},
				&v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
						},
					},
				},
			},
		},
		{
			name: "Don't Delete Namespace",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "bar",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "bar",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
				deleteEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
			},
			expected: []metav1.Object{
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "bar",
						Namespace: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				},
				&v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "bar",
						Namespace: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
							sourceLabelKey:    "foo",
						},
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				},
				&v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bar",
						Labels: map[string]string{
							reflectedLabelKey: "true",
						},
					},
				},
			},
		},
		{
			name: "Don't Manage Namespace",
			clients: []*NamedClient{
				{
					Name:   "foo",
					Client: fake.NewSimpleClientset(),
				},
			},
			events: []event{
				createEvent("local", &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bar",
					},
				}),
				createEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
				createEvent("foo", &v1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP: "10.0.0.1",
								},
							},
							Ports: []v1.EndpointPort{
								{
									Port: 8080,
								},
							},
						},
					},
				}),
				deleteEvent("foo", &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "foo",
						Namespace: "bar",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Port: 80,
							},
						},
					},
				}),
			},
			expected: []metav1.Object{
				&v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "bar",
					},
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			clients := map[string]kubernetes.Interface{
				"local": client,
			}
			for i := range tt.clients {
				clients[tt.clients[i].Name] = tt.clients[i].Client
			}
			factory := informers.NewSharedInformerFactoryWithOptions(client, 5*time.Minute, informers.WithNamespace(metav1.NamespaceAll), informers.WithTweakListOptions(func(*metav1.ListOptions) {}))
			c := New(client, factory, tt.clients, metav1.NamespaceAll, tt.selector, nil)
			stop := make(chan struct{})
			defer close(stop)

			go factory.Start(stop)

			go func() {
				if err := c.Run(stop); err != nil {
					t.Fatalf("got unexpected error: %v", err)
				}
			}()

			// Allow the informers time to sync.
			<-time.After(100 * time.Millisecond)
			for _, ev := range tt.events {
				ev(t, clients)
			}

			// Reconciliation is async, so we need to wait a bit.
			<-time.After(1000 * time.Millisecond)
			for _, obj := range tt.expected {
				switch v := obj.(type) {
				case *v1.Endpoints:
					e, err := client.CoreV1().Endpoints(v.Namespace).Get(v.Name, metav1.GetOptions{})
					if err != nil {
						t.Fatalf("failed to get Endpoints: %v", err)
					}
					if !reflect.DeepEqual(v, e) {
						t.Errorf("expected Endpoints:\n%v\ngot:\n%v", v, e)
					}
					if !c.endpointsEquivalent(e, v) {
						t.Errorf("Endpoints should be equivalent; expected:\n%v\ngot:\n%v", v, e)
					}
				case *v1.Service:
					s, err := client.CoreV1().Services(v.Namespace).Get(v.Name, metav1.GetOptions{})
					if err != nil {
						t.Fatalf("failed to get Service: %v", err)
					}
					if !reflect.DeepEqual(v, s) {
						t.Errorf("expected Service:\n%v\ngot:\n%v", v, s)
					}
					if !c.servicesEquivalent(s, v) {
						t.Errorf("Services should be equivalent; expected:\n%v\ngot:\n%v", v, s)
					}
				case *v1.Namespace:
					n, err := client.CoreV1().Namespaces().Get(v.Name, metav1.GetOptions{})
					if err != nil {
						t.Fatalf("failed to get namespace: %v", err)
					}
					if !reflect.DeepEqual(v, n) {
						t.Errorf("expected namespace:\n%v\ngot:\n%v", v, n)
					}
				default:
					t.Fatalf("got unexpected type %T", v)
				}
			}

			// Count all the items that actually were created.
			var n int
			s, err := client.CoreV1().Services(metav1.NamespaceAll).List(metav1.ListOptions{})
			if err != nil {
				t.Fatalf("failed to list Services: %v", err)
			}
			n += len(s.Items)
			e, err := client.CoreV1().Endpoints(metav1.NamespaceAll).List(metav1.ListOptions{})
			if err != nil {
				t.Fatalf("failed to list Endpoints: %v", err)
			}
			n += len(e.Items)
			ns, err := client.CoreV1().Namespaces().List(metav1.ListOptions{})
			if err != nil {
				t.Fatalf("failed to list namespaces: %v", err)
			}
			n += len(ns.Items)
			if n != len(tt.expected) {
				t.Errorf("expected %d resources, got %d", len(tt.expected), n)
			}
			t.Log(client.Actions())
		})
	}
}
