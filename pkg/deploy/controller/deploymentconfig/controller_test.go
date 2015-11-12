package deploymentconfig

import (
	"sort"
	"strings"
	"testing"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/record"
	ktestclient "k8s.io/kubernetes/pkg/client/unversioned/testclient"
	"k8s.io/kubernetes/pkg/runtime"
	kutil "k8s.io/kubernetes/pkg/util"

	deployapi "github.com/openshift/origin/pkg/deploy/api"
	deploytest "github.com/openshift/origin/pkg/deploy/api/test"
	deployutil "github.com/openshift/origin/pkg/deploy/util"
)

func TestHandleScenarios(t *testing.T) {
	type deployment struct {
		version   int
		replicas  int
		status    deployapi.DeploymentStatus
		cancelled bool
	}

	mkdeployment := func(d deployment) kapi.ReplicationController {
		deployment, _ := deployutil.MakeDeployment(deploytest.OkDeploymentConfig(d.version), kapi.Codec)
		deployment.Annotations[deployapi.DeploymentStatusAnnotation] = string(d.status)
		if d.cancelled {
			deployment.Annotations[deployapi.DeploymentCancelledAnnotation] = deployapi.DeploymentCancelledAnnotationValue
			deployment.Annotations[deployapi.DeploymentStatusReasonAnnotation] = deployapi.DeploymentCancelledNewerDeploymentExists
		}
		deployment.Spec.Replicas = d.replicas
		return *deployment
	}

	tests := []struct {
		name        string
		newVersion  int
		before      []deployment
		after       []deployment
		errExpected bool
	}{
		{
			name:        "version is zero",
			newVersion:  0,
			before:      []deployment{},
			after:       []deployment{},
			errExpected: false,
		},
		{
			name:       "initial deployment of this version",
			newVersion: 1,
			before:     []deployment{},
			after: []deployment{
				{version: 1, replicas: 0, status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:       "this version deployment in progress (no older deployments present)",
			newVersion: 1,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:       "this version deployment in progress (older deployments present)",
			newVersion: 2,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:       "this version already deployed (no older deployments present)",
			newVersion: 1,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:       "this version awaiting cancellation of older deployments (not yet cancelled)",
			newVersion: 3,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusRunning, cancelled: false},
				{version: 2, replicas: 1, status: deployapi.DeploymentStatusRunning, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusRunning, cancelled: true},
				{version: 2, replicas: 1, status: deployapi.DeploymentStatusRunning, cancelled: true},
			},
			errExpected: true,
		},
		{
			name:       "this version awaiting cancellation of older deployments (previously cancelled)",
			newVersion: 2,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusRunning, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusRunning, cancelled: true},
			},
			errExpected: true,
		},
		{
			name:       "this version already deployed with replica count divergence (latest != active)",
			newVersion: 5,
			before: []deployment{
				{version: 1, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 1, status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 5, replicas: 1, status: deployapi.DeploymentStatusFailed, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 0, status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 5, replicas: 0, status: deployapi.DeploymentStatusFailed, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:       "this version already deployed with replica count divergence (latest == active)",
			newVersion: 5,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 1, status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 1, status: deployapi.DeploymentStatusFailed, cancelled: false},
				{version: 5, replicas: 2, status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 0, status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 0, status: deployapi.DeploymentStatusFailed, cancelled: false},
				{version: 5, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
	}

	for _, test := range tests {
		t.Logf("evaluating test: %s", test.name)

		deployments := map[string]kapi.ReplicationController{}
		for _, template := range test.before {
			deployment := mkdeployment(template)
			deployments[deployment.Name] = deployment
		}

		kubeClient := &ktestclient.Fake{}
		kubeClient.AddReactor("list", "replicationcontrollers", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
			list := []kapi.ReplicationController{}
			for _, deployment := range deployments {
				list = append(list, deployment)
			}
			return true, &kapi.ReplicationControllerList{Items: list}, nil
		})
		kubeClient.AddReactor("create", "replicationcontrollers", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
			rc := action.(ktestclient.CreateAction).GetObject().(*kapi.ReplicationController)
			deployments[rc.Name] = *rc
			return true, rc, nil
		})
		kubeClient.AddReactor("update", "replicationcontrollers", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
			rc := action.(ktestclient.UpdateAction).GetObject().(*kapi.ReplicationController)
			deployments[rc.Name] = *rc
			return true, rc, nil
		})

		recorder := &record.FakeRecorder{}
		controller := &DeploymentConfigController{
			kubeClient: kubeClient,
			codec:      kapi.Codec,
			recorder:   recorder,
		}

		config := deploytest.OkDeploymentConfig(test.newVersion)
		err := controller.Handle(config)
		if err != nil {
			if test.errExpected {
				t.Logf("got expected error: %s", err)
			} else {
				t.Errorf("unexpected error: %s", err)
				continue
			}
		}

		expectedDeployments := []kapi.ReplicationController{}
		for _, template := range test.after {
			expectedDeployments = append(expectedDeployments, mkdeployment(template))
		}
		actualDeployments := []kapi.ReplicationController{}
		for _, deployment := range deployments {
			actualDeployments = append(actualDeployments, deployment)
		}
		sort.Sort(deployutil.ByLatestVersionDesc(expectedDeployments))
		sort.Sort(deployutil.ByLatestVersionDesc(actualDeployments))

		if !kapi.Semantic.DeepEqual(expectedDeployments, actualDeployments) {
			t.Errorf("actual deployments don't match expectations: %v", kutil.ObjectDiff(expectedDeployments, actualDeployments))
			if len(recorder.Events) > 0 {
				t.Logf("events:\n%s", strings.Join(recorder.Events, "\t\n"))
			}
			return
		}
	}
}
