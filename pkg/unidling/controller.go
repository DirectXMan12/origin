package unidling

import (
	"fmt"
	"time"
	"encoding/json"

	kapi "k8s.io/kubernetes/pkg/api"
	kextapi "k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/client/cache"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kextclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/extensions/unversioned"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/runtime"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/unidling"
	"k8s.io/kubernetes/pkg/watch"

	"github.com/golang/glog"
)

type UnidlingController struct {
	controller          *framework.Controller
	scaleNamespacer     kextclient.ScalesGetter
	endpointsNamespacer kclient.EndpointsNamespacer
}

func NewUnidlingController(scaleNS kextclient.ScalesGetter, endptsNS kclient.EndpointsNamespacer, evtNS kclient.EventNamespacer, resyncPeriod time.Duration) *UnidlingController {
	fieldSet := fields.Set{}
	fieldSet["reason"] = unidling.NeedPodsReason
	fieldSelector := fieldSet.AsSelector()

	unidlingController := &UnidlingController{
		scaleNamespacer:     scaleNS,
		endpointsNamespacer: endptsNS,
	}

	_, controller := framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options kapi.ListOptions) (runtime.Object, error) {
				options.FieldSelector = fieldSelector
				return evtNS.Events(kapi.NamespaceAll).List(options)
			},
			WatchFunc: func(options kapi.ListOptions) (watch.Interface, error) {
				options.FieldSelector = fieldSelector
				return evtNS.Events(kapi.NamespaceAll).Watch(options)
			},
		},
		&kapi.Event{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				event := obj.(*kapi.Event)

				if event.Reason != unidling.NeedPodsReason {
					glog.Errorf("UnidlingController was asked to handle a non-NeedPods event")
					return
				}

				if err := unidlingController.handleEvent(event); err != nil {
					targetNamespace := event.InvolvedObject.Namespace
					targetServiceName := event.InvolvedObject.Name
					glog.Errorf("Unable to unidle %s/%s: %v", targetNamespace, targetServiceName, err)
				}
			},
		},
	)

	unidlingController.controller = controller

	return unidlingController
}

func (c *UnidlingController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	go c.controller.Run(stopCh)
}

// TODO: does this need to send error events?
func (c *UnidlingController) handleEvent(event *kapi.Event) error {
	targetNamespace := event.InvolvedObject.Namespace
	targetServiceName := event.InvolvedObject.Name

	targetEndpoints, err := c.endpointsNamespacer.Endpoints(targetNamespace).Get(targetServiceName)
	if err != nil {
		return fmt.Errorf("unable to retrieve service: %v", err)
	}

	// check when we were idled to prevent historical unidling
	idledTimeRaw, wasIdled := targetEndpoints.Annotations[unidling.IdledAtAnnotation]
	if !wasIdled {
		glog.V(5).Infof("UnidlingController received a NeedPods event for a service that was not idled, ignoring")
		return nil
	}

	idledTime, err := time.Parse(time.RFC3339, idledTimeRaw)
	if err != nil {
		return fmt.Errorf("unable to check idled-at time: %v", err)
	}
	if event.LastTimestamp.Time.Before(idledTime) {
		glog.V(5).Infof("UnidlingController received an out-of-date NeedPods event, ignoring")
		return nil
	}

	// TODO: update this to a CrossVersionObjectReference once we pull in Kube version where apis/autoscaling has it unversioned
	// TODO: ew, this is unversioned.  Such is life when working with annotations.
	var targetScalables []kextapi.SubresourceReference
	if targetScalablesStr, hasTargetScalables := targetEndpoints.Annotations[unidling.UnidleTargetAnnotation]; hasTargetScalables {
		if err := json.Unmarshal([]byte(targetScalablesStr), &targetScalables); err != nil {
			glog.Errorf("Unable to unmarshal target scalable references while unidling service %s/%s: %v", targetNamespace, targetServiceName, err)
			return nil
		}
	} else {
		glog.V(4).Infof("Service %s/%s had no scalables to unidle", targetNamespace, targetServiceName)
		targetScalables = []kextapi.SubresourceReference{}
	}

	targetScalablesSet := make(map[kextapi.SubresourceReference]bool, len(targetScalables))
	for _, v := range targetScalables {
		targetScalablesSet[v] = true
	}

	scales := c.scaleNamespacer.Scales(targetNamespace)
	for _, scalableRef := range targetScalables {
		scale, err := scales.Get(scalableRef.Kind, scalableRef.Name)
		if err != nil {
			glog.Errorf("Unable get scale for %s %q while unidling service %s/%s: %v", scalableRef.Kind, scalableRef.Name, targetNamespace, targetServiceName, err)
			if errors.IsNotFound(err) {
				delete(targetScalablesSet, scalableRef)
			}
			continue
		}

		if scale.Spec.Replicas > 0 {
			glog.Errorf("%s %q is not idle, skipping while unidling service %s/%s", scalableRef.Kind, scalableRef.Name, targetNamespace, targetServiceName)
			continue
		}

		scale.Spec.Replicas = 1

		if _, err := scales.Update(scalableRef.Kind, scale); err != nil {
			glog.Errorf("Unable to scale up %s %q while unidling service %s/%s: %v", scalableRef.Kind, scalableRef.Name, targetNamespace, targetServiceName, err)
			if errors.IsNotFound(err) {
				delete(targetScalablesSet, scalableRef)
			}
			continue
		} else {
			glog.V(4).Infof("Scaled up %s %q while unidling service %s/%s", scalableRef.Kind, scalableRef.Name, targetNamespace, targetServiceName)
		}

		delete(targetScalablesSet, scalableRef)
	}

	newAnnotationList := make([]kextapi.SubresourceReference, 0, len(targetScalablesSet))
	for k := range targetScalablesSet {
		newAnnotationList = append(newAnnotationList, k)
	}

	// TODO(directxman12): do we need to re-fetch the service to avoid conflicts while updating?
	if len(newAnnotationList) == 0 {
		delete(targetEndpoints.Annotations, unidling.UnidleTargetAnnotation)
		delete(targetEndpoints.Annotations, unidling.IdledAtAnnotation)
	} else {
		newAnnotationBytes, err := json.Marshal(newAnnotationList)
		if err != nil {
			return fmt.Errorf("unable to update/remove idle annotations from %s/%s: unable to marshal list of remaining scalables: %v", targetNamespace, targetServiceName, err)
		}
		targetEndpoints.Annotations[unidling.UnidleTargetAnnotation] = string(newAnnotationBytes)
	}

	if _, err := c.endpointsNamespacer.Endpoints(targetNamespace).Update(targetEndpoints); err != nil {
		return fmt.Errorf("unable to update/remove idle annotations from %s/%s: %v", targetNamespace, targetServiceName, err)
	}

	return nil
}
