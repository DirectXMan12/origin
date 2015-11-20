package deploymentconfig

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/record"
	ktestclient "k8s.io/kubernetes/pkg/client/unversioned/testclient"
	"k8s.io/kubernetes/pkg/runtime"
	kutil "k8s.io/kubernetes/pkg/util"

	"github.com/openshift/origin/pkg/client/testclient"
	deployapi "github.com/openshift/origin/pkg/deploy/api"
	deploytest "github.com/openshift/origin/pkg/deploy/api/test"
	deployutil "github.com/openshift/origin/pkg/deploy/util"
)

func TestHandleScenarios(t *testing.T) {
	type deployment struct {
		// version is the deployment version
		version int
		// replicas is the spec replicas of the deployment
		replicas int
		// replicasA is the annotated replica value for backwards compat checks
		replicasA *int
		desiredA  *int
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
		if d.replicasA != nil {
			deployment.Annotations[deployapi.DeploymentReplicasAnnotation] = strconv.Itoa(*d.replicasA)
		} else {
			delete(deployment.Annotations, deployapi.DeploymentReplicasAnnotation)
		}
		if d.desiredA != nil {
			deployment.Annotations[deployapi.DesiredReplicasAnnotation] = strconv.Itoa(*d.desiredA)
		} else {
			delete(deployment.Annotations, deployapi.DesiredReplicasAnnotation)
		}
		deployment.Spec.Replicas = d.replicas
		return *deployment
	}

	tests := []struct {
		name string
		// replicas is the config replicas prior to the update
		replicas int
		// newVersion is the version of the config at the time of the update
		newVersion int
		// expectedReplicas is the expected config replica count after the update
		expectedReplicas int
		// before is the state of all deployments prior to the update
		before []deployment
		// after is the expected state of all deployments after the update
		after []deployment
		// errExpected is whether the update should produce an error
		errExpected bool
	}{
		{
			name:             "version is zero",
			replicas:         1,
			newVersion:       0,
			expectedReplicas: 1,
			before:           []deployment{},
			after:            []deployment{},
			errExpected:      false,
		},
		{
			name:             "initial deployment of this version",
			replicas:         1,
			newVersion:       1,
			expectedReplicas: 1,
			before:           []deployment{},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version deployment in progress (no older deployments present)",
			replicas:         1,
			newVersion:       1,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version deployment in progress (older deployments present)",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusNew, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version already deployed (no older deployments present)",
			replicas:         1,
			newVersion:       1,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newint(1), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version awaiting cancellation of older deployments (not yet cancelled)",
			replicas:         1,
			newVersion:       3,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newint(1), desiredA: newint(1), status: deployapi.DeploymentStatusRunning, cancelled: false},
				{version: 2, replicas: 1, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusRunning, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newint(1), desiredA: newint(1), status: deployapi.DeploymentStatusRunning, cancelled: true},
				{version: 2, replicas: 1, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusRunning, cancelled: true},
			},
			errExpected: true,
		},
		{
			name:             "this version awaiting cancellation of older deployments (previously cancelled)",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusRunning, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusRunning, cancelled: true},
			},
			errExpected: true,
		},
		{
			name:             "this version already deployed with replica count divergence (latest != active)",
			replicas:         1,
			newVersion:       5,
			expectedReplicas: 2,
			before: []deployment{
				{version: 1, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 1, desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 5, replicas: 0, desiredA: newint(2), status: deployapi.DeploymentStatusFailed, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 2, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 5, replicas: 0, replicasA: newint(0), desiredA: newint(2), status: deployapi.DeploymentStatusFailed, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version already deployed with replica count divergence (latest == active)",
			replicas:         1,
			newVersion:       5,
			expectedReplicas: 2,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 1, desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 1, desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: false},
				{version: 5, replicas: 2, status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 3, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: true},
				{version: 4, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: false},
				{version: 5, replicas: 2, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version already deployed, scaled externally (latest == active)",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 5,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 5, replicasA: newint(1), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 5, replicasA: newint(5), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version already deployed, scaled externally (latest == active)",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 5,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 5, replicasA: newint(1), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 5, replicasA: newint(5), status: deployapi.DeploymentStatusComplete, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version already deployed, no active deployment",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: false},
			},
			after: []deployment{
				{version: 1, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(1), status: deployapi.DeploymentStatusFailed, cancelled: false},
			},
			errExpected: false,
		},
		{
			name:             "this version already failed, fallback, previously unsynced",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 2,
			before: []deployment{
				{version: 1, replicas: 0, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, desiredA: newint(2), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 2, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(2), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "this version already failed, fallback, previously unsynced, partial deployment",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 2,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 1, desiredA: newint(2), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 2, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(2), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "this version already failed, fallback, previously unsynced, partial deployment (desired unavailable)",
			replicas:         1,
			newVersion:       2,
			expectedReplicas: 1,
			before: []deployment{
				{version: 1, replicas: 1, status: deployapi.DeploymentStatusComplete, cancelled: false},
				// Not sure how desired could get lost, but we'll account for it anyway
				{version: 2, replicas: 1, status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 1, replicasA: newint(1), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "this version already failed, fallback, previously synced",
			replicas:         2,
			newVersion:       2,
			expectedReplicas: 2,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(2), desiredA: newint(3), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 2, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(3), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "this version already failed, fallback, previously synced (different DC count)",
			replicas:         3,
			newVersion:       2,
			expectedReplicas: 3,
			before: []deployment{
				{version: 1, replicas: 0, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(2), desiredA: newint(5), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 3, replicasA: newint(3), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(5), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			errExpected: false,
		},
		{
			name:             "already did fallback, previously synced, new scale",
			replicas:         3,
			newVersion:       2,
			expectedReplicas: 3,
			before: []deployment{
				{version: 1, replicas: 2, replicasA: newint(2), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(2), desiredA: newint(5), status: deployapi.DeploymentStatusFailed, cancelled: true},
			},
			after: []deployment{
				{version: 1, replicas: 3, replicasA: newint(3), status: deployapi.DeploymentStatusComplete, cancelled: false},
				{version: 2, replicas: 0, replicasA: newint(0), desiredA: newint(5), status: deployapi.DeploymentStatusFailed, cancelled: true},
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

		kc := &ktestclient.Fake{}
		kc.AddReactor("list", "replicationcontrollers", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
			list := []kapi.ReplicationController{}
			for _, deployment := range deployments {
				list = append(list, deployment)
			}
			return true, &kapi.ReplicationControllerList{Items: list}, nil
		})
		kc.AddReactor("create", "replicationcontrollers", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
			rc := action.(ktestclient.CreateAction).GetObject().(*kapi.ReplicationController)
			deployments[rc.Name] = *rc
			return true, rc, nil
		})
		kc.AddReactor("update", "replicationcontrollers", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
			rc := action.(ktestclient.UpdateAction).GetObject().(*kapi.ReplicationController)
			deployments[rc.Name] = *rc
			return true, rc, nil
		})

		oc := &testclient.Fake{}

		recorder := &record.FakeRecorder{}
		controller := &DeploymentConfigController{
			kubeClient: kc,
			osClient:   oc,
			codec:      kapi.Codec,
			recorder:   recorder,
		}

		config := deploytest.OkDeploymentConfig(test.newVersion)
		config.Template.ControllerTemplate.Replicas = test.replicas
		err := controller.Handle(config)
		if err != nil {
			if test.errExpected {
				t.Logf("got expected error: %s", err)
			} else {
				t.Fatalf("unexpected error: %s", err)
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

		if e, a := test.expectedReplicas, config.Template.ControllerTemplate.Replicas; e != a {
			t.Errorf("expected config replicas to be %d, got %d", e, a)
			t.Fatalf("events:\n%s", strings.Join(recorder.Events, "\t\n"))
		}
		anyDeploymentMismatches := false
		for i := 0; i < len(expectedDeployments); i++ {
			expected, actual := expectedDeployments[i], actualDeployments[i]
			if !kapi.Semantic.DeepEqual(expected, actual) {
				anyDeploymentMismatches = true
				t.Errorf("actual deployment don't match expected: %v", kutil.ObjectDiff(expected, actual))
			}
		}
		if anyDeploymentMismatches {
			t.Fatalf("events:\n%s", strings.Join(recorder.Events, "\t\n"))
		}
	}
}

func newint(i int) *int {
	return &i
}
