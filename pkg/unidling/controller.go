package unidling

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/client/cache"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/unidling"
	"k8s.io/kubernetes/pkg/watch"

	"github.com/golang/glog"
)

type UnidlingController struct {
	controller          *framework.Controller
	scaleNamespacer     kclient.ScaleNamespacer
	endpointsNamespacer kclient.EndpointsNamespacer
}

func NewUnidlingController(scaleNS kclient.ScaleNamespacer, endptsNS kclient.EndpointsNamespacer, evtNS kclient.EventNamespacer, resyncPeriod time.Duration) *UnidlingController {
	fieldSet := fields.Set{}
	fieldSet["reason"] = unidling.NeedPodsReason
	fieldSelector := fieldSet.AsSelector()

	unidlingController := &UnidlingController{
		scaleNamespacer:     scaleNS,
		endpointsNamespacer: endptsNS,
	}

	_, controller := framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return evtNS.Events(api.NamespaceAll).List(labels.Everything(), fieldSelector)
			},
			WatchFunc: func(resourceVersion string) (watch.Interface, error) {
				return evtNS.Events(api.NamespaceAll).Watch(labels.Everything(), fieldSelector, resourceVersion)
			},
		},
		&api.Event{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				event := obj.(*api.Event)

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
	defer util.HandleCrash()
	go c.controller.Run(stopCh)
}

// TODO: does this need to send error events?
func (c *UnidlingController) handleEvent(event *api.Event) error {
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

	var targetScalablesRaw []string
	if targetScalablesStr, hasTargetScalables := targetEndpoints.Annotations[unidling.UnidleTargetAnnotation]; hasTargetScalables {
		targetScalablesRaw = strings.Split(targetScalablesStr, ",")
	} else {
		glog.V(4).Infof("Service %s/%s had no scalables to unidle", targetNamespace, targetServiceName)
		targetScalablesRaw = []string{}
	}

	targetScalablesSet := make(map[string]bool, len(targetScalablesRaw))
	for _, v := range targetScalablesRaw {
		targetScalablesSet[v] = true
	}

	scales := c.scaleNamespacer.Scales(targetNamespace)
	for _, scalableRef := range targetScalablesRaw {
		kind, name, err := extractScalableInfo(scalableRef)
		if err != nil {
			glog.Errorf("Invalid scalable reference while unidling service %s/%s: %v", targetNamespace, targetServiceName, err)
			delete(targetScalablesSet, scalableRef)
			continue
		}

		scale, err := scales.Get(kind, name)
		if err != nil {
			glog.Errorf("Unable get scale for %s while unidling service %s/%s: %v", scalableRef, targetNamespace, targetServiceName, err)
			if errors.IsNotFound(err) {
				delete(targetScalablesSet, scalableRef)
			}
			continue
		}

		if scale.Spec.Replicas > 0 {
			glog.Errorf("%s is not idle, skipping while unidling service %s/%s", scalableRef, targetNamespace, targetServiceName)
			continue
		}

		scale.Spec.Replicas = 1

		if _, err := scales.Update(kind, scale); err != nil {
			glog.Errorf("Unable to scale up %s while unidling service %s/%s: %v", scalableRef, err)
			if errors.IsNotFound(err) {
				delete(targetScalablesSet, scalableRef)
			}
			continue
		} else {
			glog.V(4).Infof("Scaled up %s while unidling service %s/%s", scalableRef, targetNamespace, targetServiceName)
		}

		delete(targetScalablesSet, scalableRef)
	}

	newAnnotationList := make([]string, 0, len(targetScalablesSet))
	for k := range targetScalablesSet {
		newAnnotationList = append(newAnnotationList, k)
	}

	// TODO(directxman12): do we need to re-fetch the service to avoid conflicts while updating?
	if len(newAnnotationList) == 0 {
		delete(targetEndpoints.Annotations, unidling.UnidleTargetAnnotation)
		delete(targetEndpoints.Annotations, unidling.IdledAtAnnotation)
	} else {
		targetEndpoints.Annotations[unidling.UnidleTargetAnnotation] = strings.Join(newAnnotationList, ",")
	}

	if _, err := c.endpointsNamespacer.Endpoints(targetNamespace).Update(targetEndpoints); err != nil {
		return fmt.Errorf("unable to update/remove idle annotations from %s/%s: %v")
	}

	return nil
}

func extractScalableInfo(scalableRef string) (string, string, error) {
	// TODO: this should probably be something more concrete than "resource/name" (or here, for convenience, Kind/name)
	refParts := strings.Split(scalableRef, "/")
	if len(refParts) != 2 {
		return "", "", fmt.Errorf("%s was not in the form kind/name", scalableRef)
	}

	return refParts[0], refParts[1], nil
}
