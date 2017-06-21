package master

import (
	"fmt"
	"net"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	kclientv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/record"
	kubecontroller "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	kapi "k8s.io/kubernetes/pkg/api"
	kclientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	"k8s.io/kubernetes/pkg/cloudprovider"
	nodecontroller "k8s.io/kubernetes/pkg/controller/node"
	hpacontroller "k8s.io/kubernetes/pkg/controller/podautoscaler"
	hpametrics "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
	servicecontroller "k8s.io/kubernetes/pkg/controller/service"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/plugin/pkg/scheduler"
	_ "k8s.io/kubernetes/plugin/pkg/scheduler/algorithmprovider"
	schedulerapi "k8s.io/kubernetes/plugin/pkg/scheduler/api"
	"k8s.io/kubernetes/plugin/pkg/scheduler/factory"

	"github.com/golang/glog"
	osclient "github.com/openshift/origin/pkg/client"
	oscontroller "github.com/openshift/origin/pkg/cmd/server/origin/controller"
)

type NodeControllerConfig struct {
	CloudProvider cloudprovider.Interface
}

func (c *NodeControllerConfig) RunController(ctx kubecontroller.ControllerContext) (bool, error) {
	_, clusterCIDR, err := net.ParseCIDR(ctx.Options.ClusterCIDR)
	if err != nil {
		glog.Warningf("NodeController failed parsing cluster CIDR %v: %v", ctx.Options.ClusterCIDR, err)
	}

	_, serviceCIDR, err := net.ParseCIDR(ctx.Options.ServiceCIDR)
	if err != nil {
		glog.Warningf("NodeController failed parsing service CIDR %v: %v", ctx.Options.ServiceCIDR, err)
	}

	controller, err := nodecontroller.NewNodeController(
		ctx.InformerFactory.Core().V1().Pods(),
		ctx.InformerFactory.Core().V1().Nodes(),
		ctx.InformerFactory.Extensions().V1beta1().DaemonSets(),
		c.CloudProvider,
		ctx.ClientBuilder.ClientOrDie("node-controller"),

		ctx.Options.PodEvictionTimeout.Duration,
		ctx.Options.NodeEvictionRate,
		ctx.Options.SecondaryNodeEvictionRate,
		ctx.Options.LargeClusterSizeThreshold,
		ctx.Options.UnhealthyZoneThreshold,
		ctx.Options.NodeMonitorGracePeriod.Duration,
		ctx.Options.NodeStartupGracePeriod.Duration,
		ctx.Options.NodeMonitorPeriod.Duration,

		clusterCIDR,
		serviceCIDR,

		int(ctx.Options.NodeCIDRMaskSize),
		ctx.Options.AllocateNodeCIDRs,
		ctx.Options.EnableTaintManager,
		utilfeature.DefaultFeatureGate.Enabled(features.TaintBasedEvictions),
	)
	if err != nil {
		return false, fmt.Errorf("unable to start node controller: %v", err)
	}

	go controller.Run()

	return true, nil
}

type ServiceLoadBalancerControllerConfig struct {
	CloudProvider cloudprovider.Interface
}

func (c *ServiceLoadBalancerControllerConfig) RunController(ctx kubecontroller.ControllerContext) (bool, error) {
	if c.CloudProvider == nil {
		glog.Warningf("ServiceLoadBalancer controller will not start - no cloud provider configured")
		return false, nil
	}
	serviceController, err := servicecontroller.New(
		c.CloudProvider,
		ctx.ClientBuilder.ClientOrDie("service-controller"),
		ctx.InformerFactory.Core().V1().Services(),
		ctx.InformerFactory.Core().V1().Nodes(),
		ctx.Options.ClusterName,
	)
	if err != nil {
		return true, fmt.Errorf("unable to start service load balancer controller: %v", err)
	}

	go serviceController.Run(ctx.Stop, int(ctx.Options.ConcurrentServiceSyncs))
	return true, nil
}

type SchedulerControllerConfig struct {
	// TODO: Move this closer to upstream, we want unprivileged client here.
	PrivilegedClient               kclientset.Interface
	SchedulerName                  string
	HardPodAffinitySymmetricWeight int
	SchedulerPolicy                *schedulerapi.Policy
}

func (c *SchedulerControllerConfig) RunController(ctx kubecontroller.ControllerContext) (bool, error) {
	// TODO make the rate limiter configurable
	configFactory := factory.NewConfigFactory(
		c.SchedulerName,
		c.PrivilegedClient,
		ctx.InformerFactory.Core().V1().Nodes(),
		ctx.InformerFactory.Core().V1().Pods(),
		ctx.InformerFactory.Core().V1().PersistentVolumes(),
		ctx.InformerFactory.Core().V1().PersistentVolumeClaims(),
		ctx.InformerFactory.Core().V1().ReplicationControllers(),
		ctx.InformerFactory.Extensions().V1beta1().ReplicaSets(),
		ctx.InformerFactory.Apps().V1beta1().StatefulSets(),
		ctx.InformerFactory.Core().V1().Services(),
		c.HardPodAffinitySymmetricWeight,
	)

	var (
		config *scheduler.Config
		err    error
	)

	if c.SchedulerPolicy != nil {
		config, err = configFactory.CreateFromConfig(*c.SchedulerPolicy)
		if err != nil {
			return true, fmt.Errorf("failed to create scheduler config from policy: %v", err)
		}
	} else {
		config, err = configFactory.CreateFromProvider(factory.DefaultProvider)
		if err != nil {
			return true, fmt.Errorf("failed to create scheduler config: %v", err)
		}
	}

	eventcast := record.NewBroadcaster()
	config.Recorder = eventcast.NewRecorder(kapi.Scheme, kclientv1.EventSource{Component: kapi.DefaultSchedulerName})
	eventcast.StartRecordingToSink(&kv1core.EventSinkImpl{Interface: kv1core.New(c.PrivilegedClient.CoreV1().RESTClient()).Events("")})

	s := scheduler.New(config)
	go s.Run()

	return true, nil
}

// NB: this is funky -- it's actually a Kubernetes controller, but we run it as an OpenShift controller in order
// to get a handle on OpenShift clients, so that our delegating scales getter can work.

type HorizontalPodAutoscalerControllerConfig struct {
	HeapsterNamespace string
}

func (c *HorizontalPodAutoscalerControllerConfig) RunController(ctx oscontroller.ControllerContext) (bool, error) {
	hpaClient := ctx.KubeControllerContext.ClientBuilder.ClientOrDie("horizontal-pod-autoscaler")
	hpaOriginClient := ctx.ClientBuilder.DeprecatedOpenshiftClientOrDie("horizontal-pod-autoscaler")

	metricsClient := hpametrics.NewHeapsterMetricsClient(
		hpaClient,
		c.HeapsterNamespace,
		"https",
		"heapster",
		"",
	)
	replicaCalc := hpacontroller.NewReplicaCalculator(metricsClient, hpaClient.Core())

	delegatingScalesGetter := osclient.NewDelegatingScaleNamespacer(hpaOriginClient, hpaClient.ExtensionsV1beta1())

	go hpacontroller.NewHorizontalController(
		ctx.KubeControllerContext.ClientBuilder.ClientGoClientOrDie("horizontal-pod-autoscaler").Core(),
		delegatingScalesGetter,
		hpaClient.Autoscaling(),
		replicaCalc,
		ctx.KubeControllerContext.InformerFactory.Autoscaling().V1().HorizontalPodAutoscalers(),
		ctx.KubeControllerContext.Options.HorizontalPodAutoscalerSyncPeriod.Duration,
	).Run(ctx.Stop)

	return true, nil
}
