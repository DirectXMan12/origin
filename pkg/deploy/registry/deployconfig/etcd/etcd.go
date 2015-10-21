package etcd

import (
	"fmt"

	kapi "k8s.io/kubernetes/pkg/api"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/registry/generic"
	etcdgeneric "k8s.io/kubernetes/pkg/registry/generic/etcd"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/storage"

	"github.com/openshift/origin/pkg/deploy/api"
	"github.com/openshift/origin/pkg/deploy/registry/deployconfig"
	"github.com/openshift/origin/pkg/deploy/util"
)

const DeploymentConfigPath string = "/deploymentconfigs"

// DeploymentConfigStorage contains the REST storage information for both DeploymentConfigs
// and their Scale subresources.
type DeploymentConfigStorage struct {
	DeploymentConfig *REST
	Scale            *ScaleREST
}

// NewStorage returns a DeploymentConfigStorage containing the REST storage for
// DeploymentConfig objects and their Scale subresources.
func NewStorage(s storage.Interface, rcNamespacer kclient.ReplicationControllersNamespacer) DeploymentConfigStorage {
	deploymentConfigREST := newREST(s)
	deploymentConfigRegistry := deployconfig.NewRegistry(deploymentConfigREST)
	return DeploymentConfigStorage{
		DeploymentConfig: deploymentConfigREST,
		Scale: &ScaleREST{
			registry: &deploymentConfigRegistry,
			rcNamespacer: rcNamespacer,
		},
	}
}

// REST contains the REST storage for DeploymentConfig objects.
type REST struct {
	*etcdgeneric.Etcd
}

// newREST returns a RESTStorage object that will work against DeploymentConfig objects.
func newREST(s storage.Interface) *REST {
	store := &etcdgeneric.Etcd{
		NewFunc:      func() runtime.Object { return &api.DeploymentConfig{} },
		NewListFunc:  func() runtime.Object { return &api.DeploymentConfigList{} },
		EndpointName: "deploymentConfig",
		KeyRootFunc: func(ctx kapi.Context) string {
			return etcdgeneric.NamespaceKeyRootFunc(ctx, DeploymentConfigPath)
		},
		KeyFunc: func(ctx kapi.Context, id string) (string, error) {
			return etcdgeneric.NamespaceKeyFunc(ctx, DeploymentConfigPath, id)
		},
		ObjectNameFunc: func(obj runtime.Object) (string, error) {
			return obj.(*api.DeploymentConfig).Name, nil
		},
		PredicateFunc: func(label labels.Selector, field fields.Selector) generic.Matcher {
			return deployconfig.Matcher(label, field)
		},
		CreateStrategy:      deployconfig.Strategy,
		UpdateStrategy:      deployconfig.Strategy,
		DeleteStrategy:      deployconfig.Strategy,
		ReturnDeletedObject: false,
		Storage:             s,
	}

	return &REST{store}
}

// ScaleREST contains the REST storage for the Scale subresource of DeploymentConfigs.
type ScaleREST struct {
	registry     *deployconfig.Registry
	rcNamespacer kclient.ReplicationControllersNamespacer
}

// ScaleREST implements Patcher
var _ = rest.Patcher(&ScaleREST{})

// New creates a new Scale object
func (r *ScaleREST) New() runtime.Object {
	return &extensions.Scale{}
}

// Get retrieves (computes) the Scale subresource for the given DeploymentConfig name.
func (r *ScaleREST) Get(ctx kapi.Context, name string) (runtime.Object, error) {
	deployment, err := (*r.registry).GetDeploymentConfig(ctx, name)
	if err != nil {
		return nil, errors.NewNotFound("scale", name)
	}

	// TODO(directxman12): this is going to be a bit out of sync, since we are calculating it
	// here and not as part of the deploymentconfig loop -- is there a better way of doing it?
	totalReplicas, err := r.replicasForDeploymentConfig(deployment.Namespace, deployment.Name)
	if err != nil {
		return nil, err
	}

	var specReplicas int

	// current replicas reflects either the scale of the current deployment,
	// or the scale of the RC template if no current deployment exists
	controller, err := r.rcNamespacer.ReplicationControllers(deployment.Namespace).Get(util.LatestDeploymentNameForConfig(deployment))
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, err
		}

		specReplicas = deployment.Template.ControllerTemplate.Replicas
	} else {
		specReplicas = controller.Spec.Replicas
	}

	return &extensions.Scale{
		ObjectMeta: kapi.ObjectMeta{
			Name:              name,
			Namespace:         deployment.Namespace,
			CreationTimestamp: deployment.CreationTimestamp,
		},
		Spec: extensions.ScaleSpec{
			Replicas: specReplicas,
		},
		Status: extensions.ScaleStatus{
			Replicas: totalReplicas,
			Selector: deployment.Template.ControllerTemplate.Selector,
		},
	}, nil
}

// Update scales the DeploymentConfig for the given Scale subresource, returning the updated Scale.
func (r *ScaleREST) Update(ctx kapi.Context, obj runtime.Object) (runtime.Object, bool, error) {
	if obj == nil {
		return nil, false, errors.NewBadRequest(fmt.Sprintf("nil update passed to Scale"))
	}
	scale, ok := obj.(*extensions.Scale)
	if !ok {
		return nil, false, errors.NewBadRequest(fmt.Sprintf("wrong object passed to Scale update: %v", obj))
	}
	deployment, err := (*r.registry).GetDeploymentConfig(ctx, scale.Name)
	if err != nil {
		return nil, false, errors.NewNotFound("scale", scale.Name)
	}

	useDC := false

	// this tries to update the current deployment RC first, since updating
	// Replicas on a DeploymentConfig doesn't do anything after the first deployment
	// has been made
	controller, err := r.rcNamespacer.ReplicationControllers(deployment.Namespace).Get(util.LatestDeploymentNameForConfig(deployment))
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, false, err
		}

		useDC = true
	}

	if deploymentStatus := util.DeploymentStatusFor(deployment); deploymentStatus != api.DeploymentStatusComplete {
		return nil, false, errors.NewConflict("Scale", scale.Name, fmt.Errorf("deployment currently in progress or failed"))
	}

	// TODO(directxman12): this is going to be a bit out of sync, since we are calculating it
	// here and not as part of the deploymentconfig loop -- is there a better way of doing it?
	totalReplicas, err := r.replicasForDeploymentConfig(deployment.Namespace, deployment.Name)
	if err != nil {
		return nil, false, err
	}

	if useDC {
		deployment.Template.ControllerTemplate.Replicas = scale.Spec.Replicas
		if err = (*r.registry).UpdateDeploymentConfig(ctx, deployment); err != nil {
			return nil, false, errors.NewConflict("scale", scale.Name, err)
		}
	} else {
		oldReplicas := controller.Spec.Replicas
		controller.Spec.Replicas = scale.Spec.Replicas
		if _, err = r.rcNamespacer.ReplicationControllers(deployment.Namespace).Update(controller); err != nil {
			return nil, false, errors.NewConflict("scale", scale.Name, err)
		}
		totalReplicas += (scale.Spec.Replicas - oldReplicas)
	}

	return &extensions.Scale{
		ObjectMeta: kapi.ObjectMeta{
			Name:              deployment.Name,
			Namespace:         deployment.Namespace,
			CreationTimestamp: deployment.CreationTimestamp,
		},
		Spec: extensions.ScaleSpec{
			Replicas: scale.Spec.Replicas,
		},
		Status: extensions.ScaleStatus{
			Replicas: totalReplicas,
			Selector: deployment.Template.ControllerTemplate.Selector,
		},
	}, false, nil
}

func (r *ScaleREST) deploymentsForConfig(namespace, configName string) (*kapi.ReplicationControllerList, error) {
	selector := util.ConfigSelector(configName)
	return r.rcNamespacer.ReplicationControllers(namespace).List(selector, fields.Everything())
}

func (r *ScaleREST) replicasForDeploymentConfig(namespace, configName string) (int, error) {
	rcList, err := r.deploymentsForConfig(namespace, configName)
	if err != nil {
		return 0, err
	}

	replicas := 0
	for _, rc := range rcList.Items {
		replicas += rc.Spec.Replicas
	}

	return replicas, nil
}
