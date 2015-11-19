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
		return c.reconcileExistingDeployments(existingDeployments, config)
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

// reconcileExistingDeployments reconciles existing deployment replica counts
// which could have diverged outside the deployment process (e.g. due to auto
// or manual scaling). The active deployment is the last successful
// deployment, not necessarily the latest in terms of the config version. The
// active deployment replica count should follow the config, and all other
// deployments should be scaled to zero.
//
// Previously, scaling behavior was that the config replica count was used
// only for initial deployments and the active deployment had to be scaled up
// directly. To continue supporting that old behavior we must detect when the
// deployment has been directly manipulated, and if so, preserve the directly
// updated value and sync the config with the deployment.
func (c *DeploymentConfigController) reconcileExistingDeployments(existingDeployments *kapi.ReplicationControllerList, config *deployapi.DeploymentConfig) error {
	activeDeployment := deployutil.ActiveDeployment(config, existingDeployments)
	glog.Infof("reconciling")
	if activeDeployment != nil {
		glog.Infof("active deployment %s (%s)", activeDeployment.Name, deployutil.DeploymentStatusFor(activeDeployment))
	}
	for _, deployment := range existingDeployments.Items {
		isActiveDeployment := activeDeployment != nil && deployment.Name == activeDeployment.Name
		glog.Infof("deployment %s replicas %d", deployment.Name, deployment.Spec.Replicas)
		oldReplicaCount := deployment.Spec.Replicas
		var newReplicaCount int
		// The active deployment should be scaled to the config replica count, and
		// everything else should be at zero.
		if isActiveDeployment {
			lastSyncedReplicaCountStr, syncedPreviously := deployment.Annotations["openshift.io/deployment.replicas"]
			if syncedPreviously {
				// The RC has been synced by the controller at least once before, so
				// reconcile it with the config and against any external updates to
				// the RC's replica count.
				lastSyncedReplicaCount, err := strconv.Atoi(lastSyncedReplicaCountStr)
				if err != nil {
					// Replace the invalid value with a new valid one.
					glog.Infof("setting newReplicaCount for %s A", deployment.Name)
					newReplicaCount = config.Template.ControllerTemplate.Replicas
					// TODO: Should this be logged?
				} else {
					if lastSyncedReplicaCount == deployment.Spec.Replicas {
						// The RC hasn't been manipulated externally since the last
						// sync, so let the RC follow the config value.
						glog.Infof("setting newReplicaCount for %s B", deployment.Name)
						newReplicaCount = config.Template.ControllerTemplate.Replicas
					} else {
						// The RC was updated by something other than the controller;
						// preserve the externally updated value and let the config be
						// synced to match the RC.
						glog.Infof("setting newReplicaCount for %s C", deployment.Name)
						newReplicaCount = oldReplicaCount
					}
				}
			} else {
				// The RC has not yet been synced by the controller. If the active
				// replica count is greater than 0, use it as the new basis for
				// reconciliation. Otherwise, fall back to the config and assume the
				// deployment is an old one being scaled up due to a later version
				// failure.
				//
				// TODO: This assumption means we'll scale up an older deployment even
				// if it was explicitly scaled to 0, but that edge case seems more
				// tolerable than further complexity at this moment.
				if oldReplicaCount > 0 {
					newReplicaCount = oldReplicaCount
					glog.Infof("setting newReplicaCount %d for %s D", newReplicaCount, deployment.Name)
				} else {
					// Since we (for some reason) delete the desired annotation from
					// complete deployments, we'll have to use the desired count of the
					// latest (failed) deployment and  fall back to the config if that
					// value's not present.
					hasLatest, latestDeployment := deployutil.LatestDeploymentInfo(config, existingDeployments)
					if hasLatest {
						glog.Infof("setting newReplicaCount for %s E", deployment.Name)
						newReplicaCount = latestDeployment.Spec.Replicas
					} else {
						glog.Infof("setting newReplicaCount for %s F", deployment.Name)
						newReplicaCount = config.Template.ControllerTemplate.Replicas
					}
				}
			}
		} else {
			// All RCs other than the active deployment should be scaled to 0.
			newReplicaCount = 0
			glog.Infof("setting newReplicaCount for %s F", deployment.Name)
		}
		// Keep the config in sync with the active deployment. This logic can be
		// removed if direct deployment scaling becomes prohibited in the future.
		if isActiveDeployment && newReplicaCount != config.Template.ControllerTemplate.Replicas {
			config.Template.ControllerTemplate.Replicas = newReplicaCount
			updated, err := c.osClient.DeploymentConfigs(config.Namespace).Update(config)
			if err != nil {
				c.recorder.Eventf(config, "DeploymentConfigReplicasSyncFailed",
					"Failed to update deploymentConfig %q replicas to %s to match deployment %q following detection of external scaling of the deployment: %s",
					config.Name, newReplicaCount, deployment.Name, err)
				return err
			}
			config = updated
			c.recorder.Eventf(config, "DeploymentConfigReplicasSynced",
				"Updated deploymentConfig %q replicas to %d to match deployment %q because external scaling of the deployment was detected",
				config.Name, newReplicaCount, deployment.Name)
		}
		// Only process updates if the replica count actually changed.
		if oldReplicaCount != newReplicaCount {
			deployment.Spec.Replicas = newReplicaCount
			deployment.Annotations["openshift.io/deployment.replicas"] = strconv.Itoa(newReplicaCount)
			_, err := c.kubeClient.ReplicationControllers(deployment.Namespace).Update(&deployment)
			if err != nil {
				c.recorder.Eventf(config, "DeploymentScaleFailed",
					"Failed to scale deployment %q from %d to %d: %s", deployment.Name, oldReplicaCount, newReplicaCount, err)
				return err
			}
			c.recorder.Eventf(config, "DeploymentScaled",
				"Scaled deployment %q from %d to %d", deployment.Name, oldReplicaCount, newReplicaCount)
		}
	}
	return nil
}
