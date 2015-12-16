package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	osgraph "github.com/openshift/origin/pkg/api/graph"
	"github.com/openshift/origin/pkg/api/graph/graphview"
	kubeedges "github.com/openshift/origin/pkg/api/kubegraph"
	kubegraph "github.com/openshift/origin/pkg/api/kubegraph/nodes"
	deploygraph "github.com/openshift/origin/pkg/deploy/graph/nodes"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"

	osclient "github.com/openshift/origin/pkg/client"
	"github.com/openshift/origin/pkg/cmd/cli/describe"
	"github.com/openshift/origin/pkg/cmd/util/clientcmd"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/strategicpatch"
	"k8s.io/kubernetes/pkg/util/unidling"
)

const (
	idleLong = `
Associate objects in the current project with each other.

This command allows you to associate different objects in the current project with each other
(like the status command).  Currently, it associates services with RCs and DCs.  It will
only list services, DCs, and RCs that are associated -- services that only point to pods,
as well as RCs and DCs with no services, will not be listed.`

	idleExample = `  # Associate services with RCs and DCs
  $ %[1]s idle`
)

// NewCmdStatus implements the OpenShift cli status command
func NewCmdIdle(fullName string, f *clientcmd.Factory, out io.Writer) *cobra.Command {
	var inputFile string
	dryRun := false
	loadByService := false

	cmd := &cobra.Command{
		Use:     "idle -f FILENAME",
		Short:   "Idle scalable objects",
		Long:    idleLong,
		Example: fmt.Sprintf(idleExample, fullName),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(RunIdle(f, out, cmd, inputFile, loadByService, dryRun))
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "If true, only print the annotations that would be written, without annotating or idling the relevant objects")
	cmd.Flags().StringVarP(&inputFile, "filename", "f", "", "file containing list of scalable objects to idle")
	cmd.Flags().BoolVar(&loadByService, "by-service", loadByService, "If true, the input list will be treated as a list of services whose associated RCs and DCs should be idle, instead of being a list of RCs and DCs to idle")
	cmd.MarkFlagRequired("filename")

	return cmd
}

func getPodTemplateSpecNode(g osgraph.Graph, node osgraph.Node) *kubegraph.PodTemplateSpecNode {
	rcSpecNodes := g.SuccessorNodesByNodeAndEdgeKind(node, kubegraph.ReplicationControllerSpecNodeKind, osgraph.ContainsEdgeKind)
	if len(rcSpecNodes) != 1 {
		panic(fmt.Sprintf("expected ReplicationControllerNode/DeploymentConfigNode %s to have a single ReplicationControllerSpecNode contained within it", g.GraphDescriber.Name(node)))
	}

	rcPodTemplateSpecNodes := g.SuccessorNodesByNodeAndEdgeKind(rcSpecNodes[0], kubegraph.PodTemplateSpecNodeKind, osgraph.ContainsEdgeKind)

	if len(rcPodTemplateSpecNodes) == 0 {
		return nil
	} else if len(rcPodTemplateSpecNodes) > 1 {
		panic(fmt.Sprintf("expected ReplicationControllerSpecNode %s to have no more than one PodTemplateSpecNode contained within it", g.GraphDescriber.Name(rcSpecNodes[0])))
	}

	return rcPodTemplateSpecNodes[0].(*kubegraph.PodTemplateSpecNode)
}

func getEndpointsNode(g osgraph.Graph, node osgraph.Node) *kubegraph.EndpointsNode {
	endpointsNodes := g.SuccessorNodesByNodeAndEdgeKind(node, kubegraph.EndpointsNodeKind, kubeedges.HasEndpointsEdgeKind)
	if len(endpointsNodes) != 1 {
		panic(fmt.Sprintf("expected ServiceNode %s to have a single EndpointsNode corresponding to it", g.GraphDescriber.Name(node)))
	}

	return endpointsNodes[0].(*kubegraph.EndpointsNode)
}

func scanIdlableResourcesFromFile(filename string) (map[string]bool, error) {
	targetResources := make(map[string]bool)
	var targetsInput io.Reader
	if filename == "-" {
		targetsInput = os.Stdin
	} else if filename == "" {
		return nil, fmt.Errorf("you must specify an list of resources to idle")
	} else {
		inputFile, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		defer inputFile.Close()
		targetsInput = inputFile
	}

	targetsScanner := bufio.NewScanner(targetsInput)
	for targetsScanner.Scan() {
		targetResources[targetsScanner.Text()] = true
	}
	if err := targetsScanner.Err(); err != nil {
		return nil, err
	}

	return targetResources, nil
}

type idleUpdateInfo struct {
	obj         runtime.Object
	annotations []extensions.SubresourceReference
}

func calculateIdlableAnnotationsByScalable(f *clientcmd.Factory, cmd *cobra.Command, namespace, filename string) (map[string]idleUpdateInfo, map[string]idleUpdateInfo, error) {
	// load the target scalables (they should be in the form of "fullresourcetype/resourcename")
	actualScalables, err := scanIdlableResourcesFromFile(filename)
	if err != nil {
		return nil, nil, err
	}

	// use a visitor to resolve the correct names (and check the validity of the resolved resources)
	client, kclient, err := f.Clients()
	if err != nil {
		return nil, nil, err
	}

	// set up our clients for creating the graph
	config, err := f.OpenShiftClientConfig.ClientConfig()
	if err != nil {
		return nil, nil, err
	}

	describer := &describe.ProjectStatusDescriber{K: kclient, C: client, Server: config.Host}
	// we only need services, rcs, and dcs, not the rest of the objects in the graph
	g, _, err := describer.MakeGraph(namespace, "services", "endpoints", "replicationcontrollers", "deploymentconfigs")

	if err != nil {
		return nil, nil, err
	}

	byService := make(map[string]idleUpdateInfo)
	byScalable := make(map[string]idleUpdateInfo)

	// calculate associated services for all RC and DC nodes
	for _, uncastRCNode := range g.NodesByKind(kubegraph.ReplicationControllerNodeKind) {
		rcNode := uncastRCNode.(*kubegraph.ReplicationControllerNode)
		rcRes := rcNode.Kind() + "/" + rcNode.Name
		rcRef := extensions.SubresourceReference{
			Kind: rcNode.Kind(),
			Name: rcNode.Name,
		}

		if _, ok := actualScalables[rcRes]; !ok {
			continue
		}

		rcPodTemplateSpecNode := getPodTemplateSpecNode(g, rcNode.Node)
		if rcPodTemplateSpecNode == nil {
			continue
		}
		for _, uncastServiceNode := range g.SuccessorNodesByEdgeKind(rcPodTemplateSpecNode, kubeedges.ExposedThroughServiceEdgeKind) {
			serviceNode := uncastServiceNode.(*kubegraph.ServiceNode)
			endpointsNode := getEndpointsNode(g, serviceNode.Node)

			serviceInfo := byService[endpointsNode.Name]
			serviceInfo.annotations = append(serviceInfo.annotations, rcRef)
			endpointsNode.Endpoints.Kind = endpointsNode.Kind()
			serviceInfo.obj = endpointsNode.Endpoints
			byService[endpointsNode.Name] = serviceInfo

			scalableInfo := byScalable[rcRes]
			//scalableInfo.annotations = append(scalableInfo.annotations, endpointsNode.Name)
			rcNode.ReplicationController.Kind = rcNode.Kind()
			scalableInfo.obj = rcNode.ReplicationController
			byScalable[rcRes] = scalableInfo
		}
	}

	for _, uncastDCNode := range g.NodesByKind(deploygraph.DeploymentConfigNodeKind) {
		dcNode := uncastDCNode.(*deploygraph.DeploymentConfigNode)
		dcRes := dcNode.Kind() + "/" + dcNode.Name
		dcRef := extensions.SubresourceReference{
			Kind: dcNode.Kind(),
			Name: dcNode.Name,
		}

		if _, ok := actualScalables[dcRes]; !ok {
			continue
		}

		dcPodTemplateSpecNode := getPodTemplateSpecNode(g, dcNode.Node)
		if dcPodTemplateSpecNode == nil {
			continue
		}
		for _, uncastServiceNode := range g.SuccessorNodesByEdgeKind(dcPodTemplateSpecNode, kubeedges.ExposedThroughServiceEdgeKind) {
			serviceNode := uncastServiceNode.(*kubegraph.ServiceNode)
			endpointsNode := getEndpointsNode(g, serviceNode.Node)

			serviceInfo := byService[endpointsNode.Name]
			serviceInfo.annotations = append(serviceInfo.annotations, dcRef)
			endpointsNode.Endpoints.Kind = endpointsNode.Kind()
			serviceInfo.obj = endpointsNode.Endpoints
			byService[endpointsNode.Name] = serviceInfo

			scalableInfo := byScalable[dcRes]
			//scalableInfo.annotations = append(scalableInfo.annotations, endpointsNode.Name)
			dcNode.DeploymentConfig.Kind = dcNode.Kind()
			scalableInfo.obj = dcNode.DeploymentConfig
			byScalable[dcRes] = scalableInfo
		}
	}

	return byService, byScalable, nil
}

func calculateIdlableAnnotationsByService(f *clientcmd.Factory, cmd *cobra.Command, namespace, filename string) (map[string]idleUpdateInfo, map[string]idleUpdateInfo, error) {
	// load our set of services
	targetServices, err := scanIdlableResourcesFromFile(filename)
	if err != nil {
		return nil, nil, err
	}

	client, kclient, err := f.Clients()
	if err != nil {
		return nil, nil, err
	}

	// set up our clients for creating the graph
	config, err := f.OpenShiftClientConfig.ClientConfig()
	if err != nil {
		return nil, nil, err
	}

	describer := &describe.ProjectStatusDescriber{K: kclient, C: client, Server: config.Host}
	g, _, err := describer.MakeGraph(namespace, "services", "endpoints", "replicationcontrollers", "deploymentconfigs")
	if err != nil {
		return nil, nil, err
	}

	byService := make(map[string]idleUpdateInfo)
	byScalable := make(map[string]idleUpdateInfo)

	// get all services, make associations
	// NB: ResourceString uses the abbreviated form, we want the expanded form
	services, _ := graphview.AllServiceGroups(g, graphview.IntSet{})
	for _, serviceGroup := range services {
		if !serviceGroup.Service.Found() {
			continue
		}

		if _, ok := targetServices[serviceGroup.Service.Name]; !ok {
			continue
		}

		coveredRCs := graphview.IntSet{}
		svcRes := serviceGroup.Service.Name

		endpointsNode := getEndpointsNode(g, serviceGroup.Service.Node)
		endpointsNode.Endpoints.Kind = endpointsNode.Kind()
		serviceInfo := byService[svcRes]
		serviceInfo.obj = endpointsNode.Endpoints
		byService[svcRes] = serviceInfo

		for _, dc := range serviceGroup.DeploymentConfigPipelines {
			dcRes := dc.Deployment.Kind() + "/" + dc.Deployment.Name
			dcRef := extensions.SubresourceReference{
				Kind: dc.Deployment.Kind(),
				Name: dc.Deployment.Name,
			}

			serviceInfo := byService[svcRes]
			serviceInfo.annotations = append(serviceInfo.annotations, dcRef)
			byService[svcRes] = serviceInfo

			scalableInfo := byScalable[dcRes]
			dc.Deployment.DeploymentConfig.Kind = dc.Deployment.Kind()
			scalableInfo.obj = dc.Deployment.DeploymentConfig
			byScalable[dcRes] = scalableInfo

			// don't also add RCs covered by DCs
			for _, rc := range dc.InactiveDeployments {
				coveredRCs.Insert(rc.ID())
			}
			coveredRCs.Insert(dc.ActiveDeployment.ID())
		}

		for _, rc := range serviceGroup.FulfillingRCs {
			if coveredRCs.Has(rc.ID()) {
				continue
			}
			rcRes := rc.Kind() + "/" + rc.Name
			rcRef := extensions.SubresourceReference{
				Kind: rc.Kind(),
				Name: rc.Name,
			}
			serviceInfo := byService[svcRes]
			serviceInfo.annotations = append(serviceInfo.annotations, rcRef)
			byService[svcRes] = serviceInfo

			scalableInfo := byScalable[rcRes]
			rc.ReplicationController.Kind = rc.Kind()
			scalableInfo.obj = rc.ReplicationController
			byScalable[rcRes] = scalableInfo
		}
	}

	return byService, byScalable, nil
}

func setIdleAnnotations(obj runtime.Object, key string, vals []extensions.SubresourceReference) error {
	meta, err := api.ObjectMetaFor(obj)
	if err != nil {
		return err
	}

	if meta.Annotations == nil {
		meta.Annotations = make(map[string]string)
	}

	// TODO(directxman12): preserve values across calls?
	var valBytes []byte
	if valBytes, err = json.Marshal(vals); err != nil {
		return err
	}
	meta.Annotations[key] = string(valBytes)
	meta.Annotations[unidling.IdledAtAnnotation] = time.Now().UTC().Format(time.RFC3339)

	return nil
}

func patchObj(obj runtime.Object, metadata meta.Object, oldData []byte, mapping *meta.RESTMapping, f *clientcmd.Factory) (runtime.Object, error) {
	newData, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, obj)
	if err != nil {
		return nil, err
	}

	client, err := f.ClientForMapping(mapping)
	if err != nil {
		return nil, err
	}
	helper := resource.NewHelper(client, mapping)

	return helper.Patch(metadata.GetNamespace(), metadata.GetName(), api.StrategicMergePatchType, patchBytes)
}

func RunIdle(f *clientcmd.Factory, out io.Writer, cmd *cobra.Command, filename string, loadByService, dryRun bool) error {
	namespace, _, err := f.DefaultNamespace()
	if err != nil {
		return err
	}

	var byService map[string]idleUpdateInfo
	var byScalable map[string]idleUpdateInfo

	mapper, typer := f.Object(false)
	if loadByService {
		byService, byScalable, err = calculateIdlableAnnotationsByService(f, cmd, namespace, filename)
	} else {
		byService, byScalable, err = calculateIdlableAnnotationsByScalable(f, cmd, namespace, filename)
	}

	if err != nil {
		return err
	}

	oclient, kclient, err := f.Clients()
	if err != nil {
		return err
	}
	delegScaleNamespacer := osclient.NewDelegatingScaleNamespacer(oclient, kclient)

	// load all the resources
	for serviceName, info := range byService {
		if !dryRun {
			metadata, err := meta.Accessor(info.obj)
			if err != nil {
				// TODO: return list of errors at the end
				return err
			}
			gvk, err := typer.ObjectKind(info.obj)
			if err != nil {
				return err
			}
			oldData, err := json.Marshal(info.obj)
			if err != nil {
				return err
			}

			mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
			if err != nil {
				return err
			}

			if err := setIdleAnnotations(info.obj, unidling.UnidleTargetAnnotation, info.annotations); err != nil {
				return err
			}
			if _, err := patchObj(info.obj, metadata, oldData, mapping, f); err != nil {
				return err
			}
		}

		fmt.Fprintf(out, "Annotated service/%s with %v\n", serviceName, info.annotations)
	}

	for fullName, info := range byScalable {
		if !dryRun {
			metadata, err := meta.Accessor(info.obj)
			if err != nil {
				// TODO: return list of errors at the end
				return err
			}
			gvk, err := typer.ObjectKind(info.obj)
			if err != nil {
				return err
			}

			scale, err := delegScaleNamespacer.Scales(namespace).Get(gvk.Kind, metadata.GetName())
			if err != nil {
				return err
			}

			scale.Spec.Replicas = 0

			if _, err := delegScaleNamespacer.Scales(namespace).Update(gvk.Kind, scale); err != nil {
				// TODO: use retry logic from scaler package?
				return err
			}
		}

		fmt.Fprintf(out, "Scaled %s to 0\n", fullName)
	}

	return nil
}
