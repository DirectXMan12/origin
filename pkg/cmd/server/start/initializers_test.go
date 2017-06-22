package start

import (
	"io"
	"os"
	"path"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/stretchr/testify/assert"

	osclient "github.com/openshift/origin/pkg/client"
	osfake "github.com/openshift/origin/pkg/client/testclient"
	"github.com/openshift/origin/pkg/cmd/server/admin"
	configapi "github.com/openshift/origin/pkg/cmd/server/api"
	"github.com/openshift/origin/pkg/cmd/server/crypto"
	"github.com/openshift/origin/pkg/cmd/server/origin"
	origincontrollers "github.com/openshift/origin/pkg/cmd/server/origin/controller"
	templateclient "github.com/openshift/origin/pkg/template/generated/internalclientset"
	testutil "github.com/openshift/origin/test/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientgoclientset "k8s.io/client-go/kubernetes"
	clientgofake "k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	core "k8s.io/client-go/testing"
	kctrlmgr "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	autoscaling "k8s.io/kubernetes/pkg/apis/autoscaling/v1"
	extensions "k8s.io/kubernetes/pkg/apis/extensions/v1beta1"
	kclientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	kubefake "k8s.io/kubernetes/pkg/client/clientset_generated/clientset/fake"
	kclientsetinternal "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	kinformers "k8s.io/kubernetes/pkg/client/informers/informers_generated/externalversions"
)

// copied from test/util/server to avoid circular references
func CreateMasterCerts(masterArgs *MasterArgs) error {
	hostnames, err := masterArgs.GetServerCertHostnames()
	if err != nil {
		return err
	}
	masterURL, err := masterArgs.GetMasterAddress()
	if err != nil {
		return err
	}
	publicMasterURL, err := masterArgs.GetMasterPublicAddress()
	if err != nil {
		return err
	}

	createMasterCerts := admin.CreateMasterCertsOptions{
		CertDir:    masterArgs.ConfigDir.Value(),
		SignerName: admin.DefaultSignerName(),
		Hostnames:  hostnames.List(),

		ExpireDays:       crypto.DefaultCertificateLifetimeInDays,
		SignerExpireDays: crypto.DefaultCACertificateLifetimeInDays,

		APIServerURL:       masterURL.String(),
		PublicAPIServerURL: publicMasterURL.String(),

		Output: os.Stderr,
	}

	if err := createMasterCerts.Validate(nil); err != nil {
		return err
	}
	if err := createMasterCerts.CreateMasterCerts(); err != nil {
		return err
	}

	return nil
}
func DefaultMasterOptions() (*configapi.MasterConfig, error) {
	startOptions := MasterOptions{}
	startOptions.MasterArgs, _, _, _, _ = GetAllInOneArgs()
	startOptions.Complete()
	startOptions.MasterArgs.ConfigDir.Default(path.Join(testutil.GetBaseDir(), "openshift.local.config", "master"))

	if err := CreateMasterCerts(startOptions.MasterArgs); err != nil {
		return nil, err
	}

	masterConfig, err := startOptions.MasterArgs.BuildSerializeableMasterConfig()
	if err != nil {
		return nil, err
	}

	return masterConfig, nil
}

// fakeClientBuilder is a OpenshiftControllerClientBuilder that returns fake clients
type fakeClientBuilder struct {
	kubeClientset     *kubefake.Clientset
	osInterface       *osfake.Fake
	clientgoClientset *clientgofake.Clientset
}

func (f *fakeClientBuilder) Client(name string) (kclientset.Interface, error) {
	return f.kubeClientset, nil
}
func (f *fakeClientBuilder) DeprecatedOpenshiftClient(name string) (osclient.Interface, error) {
	return f.osInterface, nil
}
func (f *fakeClientBuilder) DeprecatedOpenshiftClientOrDie(name string) osclient.Interface {
	client, err := f.DeprecatedOpenshiftClient(name)
	if err != nil {
		glog.Fatal(err)
	}
	return client
}
func (f *fakeClientBuilder) ClientOrDie(name string) kclientset.Interface {
	client, err := f.Client(name)
	if err != nil {
		glog.Fatal(err)
	}
	return client
}
func (f *fakeClientBuilder) ClientGoClient(name string) (clientgoclientset.Interface, error) {
	return f.clientgoClientset, nil
}
func (f *fakeClientBuilder) ClientGoClientOrDie(name string) clientgoclientset.Interface {
	client, err := f.ClientGoClient(name)
	if err != nil {
		glog.Fatal(err)
	}
	return client
}

// the below methods are not needed for these tests

func (f *fakeClientBuilder) KubeInternalClient(name string) (kclientsetinternal.Interface, error) {
	return nil, nil
}
func (f *fakeClientBuilder) KubeInternalClientOrDie(name string) kclientsetinternal.Interface {
	return nil
}
func (f *fakeClientBuilder) OpenshiftTemplateClient(name string) (templateclient.Interface, error) {
	// TODO: implement this when we need it
	return nil, nil
}
func (f *fakeClientBuilder) Config(name string) (*restclient.Config, error) {
	return nil, nil
}
func (f *fakeClientBuilder) ConfigOrDie(name string) *restclient.Config {
	return nil
}

type fakeResponse struct{}

func (w fakeResponse) DoRaw() ([]byte, error)         { return []byte{}, nil }
func (w fakeResponse) Stream() (io.ReadCloser, error) { return nil, nil }

func TestHPAIsCustomInitializer(t *testing.T) {
	// this test tests that the HPA is able to succesfully get the scale of a DC and
	// requests metrics from the correct Heapster location
	capiMasterConfig, err := DefaultMasterOptions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oc, err := origin.BuildMasterConfig(*capiMasterConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	kc, err := BuildKubernetesMasterConfig(oc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	initializers, err := collectInitializers(oc, kc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hpaInitializer, ok := initializers["horizontalpodautoscaling"]
	if !ok {
		t.Fatalf("HPA controller initializer was not present")
	}

	gotDC := false
	done := make(chan struct{})

	fakeCB := &fakeClientBuilder{
		kubeClientset:     &kubefake.Clientset{},
		osInterface:       &osfake.Fake{},
		clientgoClientset: &clientgofake.Clientset{},
	}
	fakeCB.kubeClientset.AddProxyReactor("services", func(action core.Action) (bool, restclient.ResponseWrapper, error) {
		defer close(done)
		proxyAction := action.(core.ProxyGetAction)
		assert.Equal(t, proxyAction.GetNamespace(), "openshift-infra")
		assert.Equal(t, proxyAction.GetName(), "heapster")
		assert.Equal(t, proxyAction.GetScheme(), "https")
		assert.Equal(t, proxyAction.GetPort(), "")

		return true, fakeResponse{}, nil
	})
	fakeCB.kubeClientset.AddReactor("list", "horizontalpodautoscalers", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		targetPercentage := int32(80)
		obj := &autoscaling.HorizontalPodAutoscalerList{
			Items: []autoscaling.HorizontalPodAutoscaler{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "somehpa",
						Namespace: "somens",
					},
					Spec: autoscaling.HorizontalPodAutoscalerSpec{
						ScaleTargetRef: autoscaling.CrossVersionObjectReference{
							Kind:       "DeploymentConfig",
							Name:       "somedc",
							APIVersion: "v1",
						},
						MaxReplicas:                    10,
						TargetCPUUtilizationPercentage: &targetPercentage,
					},
				},
			},
		}

		return true, obj, nil
	})
	fakeCB.osInterface.AddReactor("get", "deploymentconfigs/scale", func(action core.Action) (bool, runtime.Object, error) {
		gotDC = true
		obj := &extensions.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "somedc",
				Namespace: "somens",
			},
			Spec: extensions.ScaleSpec{
				Replicas: 1,
			},
			Status: extensions.ScaleStatus{
				Replicas: 1,
				Selector: map[string]string{"name": "somedc"},
			},
		}
		return true, obj, nil
	})
	fakeWatch := watch.NewFake()
	fakeCB.kubeClientset.AddWatchReactor("*", core.DefaultWatchReactor(fakeWatch, nil))

	kc.ControllerManager.HorizontalPodAutoscalerSyncPeriod.Duration = 0

	stopChan := make(chan struct{})
	informers := kinformers.NewSharedInformerFactory(fakeCB.kubeClientset, 0)
	openshiftControllerContext := origincontrollers.ControllerContext{
		KubeControllerContext: kctrlmgr.ControllerContext{
			ClientBuilder:   fakeCB,
			InformerFactory: informers,
			Options:         *kc.ControllerManager,
		},
		ClientBuilder: fakeCB,
		Stop:          stopChan,
	}
	defer close(stopChan)

	initialized, err := hpaInitializer(openshiftControllerContext)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !initialized {
		t.Fatalf("expected the HPA controller to have run, but it did not")
	}
	informers.Start(stopChan)
	go informers.Autoscaling().V1().HorizontalPodAutoscalers().Informer().Run(stopChan)

	select {
	case <-done:
		assert.True(t, gotDC, "expected to have tried to fetch the scale for a DC, but did not")
	case <-time.After(30 * time.Second):
		t.Errorf("expected proper clients to be called, but they were not")
		close(done)
	}
}
