package manager

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	log "log/slog"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	svcs "github.com/kube-vip/kube-vip/pkg/services"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// services keeps track of services that already were processed
var svcMap sync.Map

// This function handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) servicesWatcher(ctx context.Context, serviceFunc func(context.Context, *v1.Service) error) error {
	// first start port mirroring if enabled
	if err := sm.startTrafficMirroringIfEnabled(); err != nil {
		return err
	}
	defer func() {
		// clean up traffic mirror related config
		err := sm.stopTrafficMirroringIfEnabled()
		if err != nil {
			log.Error("Stopping traffic mirroring", "err", err)
		}
	}()

	if sm.config.ServiceNamespace == "" {
		// v1.NamespaceAll is actually "", but we'll stay with the const in case things change upstream
		sm.config.ServiceNamespace = v1.NamespaceAll
		log.Info("(svcs) starting services watcher for all namespaces")
	} else {
		log.Info("(svcs) starting services watcher", "namespace", sm.config.ServiceNamespace)
	}

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return sm.rwClientSet.CoreV1().Services(sm.config.ServiceNamespace).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}
	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-sm.shutdownChan:
			log.Debug("(svcs) shutdown called")
			// Stop the retry watcher
			rw.Stop()
			return
		case <-exitFunction:
			log.Debug("(svcs) function ending")
			// Stop the retry watcher
			rw.Stop()
			return
		}
	}()
	ch := rw.ResultChan()

	// Used for tracking an active endpoint / pod
	for event := range ch {
		sm.countServiceWatchEvent.With(prometheus.Labels{"type": string(event.Type)}).Add(1)

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes services from API watcher")
			}

			// We only care about LoadBalancer services
			if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
				break
			}

			// Check if we ignore this service
			if svc.Annotations["kube-vip.io/ignore"] == "true" {
				log.Info("ignore annotation for kube-vip", "service name", svc.Name)
				break
			}

			// Select loadbalancer class filtering function
			lbClassFilterFunc := sm.lbClassFilter
			if sm.config.LoadBalancerClassLegacyHandling {
				lbClassFilterFunc = sm.lbClassFilterLegacy
			}

			// Check the loadBalancer class
			if lbClassFilterFunc(svc) {
				break
			}

			svcAddresses := svcs.FetchServiceAddresses(svc)

			// We only care about LoadBalancer services that have been allocated an address
			if len(svcAddresses) <= 0 {
				break
			}

			svcCtx, err := getServiceContext(svc.UID)
			if err != nil {
				return fmt.Errorf("failed to get service context: %w", err)
			}

			// The modified event should only be triggered if the service has been modified (i.e. moved somewhere else)
			if event.Type == watch.Modified {
				i := sm.findServiceInstance(svc)
				var originalService []string
				shouldGarbageCollect := true
				if i != nil {
					originalService = svcs.FetchServiceAddresses(i.ServiceSnapshot)
					shouldGarbageCollect = !reflect.DeepEqual(originalService, svcAddresses)
				}
				if shouldGarbageCollect {
					for _, addr := range svcAddresses {
						// log.Debugf("(svcs) Retreiving local addresses, to ensure that this modified address doesn't exist: %s", addr)
						f, err := vip.GarbageCollect(sm.config.Interface, addr, sm.intfMgr)
						if err != nil {
							log.Error("(svcs) cleaning existing address error", "err", err)
						}
						if f {
							log.Warn("(svcs) already found existing config", "address", addr, "adapter", sm.config.Interface)
						}
					}
				}
				// This service has been modified, but it was also active.
				if svcCtx != nil && svcCtx.IsActive {
					if i != nil {
						originalService := svcs.FetchServiceAddresses(i.ServiceSnapshot)
						newService := svcs.FetchServiceAddresses(svc)
						if !reflect.DeepEqual(originalService, newService) {

							// Calls the cancel function of the context
							if svcCtx != nil {
								log.Warn("(svcs) The load balancer has changed, cancelling original load balancer")
								svcCtx.Cancel()
								log.Warn("(svcs) waiting for load balancer to finish")
								<-svcCtx.Ctx.Done()
							}

							err = sm.deleteService(svc.UID)
							if err != nil {
								log.Error("(svc) unable to remove", "service", svc.UID)
							}

							svcMap.Delete(svc.UID)
						}
						// in theory this should never fail
					}
				}
			}

			// Architecture walkthrough: (Had to do this as this code path is making my head hurt)

			// Is the service active (bool), if not then process this new service
			// Does this service use an election per service?
			//

			if svcCtx == nil || svcCtx != nil && !svcCtx.IsActive {
				log.Debug("(svcs) has been added/modified with addresses", "service name", svc.Name, "ip", svcs.FetchServiceAddresses(svc))

				if svcCtx == nil {
					svcCtx = svcs.NewContext(ctx)
					svcMap.Store(svc.UID, svcCtx)
				}

				if sm.config.EnableServicesElection || // Service Election
					((sm.config.EnableRoutingTable || sm.config.EnableBGP) && // Routing table mode or BGP
						(!sm.config.EnableLeaderElection && !sm.config.EnableServicesElection)) { // No leaderelection or services election

					// If this load balancer Traffic Policy is "local"
					if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {

						// Start an endpoint watcher if we're not watching it already
						if !svcCtx.IsWatched {
							// background the endpoint watcher
							if (sm.config.EnableRoutingTable || sm.config.EnableBGP) && (!sm.config.EnableLeaderElection && !sm.config.EnableServicesElection) {
								err = serviceFunc(svcCtx.Ctx, svc)
								if err != nil {
									log.Error(err.Error())
								}
							}

							go func() {
								if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
									// Add Endpoint or EndpointSlices watcher
									var provider providers.Provider
									if !sm.config.EnableEndpointSlices {
										provider = providers.NewEndpoints()
									} else {
										provider = providers.NewEndpointslices()
									}
									if err = sm.watchEndpoint(svcCtx, sm.config.NodeName, svc, provider); err != nil {
										log.Error(err.Error())
									}
								}
							}()

							// We're now watching this service
							svcCtx.IsWatched = true
						}
					} else if (sm.config.EnableBGP || sm.config.EnableRoutingTable) && (!sm.config.EnableLeaderElection && !sm.config.EnableServicesElection) {
						err = serviceFunc(svcCtx.Ctx, svc)
						if err != nil {
							log.Error(err.Error())
						}

						go func() {
							if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
								// Add Endpoint watcher
								var provider providers.Provider
								if !sm.config.EnableEndpointSlices {
									provider = providers.NewEndpoints()
								} else {
									provider = providers.NewEndpointslices()
								}
								if err = sm.watchEndpoint(svcCtx, sm.config.NodeName, svc, provider); err != nil {
									log.Error(err.Error())
								}
							}
						}()
						// We're now watching this service
						svcCtx.IsWatched = true
					} else {

						go func() {
							for {
								select {
								case <-svcCtx.Ctx.Done():
									log.Warn("(svcs) restartable service watcher ending", "uid", svc.UID)
									return
								default:
									log.Info("(svcs) restartable service watcher starting", "uid", svc.UID)
									err = serviceFunc(svcCtx.Ctx, svc)

									if err != nil {
										log.Error(err.Error())
									}
								}
							}

						}()
					}
				} else {
					// Increment the waitGroup before the service Func is called (Done is completed in there)
					err = serviceFunc(svcCtx.Ctx, svc)
					if err != nil {
						log.Error(err.Error())
					}
				}
				svcCtx.IsActive = true
			}
		case watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes services from API watcher")
			}
			svcCtx, err := getServiceContext(svc.UID)
			if err != nil {
				return fmt.Errorf("(svcs) unable to get context: %w", err)
			}
			if svcCtx != nil && svcCtx.IsActive {
				// We only care about LoadBalancer services
				if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
					break
				}

				// We can ignore this service
				if svc.Annotations["kube-vip.io/ignore"] == "true" {
					log.Info("(svcs)ignore annotation for kube-vip", "service name", svc.Name)
					break
				}

				// If no leader election is enabled, delete routes here
				if !sm.config.EnableLeaderElection && !sm.config.EnableServicesElection &&
					sm.config.EnableRoutingTable && svcCtx.HasConfiguredNetworks() {
					if errs := sm.clearRoutes(svc); len(errs) == 0 {
						svcCtx.ConfiguredNetworks.Clear()
					}
				}

				// If this is an active service then and additional leaderElection will handle stopping
				err = sm.deleteService(svc.UID)
				if err != nil {
					log.Error(err.Error())
				}

				// Calls the cancel function of the context
				log.Warn("(svcs) The load balancer was deleted, cancelling context")
				svcCtx.Cancel()
				log.Warn("(svcs) waiting for load balancer to finish")
				<-svcCtx.Ctx.Done()
				svcMap.Delete(svc.UID)
			}

			if sm.config.EnableLeaderElection && !sm.config.EnableServicesElection {
				if sm.config.EnableBGP {
					sm.clearBGPHosts(svc)
				} else if sm.config.EnableRoutingTable {
					sm.clearRoutes(svc)
				}
			}

			log.Info("(svcs) deleted", "service name", svc.Name, "namespace", svc.Namespace)
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Error(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
			}

			status := statusErr.ErrStatus
			log.Error("services", "err", status)
		default:
		}
	}
	close(exitFunction)
	log.Warn("Stopping watching services for type: LoadBalancer in all namespaces")
	return nil
}

func (sm *Manager) lbClassFilterLegacy(svc *v1.Service) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass != nil {
		// if this isn't nil then it has been configured, check if it the kube-vip loadBalancer class
		if *svc.Spec.LoadBalancerClass != sm.config.LoadBalancerClassName {
			log.Info("(svcs) specified the wrong loadBalancer class", "service name", svc.Name, "lbClass", *svc.Spec.LoadBalancerClass)
			return true
		}
	} else if sm.config.LoadBalancerClassOnly {
		// if kube-vip is configured to only recognize services with kube-vip's lb class, then ignore the services without any lb class
		log.Info("(svcs) kube-vip configured to only recognize services with kube-vip's lb class but the service didn't specify any loadBalancer class, ignoring", "service name", svc.Name)
		return true
	}
	return false
}

func (sm *Manager) lbClassFilter(svc *v1.Service) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && sm.config.LoadBalancerClassName != "" {
		log.Info("(svcs) no loadBalancer class, ignoring", "service name", svc.Name, "expected lbClass", sm.config.LoadBalancerClassName)
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && sm.config.LoadBalancerClassName == "" {
		return false
	}
	if *svc.Spec.LoadBalancerClass != sm.config.LoadBalancerClassName {
		log.Info("(svcs) specified wrong loadBalancer class, ignoring", "service name", svc.Name, "wrong lbClass", *svc.Spec.LoadBalancerClass, "expected lbClass", sm.config.LoadBalancerClassName)
		return true
	}
	return false
}

func getServiceContext(uid types.UID) (*svcs.Context, error) {
	svcCtx, ok := svcMap.Load(uid)
	if !ok {
		return nil, nil
	}
	ctx, ok := svcCtx.(*svcs.Context)
	if !ok {
		return nil, fmt.Errorf("failed to cast service context pointer - UID: %s", uid)
	}
	return ctx, nil
}
