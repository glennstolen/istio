// Copyright Istio Authors
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

package serviceentry

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"istio.io/api/label"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/config/memory"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/util/xdsfake"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/spiffe"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/retry"
)

func createConfigs(configs []*config.Config, store model.ConfigStore, t testing.TB) {
	t.Helper()
	for _, cfg := range configs {
		_, err := store.Create(*cfg)
		if err != nil && strings.Contains(err.Error(), "item already exists") {
			_, err := store.Update(*cfg)
			if err != nil {
				t.Fatalf("error occurred updating ServiceEntry config: %v", err)
			}
		} else if err != nil {
			t.Fatalf("error occurred creating ServiceEntry config: %v", err)
		}
	}
}

func callInstanceHandlers(instances []*model.WorkloadInstance, sd *Controller, ev model.Event, t testing.TB) {
	t.Helper()
	for _, instance := range instances {
		sd.WorkloadInstanceHandler(instance, ev)
	}
}

func deleteConfigs(configs []*config.Config, store model.ConfigStore, t testing.TB) {
	t.Helper()
	for _, cfg := range configs {
		err := store.Delete(cfg.GroupVersionKind, cfg.Name, cfg.Namespace, nil)
		if err != nil {
			t.Errorf("error occurred crearting ServiceEntry config: %v", err)
		}
	}
}

type Event = xdsfake.Event

func initServiceDiscovery(t test.Failer) (model.ConfigStore, *Controller, *xdsfake.Updater) {
	return initServiceDiscoveryWithOpts(t, false)
}

// initServiceDiscoveryWithoutEvents initializes a test setup with no events. This avoids excessive attempts to push
// EDS updates to a full queue
func initServiceDiscoveryWithoutEvents(t test.Failer) (model.ConfigStore, *Controller) {
	store := memory.Make(collections.Pilot)
	configController := memory.NewController(store)

	stop := test.NewStop(t)
	go configController.Run(stop)
	fx := xdsfake.NewFakeXDS()
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-fx.Events: // drain
			}
		}
	}()

	serviceController := NewController(configController, fx)
	return configController, serviceController
}

func initServiceDiscoveryWithOpts(t test.Failer, workloadOnly bool, opts ...Option) (model.ConfigStore, *Controller, *xdsfake.Updater) {
	store := memory.Make(collections.Pilot)
	configController := memory.NewSyncController(store)

	stop := test.NewStop(t)
	go configController.Run(stop)

	endpoints := model.NewEndpointIndex(model.DisabledCache{})
	delegate := model.NewEndpointIndexUpdater(endpoints)
	xdsUpdater := xdsfake.NewWithDelegate(delegate)

	istioStore := configController
	var controller *Controller
	if !workloadOnly {
		controller = NewController(configController, xdsUpdater, opts...)
	} else {
		controller = NewWorkloadEntryController(configController, xdsUpdater, opts...)
	}
	go controller.Run(stop)
	return istioStore, controller, xdsUpdater
}

func TestServiceDiscoveryServices(t *testing.T) {
	store, sd, fx := initServiceDiscovery(t)
	expectedServices := []*model.Service{
		makeService("*.istio.io", "httpDNSRR", constants.UnspecifiedIP, map[string]int{"http-port": 80, "http-alt-port": 8080}, true, model.DNSRoundRobinLB),
		makeService("*.google.com", "httpDNS", constants.UnspecifiedIP, map[string]int{"http-port": 80, "http-alt-port": 8080}, true, model.DNSLB),
		makeService("tcpstatic.com", "tcpStatic", "172.217.0.1", map[string]int{"tcp-444": 444}, true, model.ClientSideLB),
	}

	createConfigs([]*config.Config{httpDNS, httpDNSRR, tcpStatic}, store, t)

	expectEvents(t, fx,
		Event{Type: "xds full", ID: "*.google.com"},
		Event{Type: "xds full", ID: "*.istio.io"},
		Event{Type: "xds full", ID: "tcpstatic.com"},
		Event{Type: "service", ID: "*.google.com", Namespace: httpDNS.Namespace},
		Event{Type: "eds cache", ID: "*.google.com", Namespace: httpDNS.Namespace},
		Event{Type: "service", ID: "*.istio.io", Namespace: httpDNSRR.Namespace},
		Event{Type: "eds cache", ID: "*.istio.io", Namespace: httpDNSRR.Namespace},
		Event{Type: "service", ID: "tcpstatic.com", Namespace: tcpStatic.Namespace},
		Event{Type: "eds cache", ID: "tcpstatic.com", Namespace: tcpStatic.Namespace})
	services := sd.Services()
	sortServices(services)
	sortServices(expectedServices)
	if err := compare(t, services, expectedServices); err != nil {
		t.Error(err)
	}
}

func TestServiceDiscoveryGetService(t *testing.T) {
	hostname := "*.google.com"
	hostDNE := "does.not.exist.local"

	store, sd, fx := initServiceDiscovery(t)

	createConfigs([]*config.Config{httpDNS, tcpStatic}, store, t)
	fx.WaitOrFail(t, "xds full")
	fx.WaitOrFail(t, "xds full")
	service := sd.GetService(host.Name(hostDNE))
	if service != nil {
		t.Errorf("GetService(%q) => should not exist, got %s", hostDNE, service.Hostname)
	}

	service = sd.GetService(host.Name(hostname))
	if service == nil {
		t.Fatalf("GetService(%q) => should exist", hostname)
	}
	if service.Hostname != host.Name(hostname) {
		t.Errorf("GetService(%q) => %q, want %q", hostname, service.Hostname, hostname)
	}
}

// TestServiceDiscoveryServiceUpdate test various add/update/delete events for ServiceEntry
// nolint: lll
func TestServiceDiscoveryServiceUpdate(t *testing.T) {
	store, sd, events := initServiceDiscovery(t)
	// httpStaticOverlayUpdated is the same as httpStaticOverlay but with an extra endpoint added to test updates
	httpStaticOverlayUpdated := func() *config.Config {
		c := httpStaticOverlay.DeepCopy()
		se := c.Spec.(*networking.ServiceEntry)
		se.Endpoints = append(se.Endpoints, &networking.WorkloadEntry{
			Address: "6.6.6.6",
			Labels:  map[string]string{"other": "bar"},
		})
		return &c
	}()
	// httpStaticOverlayUpdatedInstance is the same as httpStaticOverlayUpdated but with an extra endpoint added that has the same address
	httpStaticOverlayUpdatedInstance := func() *config.Config {
		c := httpStaticOverlayUpdated.DeepCopy()
		se := c.Spec.(*networking.ServiceEntry)
		se.Endpoints = append(se.Endpoints, &networking.WorkloadEntry{
			Address: "6.6.6.6",
			Labels:  map[string]string{"some-new-label": "bar"},
		})
		return &c
	}()

	// httpStaticOverlayUpdatedNop is the same as httpStaticOverlayUpdated but with a NOP change
	httpStaticOverlayUpdatedNop := func() *config.Config {
		return ptr.Of(httpStaticOverlayUpdated.DeepCopy())
	}()

	// httpStaticOverlayUpdatedNs is the same as httpStaticOverlay but with an extra endpoint and different namespace added to test updates
	httpStaticOverlayUpdatedNs := func() *config.Config {
		c := httpStaticOverlay.DeepCopy()
		c.Namespace = "other"
		se := c.Spec.(*networking.ServiceEntry)
		se.Endpoints = append(se.Endpoints, &networking.WorkloadEntry{
			Address: "7.7.7.7",
			Labels:  map[string]string{"namespace": "bar"},
		})
		return &c
	}()

	// Setup the expected instances for `httpStatic`. This will be added/removed from as we add various configs
	baseInstances := []*model.ServiceInstance{
		makeInstance(httpStatic, "2.2.2.2", 7080, httpStatic.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpStatic, "2.2.2.2", 18080, httpStatic.Spec.(*networking.ServiceEntry).Ports[1], nil, MTLS),
		makeInstance(httpStatic, "3.3.3.3", 1080, httpStatic.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpStatic, "3.3.3.3", 8080, httpStatic.Spec.(*networking.ServiceEntry).Ports[1], nil, MTLS),
		makeInstance(httpStatic, "4.4.4.4", 1080, httpStatic.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"foo": "bar"}, PlainText),
		makeInstance(httpStatic, "4.4.4.4", 8080, httpStatic.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"foo": "bar"}, PlainText),
	}

	t.Run("simple entry", func(t *testing.T) {
		// Create a SE, expect the base instances
		createConfigs([]*config.Config{httpStatic}, store, t)
		instances := baseInstances
		expectServiceInstances(t, sd, httpStatic, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "*.google.com", Namespace: httpStatic.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStatic.Namespace},
			Event{Type: "xds full", ID: httpStatic.Spec.(*networking.ServiceEntry).Hosts[0]})
	})

	t.Run("add entry", func(t *testing.T) {
		// Create another SE for the same host, expect these instances to get added
		createConfigs([]*config.Config{httpStaticOverlay}, store, t)
		instances := append(baseInstances,
			makeInstance(httpStaticOverlay, "5.5.5.5", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText))
		expectServiceInstances(t, sd, httpStatic, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "xds full", ID: httpStatic.Spec.(*networking.ServiceEntry).Hosts[0]})
	})

	t.Run("add endpoint", func(t *testing.T) {
		// Update the SE for the same host, expect these instances to get added
		createConfigs([]*config.Config{httpStaticOverlayUpdated}, store, t)
		instances := append(baseInstances,
			makeInstance(httpStaticOverlay, "5.5.5.5", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText))
		expectServiceInstances(t, sd, httpStatic, 0, instances)
		expectEvents(t, events, Event{Type: "eds", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace, EndpointCount: len(instances)})

		// Make a NOP change, expect that there are no changes
		createConfigs([]*config.Config{httpStaticOverlayUpdatedNop}, store, t)
		expectServiceInstances(t, sd, httpStaticOverlayUpdatedNop, 0, instances)
		// TODO this could trigger no changes
		expectEvents(t, events, Event{Type: "eds", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace, EndpointCount: len(instances)})
	})

	t.Run("overlapping address", func(t *testing.T) {
		// Add another SE with an additional endpoint with a matching address
		createConfigs([]*config.Config{httpStaticOverlayUpdatedInstance}, store, t)
		instances := append(baseInstances,
			makeInstance(httpStaticOverlay, "5.5.5.5", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText),
			makeInstance(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"some-new-label": "bar"}, PlainText))
		expectServiceInstances(t, sd, httpStaticOverlayUpdatedInstance, 0, instances)
		proxyInstances := []model.ServiceTarget{
			makeTarget(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText),
			makeTarget(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"some-new-label": "bar"}, PlainText),
		}
		expectProxyTargets(t, sd, proxyInstances, "6.6.6.6")
		// TODO 45 is wrong
		expectEvents(t, events, Event{Type: "eds", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace, EndpointCount: len(instances)})

		// Remove the additional endpoint
		createConfigs([]*config.Config{httpStaticOverlayUpdated}, store, t)
		instances = append(baseInstances,
			makeInstance(httpStaticOverlay, "5.5.5.5", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText))
		expectServiceInstances(t, sd, httpStatic, 0, instances)
		proxyInstances = []model.ServiceTarget{
			makeTarget(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText),
		}
		expectProxyTargets(t, sd, proxyInstances, "6.6.6.6")
		expectEvents(t, events, Event{Type: "eds", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace, EndpointCount: len(instances)})
	})

	t.Run("update removes endpoint", func(t *testing.T) {
		// Update the SE for the same host to remove the endpoint
		createConfigs([]*config.Config{httpStaticOverlay}, store, t)
		instances := append(baseInstances,
			makeInstance(httpStaticOverlay, "5.5.5.5", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText))
		expectServiceInstances(t, sd, httpStaticOverlay, 0, instances)
		expectEvents(t, events,
			Event{Type: "eds", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace, EndpointCount: len(instances)})
	})

	t.Run("different namespace", func(t *testing.T) {
		// Update the SE for the same host in a different ns, expect these instances to get added
		createConfigs([]*config.Config{httpStaticOverlayUpdatedNs}, store, t)
		instances := []*model.ServiceInstance{
			makeInstance(httpStaticOverlayUpdatedNs, "5.5.5.5", 4567, httpStaticOverlayUpdatedNs.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(httpStaticOverlayUpdatedNs, "7.7.7.7", 4567, httpStaticOverlayUpdatedNs.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"namespace": "bar"}, PlainText),
		}
		// This lookup is per-namespace, so we should only see the objects in the same namespace
		expectServiceInstances(t, sd, httpStaticOverlayUpdatedNs, 0, instances)
		// Expect a full push, as the Service has changed
		expectEvents(t, events,
			Event{Type: "service", ID: "*.google.com", Namespace: "other"},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: "other"},
			Event{Type: "xds full", ID: httpStaticOverlayUpdatedNs.Spec.(*networking.ServiceEntry).Hosts[0]})
	})

	t.Run("delete entry", func(t *testing.T) {
		// Delete the additional SE in same namespace , expect it to get removed
		deleteConfigs([]*config.Config{httpStaticOverlayUpdated}, store, t)
		expectServiceInstances(t, sd, httpStatic, 0, baseInstances)
		// Check the other namespace is untouched
		instances := []*model.ServiceInstance{
			makeInstance(httpStaticOverlayUpdatedNs, "5.5.5.5", 4567, httpStaticOverlayUpdatedNs.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(httpStaticOverlayUpdatedNs, "7.7.7.7", 4567, httpStaticOverlayUpdatedNs.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"namespace": "bar"}, PlainText),
		}
		expectServiceInstances(t, sd, httpStaticOverlayUpdatedNs, 0, instances)
		// svcUpdate is not triggered since `httpStatic` is there and has instances, so we should
		// not delete the endpoints shards of "*.google.com". We xpect a full push as the service has changed.
		expectEvents(t, events,
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStaticOverlayUpdated.Namespace},
			Event{Type: "xds full", ID: "*.google.com"},
		)

		// delete httpStatic, no "*.google.com" service exists now.
		deleteConfigs([]*config.Config{httpStatic}, store, t)
		// svcUpdate is triggered since "*.google.com" in same namespace is deleted and
		// we need to delete endpoint shards. We expect a full push as the service has changed.
		expectEvents(t, events,
			Event{Type: "service", ID: "*.google.com", Namespace: httpStatic.Namespace},
			Event{Type: "xds full", ID: "*.google.com"},
		)

		// add back httpStatic
		createConfigs([]*config.Config{httpStatic}, store, t)
		instances = baseInstances
		expectServiceInstances(t, sd, httpStatic, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "*.google.com", Namespace: httpStatic.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStatic.Namespace},
			Event{Type: "xds full", ID: httpStatic.Spec.(*networking.ServiceEntry).Hosts[0]})

		// Add back the ServiceEntry, expect these instances to get added
		createConfigs([]*config.Config{httpStaticOverlayUpdated}, store, t)
		instances = append(baseInstances,
			makeInstance(httpStaticOverlay, "5.5.5.5", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText))
		expectServiceInstances(t, sd, httpStatic, 0, instances)
		// Service change, so we need a full push
		expectEvents(t, events,
			Event{Type: "service", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "xds full", ID: "*.google.com"})
	})

	t.Run("change host", func(t *testing.T) {
		// same as httpStaticOverlayUpdated but with an additional host
		httpStaticHost := func() *config.Config {
			c := httpStaticOverlayUpdated.DeepCopy()
			se := c.Spec.(*networking.ServiceEntry)
			se.Hosts = append(se.Hosts, "other.com")
			return &c
		}()
		createConfigs([]*config.Config{httpStaticHost}, store, t)
		instances := append(baseInstances,
			makeInstance(httpStaticOverlay, "5.5.5.5", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(httpStaticOverlay, "6.6.6.6", 4567, httpStaticOverlay.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText))
		// This is not applied, just to make makeInstance pick the right service.
		otherHost := func() *config.Config {
			c := httpStaticOverlayUpdated.DeepCopy()
			se := c.Spec.(*networking.ServiceEntry)
			se.Hosts = []string{"other.com"}
			return &c
		}()
		instances2 := []*model.ServiceInstance{
			makeInstance(otherHost, "5.5.5.5", 4567, httpStaticHost.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"overlay": "bar"}, PlainText),
			makeInstance(otherHost, "6.6.6.6", 4567, httpStaticHost.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"other": "bar"}, PlainText),
		}
		expectServiceInstances(t, sd, httpStaticHost, 0, instances, instances2)
		// Service change, so we need a full push
		expectEvents(t, events,
			Event{Type: "service", ID: "other.com", Namespace: httpStaticOverlayUpdated.Namespace},
			Event{Type: "eds cache", ID: "other.com", Namespace: httpStaticOverlayUpdated.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStaticOverlayUpdated.Namespace},
			Event{Type: "xds full", ID: "other.com"}) // service added

		// restore this config and remove the added host.
		createConfigs([]*config.Config{httpStaticOverlayUpdated}, store, t)
		expectEvents(t, events,
			Event{Type: "service", ID: "other.com", Namespace: httpStatic.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStatic.Namespace},
			Event{Type: "xds full", ID: "other.com"}) // service deleted
	})

	t.Run("change dns endpoints", func(t *testing.T) {
		// Setup the expected instances for DNS. This will be added/removed from as we add various configs
		instances1 := []*model.ServiceInstance{
			makeInstance(tcpDNS, "lon.google.com", 444, tcpDNS.Spec.(*networking.ServiceEntry).Ports[0],
				nil, MTLS),
			makeInstance(tcpDNS, "in.google.com", 444, tcpDNS.Spec.(*networking.ServiceEntry).Ports[0],
				nil, MTLS),
		}

		// This is not applied, just to make makeInstance pick the right service.
		tcpDNSUpdated := func() *config.Config {
			c := tcpDNS.DeepCopy()
			se := c.Spec.(*networking.ServiceEntry)
			se.Endpoints = []*networking.WorkloadEntry{
				{
					Address: "lon.google.com",
					Labels:  map[string]string{label.SecurityTlsMode.Name: model.IstioMutualTLSModeLabel},
				},
			}
			return &c
		}()

		instances2 := []*model.ServiceInstance{
			makeInstance(tcpDNS, "lon.google.com", 444, tcpDNS.Spec.(*networking.ServiceEntry).Ports[0],
				nil, MTLS),
		}

		createConfigs([]*config.Config{tcpDNS}, store, t)
		expectServiceInstances(t, sd, tcpDNS, 0, instances1)
		// Service change, so we need a full push
		expectEvents(t, events,
			Event{Type: "service", ID: "tcpdns.com", Namespace: tcpDNS.Namespace},
			Event{Type: "eds cache", ID: "tcpdns.com", Namespace: tcpDNS.Namespace},
			Event{Type: "xds full", ID: "tcpdns.com"}) // service added

		// now update the config
		createConfigs([]*config.Config{tcpDNSUpdated}, store, t)
		expectEvents(t, events,
			Event{Type: "xds full", ID: "tcpdns.com"},
			Event{Type: "eds cache", ID: "tcpdns.com"},
		) // service deleted
		expectServiceInstances(t, sd, tcpDNS, 0, instances2)
	})

	t.Run("change workload selector", func(t *testing.T) {
		// same as selector but with an additional host
		selector1 := func() *config.Config {
			c := httpStaticOverlay.DeepCopy()
			se := c.Spec.(*networking.ServiceEntry)
			se.Hosts = append(se.Hosts, "selector1.com")
			se.Endpoints = nil
			se.WorkloadSelector = &networking.WorkloadSelector{
				Labels: map[string]string{"app": "wle"},
			}
			return &c
		}()
		createConfigs([]*config.Config{selector1}, store, t)
		// Service change, so we need a full push
		expectEvents(t, events,
			Event{Type: "service", ID: "selector1.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "service", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "xds full", ID: "*.google.com,selector1.com"}) // service added

		selector1Updated := func() *config.Config {
			c := selector1.DeepCopy()
			se := c.Spec.(*networking.ServiceEntry)
			se.WorkloadSelector = &networking.WorkloadSelector{
				Labels: map[string]string{"app": "wle1"},
			}
			return &c
		}()
		createConfigs([]*config.Config{selector1Updated}, store, t)
		expectEvents(t, events,
			Event{Type: "service", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "service", ID: "selector1.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "eds cache", ID: "*.google.com", Namespace: httpStaticOverlay.Namespace},
			Event{Type: "xds full", ID: "*.google.com,selector1.com"}) // service updated
	})
}

func TestServiceDiscoveryWorkloadUpdate(t *testing.T) {
	store, sd, events := initServiceDiscovery(t)

	// Setup a couple workload entries for test. These will be selected by the `selector` SE
	wle := createWorkloadEntry("wl", selector.Name,
		&networking.WorkloadEntry{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})
	wle2 := createWorkloadEntry("wl2", selector.Name,
		&networking.WorkloadEntry{
			Address:        "3.3.3.3",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})
	wle3 := createWorkloadEntry("wl3", selector.Name,
		&networking.WorkloadEntry{
			Address:        "abc.def",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})
	dnsWle := createWorkloadEntry("dnswl", dnsSelector.Namespace,
		&networking.WorkloadEntry{
			Address:        "4.4.4.4",
			Labels:         map[string]string{"app": "dns-wle"},
			ServiceAccount: "default",
		})

	t.Run("service entry", func(t *testing.T) {
		// Add just the ServiceEntry with selector. We should see no instances
		createConfigs([]*config.Config{selector}, store, t)
		instances := []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "eds cache", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "xds full", ID: "selector.com"})
	})

	t.Run("add workload", func(t *testing.T) {
		// Add a WLE, we expect this to update
		createConfigs([]*config.Config{wle}, store, t)

		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "proxy", ID: "2.2.2.2"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2},
		)
	})

	t.Run("update service entry host", func(t *testing.T) {
		updated := func() *config.Config {
			d := selector.DeepCopy()
			se := d.Spec.(*networking.ServiceEntry)
			se.Hosts = []string{"updated.com"}
			return &d
		}()

		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(updated, "2.2.2.2", 444,
				updated.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(updated, "2.2.2.2", 445,
				updated.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = updated.Name
		}

		createConfigs([]*config.Config{updated}, store, t)
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, []*model.ServiceInstance{})
		expectServiceInstances(t, sd, updated, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "updated.com", Namespace: selector.Namespace},
			Event{Type: "service", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "eds cache", ID: "updated.com", Namespace: selector.Namespace},
			Event{Type: "xds full", ID: "selector.com,updated.com"},
		)
	})

	t.Run("restore service entry host", func(t *testing.T) {
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		updated := func() *config.Config {
			d := selector.DeepCopy()
			se := d.Spec.(*networking.ServiceEntry)
			se.Hosts = []string{"updated.com"}
			return &d
		}()

		createConfigs([]*config.Config{selector}, store, t)
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectServiceInstances(t, sd, updated, 0, []*model.ServiceInstance{})
		expectEvents(t, events,
			Event{Type: "service", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "service", ID: "updated.com", Namespace: selector.Namespace},
			Event{Type: "eds cache", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "xds full", ID: "selector.com,updated.com"},
		)
	})

	t.Run("add dns service entry", func(t *testing.T) {
		// Add just the ServiceEntry with selector. We should see no instances
		createConfigs([]*config.Config{dnsSelector}, store, t)
		instances := []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "4.4.4.4")
		expectServiceInstances(t, sd, dnsSelector, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "dns.selector.com", Namespace: dnsSelector.Namespace},
			Event{Type: "eds cache", ID: "dns.selector.com", Namespace: dnsSelector.Namespace},
			Event{Type: "xds full", ID: "dns.selector.com"})
	})

	t.Run("add dns workload", func(t *testing.T) {
		// Add a WLE, we expect this to update
		createConfigs([]*config.Config{dnsWle}, store, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(dnsSelector, "4.4.4.4", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "dns-wle"}, "default"),
			makeInstanceWithServiceAccount(dnsSelector, "4.4.4.4", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "dns-wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "dnswl"
			i.Endpoint.Namespace = dnsSelector.Namespace
		}
		expectProxyInstances(t, sd, instances, "4.4.4.4")
		expectServiceInstances(t, sd, dnsSelector, 0, instances)
		expectEvents(t, events,
			Event{Type: "eds cache", ID: "dns.selector.com", Namespace: dnsSelector.Namespace},
			Event{Type: "xds full", ID: "dns.selector.com"})
	})

	t.Run("another workload", func(t *testing.T) {
		// Add a different WLE
		createConfigs([]*config.Config{wle2}, store, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		instances = append(instances,
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"))
		for _, i := range instances[2:] {
			i.Endpoint.WorkloadName = "wl2"
			i.Endpoint.Namespace = selector.Name
		}
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "proxy", ID: "3.3.3.3"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 4},
		)
	})

	t.Run("ignore host workload", func(t *testing.T) {
		// Add a WLE with host address. Should be ignored by static service entry.
		createConfigs([]*config.Config{wle3}, store, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		instances = append(instances,
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"))
		for _, i := range instances[2:] {
			i.Endpoint.WorkloadName = "wl2"
			i.Endpoint.Namespace = selector.Name
		}
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "proxy", ID: "abc.def"},
		)
	})

	t.Run("deletion", func(t *testing.T) {
		// Delete the configs, it should be gone
		deleteConfigs([]*config.Config{wle2}, store, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events, Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2})

		// Delete the other config
		deleteConfigs([]*config.Config{wle}, store, t)
		instances = []*model.ServiceInstance{}
		expectServiceInstances(t, sd, selector, 0, instances)
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectEvents(t, events, Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 0})

		// Add the config back
		createConfigs([]*config.Config{wle}, store, t)
		instances = []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "proxy", ID: "2.2.2.2"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2},
		)
	})
}

func TestServiceDiscoveryWorkloadChangeLabel(t *testing.T) {
	store, sd, events := initServiceDiscovery(t)

	wle := createWorkloadEntry("wl", selector.Name,
		&networking.WorkloadEntry{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})

	wle2 := createWorkloadEntry("wl", selector.Name,
		&networking.WorkloadEntry{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "wle2"},
			ServiceAccount: "default",
		})
	wle3 := createWorkloadEntry("wl3", selector.Name,
		&networking.WorkloadEntry{
			Address:        "3.3.3.3",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})

	t.Run("service entry", func(t *testing.T) {
		// Add just the ServiceEntry with selector. We should see no instances
		createConfigs([]*config.Config{selector}, store, t)
		instances := []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "eds cache", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "xds full", ID: "selector.com"})
	})

	t.Run("change label removing all", func(t *testing.T) {
		// Add a WLE, we expect this to update
		createConfigs([]*config.Config{wle}, store, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "proxy", ID: "2.2.2.2"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2},
		)

		createConfigs([]*config.Config{wle2}, store, t)
		instances = []*model.ServiceInstance{}
		expectServiceInstances(t, sd, selector, 0, instances)
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectEvents(t, events,
			Event{Type: "proxy", ID: "2.2.2.2"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 0})
	})

	t.Run("change label removing one", func(t *testing.T) {
		// Add a WLE, we expect this to update
		createConfigs([]*config.Config{wle}, store, t)
		expectEvents(t, events,
			Event{Type: "proxy", ID: "2.2.2.2"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2},
		)
		// add a wle, expect this to be an add
		createConfigs([]*config.Config{wle3}, store, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances[:2] {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selector.Name
		}
		for _, i := range instances[2:] {
			i.Endpoint.WorkloadName = "wl3"
			i.Endpoint.Namespace = selector.Name
		}
		expectProxyInstances(t, sd, instances[:2], "2.2.2.2")
		expectProxyInstances(t, sd, instances[2:], "3.3.3.3")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "proxy", ID: "3.3.3.3"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 4},
		)

		createConfigs([]*config.Config{wle2}, store, t)
		instances = []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl3"
			i.Endpoint.Namespace = selector.Name
		}
		expectServiceInstances(t, sd, selector, 0, instances)
		expectProxyInstances(t, sd, instances, "3.3.3.3")
		expectEvents(t, events,
			Event{Type: "proxy", ID: "2.2.2.2"},
			Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2})
	})
}

func TestWorkloadInstanceFullPush(t *testing.T) {
	store, sd, events := initServiceDiscovery(t)

	// Setup a WorkloadEntry with selector the same as ServiceEntry
	wle := createWorkloadEntry("wl", selectorDNS.Name,
		&networking.WorkloadEntry{
			Address:        "postman-echo.com",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})

	fi1 := &model.WorkloadInstance{
		Name:      "additional-name",
		Namespace: selectorDNS.Name,
		Endpoint: &model.IstioEndpoint{
			Address:        "4.4.4.4",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(selectorDNS.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}

	fi2 := &model.WorkloadInstance{
		Name:      "another-name",
		Namespace: selectorDNS.Namespace,
		Endpoint: &model.IstioEndpoint{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(selectorDNS.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}

	t.Run("service entry", func(t *testing.T) {
		// Add just the ServiceEntry with selector. We should see no instances
		createConfigs([]*config.Config{selectorDNS}, store, t)
		instances := []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "4.4.4.4")
		expectServiceInstances(t, sd, selectorDNS, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "selector.com", Namespace: selectorDNS.Namespace},
			Event{Type: "eds cache", ID: "selector.com", Namespace: selectorDNS.Namespace},
			Event{Type: "xds full", ID: "selector.com"})
	})

	t.Run("add workload", func(t *testing.T) {
		// Add a WLE, we expect this to update
		createConfigs([]*config.Config{wle}, store, t)

		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selectorDNS, "postman-echo.com", 444,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[0],
				map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selectorDNS, "postman-echo.com", 445,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[1],
				map[string]string{"app": "wle"}, "default"),
		}
		for _, i := range instances {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selectorDNS.Name
		}
		expectProxyInstances(t, sd, instances, "postman-echo.com")
		expectServiceInstances(t, sd, selectorDNS, 0, instances)
		expectEvents(t, events,
			Event{Type: "eds cache", ID: "selector.com", Namespace: selectorDNS.Namespace},
			Event{Type: "xds full", ID: "selector.com"},
		)
	})

	t.Run("full push for new instance", func(t *testing.T) {
		callInstanceHandlers([]*model.WorkloadInstance{fi1}, sd, model.EventAdd, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selectorDNS, "4.4.4.4", 444,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selectorDNS, "4.4.4.4", 445,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selectorDNS, "postman-echo.com", 444,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selectorDNS, "postman-echo.com", 445,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}

		for _, i := range instances[2:] {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selectorDNS.Name
		}

		expectProxyInstances(t, sd, instances[:2], "4.4.4.4")
		expectProxyInstances(t, sd, instances[2:], "postman-echo.com")
		expectServiceInstances(t, sd, selectorDNS, 0, instances)
		expectEvents(t, events,
			Event{Type: "eds", ID: "selector.com", Namespace: selectorDNS.Namespace, EndpointCount: len(instances)},
			Event{Type: "xds full", ID: "selector.com"})
	})

	t.Run("full push for another new workload instance", func(t *testing.T) {
		callInstanceHandlers([]*model.WorkloadInstance{fi2}, sd, model.EventAdd, t)
		expectEvents(t, events,
			Event{Type: "eds", ID: "selector.com", Namespace: selectorDNS.Namespace, EndpointCount: 6},
			Event{Type: "xds full", ID: "selector.com"})
	})

	t.Run("full push on delete workload instance", func(t *testing.T) {
		callInstanceHandlers([]*model.WorkloadInstance{fi1}, sd, model.EventDelete, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selectorDNS, "2.2.2.2", 444,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selectorDNS, "2.2.2.2", 445,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selectorDNS, "postman-echo.com", 444,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selectorDNS, "postman-echo.com", 445,
				selectorDNS.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}

		for _, i := range instances[2:] {
			i.Endpoint.WorkloadName = "wl"
			i.Endpoint.Namespace = selectorDNS.Name
		}

		expectProxyInstances(t, sd, instances[:2], "2.2.2.2")
		expectProxyInstances(t, sd, instances[2:], "postman-echo.com")
		expectServiceInstances(t, sd, selectorDNS, 0, instances)

		expectEvents(t, events,
			Event{Type: "eds", ID: "selector.com", Namespace: selectorDNS.Namespace, EndpointCount: len(instances)},
			Event{Type: "xds full", ID: "selector.com"})
	})
}

func TestServiceDiscoveryWorkloadInstance(t *testing.T) {
	store, sd, events := initServiceDiscovery(t)

	// Setup a couple of workload instances for test. These will be selected by the `selector` SE
	fi1 := &model.WorkloadInstance{
		Name:      selector.Name,
		Namespace: selector.Namespace,
		Endpoint: &model.IstioEndpoint{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(selector.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}

	fi2 := &model.WorkloadInstance{
		Name:      "some-other-name",
		Namespace: selector.Namespace,
		Endpoint: &model.IstioEndpoint{
			Address:        "3.3.3.3",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(selector.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}

	fi3 := &model.WorkloadInstance{
		Name:      "another-name",
		Namespace: dnsSelector.Namespace,
		Endpoint: &model.IstioEndpoint{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "dns-wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(dnsSelector.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}

	t.Run("service entry", func(t *testing.T) {
		// Add just the ServiceEntry with selector. We should see no instances
		createConfigs([]*config.Config{selector}, store, t)
		instances := []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "eds cache", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "xds full", ID: "selector.com"})
	})

	t.Run("add another service entry", func(t *testing.T) {
		createConfigs([]*config.Config{dnsSelector}, store, t)
		instances := []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, dnsSelector, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "dns.selector.com", Namespace: dnsSelector.Namespace},
			Event{Type: "eds cache", ID: "dns.selector.com", Namespace: dnsSelector.Namespace},
			Event{Type: "xds full", ID: "dns.selector.com"})
	})

	t.Run("add workload instance", func(t *testing.T) {
		// Add a workload instance, we expect this to update
		callInstanceHandlers([]*model.WorkloadInstance{fi1}, sd, model.EventAdd, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events, Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2})
	})

	t.Run("another workload instance", func(t *testing.T) {
		// Add a different instance
		callInstanceHandlers([]*model.WorkloadInstance{fi2}, sd, model.EventAdd, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		instances = append(instances,
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "3.3.3.3", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"))
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events, Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 4})
	})

	t.Run("delete workload instance", func(t *testing.T) {
		// Delete the instances, it should be gone
		callInstanceHandlers([]*model.WorkloadInstance{fi2}, sd, model.EventDelete, t)
		instances := []*model.ServiceInstance{
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 444,
				selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
			makeInstanceWithServiceAccount(selector, "2.2.2.2", 445,
				selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events, Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 2})

		key := instancesKey{namespace: selector.Namespace, hostname: "selector.com"}
		namespacedName := selector.NamespacedName()
		if len(sd.serviceInstances.ip2instance) != 1 {
			t.Fatalf("service instances store `ip2instance` memory leak, expect 1, got %d", len(sd.serviceInstances.ip2instance))
		}
		if len(sd.serviceInstances.instances[key]) != 1 {
			t.Fatalf("service instances store `instances` memory leak, expect 1, got %d", len(sd.serviceInstances.instances[key]))
		}
		if len(sd.serviceInstances.instancesBySE[namespacedName]) != 1 {
			t.Fatalf("service instances store `instancesBySE` memory leak, expect 1, got %d", len(sd.serviceInstances.instancesBySE[namespacedName]))
		}

		// The following sections mimic this scenario:
		// f1 starts terminating, f3 picks up the IP, f3 delete event (pod
		// not ready yet) comes before f1
		//
		// Delete f3 event
		callInstanceHandlers([]*model.WorkloadInstance{fi3}, sd, model.EventDelete, t)
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)

		// Delete f1 event
		callInstanceHandlers([]*model.WorkloadInstance{fi1}, sd, model.EventDelete, t)
		instances = []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events, Event{Type: "eds", ID: "selector.com", Namespace: selector.Namespace, EndpointCount: 0})

		if len(sd.serviceInstances.ip2instance) != 0 {
			t.Fatalf("service instances store `ip2instance` memory leak, expect 0, got %d", len(sd.serviceInstances.ip2instance))
		}
		if len(sd.serviceInstances.instances[key]) != 0 {
			t.Fatalf("service instances store `instances` memory leak, expect 0, got %d", len(sd.serviceInstances.instances[key]))
		}
		if len(sd.serviceInstances.instancesBySE[namespacedName]) != 0 {
			t.Fatalf("service instances store `instancesBySE` memory leak, expect 0, got %d", len(sd.serviceInstances.instancesBySE[namespacedName]))
		}

		// Add f3 event
		callInstanceHandlers([]*model.WorkloadInstance{fi3}, sd, model.EventAdd, t)
		instances = []*model.ServiceInstance{
			makeInstanceWithServiceAccount(dnsSelector, "2.2.2.2", 444,
				dnsSelector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "dns-wle"}, "default"),
			makeInstanceWithServiceAccount(dnsSelector, "2.2.2.2", 445,
				dnsSelector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "dns-wle"}, "default"),
		}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, dnsSelector, 0, instances)
		expectEvents(t, events, Event{Type: "eds", ID: "dns.selector.com", Namespace: dnsSelector.Namespace, EndpointCount: 2})
	})
}

func TestServiceDiscoveryWorkloadInstanceChangeLabel(t *testing.T) {
	store, sd, events := initServiceDiscovery(t)

	type expectedProxyInstances struct {
		instancesWithSA []*model.ServiceInstance
		address         string
	}

	type testWorkloadInstance struct {
		name                   string
		namespace              string
		address                string
		labels                 map[string]string
		serviceAccount         string
		tlsmode                string
		expectedProxyInstances []expectedProxyInstances
	}

	t.Run("service entry", func(t *testing.T) {
		// Add just the ServiceEntry with selector. We should see no instances
		createConfigs([]*config.Config{selector}, store, t)
		instances := []*model.ServiceInstance{}
		expectProxyInstances(t, sd, instances, "2.2.2.2")
		expectServiceInstances(t, sd, selector, 0, instances)
		expectEvents(t, events,
			Event{Type: "service", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "eds cache", ID: "selector.com", Namespace: selector.Namespace},
			Event{Type: "xds full"})
	})

	cases := []struct {
		name      string
		instances []testWorkloadInstance
	}{
		{
			name: "change label removing all",
			instances: []testWorkloadInstance{
				{
					name:           selector.Name,
					namespace:      selector.Namespace,
					address:        "2.2.2.2",
					labels:         map[string]string{"app": "wle"},
					serviceAccount: "default",
					tlsmode:        model.IstioMutualTLSModeLabel,
					expectedProxyInstances: []expectedProxyInstances{
						{
							instancesWithSA: []*model.ServiceInstance{
								makeInstanceWithServiceAccount(selector, "2.2.2.2", 444, selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
								makeInstanceWithServiceAccount(selector, "2.2.2.2", 445, selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
							},
							address: "2.2.2.2",
						},
					},
				},
				{
					name:           selector.Name,
					namespace:      selector.Namespace,
					address:        "2.2.2.2",
					labels:         map[string]string{"app": "wle2"},
					serviceAccount: "default",
					tlsmode:        model.IstioMutualTLSModeLabel,
					expectedProxyInstances: []expectedProxyInstances{
						{
							instancesWithSA: []*model.ServiceInstance{}, // The instance labels don't match the se anymore, so adding this wi removes 2 instances
							address:         "2.2.2.2",
						},
					},
				},
			},
		},
		{
			name: "change label removing all",
			instances: []testWorkloadInstance{
				{
					name:           selector.Name,
					namespace:      selector.Namespace,
					address:        "2.2.2.2",
					labels:         map[string]string{"app": "wle"},
					serviceAccount: "default",
					tlsmode:        model.IstioMutualTLSModeLabel,
					expectedProxyInstances: []expectedProxyInstances{
						{
							instancesWithSA: []*model.ServiceInstance{
								makeInstanceWithServiceAccount(selector, "2.2.2.2", 444, selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
								makeInstanceWithServiceAccount(selector, "2.2.2.2", 445, selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
							},
							address: "2.2.2.2",
						},
					},
				},
				{
					name:           "another-name",
					namespace:      selector.Namespace,
					address:        "3.3.3.3",
					labels:         map[string]string{"app": "wle"},
					serviceAccount: "default",
					tlsmode:        model.IstioMutualTLSModeLabel,
					expectedProxyInstances: []expectedProxyInstances{
						{
							instancesWithSA: []*model.ServiceInstance{
								makeInstanceWithServiceAccount(selector, "2.2.2.2", 444, selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
								makeInstanceWithServiceAccount(selector, "2.2.2.2", 445, selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
							},
							address: "2.2.2.2",
						},
						{
							instancesWithSA: []*model.ServiceInstance{
								makeInstanceWithServiceAccount(selector, "3.3.3.3", 444, selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
								makeInstanceWithServiceAccount(selector, "3.3.3.3", 445, selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
							},
							address: "3.3.3.3",
						},
					},
				},
				{
					name:           selector.Name,
					namespace:      selector.Namespace,
					address:        "2.2.2.2",
					labels:         map[string]string{"app": "wle2"},
					serviceAccount: "default",
					tlsmode:        model.IstioMutualTLSModeLabel,
					expectedProxyInstances: []expectedProxyInstances{
						{
							instancesWithSA: []*model.ServiceInstance{
								makeInstanceWithServiceAccount(selector, "3.3.3.3", 444, selector.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"app": "wle"}, "default"),
								makeInstanceWithServiceAccount(selector, "3.3.3.3", 445, selector.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"app": "wle"}, "default"),
							},
							address: "3.3.3.3",
						},
					},
				},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, instance := range testCase.instances {

				wi := &model.WorkloadInstance{
					Name:      instance.name,
					Namespace: instance.namespace,
					Endpoint: &model.IstioEndpoint{
						Address:        instance.address,
						Labels:         instance.labels,
						ServiceAccount: spiffe.MustGenSpiffeURI(selector.Name, instance.serviceAccount),
						TLSMode:        instance.tlsmode,
					},
				}

				callInstanceHandlers([]*model.WorkloadInstance{wi}, sd, model.EventAdd, t)

				totalInstances := []*model.ServiceInstance{}
				for _, expectedProxyInstance := range instance.expectedProxyInstances {
					expectProxyInstances(t, sd, expectedProxyInstance.instancesWithSA, expectedProxyInstance.address)
					totalInstances = append(totalInstances, expectedProxyInstance.instancesWithSA...)
				}

				expectServiceInstances(t, sd, selector, 0, totalInstances)
				expectEvents(t, events,
					Event{Type: "eds", ID: selector.Spec.(*networking.ServiceEntry).Hosts[0], Namespace: selector.Namespace, EndpointCount: len(totalInstances)})
			}
		})
	}
}

func expectProxyInstances(t testing.TB, sd *Controller, expected []*model.ServiceInstance, ip string) {
	t.Helper()
	expectProxyTargets(t, sd, slices.Map(expected, model.ServiceInstanceToTarget), ip)
}

func expectProxyTargets(t testing.TB, sd *Controller, expected []model.ServiceTarget, ip string) {
	t.Helper()
	// The system is eventually consistent, so add some retries
	retry.UntilSuccessOrFail(t, func() error {
		instances := sd.GetProxyServiceTargets(&model.Proxy{IPAddresses: []string{ip}, Metadata: &model.NodeMetadata{}})
		sortServiceTargets(instances)
		sortServiceTargets(expected)
		if err := compare(t, instances, expected); err != nil {
			return err
		}
		return nil
	}, retry.Converge(2), retry.Timeout(time.Second*5))
}

func expectEvents(t testing.TB, ch *xdsfake.Updater, events ...Event) {
	t.Helper()
	ch.StrictMatchOrFail(t, events...)
}

func expectServiceInstances(t testing.TB, sd *Controller, cfg *config.Config, port int, expected ...[]*model.ServiceInstance) {
	t.Helper()
	svcs := convertServices(*cfg)
	if len(svcs) != len(expected) {
		t.Fatalf("got more services than expected: %v vs %v", len(svcs), len(expected))
	}
	expe := [][]*model.IstioEndpoint{}
	for _, o := range expected {
		res := []*model.IstioEndpoint{}
		for _, i := range o {
			res = append(res, i.Endpoint)
		}
		expe = append(expe, res)
	}
	// The system is eventually consistent, so add some retries
	retry.UntilSuccessOrFail(t, func() error {
		for i, svc := range svcs {
			endpoints := GetEndpointsForPort(svc, sd.XdsUpdater.(*xdsfake.Updater).Delegate.(*model.EndpointIndexUpdater).Index, port)
			if endpoints == nil {
				endpoints = []*model.IstioEndpoint{} // To simplify tests a bit
			}
			sortEndpoints(endpoints)
			sortEndpoints(expe[i])
			if err := compare(t, endpoints, expe[i]); err != nil {
				return fmt.Errorf("%d: %v", i, err)
			}
		}
		return nil
	}, retry.Converge(2), retry.Timeout(time.Second*1))
}

func TestServiceDiscoveryGetProxyServiceTargets(t *testing.T) {
	store, sd, _ := initServiceDiscovery(t)

	createConfigs([]*config.Config{httpStatic, tcpStatic}, store, t)

	expectProxyInstances(t, sd, []*model.ServiceInstance{
		makeInstance(httpStatic, "2.2.2.2", 7080, httpStatic.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpStatic, "2.2.2.2", 18080, httpStatic.Spec.(*networking.ServiceEntry).Ports[1], nil, MTLS),
		makeInstance(tcpStatic, "2.2.2.2", 444, tcpStatic.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
	}, "2.2.2.2")
}

// Keeping this test for legacy - but it never happens in real life.
func TestServiceDiscoveryInstances(t *testing.T) {
	store, sd, _ := initServiceDiscovery(t)

	createConfigs([]*config.Config{httpDNS, tcpStatic}, store, t)

	expectServiceInstances(t, sd, httpDNS, 0, []*model.ServiceInstance{
		makeInstance(httpDNS, "us.google.com", 7080, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpDNS, "us.google.com", 18080, httpDNS.Spec.(*networking.ServiceEntry).Ports[1], nil, MTLS),
		makeInstance(httpDNS, "uk.google.com", 1080, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpDNS, "uk.google.com", 8080, httpDNS.Spec.(*networking.ServiceEntry).Ports[1], nil, MTLS),
		makeInstance(httpDNS, "de.google.com", 80, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"foo": "bar"}, MTLS),
		makeInstance(httpDNS, "de.google.com", 8080, httpDNS.Spec.(*networking.ServiceEntry).Ports[1], map[string]string{"foo": "bar"}, MTLS),
	})
}

// Keeping this test for legacy - but it never happens in real life.
func TestServiceDiscoveryInstances1Port(t *testing.T) {
	store, sd, _ := initServiceDiscovery(t)

	createConfigs([]*config.Config{httpDNS, tcpStatic}, store, t)

	expectServiceInstances(t, sd, httpDNS, 80, []*model.ServiceInstance{
		makeInstance(httpDNS, "us.google.com", 7080, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpDNS, "uk.google.com", 1080, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpDNS, "de.google.com", 80, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"foo": "bar"}, MTLS),
	})
}

func TestNonServiceConfig(t *testing.T) {
	store, sd, _ := initServiceDiscovery(t)

	// Create a non-service configuration element. This should not affect the service registry at all.
	cfg := config.Config{
		Meta: config.Meta{
			GroupVersionKind:  gvk.DestinationRule,
			Name:              "fakeDestinationRule",
			Namespace:         "default",
			Domain:            "cluster.local",
			CreationTimestamp: GlobalTime,
		},
		Spec: &networking.DestinationRule{
			Host: "fakehost",
		},
	}
	_, err := store.Create(cfg)
	if err != nil {
		t.Errorf("error occurred crearting ServiceEntry config: %v", err)
	}

	// Now create some service entries and verify that it's added to the registry.
	createConfigs([]*config.Config{httpDNS, tcpStatic}, store, t)
	expectServiceInstances(t, sd, httpDNS, 80, []*model.ServiceInstance{
		makeInstance(httpDNS, "us.google.com", 7080, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpDNS, "uk.google.com", 1080, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], nil, MTLS),
		makeInstance(httpDNS, "de.google.com", 80, httpDNS.Spec.(*networking.ServiceEntry).Ports[0], map[string]string{"foo": "bar"}, MTLS),
	})
}

// nolint: lll
func TestServicesDiff(t *testing.T) {
	updatedHTTPDNS := &config.Config{
		Meta: config.Meta{
			GroupVersionKind:  gvk.ServiceEntry,
			Name:              "httpDNS",
			Namespace:         "httpDNS",
			CreationTimestamp: GlobalTime,
			Labels:            map[string]string{label.SecurityTlsMode.Name: model.IstioMutualTLSModeLabel},
		},
		Spec: &networking.ServiceEntry{
			Hosts: []string{"*.google.com", "*.mail.com"},
			Ports: []*networking.ServicePort{
				{Number: 80, Name: "http-port", Protocol: "http"},
				{Number: 8080, Name: "http-alt-port", Protocol: "http"},
			},
			Endpoints: []*networking.WorkloadEntry{
				{
					Address: "us.google.com",
					Ports:   map[string]uint32{"http-port": 7080, "http-alt-port": 18080},
					Labels:  map[string]string{label.SecurityTlsMode.Name: model.IstioMutualTLSModeLabel},
				},
				{
					Address: "uk.google.com",
					Ports:   map[string]uint32{"http-port": 1080},
					Labels:  map[string]string{label.SecurityTlsMode.Name: model.IstioMutualTLSModeLabel},
				},
				{
					Address: "de.google.com",
					Labels:  map[string]string{"foo": "bar", label.SecurityTlsMode.Name: model.IstioMutualTLSModeLabel},
				},
			},
			Location:   networking.ServiceEntry_MESH_EXTERNAL,
			Resolution: networking.ServiceEntry_DNS,
		},
	}

	updatedHTTPDNSPort := func() *config.Config {
		c := updatedHTTPDNS.DeepCopy()
		se := c.Spec.(*networking.ServiceEntry)
		var ports []*networking.ServicePort
		ports = append(ports, se.Ports...)
		ports = append(ports, &networking.ServicePort{Number: 9090, Name: "http-new-port", Protocol: "http"})
		se.Ports = ports
		return &c
	}()

	updatedEndpoint := func() *config.Config {
		c := updatedHTTPDNS.DeepCopy()
		se := c.Spec.(*networking.ServiceEntry)
		var endpoints []*networking.WorkloadEntry
		endpoints = append(endpoints, se.Endpoints...)
		endpoints = append(endpoints, &networking.WorkloadEntry{
			Address: "in.google.com",
			Labels:  map[string]string{"foo": "bar", label.SecurityTlsMode.Name: model.IstioMutualTLSModeLabel},
		})
		se.Endpoints = endpoints
		return &c
	}()

	stringsToHosts := func(hosts []string) []host.Name {
		ret := make([]host.Name, len(hosts))
		for i, hostname := range hosts {
			ret[i] = host.Name(hostname)
		}
		return ret
	}

	cases := []struct {
		name    string
		current *config.Config
		new     *config.Config

		added     []host.Name
		deleted   []host.Name
		updated   []host.Name
		unchanged []host.Name
	}{
		{
			name:      "same config",
			current:   updatedHTTPDNS,
			new:       updatedHTTPDNS,
			unchanged: stringsToHosts(updatedHTTPDNS.Spec.(*networking.ServiceEntry).Hosts),
		},
		{
			name:    "same config with different name",
			current: updatedHTTPDNS,
			new: func() *config.Config {
				c := updatedHTTPDNS.DeepCopy()
				c.Name = "httpDNS1"
				return &c
			}(),
			unchanged: stringsToHosts(updatedHTTPDNS.Spec.(*networking.ServiceEntry).Hosts),
		},
		{
			name:    "different resolution",
			current: updatedHTTPDNS,
			new: func() *config.Config {
				c := updatedHTTPDNS.DeepCopy()
				c.Spec.(*networking.ServiceEntry).Resolution = networking.ServiceEntry_NONE
				return &c
			}(),
			updated: stringsToHosts(updatedHTTPDNS.Spec.(*networking.ServiceEntry).Hosts),
		},
		{
			name:    "config modified with added/deleted host",
			current: updatedHTTPDNS,
			new: func() *config.Config {
				c := updatedHTTPDNS.DeepCopy()
				se := c.Spec.(*networking.ServiceEntry)
				se.Hosts = []string{"*.google.com", "host.com"}
				return &c
			}(),
			added:     []host.Name{"host.com"},
			deleted:   []host.Name{"*.mail.com"},
			unchanged: []host.Name{"*.google.com"},
		},
		{
			name:    "config modified with additional port",
			current: updatedHTTPDNS,
			new:     updatedHTTPDNSPort,
			updated: stringsToHosts(updatedHTTPDNS.Spec.(*networking.ServiceEntry).Hosts),
		},
		{
			name:      "same config with additional endpoint",
			current:   updatedHTTPDNS,
			new:       updatedEndpoint,
			unchanged: stringsToHosts(updatedHTTPDNS.Spec.(*networking.ServiceEntry).Hosts),
		},
	}

	servicesHostnames := func(services []*model.Service) []host.Name {
		if len(services) == 0 {
			return nil
		}
		ret := make([]host.Name, len(services))
		for i, svc := range services {
			ret[i] = svc.Hostname
		}
		return ret
	}

	for _, tt := range cases {
		if tt.name != "same config with additional endpoint" {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			as := convertServices(*tt.current)
			bs := convertServices(*tt.new)
			added, deleted, updated, unchanged := servicesDiff(as, bs)
			for i, item := range []struct {
				hostnames []host.Name
				services  []*model.Service
			}{
				{tt.added, added},
				{tt.deleted, deleted},
				{tt.updated, updated},
				{tt.unchanged, unchanged},
			} {
				if !reflect.DeepEqual(servicesHostnames(item.services), item.hostnames) {
					t.Errorf("ServicesChanged %d got %v, want %v", i, servicesHostnames(item.services), item.hostnames)
				}
			}
		})
	}
}

func sortServices(services []*model.Service) {
	sort.Slice(services, func(i, j int) bool { return services[i].Hostname < services[j].Hostname })
	for _, service := range services {
		sortPorts(service.Ports)
	}
}

func sortServiceTargets(instances []model.ServiceTarget) {
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Service.Hostname == instances[j].Service.Hostname {
			if instances[i].Port.TargetPort == instances[j].Port.TargetPort {
				return instances[i].Port.TargetPort < instances[j].Port.TargetPort
			}
		}
		return instances[i].Service.Hostname < instances[j].Service.Hostname
	})
}

func sortServiceInstances(instances []*model.ServiceInstance) {
	labelsToSlice := func(labels labels.Instance) []string {
		out := make([]string, 0, len(labels))
		for k, v := range labels {
			out = append(out, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(out)
		return out
	}

	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Service.Hostname == instances[j].Service.Hostname {
			if instances[i].Endpoint.EndpointPort == instances[j].Endpoint.EndpointPort {
				if instances[i].Endpoint.Address == instances[j].Endpoint.Address {
					if len(instances[i].Endpoint.Labels) == len(instances[j].Endpoint.Labels) {
						iLabels := labelsToSlice(instances[i].Endpoint.Labels)
						jLabels := labelsToSlice(instances[j].Endpoint.Labels)
						for k := range iLabels {
							if iLabels[k] < jLabels[k] {
								return true
							}
						}
					}
					return len(instances[i].Endpoint.Labels) < len(instances[j].Endpoint.Labels)
				}
				return instances[i].Endpoint.Address < instances[j].Endpoint.Address
			}
			return instances[i].Endpoint.EndpointPort < instances[j].Endpoint.EndpointPort
		}
		return instances[i].Service.Hostname < instances[j].Service.Hostname
	})
}

func sortEndpoints(endpoints []*model.IstioEndpoint) {
	labelsToSlice := func(labels labels.Instance) []string {
		out := make([]string, 0, len(labels))
		for k, v := range labels {
			out = append(out, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(out)
		return out
	}

	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].EndpointPort == endpoints[j].EndpointPort {
			if endpoints[i].Address == endpoints[j].Address {
				if len(endpoints[i].Labels) == len(endpoints[j].Labels) {
					iLabels := labelsToSlice(endpoints[i].Labels)
					jLabels := labelsToSlice(endpoints[j].Labels)
					for k := range iLabels {
						if iLabels[k] < jLabels[k] {
							return true
						}
					}
				}
				return len(endpoints[i].Labels) < len(endpoints[j].Labels)
			}
			return endpoints[i].Address < endpoints[j].Address
		}
		return endpoints[i].EndpointPort < endpoints[j].EndpointPort
	})
}

func sortPorts(ports []*model.Port) {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port == ports[j].Port {
			if ports[i].Name == ports[j].Name {
				return ports[i].Protocol < ports[j].Protocol
			}
			return ports[i].Name < ports[j].Name
		}
		return ports[i].Port < ports[j].Port
	})
}

func Test_autoAllocateIP_conditions(t *testing.T) {
	tests := []struct {
		name         string
		inServices   []*model.Service
		wantServices []*model.Service
	}{
		{
			name: "no allocation for passthrough",
			inServices: []*model.Service{
				{
					Hostname:       "foo.com",
					Resolution:     model.Passthrough,
					DefaultAddress: "0.0.0.0",
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:       "foo.com",
					Resolution:     model.Passthrough,
					DefaultAddress: "0.0.0.0",
				},
			},
		},
		{
			name: "no allocation if address exists",
			inServices: []*model.Service{
				{
					Hostname:       "foo.com",
					Resolution:     model.ClientSideLB,
					DefaultAddress: "1.1.1.1",
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:       "foo.com",
					Resolution:     model.ClientSideLB,
					DefaultAddress: "1.1.1.1",
				},
			},
		},
		{
			name: "no allocation if hostname is wildcard",
			inServices: []*model.Service{
				{
					Hostname:       "*.foo.com",
					Resolution:     model.ClientSideLB,
					DefaultAddress: "1.1.1.1",
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:       "*.foo.com",
					Resolution:     model.ClientSideLB,
					DefaultAddress: "1.1.1.1",
				},
			},
		},
		{
			name: "allocate IP for clientside lb",
			inServices: []*model.Service{
				{
					Hostname:       "foo.com",
					Resolution:     model.ClientSideLB,
					DefaultAddress: "0.0.0.0",
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:                 "foo.com",
					Resolution:               model.ClientSideLB,
					DefaultAddress:           "0.0.0.0",
					AutoAllocatedIPv4Address: "240.240.62.90",
					AutoAllocatedIPv6Address: "2001:2::f0f0:3e5a",
				},
			},
		},
		{
			name: "allocate IP for dns lb",
			inServices: []*model.Service{
				{
					Hostname:       "foo.com",
					Resolution:     model.DNSLB,
					DefaultAddress: "0.0.0.0",
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:                 "foo.com",
					Resolution:               model.DNSLB,
					DefaultAddress:           "0.0.0.0",
					AutoAllocatedIPv4Address: "240.240.62.90",
					AutoAllocatedIPv6Address: "2001:2::f0f0:3e5a",
				},
			},
		},
		{
			name: "collision",
			inServices: []*model.Service{
				{
					Hostname:       "a17061.example.com",
					Resolution:     model.DNSLB,
					DefaultAddress: "0.0.0.0",
				},
				{
					// hashes to the same value as the hostname above,
					// a new collision needs to be found if the hash algorithm changes
					Hostname:       "a44155.example.com",
					Resolution:     model.DNSLB,
					DefaultAddress: "0.0.0.0",
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:                 "a17061.example.com",
					Resolution:               model.DNSLB,
					DefaultAddress:           "0.0.0.0",
					AutoAllocatedIPv4Address: "240.240.0.1",
					AutoAllocatedIPv6Address: "2001:2::f0f0:1",
				},
				{
					Hostname:                 "a44155.example.com",
					Resolution:               model.DNSLB,
					DefaultAddress:           "0.0.0.0",
					AutoAllocatedIPv4Address: "240.240.75.79",
					AutoAllocatedIPv6Address: "2001:2::f0f0:4b4f",
				},
			},
		},
		{
			name: "stable IP - baseline test",
			inServices: []*model.Service{
				{
					Hostname:       "a.example.com",
					Resolution:     model.DNSLB,
					DefaultAddress: "0.0.0.0",
					Attributes:     model.ServiceAttributes{Namespace: "a"},
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:                 "a.example.com",
					Resolution:               model.DNSLB,
					DefaultAddress:           "0.0.0.0",
					AutoAllocatedIPv4Address: "240.240.163.38",
					AutoAllocatedIPv6Address: "2001:2::f0f0:a326",
				},
			},
		},
		{
			name: "stable IP - not affected by other namespace",
			inServices: []*model.Service{
				{
					Hostname:       "a.example.com",
					Resolution:     model.DNSLB,
					DefaultAddress: "0.0.0.0",
					Attributes:     model.ServiceAttributes{Namespace: "a"},
				},
				{
					Hostname:       "a.example.com",
					Resolution:     model.DNSLB,
					DefaultAddress: "0.0.0.0",
					Attributes:     model.ServiceAttributes{Namespace: "b"},
				},
			},
			wantServices: []*model.Service{
				{
					Hostname:                 "a.example.com",
					Resolution:               model.DNSLB,
					DefaultAddress:           "0.0.0.0",
					AutoAllocatedIPv4Address: "240.240.163.38",
					AutoAllocatedIPv6Address: "2001:2::f0f0:a326",
				},
				{
					Hostname:                 "a.example.com",
					Resolution:               model.DNSLB,
					DefaultAddress:           "0.0.0.0",
					AutoAllocatedIPv4Address: "240.240.114.198",
					AutoAllocatedIPv6Address: "2001:2::f0f0:72c6",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotServices := autoAllocateIPs(tt.inServices)
			for i, got := range gotServices {
				if got.AutoAllocatedIPv4Address != tt.wantServices[i].AutoAllocatedIPv4Address {
					t.Errorf("autoAllocateIPs() AutoAllocatedIPv4Address = %v, want %v",
						got.AutoAllocatedIPv4Address, tt.wantServices[i].AutoAllocatedIPv4Address)
				}
				if got.AutoAllocatedIPv6Address != tt.wantServices[i].AutoAllocatedIPv6Address {
					t.Errorf("autoAllocateIPs() AutoAllocatedIPv4Address = %v, want %v",
						got.AutoAllocatedIPv6Address, tt.wantServices[i].AutoAllocatedIPv6Address)
				}
			}
		})
	}
}

func Test_autoAllocateIP_values(t *testing.T) {
	inServices := make([]*model.Service, 255*255)
	for i := 0; i < 255*255; i++ {
		temp := model.Service{
			Hostname:       host.Name(fmt.Sprintf("foo%d.com", i)),
			Resolution:     model.ClientSideLB,
			DefaultAddress: constants.UnspecifiedIP,
		}
		inServices[i] = &temp
	}
	gotServices := autoAllocateIPs(inServices)

	// We dont expect the following pattern of IPs.
	// 240.240.0.0
	// 240.240.0.255
	// 240.240.1.0
	// 240.240.1.255
	// 240.240.2.0
	// 240.240.2.255
	// 240.240.3.0
	// 240.240.3.255
	// The last IP should be 240.240.202.167
	doNotWant := map[string]bool{
		"240.240.0.0":   true,
		"240.240.0.255": true,
		"240.240.1.0":   true,
		"240.240.1.255": true,
		"240.240.2.0":   true,
		"240.240.2.255": true,
	}
	expectedLastIP := "240.240.202.167"
	if gotServices[len(gotServices)-1].AutoAllocatedIPv4Address != expectedLastIP {
		t.Errorf("expected last IP address to be %s, got %s", expectedLastIP, gotServices[len(gotServices)-1].AutoAllocatedIPv4Address)
	}

	gotIPMap := make(map[string]string)
	for _, svc := range gotServices {
		if svc.AutoAllocatedIPv4Address == "" || doNotWant[svc.AutoAllocatedIPv4Address] {
			t.Errorf("unexpected value for auto allocated IP address %s", svc.AutoAllocatedIPv4Address)
		}
		if v, ok := gotIPMap[svc.AutoAllocatedIPv4Address]; ok && v != svc.Hostname.String() {
			t.Errorf("multiple allocations of same IP address to different services with different hostname: %s", svc.AutoAllocatedIPv4Address)
		}
		gotIPMap[svc.AutoAllocatedIPv4Address] = svc.Hostname.String()
	}
}

func BenchmarkAutoAllocateIPs(t *testing.B) {
	inServices := make([]*model.Service, 255*255)
	for i := 0; i < 255*255; i++ {
		temp := model.Service{
			Hostname:       host.Name(fmt.Sprintf("foo%d.com", i)),
			Resolution:     model.ClientSideLB,
			DefaultAddress: constants.UnspecifiedIP,
		}
		inServices[i] = &temp
	}
	t.ResetTimer()
	for i := 0; i < t.N; i++ {
		autoAllocateIPs(inServices)
	}
}

// Validate that ipaddress allocation is deterministic based on hash.
func Test_autoAllocateIP_deterministic(t *testing.T) {
	inServices := make([]*model.Service, 0)
	originalServices := map[string]string{
		"a.com": "240.240.81.186",
		"c.com": "240.240.79.99",
		"e.com": "240.240.175.33",
		"g.com": "240.240.106.30",
		"i.com": "240.240.124.21",
		"k.com": "240.240.234.190",
		"l.com": "240.240.142.221",
		"n.com": "240.240.41.17",
		"o.com": "240.240.31.228",
	}

	allocateAndValidate := func() {
		gotServices := autoAllocateIPs(model.SortServicesByCreationTime(inServices))
		gotIPMap := make(map[string]string)
		for _, svc := range gotServices {
			if v, ok := gotIPMap[svc.AutoAllocatedIPv4Address]; ok && v != svc.Hostname.String() {
				t.Errorf("multiple allocations of same IP address to different services with different hostname: %s", svc.AutoAllocatedIPv4Address)
			}
			gotIPMap[svc.AutoAllocatedIPv4Address] = svc.Hostname.String()
		}
		for k, v := range originalServices {
			if gotIPMap[v] != k {
				t.Errorf("ipaddress changed for service %s. expected: %s, got: %s", k, v, gotIPMap[v])
			}
		}
	}

	// Validate that IP addresses are allocated for original list of services.
	for k := range originalServices {
		inServices = append(inServices, &model.Service{
			Hostname:       host.Name(k),
			Resolution:     model.ClientSideLB,
			DefaultAddress: constants.UnspecifiedIP,
		})
	}
	allocateAndValidate()

	// Now add few services in between and validate that IPs are retained for original services.
	addServices := map[string]bool{
		"b.com": true,
		"d.com": true,
		"f.com": true,
		"h.com": true,
		"j.com": true,
		"m.com": true,
		"p.com": true,
		"q.com": true,
		"r.com": true,
	}

	for k := range addServices {
		inServices = append(inServices, &model.Service{
			Hostname:       host.Name(k),
			Resolution:     model.ClientSideLB,
			DefaultAddress: constants.UnspecifiedIP,
		})
	}
	allocateAndValidate()

	// Now delete few services and validate that IPs are retained for original services.
	deleteServices := []*model.Service{}
	for i, svc := range inServices {
		if _, exists := originalServices[svc.Hostname.String()]; !exists {
			if i%2 == 0 {
				continue
			}
		}
		deleteServices = append(deleteServices, svc)
	}
	inServices = deleteServices
	allocateAndValidate()
}

func TestWorkloadEntryOnlyMode(t *testing.T) {
	store, registry, _ := initServiceDiscoveryWithOpts(t, true)
	createConfigs([]*config.Config{httpStatic}, store, t)
	svcs := registry.Services()
	if len(svcs) > 0 {
		t.Fatalf("expected 0 services, got %d", len(svcs))
	}
	svc := registry.GetService("*.google.com")
	if svc != nil {
		t.Fatalf("expected nil, got %v", svc)
	}
}

func BenchmarkServiceEntryHandler(b *testing.B) {
	_, sd := initServiceDiscoveryWithoutEvents(b)
	stopCh := make(chan struct{})
	go sd.Run(stopCh)
	defer close(stopCh)
	for i := 0; i < b.N; i++ {
		sd.serviceEntryHandler(config.Config{}, *httpDNS, model.EventAdd)
		sd.serviceEntryHandler(config.Config{}, *httpDNSRR, model.EventAdd)
		sd.serviceEntryHandler(config.Config{}, *tcpDNS, model.EventAdd)
		sd.serviceEntryHandler(config.Config{}, *tcpStatic, model.EventAdd)

		sd.serviceEntryHandler(config.Config{}, *httpDNS, model.EventDelete)
		sd.serviceEntryHandler(config.Config{}, *httpDNSRR, model.EventDelete)
		sd.serviceEntryHandler(config.Config{}, *tcpDNS, model.EventDelete)
		sd.serviceEntryHandler(config.Config{}, *tcpStatic, model.EventDelete)
	}
}

func BenchmarkWorkloadInstanceHandler(b *testing.B) {
	store, sd := initServiceDiscoveryWithoutEvents(b)
	stopCh := make(chan struct{})
	go sd.Run(stopCh)
	defer close(stopCh)
	// Add just the ServiceEntry with selector. We should see no instances
	createConfigs([]*config.Config{selector, dnsSelector}, store, b)

	// Setup a couple of workload instances for test. These will be selected by the `selector` SE
	fi1 := &model.WorkloadInstance{
		Name:      selector.Name,
		Namespace: selector.Namespace,
		Endpoint: &model.IstioEndpoint{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(selector.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}

	fi2 := &model.WorkloadInstance{
		Name:      "some-other-name",
		Namespace: selector.Namespace,
		Endpoint: &model.IstioEndpoint{
			Address:        "3.3.3.3",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(selector.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}

	fi3 := &model.WorkloadInstance{
		Name:      "another-name",
		Namespace: dnsSelector.Namespace,
		Endpoint: &model.IstioEndpoint{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "dns-wle"},
			ServiceAccount: spiffe.MustGenSpiffeURI(dnsSelector.Name, "default"),
			TLSMode:        model.IstioMutualTLSModeLabel,
		},
	}
	for i := 0; i < b.N; i++ {
		sd.WorkloadInstanceHandler(fi1, model.EventAdd)
		sd.WorkloadInstanceHandler(fi2, model.EventAdd)
		sd.WorkloadInstanceHandler(fi3, model.EventDelete)

		sd.WorkloadInstanceHandler(fi2, model.EventDelete)
		sd.WorkloadInstanceHandler(fi1, model.EventDelete)
		sd.WorkloadInstanceHandler(fi3, model.EventDelete)
	}
}

func BenchmarkWorkloadEntryHandler(b *testing.B) {
	// Setup a couple workload entries for test. These will be selected by the `selector` SE
	wle := createWorkloadEntry("wl", selector.Name,
		&networking.WorkloadEntry{
			Address:        "2.2.2.2",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})
	wle2 := createWorkloadEntry("wl2", selector.Name,
		&networking.WorkloadEntry{
			Address:        "3.3.3.3",
			Labels:         map[string]string{"app": "wle"},
			ServiceAccount: "default",
		})
	dnsWle := createWorkloadEntry("dnswl", dnsSelector.Namespace,
		&networking.WorkloadEntry{
			Address:        "4.4.4.4",
			Labels:         map[string]string{"app": "dns-wle"},
			ServiceAccount: "default",
		})

	store, sd := initServiceDiscoveryWithoutEvents(b)
	stopCh := make(chan struct{})
	go sd.Run(stopCh)
	defer close(stopCh)
	// Add just the ServiceEntry with selector. We should see no instances
	createConfigs([]*config.Config{selector}, store, b)

	for i := 0; i < b.N; i++ {
		sd.workloadEntryHandler(config.Config{}, *wle, model.EventAdd)
		sd.workloadEntryHandler(config.Config{}, *dnsWle, model.EventAdd)
		sd.workloadEntryHandler(config.Config{}, *wle2, model.EventAdd)

		sd.workloadEntryHandler(config.Config{}, *wle, model.EventDelete)
		sd.workloadEntryHandler(config.Config{}, *dnsWle, model.EventDelete)
		sd.workloadEntryHandler(config.Config{}, *wle2, model.EventDelete)
	}
}

func GetEndpoints(s *model.Service, endpoints *model.EndpointIndex) []*model.IstioEndpoint {
	return GetEndpointsForPort(s, endpoints, 0)
}

func GetEndpointsForPort(s *model.Service, endpoints *model.EndpointIndex, port int) []*model.IstioEndpoint {
	shards, ok := endpoints.ShardsForService(string(s.Hostname), s.Attributes.Namespace)
	if !ok {
		return nil
	}
	var pn string
	for _, p := range s.Ports {
		if p.Port == port {
			pn = p.Name
			break
		}
	}
	if pn == "" && port != 0 {
		return nil
	}
	shards.RLock()
	defer shards.RUnlock()
	return slices.FilterInPlace(slices.Flatten(maps.Values(shards.Shards)), func(endpoint *model.IstioEndpoint) bool {
		return pn == "" || endpoint.ServicePortName == pn
	})
}
