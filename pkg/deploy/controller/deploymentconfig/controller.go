package deploymentconfig

import (
	"fmt"
	"strconv"

	"github.com/golang/glog"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/client/record"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/runtime"

	osclient "github.com/openshift/origin/pkg/client"
	deployapi "github.com/openshift/origin/pkg/deploy/api"
	deployutil "github.com/openshift/origin/pkg/deploy/util"
)

// DeploymentConfigController is responsible for creating a new deployment
// when:
//
//    1. The config version is > 0 and,
//    2. No deployment for the version exists.
//
// The controller reconciles deployments with the replica count specified on
// the config. The active deployment (that is, the latest successful
// deployment) will always be scaled to the config replica count. All other
// deployments will be scaled to zero.
//
// If a new version is observed for which no deployment exists, any running
// deployments will be cancelled. The controller will not attempt to scale
// running deployments.
type DeploymentConfigController struct {
	// kubeClient provides acceess to Kube resources.
	kubeClient kclient.Interface
	// osClient provides access to OpenShift resources.
	osClient osclient.Interface
	// codec is used to build deployments from configs.
	codec runtime.Codec
	// recorder is used to record events.
	recorder record.EventRecorder
}

// fatalError is an error which can't be retried.
type fatalError string

// transientError is an error which should always be retried (indefinitely).
type transientError string

func (e fatalError) Error() string {
	return fmt.Sprintf("fatal error handling deployment config: %s", string(e))
}
func (e transientError) Error() string {
	return "transient error handling deployment config: " + string(e)
}

func NewDeploymentConfigController(kubeClient kclient.Interface, codec runtime.Codec, recorder record.EventRecorder) *DeploymentConfigController {
	return &DeploymentConfigController{
		kubeClient: kubeClient,
		codec:      codec,
		recorder:   recorder,
	}
}

func (c *DeploymentConfigController) Handle(config *deployapi.DeploymentConfig) error {
	// There's nothing to reconcile until the version is nonzero.
	if config.LatestVersion == 0 {
		glog.V(5).Infof("Waiting for first version of %s", deployutil.LabelForDeploymentConfig(config))
		return nil
	}

	// Find all deployments owned by the deploymentConfig.
	sel := deployutil.ConfigSelector(config.Name)
	existingDeployments, err := c.kubeClient.ReplicationControllers(config.Namespace).List(sel, fields.Everything())
	if err != nil {
		return err
	}

	latestDeploymentExists, latestDeployment := deployutil.LatestDeploymentInfo(config, existingDeployments)
	// If the latest deployment doesn't exist yet, cancel any running
	// deployments to allow them to be superceded by the new config version.
	awaitingCancellations := false
	if !latestDeploymentExists {
		for _, deployment := range existingDeployments.Items {
			// Skip deployments with an outcome.
			if deployutil.IsTerminatedDeployment(&deployment) {
				continue
			}
			// Cancel running deployments.
			awaitingCancellations = true
			if !deployutil.IsDeploymentCancelled(&deployment) {
				deployment.Annotations[deployapi.DeploymentCancelledAnnotation] = deployapi.DeploymentCancelledAnnotationValue
				deployment.Annotations[deployapi.DeploymentStatusReasonAnnotation] = deployapi.DeploymentCancelledNewerDeploymentExists
				_, err := c.kubeClient.ReplicationControllers(deployment.Namespace).Update(&deployment)
				if err != nil {
					c.recorder.Eventf(config, "DeploymentCancellationFailed", "Failed to cancel deployment %q superceded by version %d: %s", deployment.Name, config.LatestVersion, err)
				} else {
					c.recorder.Eventf(config, "DeploymentCancelled", "Cancelled deployment %q superceded by version %d", deployment.Name, config.LatestVersion)
				}
			}
		}
	}
	// Wait for deployment cancellations before reconciling or creating a new
	// deployment to avoid competing with existing deployment processes.
	if awaitingCancellations {
		c.recorder.Eventf(config, "DeploymentAwaitingCancellation", "Deployment of version %d awaiting cancellation of older running deployments", config.LatestVersion)
		// raise a transientError so that the deployment config can be re-queued
		return transientError(fmt.Sprintf("found previous inflight deployment for %s - requeuing", deployutil.LabelForDeploymentConfig(config)))
	}
	// If the latest deployment already exists, reconcile existing deployments
	// and return early.
	if latestDeploymentExists {
		// If the latest deployment is still running, try again later. We don't
		// want to compete with the deployer.
		if !deployutil.IsTerminatedDeployment(latestDeployment) {
			return nil
		}
		return c.reconcileDeployments(existingDeployments, config)
	}
	// No deployments are running and the latest deployment doesn't exist, so
	// create the new deployment.
	deployment, err := deployutil.MakeDeployment(config, c.codec)
	if err != nil {
		return fatalError(fmt.Sprintf("couldn't make deployment from (potentially invalid) deployment config %s: %v", deployutil.LabelForDeploymentConfig(config), err))
	}
	created, err := c.kubeClient.ReplicationControllers(config.Namespace).Create(deployment)
	if err != nil {
		// If the deployment was already created, just move on. The cache could be
		// stale, or another process could have already handled this update.
		if errors.IsAlreadyExists(err) {
			return nil
		}
		c.recorder.Eventf(config, "DeploymentCreationFailed", "Couldn't deploy version %d: %s", config.LatestVersion, err)
		return fmt.Errorf("couldn't create deployment for deployment config %s: %v", deployutil.LabelForDeploymentConfig(config), err)
	}
	c.recorder.Eventf(config, "DeploymentCreated", "Created new deployment %q for version %d", created.Name, config.LatestVersion)
	return nil
}

// reconcileDeployments reconciles existing deployment replica counts which
// could have diverged outside the deployment process (e.g. due to auto or
// manual scaling, or partially deployments). The active deployment is the
// last successful deployment, not necessarily the latest in terms of the
// config version. The active deployment replica count should follow the
// config, and all other deployments should be scaled to zero.
//
// Previously, scaling behavior was that the config replica count was used
// only for initial deployments and the active deployment had to be scaled up
// directly. To continue supporting that old behavior we must detect when the
// deployment has been directly manipulated, and if so, preserve the directly
// updated value and sync the config with the deployment.
func (c *DeploymentConfigController) reconcileDeployments(existingDeployments *kapi.ReplicationControllerList, config *deployapi.DeploymentConfig) error {
	activeDeployment := deployutil.ActiveDeployment(config, existingDeployments)
	_, latestDeployment := deployutil.LatestDeploymentInfo(config, existingDeployments)
	activeDeploymentIsLatest := activeDeployment.Name == latestDeployment.Name
	lastActiveReplicas, hasLastActiveReplicas := deployutil.DeploymentReplicas(activeDeployment)

	// Compute the replica count for the active deployment (even if the active
	// deployment doesn't exist). The active replica count is the value that
	// should be assigned to the config, to allow the replica propagation to
	// flow downward from the config.
	//
	// This takes into account resources predating the propagation behavior
	// change, as well as external modifications to the deployments (e.g.
	// scalers).
	activeReplicas := config.Template.ControllerTemplate.Replicas
	if activeDeployment != nil {
		if activeDeploymentIsLatest {
			if !hasLastActiveReplicas || lastActiveReplicas != activeDeployment.Spec.Replicas {
				activeReplicas = activeDeployment.Spec.Replicas
			}
		} else {
			if activeDeployment.Spec.Replicas > 0 {
				activeReplicas = activeDeployment.Spec.Replicas
			}
			if latestDeployment != nil {
				latestDesiredReplicas, latestHasDesiredReplicas := deployutil.DeploymentDesiredReplicas(latestDeployment)
				if latestHasDesiredReplicas {
					activeReplicas = latestDesiredReplicas
				}
			}
		}
	} else {
		if latestDeployment != nil {
			latestDesiredReplicas, latestHasDesiredReplicas := deployutil.DeploymentDesiredReplicas(latestDeployment)
			if latestHasDesiredReplicas {
				activeReplicas = latestDesiredReplicas
			}
		}
	}
	// Bring the config in sync with the deployment. Once we know the config
	// accurately represents the desired replica count of the active deployment,
	// we can safely reconcile deployments.
	if config.Template.ControllerTemplate.Replicas != activeReplicas {
		config.Template.ControllerTemplate.Replicas = activeReplicas
		_, err := c.osClient.DeploymentConfigs(config.Namespace).Update(config)
		if err != nil {
			return err
		}
	}
	// Reconcile deployments. The active deployment follows the config, and all
	// other deployments should be scaled to zero.
	for _, deployment := range existingDeployments.Items {
		isActiveDeployment := activeDeployment != nil && deployment.Name == activeDeployment.Name

		oldReplicaCount := deployment.Spec.Replicas
		newReplicaCount := 0
		if isActiveDeployment {
			newReplicaCount = activeReplicas
		}
		deployment.Spec.Replicas = newReplicaCount
		deployment.Annotations[deployapi.DeploymentReplicasAnnotation] = strconv.Itoa(newReplicaCount)
		_, err := c.kubeClient.ReplicationControllers(deployment.Namespace).Update(&deployment)
		if err != nil {
			c.recorder.Eventf(config, "DeploymentScaleFailed",
				"Failed to scale deployment %q from %d to %d: %s", deployment.Name, oldReplicaCount, newReplicaCount, err)
			return err
		}
		c.recorder.Eventf(config, "DeploymentScaled",
			"Scaled deployment %q from %d to %d", deployment.Name, oldReplicaCount, newReplicaCount)
	}
	return nil
}
