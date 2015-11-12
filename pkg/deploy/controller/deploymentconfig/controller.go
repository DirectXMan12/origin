package deploymentconfig

import (
	"fmt"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/client/record"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/runtime"

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
	inflightDeploymentsExist := false
	if !latestDeploymentExists {
		for _, deployment := range existingDeployments.Items {
			// Skip deployments with an outcome.
			if deployutil.IsTerminatedDeployment(&deployment) {
				continue
			}
			// Cancel running deployments.
			inflightDeploymentsExist = true
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
	// Wait for deployment cancellations before doing any more scaling or
	// creating a new deployment to avoid competing with existing deployment
	// processes.
	if inflightDeploymentsExist {
		c.recorder.Eventf(config, "DeploymentAwaitingCancellation", "Deployment of version %d awaiting cancellation of older running deployments", config.LatestVersion)
		// raise a transientError so that the deployment config can be re-queued
		return transientError(fmt.Sprintf("found previous inflight deployment for %s - requeuing", deployutil.LabelForDeploymentConfig(config)))
	}
	// If the latest deployment already exists, reconcile replica counts and
	// return early.
	if latestDeploymentExists {
		// If the latest deployment exists and is still running, return early and
		// reconcile later. We don't want to compete with the deployer.
		if !deployutil.IsTerminatedDeployment(latestDeployment) {
			return nil
		}
		// Reconcile existing deployment replica counts which could have diverged
		// outside the deployment process (e.g. due to auto or manual scaling).
		// The active deployment is the last successful deployment, not
		// necessarily the latest in terms of the config version. If the latest
		// version failed to deploy, it should be scaled to zero and the last
		// successful should be scaled back up.
		activeDeployment := deployutil.ActiveDeployment(config, existingDeployments)
		for _, deployment := range existingDeployments.Items {
			oldReplicaCount := deployment.Spec.Replicas
			newReplicaCount := oldReplicaCount
			// The active deployment should be scaled to the config replica count, and
			// everything else should be at zero.
			if activeDeployment != nil && deployment.Name == activeDeployment.Name {
				newReplicaCount = config.Template.ControllerTemplate.Replicas
			} else {
				newReplicaCount = 0
			}
			// Only process updates if the replica count actually changed.
			if oldReplicaCount != newReplicaCount {
				deployment.Spec.Replicas = newReplicaCount
				_, err := c.kubeClient.ReplicationControllers(deployment.Namespace).Update(&deployment)
				if err != nil {
					c.recorder.Eventf(config, "DeploymentScaleFailed", "Failed to scale deployment %q from %d to %d: %s", deployment.Name, oldReplicaCount, newReplicaCount, err)
					return err
				}
				c.recorder.Eventf(config, "DeploymentScaled", "Scaled deployment %q from %d to %d", deployment.Name, oldReplicaCount, newReplicaCount)
			}
		}
		return nil
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
