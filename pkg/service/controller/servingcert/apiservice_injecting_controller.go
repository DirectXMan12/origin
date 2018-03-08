package servingcert

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	kapierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	aggapi "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1"
	aggclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/typed/apiregistration/v1beta1"
	agginformers "k8s.io/kube-aggregator/pkg/client/informers/externalversions/apiregistration/v1beta1"
	agglisters "k8s.io/kube-aggregator/pkg/client/listers/apiregistration/v1beta1"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/openshift/origin/pkg/cmd/server/crypto"
)

const (
	// ServiceCertCAInjectAnnotation indicates that we should inject the CA used to sign
	// serving certs as the CABundle for an APIService object.
	ServingCertCAInjectAnnotation = "service.alpha.openshift.io/inject-serving-cert-ca"
)

// APIServiceCertInjectingController is responsible for injecting
// the serving cert signing CA into the APIService CABundle field,
// when requested.
type APIServiceCertInjectingController struct {
	apiServiceClient aggclient.APIServicesGetter

	// APIServices that need to be checked
	queue      workqueue.RateLimitingInterface
	maxRetries int

	ca *crypto.CA

	apiServiceLister    agglisters.APIServiceLister
	apiServiceHasSynced cache.InformerSynced

	// syncHandler does the work. It's factored out for unit testing
	syncHandler func(serviceKey string) error
}

// NewAPIServiceCertInjectingController creates a new APIServiceCertInjectingController,
// to ensure that APIServices properly annotated have their CA bundles auto-injected from
// appropriate secrets.
func NewAPIServiceCertInjectingController(apiServices agginformers.APIServiceInformer, apiServiceClient aggclient.APIServicesGetter, ca *crypto.CA, resyncInterval time.Duration) *APIServiceCertInjectingController {

	cont := &APIServiceCertInjectingController{
		apiServiceClient: apiServiceClient,

		ca: ca,

		queue:      workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		maxRetries: 10,
	}

	apiServices.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: cont.enqueueAPIService,
			UpdateFunc: func(old, cur interface{}) {
				cont.enqueueAPIService(cur)
			},
		},
		resyncInterval,
	)
	cont.apiServiceLister = apiServices.Lister()
	cont.apiServiceHasSynced = apiServices.Informer().GetController().HasSynced

	cont.syncHandler = cont.syncAPIService

	return cont
}

func (c *APIServiceCertInjectingController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(stopCh, c.apiServiceHasSynced) {
		return
	}

	glog.V(5).Infof("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}

	<-stopCh
	glog.V(1).Infof("Shutting down")
}

func (c *APIServiceCertInjectingController) enqueueAPIService(obj interface{}) {
	_, ok := obj.(*aggapi.APIService)
	if !ok {
		return
	}
	key, err := controller.KeyFunc(obj)
	if err != nil {
		glog.Errorf("couldn't get key for object %+v: %v", obj, err)
		return
	}

	c.queue.Add(key)
}

func (c *APIServiceCertInjectingController) worker() {
	for {
		if !c.work() {
			return
		}
	}
}

// work pops a key of the queue, attempts to process it, and requeues it
// if necessary.  It returns false when work should stop.
func (c *APIServiceCertInjectingController) work() bool {
	key, stopWorking := c.queue.Get()
	if stopWorking {
		return false
	}
	defer c.queue.Done(key)

	if err := c.syncHandler(key.(string)); err == nil {
		// we're all set for this round, don't requeue
		c.queue.Forget(key)
	} else {
		// need to retry later
		utilruntime.HandleError(fmt.Errorf("error syncing APIService, will retry: %v", err))
		c.queue.AddRateLimited(key)
	}

	return true
}

func (c *APIServiceCertInjectingController) syncAPIService(key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	cachedAPIService, err := c.apiServiceLister.Get(name)
	if kapierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if cachedAPIService.Annotations == nil {
		return nil
	}

	if injectAnnot, hasInjectAnnot := cachedAPIService.Annotations[ServingCertCAInjectAnnotation]; !hasInjectAnnot || injectAnnot != "true" {
		return nil
	}

	// make a copy to avoid mutating cache state
	apiServiceCopy := cachedAPIService.DeepCopy()

	// fetch the CA's certificate bytes
	caBytes, _, err := c.ca.Config.GetPEMBytes()
	if err != nil {
		return err
	}

	apiServiceCopy.Spec.CABundle = caBytes
	apiServiceCopy.Spec.InsecureSkipTLSVerify = false

	_, err = c.apiServiceClient.APIServices().Update(apiServiceCopy)
	return err
}
