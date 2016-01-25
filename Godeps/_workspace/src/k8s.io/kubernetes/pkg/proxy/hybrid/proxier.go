package hybrid

import (
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/proxy/iptables"
	"k8s.io/kubernetes/pkg/proxy/userspace"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/unidling"

	"github.com/golang/glog"
)

type Proxier struct {
	userspaceProxy        *userspace.Proxier
	userspaceLoadBalancer config.EndpointsConfigHandler
	iptablesProxy         *iptables.Proxier

	serviceConfig *config.ServiceConfig

	// TODO(directxman12): figure out a good way to avoid duplicating this information
	// (it's saved in the individual proxies as well)
	usingUserspace map[types.NamespacedName]bool

	syncPeriod time.Duration
}

func NewProxier(loadBalancer config.EndpointsConfigHandler, iptablesProxy *iptables.Proxier, userspaceProxy *userspace.Proxier, syncPeriod time.Duration, serviceConfig *config.ServiceConfig) (*Proxier, error) {
	return &Proxier{
		userspaceProxy:        userspaceProxy,
		iptablesProxy:         iptablesProxy,
		userspaceLoadBalancer: loadBalancer,

		serviceConfig: serviceConfig,

		usingUserspace: nil,

		syncPeriod: syncPeriod,
	}, nil
}

func (p *Proxier) OnServiceUpdate(services []api.Service) {
	forIPTables := make([]api.Service, 0, len(services))
	forUserspace := []api.Service{}

	for _, service := range services {
		if !api.IsServiceIPSet(&service) {
			// Skip service with no ClusterIP set
			continue
		}
		svcName := types.NamespacedName{
			Namespace: service.Namespace,
			Name:      service.Name,
		}
		if _, ok := p.usingUserspace[svcName]; ok {
			forUserspace = append(forUserspace, service)
		} else {
			forIPTables = append(forIPTables, service)
		}
	}

	p.userspaceProxy.OnServiceUpdate(forUserspace)
	p.iptablesProxy.OnServiceUpdate(forIPTables)
}

func (p *Proxier) updateUsingUserspace(endpoints []api.Endpoints) {
	p.usingUserspace = make(map[types.NamespacedName]bool, len(endpoints))
	for _, endpoint := range endpoints {
		hasEndpoints := false
		for _, subset := range endpoint.Subsets {
			if len(subset.Addresses) > 0 {
				hasEndpoints = true
				break
			}
		}

		if !hasEndpoints {
			if _, ok := endpoint.Annotations[unidling.IdledAtAnnotation]; ok {
				svcName := types.NamespacedName{
					Namespace: endpoint.Namespace,
					Name:      endpoint.Name,
				}
				p.usingUserspace[svcName] = true
			}
		}
	}
}

func (p *Proxier) OnEndpointsUpdate(endpoints []api.Endpoints) {
	p.updateUsingUserspace(endpoints)

	forIPTables := []api.Endpoints{}

	for _, endpoint := range endpoints {
		svcName := types.NamespacedName{
			Namespace: endpoint.Namespace,
			Name:      endpoint.Name,
		}
		if _, ok := p.usingUserspace[svcName]; !ok {
			forIPTables = append(forIPTables, endpoint)
		}
	}

	p.userspaceLoadBalancer.OnEndpointsUpdate(endpoints)
	p.iptablesProxy.OnEndpointsUpdate(forIPTables)

	p.OnServiceUpdate(p.serviceConfig.Config())
}

// Sync is called to immediately synchronize the proxier state to iptables
func (p *Proxier) Sync() {
	p.iptablesProxy.Sync()
	p.userspaceProxy.Sync()
}

// SyncLoop runs periodic work.  This is expected to run as a goroutine or as the main loop of the app.  It does not return.
func (p *Proxier) SyncLoop() {
	t := time.NewTicker(p.syncPeriod)
	defer t.Stop()
	for {
		<-t.C
		glog.V(6).Infof("Periodic sync")
		p.iptablesProxy.Sync()
		p.userspaceProxy.Sync()
	}
}
