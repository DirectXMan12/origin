package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	utilerrors "github.com/openshift/origin/pkg/util/errors"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"

	osclient "github.com/openshift/origin/pkg/client"
	"github.com/openshift/origin/pkg/cmd/util/clientcmd"
	deployapi "github.com/openshift/origin/pkg/deploy/api"
	deployclient "github.com/openshift/origin/pkg/deploy/client/clientset_generated/internalclientset/typed/core/unversioned"
	unidlingapi "github.com/openshift/origin/pkg/unidling/api"
	utilunidling "github.com/openshift/origin/pkg/unidling/util"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/extensions"
	clientset "k8s.io/kubernetes/pkg/client/unversioned/adapters/internalclientset"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/strategicpatch"
)

const (
	idleLong = `
Idle scalable resources.

This command idles the provides list of resources by scaling scalable resources (e.g. RCs and
DCs) down to zero replicas, and then marking associated services so that that the scalables
will be "woken up" when traffic occurs on those services.

Only DCs and RCs with associated services will be idled -- services that only point to pods,
as well as RCs and DCs with no services, will not be idled.`

	idleExample = `  # Idle the scalable controllers associated with some services listed in to-idle.txt
  $ %[1]s idle -f to-idle.txt`
)

// NewCmdStatus implements the OpenShift cli status command
func NewCmdIdle(fullName string, f *clientcmd.Factory, out io.Writer) *cobra.Command {
	var inputFile string
	dryRun := false
	loadByService := false

	cmd := &cobra.Command{
		Use:     "idle -f FILENAME",
		Short:   "Idle scalable resources",
		Long:    idleLong,
		Example: fmt.Sprintf(idleExample, fullName),
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(RunIdle(f, out, cmd, inputFile, loadByService, dryRun))
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "If true, only print the annotations that would be written, without annotating or idling the relevant objects")
	cmd.Flags().StringVarP(&inputFile, "filename", "f", "", "file containing list of services whose scalables will be idled")
	cmd.MarkFlagRequired("filename")

	return cmd
}

// scanLinesFromFile loads lines from either standard in or a file
func scanLinesFromFile(filename string) ([]string, error) {
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

	lines := []string{}

	// grab the raw resources from the file
	lineScanner := bufio.NewScanner(targetsInput)
	for lineScanner.Scan() {
		line := lineScanner.Text()
		if line == "" {
			// skip empty lines
			continue
		}
		lines = append(lines, line)
	}
	if err := lineScanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

// idleUpdateInfo contains the required info to annotate an endpoints object
// with the scalables that it should unidle
type idleUpdateInfo struct {
	obj       *api.Endpoints
	scalables map[unidlingapi.CrossGroupObjectReference]struct{}
}

// calculateIdlableAnnotationsByService calculates the list of objects involved in the idling process from a list of services in a file.
// Using the list of services, it figures out the associated scalable objects, and returns a map from the endpoints object for the services to
// the list of scalables associated with that endpoints object, as well as a map from CrossGroupObjectReferences to scale to 0 to the name of the associated service.
func calculateIdlableAnnotationsByService(f *clientcmd.Factory, namespace, filename string, out io.Writer) (map[string]idleUpdateInfo, map[unidlingapi.CrossGroupObjectReference]string, error) {
	// load our set of services
	targetServiceNames, err := scanLinesFromFile(filename)
	if err != nil {
		return nil, nil, err
	}

	client, err := f.Client()
	if err != nil {
		return nil, nil, err
	}

	mapper, typer := f.Object(false)

	podsLoaded := make(map[api.ObjectReference]*api.Pod)
	getPod := func(ref api.ObjectReference) (*api.Pod, error) {
		if pod, ok := podsLoaded[ref]; ok {
			return pod, nil
		}
		pod, err := client.Pods(ref.Namespace).Get(ref.Name)
		if err != nil {
			return nil, err
		}

		podsLoaded[ref] = pod

		return pod, nil
	}

	controllersLoaded := make(map[api.ObjectReference]runtime.Object)
	helpers := make(map[unversioned.GroupKind]*resource.Helper)
	getController := func(ref api.ObjectReference) (runtime.Object, error) {
		if controller, ok := controllersLoaded[ref]; ok {
			return controller, nil
		}
		gv, err := unversioned.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return nil, err
		}
		// just get the unversioned version of this
		gk := unversioned.GroupKind{Group: gv.Group, Kind: ref.Kind}
		helper, ok := helpers[gk]
		if !ok {
			var mapping *meta.RESTMapping
			mapping, err = mapper.RESTMapping(unversioned.GroupKind{Group: gv.Group, Kind: ref.Kind}, "")
			if err != nil {
				return nil, err
			}
			var client resource.RESTClient
			client, err = f.ClientForMapping(mapping)
			if err != nil {
				return nil, err
			}
			helper = resource.NewHelper(client, mapping)
			helpers[gk] = helper
		}

		var controller runtime.Object
		controller, err = helper.Get(ref.Namespace, ref.Name, false)
		if err != nil {
			return nil, err
		}

		controllersLoaded[ref] = controller

		return controller, nil
	}

	targetScalables := make(map[unidlingapi.CrossGroupObjectReference]string)
	endpointsInfo := make(map[string]idleUpdateInfo)

	decoder := f.Decoder(true)
	b := resource.NewBuilder(mapper, typer, resource.ClientMapperFunc(f.ClientForMapping), api.Codecs.UniversalDecoder()).
		ContinueOnError().
		NamespaceParam(namespace).DefaultNamespace().
		ResourceNames("endpoints", targetServiceNames...).
		SingleResourceType().
		Flatten()

	err = b.Do().Visit(func(info *resource.Info, err error) error {
		endpoints := info.Object.(*api.Endpoints)
		idlableRefs, err := findIdlablesForEndpoints(endpoints, decoder, getPod, getController)
		if err != nil {
			return fmt.Errorf("unable to calculate idlables for service %s/%s: %v", endpoints.Namespace, endpoints.Name, err)
		}

		for ref := range idlableRefs {
			targetScalables[ref] = endpoints.Name
		}

		idleInfo := idleUpdateInfo{
			obj:       endpoints,
			scalables: idlableRefs,
		}

		endpointsInfo[endpoints.Name] = idleInfo

		return nil
	})

	return endpointsInfo, targetScalables, err
}

// getControllerRef returns a subresource reference to the owning controller of the given object.
// It will use both the CreatedByAnnotation from Kubernetes, as well as the DeploymentConfigAnnotation
// from Origin to look this up.  If neither are found, it will return nil.
func getControllerRef(obj runtime.Object, decoder runtime.Decoder) (*api.ObjectReference, error) {
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	annotations := objMeta.GetAnnotations()

	creatorRefRaw, creatorListed := annotations[controller.CreatedByAnnotation]
	if !creatorListed {
		// if we don't have a creator listed, try the openshift-specific Deployment annotation
		dcName, dcNameListed := annotations[deployapi.DeploymentConfigAnnotation]
		if !dcNameListed {
			return nil, nil
		}

		return &api.ObjectReference{
			Name:      dcName,
			Namespace: objMeta.GetNamespace(),
			Kind:      "DeploymentConfig",
		}, nil
	}

	serializedRef := &api.SerializedReference{}
	if err := runtime.DecodeInto(decoder, []byte(creatorRefRaw), serializedRef); err != nil {
		return nil, fmt.Errorf("could not decoded pod's creator reference: %v", err)
	}

	return &serializedRef.Reference, nil
}

func makeCrossGroupObjRef(ref *api.ObjectReference) (unidlingapi.CrossGroupObjectReference, error) {
	gv, err := unversioned.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return unidlingapi.CrossGroupObjectReference{}, err
	}

	return unidlingapi.CrossGroupObjectReference{
		Kind:  ref.Kind,
		Name:  ref.Name,
		Group: gv.Group,
	}, nil
}

// findIdlablesForEndpoints takes an Endpoints object and looks for the associated
// scalable objects by checking each address in each subset to see if it has a pod
// reference, and the following that pod reference to find the owning controller,
// and returning the unique set of controllers found this way.
func findIdlablesForEndpoints(endpoints *api.Endpoints, decoder runtime.Decoder, getPod func(api.ObjectReference) (*api.Pod, error), getController func(api.ObjectReference) (runtime.Object, error)) (map[unidlingapi.CrossGroupObjectReference]struct{}, error) {
	// To find all RCs and DCs for an endpoint, we first figure out which pods are pointed to by that endpoint...
	podRefs := map[api.ObjectReference]*api.Pod{}
	for _, subset := range endpoints.Subsets {
		for _, addr := range subset.Addresses {
			if addr.TargetRef != nil && addr.TargetRef.Kind == "Pod" {
				pod, err := getPod(*addr.TargetRef)
				if utilerrors.TolerateNotFoundError(err) != nil {
					return nil, fmt.Errorf("unable to find controller for pod %s/%s: %v", addr.TargetRef.Namespace, addr.TargetRef.Name, err)
				}

				if pod != nil {
					podRefs[*addr.TargetRef] = pod
				}
			}
		}
	}

	// ... then, for each pod, we check the controller, and find the set of unique controllers...
	immediateControllerRefs := make(map[api.ObjectReference]struct{})
	for _, pod := range podRefs {
		controllerRef, err := getControllerRef(pod, decoder)
		if err != nil {
			return nil, fmt.Errorf("unable to find controller for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		} else if controllerRef == nil {
			return nil, fmt.Errorf("unable to find controller for pod %s/%s: no creator reference listed", pod.Namespace, pod.Name)
		}

		immediateControllerRefs[*controllerRef] = struct{}{}
	}

	// ... finally, for each controller, we load it, and see if there is a corresponding owner (to cover cases like DCs, Deployments, etc)
	controllerRefs := make(map[unidlingapi.CrossGroupObjectReference]struct{})
	for controllerRef := range immediateControllerRefs {
		controller, err := getController(controllerRef)
		if utilerrors.TolerateNotFoundError(err) != nil {
			return nil, fmt.Errorf("unable to load %s %q: %v", controllerRef.Kind, controllerRef.Name, err)
		}

		if controller != nil {
			var parentControllerRef *api.ObjectReference
			parentControllerRef, err = getControllerRef(controller, decoder)
			if err != nil {
				return nil, fmt.Errorf("unable to load the creator of %s %q: %v", controllerRef.Kind, controllerRef.Name, err)
			}

			var crossGroupObjRef unidlingapi.CrossGroupObjectReference
			if parentControllerRef == nil {
				// if this is just a plain RC, use it
				crossGroupObjRef, err = makeCrossGroupObjRef(&controllerRef)
			} else {
				crossGroupObjRef, err = makeCrossGroupObjRef(parentControllerRef)
			}

			if err != nil {
				return nil, fmt.Errorf("unable to load the creator of %s %q: %v", controllerRef.Kind, controllerRef.Name, err)
			}
			controllerRefs[crossGroupObjRef] = struct{}{}
		}
	}

	return controllerRefs, nil
}

// pairScalesWithIdlables takes some subresource references, a map of new scales for those subresource references,
// and annotations from an existing object.  It merges the scales and references found in the existing annotations
// with the new data (using the new scale in case of conflict if present and not 0, and the old scale otherwise),
// and returns a slice of RecordedScaleReferences suitable for using as the new annotation value.
func pairScalesWithIdlables(serviceName string, annotations map[string]string, rawScaleRefs map[unidlingapi.CrossGroupObjectReference]struct{}, scales map[unidlingapi.CrossGroupObjectReference]int32) ([]unidlingapi.RecordedScaleReference, error) {
	oldTargetsRaw, hasOldTargets := annotations[unidlingapi.UnidleTargetAnnotation]

	scaleRefs := make([]unidlingapi.RecordedScaleReference, 0, len(rawScaleRefs))

	// initialize the list of new annotations
	for rawScaleRef := range rawScaleRefs {
		scaleRefs = append(scaleRefs, unidlingapi.RecordedScaleReference{
			CrossGroupObjectReference: rawScaleRef,
			Replicas:                  0,
		})
	}

	// if the new preserved scale would be 0, see if we have an old scale that we can use instead
	if hasOldTargets {
		var oldTargets []unidlingapi.RecordedScaleReference
		oldTargetsSet := make(map[unidlingapi.CrossGroupObjectReference]int)
		if err := json.Unmarshal([]byte(oldTargetsRaw), &oldTargets); err != nil {
			return nil, fmt.Errorf("unable to extract existing scale information from endpoints %s: %v", serviceName, err)
		}

		for i, target := range oldTargets {
			oldTargetsSet[target.CrossGroupObjectReference] = i
		}

		// figure out which new targets were already present...
		for _, newScaleRef := range scaleRefs {
			if oldTargetInd, ok := oldTargetsSet[newScaleRef.CrossGroupObjectReference]; ok {
				if newScale, ok := scales[newScaleRef.CrossGroupObjectReference]; !ok || newScale == 0 {
					scales[newScaleRef.CrossGroupObjectReference] = oldTargets[oldTargetInd].Replicas
				}
				delete(oldTargetsSet, newScaleRef.CrossGroupObjectReference)
			}
		}

		// ...and add in any existing targets not already on the new list to the new list
		for _, ind := range oldTargetsSet {
			scaleRefs = append(scaleRefs, oldTargets[ind])
		}
	}

	for i := range scaleRefs {
		scaleRef := &scaleRefs[i]
		newScale, ok := scales[scaleRef.CrossGroupObjectReference]
		if !ok || newScale == 0 {
			newScale = 1
			if scaleRef.Replicas != 0 {
				newScale = scaleRef.Replicas
			}
		}

		scaleRef.Replicas = newScale
	}

	return scaleRefs, nil
}

// setIdleAnnotations sets the given annotation on the given object to the marshaled list of CrossGroupObjectReferences
func setIdleAnnotations(serviceName string, annotations map[string]string, scaleRefs []unidlingapi.RecordedScaleReference, nowTime time.Time) error {
	var scaleRefsBytes []byte
	var err error
	if scaleRefsBytes, err = json.Marshal(scaleRefs); err != nil {
		return err
	}

	annotations[unidlingapi.UnidleTargetAnnotation] = string(scaleRefsBytes)
	annotations[unidlingapi.IdledAtAnnotation] = nowTime.Format(time.RFC3339)

	return nil
}

// patchObj patches calculates a patch between the given new object and the existing marshaled object
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

type scaleInfo struct {
	scale *extensions.Scale
	obj   runtime.Object
}

// RunIdle runs the idling command logic, taking a list of resources or services in a file, scaling the associated
// scalables to zero, and annotating the associated endpoints objects with the scalables to unidle when they receive traffic.
func RunIdle(f *clientcmd.Factory, out io.Writer, cmd *cobra.Command, filename string, loadByService, dryRun bool) error {
	namespace, _, err := f.DefaultNamespace()
	if err != nil {
		return err
	}

	nowTime := time.Now().UTC()

	// figure out which endpoints and resources we need to idle
	byService, byScalable, err := calculateIdlableAnnotationsByService(f, namespace, filename, out)

	if err != nil {
		if len(byService) == 0 || len(byScalable) == 0 {
			return fmt.Errorf("no valid scalables found to idle: %v", err)
		}
		fmt.Fprintf(out, "Errors while finding scalable resources to idle: %v\nContinuing for valid scalables...\n", err)
	}

	oclient, kclient, err := f.Clients()
	if err != nil {
		return err
	}
	delegScaleGetter := osclient.NewDelegatingScaleNamespacer(oclient, kclient).Scales(namespace)
	dcGetter := deployclient.New(oclient.RESTClient).DeploymentConfigs(namespace)
	rcGetter := clientset.FromUnversionedClient(kclient).ReplicationControllers(namespace)

	replicas := make(map[unidlingapi.CrossGroupObjectReference]int32, len(byScalable))
	toScale := make(map[unidlingapi.CrossGroupObjectReference]scaleInfo)

	mapper, typer := f.Object(false)

	scaleAnnotater := utilunidling.NewScaleAnnotater(delegScaleGetter, dcGetter, rcGetter, func(ref unidlingapi.CrossGroupObjectReference, annotations map[string]string) {
		annotations[unidlingapi.IdledAtAnnotation] = nowTime.UTC().Format(time.RFC3339)
	})

	// first, collect the scale info
	for scaleRef, svcName := range byScalable {
		obj, scale, err := scaleAnnotater.GetObjectWithScale(scaleRef)
		if err != nil {
			fmt.Fprintf(out, "Unable to get scale for %s %s/%s, not marking that scalable as idled...\n", scaleRef.Kind, namespace, scaleRef.Name)
			svcInfo := byService[svcName]
			delete(svcInfo.scalables, scaleRef)
			continue
		}
		replicas[scaleRef] = scale.Spec.Replicas
		toScale[scaleRef] = scaleInfo{scale: scale, obj: obj}
	}

	// annotate the endpoints objects to indicate which scalables need to be unidled on traffic
	for serviceName, info := range byService {
		if info.obj.Annotations == nil {
			info.obj.Annotations = make(map[string]string)
		}
		refsWithScale, err := pairScalesWithIdlables(serviceName, info.obj.Annotations, info.scalables, replicas)
		if err != nil {
			fmt.Fprintf(out, "Unable to mark service %s/%s as idled: %v", namespace, serviceName, err)
			continue
		}

		if !dryRun {
			if len(info.scalables) == 0 {
				fmt.Fprintf(out, "No scalables marked as idled for service %s/%s, not marking as idled...\n", namespace, serviceName)
				continue
			}

			metadata, err := meta.Accessor(info.obj)
			if err != nil {
				fmt.Fprintf(out, "Unable to mark service %s/%s as idled: %v", namespace, serviceName, err)
				continue
			}
			gvks, _, err := typer.ObjectKinds(info.obj)
			if err != nil {
				fmt.Fprintf(out, "Unable to mark service %s/%s as idled: %v", namespace, serviceName, err)
				continue
			}
			oldData, err := json.Marshal(info.obj)
			if err != nil {
				fmt.Fprintf(out, "Unable to mark service %s/%s as idled: %v", namespace, serviceName, err)
				continue
			}

			mapping, err := mapper.RESTMapping(gvks[0].GroupKind(), gvks[0].Version)
			if err != nil {
				fmt.Fprintf(out, "Unable to mark service %s/%s as idled: %v", namespace, serviceName, err)
				continue
			}

			if err = setIdleAnnotations(serviceName, info.obj.Annotations, refsWithScale, nowTime); err != nil {
				fmt.Fprintf(out, "Unable to mark service %s/%s as idled: %v", namespace, serviceName, err)
				continue
			}
			if _, err := patchObj(info.obj, metadata, oldData, mapping, f); err != nil {
				fmt.Fprintf(out, "Unable to mark service %s/%s as idled: %v", namespace, serviceName, err)
				continue
			}
		}

		fmt.Fprintf(out, "Marked service %s/%s to unidle scalables:\n", namespace, serviceName)
		for _, scaleRef := range refsWithScale {
			fmt.Fprintf(out, "    %s %s/%s (unidle to %v replicas)\n", scaleRef.Kind, namespace, scaleRef.Name, scaleRef.Replicas)
		}
	}

	// actually "idle" the scalables by scaling them down to zero
	// (scale down to zero *after* we've applied the annotation so that we don't miss any traffic)
	for scaleRef, info := range toScale {
		if !dryRun {
			info.scale.Spec.Replicas = 0
			if err := scaleAnnotater.UpdateObjectScale(scaleRef, info.obj, info.scale); err != nil {
				fmt.Fprintf(out, "Unable to scale %s %s/%s to 0, but still listed as target for unidling...\n", scaleRef.Kind, namespace, scaleRef.Name)
				continue
			}
		}

		fmt.Fprintf(out, "Idled %s %s/%s\n", scaleRef.Kind, namespace, scaleRef.Name)
	}

	return nil
}

// TODO: remove the functions below once we get the ability to mark the last-scale-reason field via the scale subresource
