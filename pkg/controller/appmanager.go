package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	platform "kolihub.io/koli/pkg/apis/core/v1alpha1"
	draft "kolihub.io/koli/pkg/apis/core/v1alpha1/draft"
)

// AppManagerController controller
type AppManagerController struct {
	kclient kubernetes.Interface
	// sysClient clientset.CoreInterface

	dpInf   cache.SharedIndexInformer
	planInf cache.SharedIndexInformer

	queue    *TaskQueue
	recorder record.EventRecorder

	defaultDomain string
}

// NewAppManagerController creates a new AppManagerController
func NewAppManagerController(
	dpInf, planInf cache.SharedIndexInformer,
	client kubernetes.Interface,
	defaultDomain string) *AppManagerController {
	c := &AppManagerController{
		kclient:       client,
		dpInf:         dpInf,
		planInf:       planInf,
		recorder:      newRecorder(client, "app-manager-controller"),
		defaultDomain: defaultDomain,
	}
	c.queue = NewTaskQueue("app_manager", c.syncHandler)
	c.dpInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addDeployment,
		UpdateFunc: c.updateDeployment,
		DeleteFunc: c.deleteDeployment,
	})
	return c
}

func (c *AppManagerController) addDeployment(d interface{}) {
	new := d.(*v1beta1.Deployment)
	glog.Infof("add-deployment(%d) - %s/%s", new.Status.ObservedGeneration, new.Namespace, new.Name)
	c.queue.Add(new)
}

func (c *AppManagerController) updateDeployment(o, n interface{}) {
	old := o.(*v1beta1.Deployment)
	new := n.(*v1beta1.Deployment)

	if old.ResourceVersion == new.ResourceVersion {
		return
	}
	c.queue.Add(new)
}

func (c *AppManagerController) deleteDeployment(o interface{}) {
	obj := o.(*v1beta1.Deployment)
	c.queue.Add(obj)
}

// enqueueForNamespace enqueues all Deployments object keys that belong to the given namespace.
func (c *AppManagerController) enqueueForNamespace(namespace string) {
	cache.ListAll(c.dpInf.GetStore(), labels.Everything(), func(obj interface{}) {
		d := obj.(*v1beta1.Deployment)
		if d.Namespace == namespace {
			c.queue.Add(d)
		}
	})
}

// Run the controller.
func (c *AppManagerController) Run(workers int, stopc <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()
	// make sure the work queue is shutdown which will trigger workers to end
	defer c.queue.shutdown()

	glog.Info("Starting App Manager Controller...")

	if !cache.WaitForCacheSync(stopc, c.dpInf.HasSynced, c.planInf.HasSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		// runWorker will loop until "something bad" happens.
		// The .Until will then rekick the worker after one second
		go c.queue.run(time.Second, stopc)
		// go wait.Until(c.runWorker, time.Second, stopc)
	}

	// wait until we're told to stop
	<-stopc
	glog.Info("Shutting down App Manager controller")
}

// TODO: validate if it's a platform resource - OK
// TODO: test with an empty plan - OK
// TODO: Error when creating PVC
// TODO: test with already exist error PVC

func (c *AppManagerController) syncHandler(key string) error {
	obj, exists, err := c.dpInf.GetStore().GetByKey(key)
	if err != nil {
		glog.Warningf("%s - failed retrieving object from store [%s]", key, err)
		return err
	}
	if !exists {
		glog.V(3).Infof("%s - the deployment doesn't exists", key)
		// This is a workaround to clean up slugbuild pods
		// TODO: use ownerReferences instead of doing this manually
		// https://kubernetes.io/docs/concepts/workloads/controllers/garbage-collection/#owners-and-dependents
		parts := strings.Split(key, "/")
		if len(parts) == 2 {
			glog.Infof("%s - removing orphan slugbuilds", key)
			if err := c.cleanBuilds(parts[0], parts[1]); err != nil {
				glog.Warningf("%s - failed removing orphan slugbuilds", key)
			}
		}
		return nil
	}

	d := draft.NewDeployment(obj.(*v1beta1.Deployment))
	if d.DeletionTimestamp != nil {
		glog.V(3).Infof("%s - object marked for deletion", key)
		return nil
	}

	if !d.GetNamespaceMetadata().Valid() {
		glog.V(3).Infof("%s - it's not a valid resource", key)
		return nil
	}
	if err := c.addDefaultRoute(d.GetObject()); err != nil && !apierrors.IsAlreadyExists(err) {
		glog.Warningf("%s - failed adding default routes [%v]", key, err)
	}

	if len(d.Spec.Template.Spec.Containers) > 0 {
		if d.Spec.Template.Spec.Containers[0].Resources.Requests == nil ||
			d.Spec.Template.Spec.Containers[0].Resources.Limits == nil {
			glog.Warningf("%s - deployment has empty 'limits' or 'requests' resources", key)
		}
	}

	planName, exists := d.GetStoragePlan().Value()
	if !exists || !d.HasSetupPVCAnnotation() {
		glog.V(3).Infof("%s - the object doesn't have a storage plan or an annotation to setup a PVC", key)
		return nil
	}
	var plan *platform.Plan
	cache.ListAll(c.planInf.GetStore(), labels.Everything(), func(obj interface{}) {
		p := obj.(*platform.Plan)
		if p.Name == planName && p.IsStorageType() {
			plan = p
		}
	})
	if plan == nil {
		msg := fmt.Sprintf(`Storage Plan "%s" not found`, planName)
		c.recorder.Event(&d.Deployment, v1.EventTypeWarning, "PlanNotFound", msg)
		return fmt.Errorf(msg)
	}
	_, err = c.kclient.Core().PersistentVolumeClaims(d.Namespace).Create(newPVC(d, plan))
	if err != nil && !apierrors.IsAlreadyExists(err) {
		pvcFailed.Inc()
		msg := fmt.Sprintf(`Failed creating PVC [%v]`, err)
		c.recorder.Event(d, v1.EventTypeWarning, "ProvisionError", msg)
		return fmt.Errorf(msg)
	}

	if err == nil {
		pvcCreated.Inc()
	}

	glog.Infof(`%s - PVC "d-%s" created with "%s"`, key, d.Name, plan.Spec.Storage.String())
	patchData := []byte(fmt.Sprintf(`{"metadata": {"annotations": {"%s": "false"}}}`, platform.AnnotationSetupStorage))
	_, err = c.kclient.Extensions().Deployments(d.Namespace).Patch(d.Name, types.MergePatchType, patchData)
	if err != nil {
		return fmt.Errorf(`%s - failed updating deployment [%v]`, key, err)
	}
	return nil
}

func newPVC(d *draft.Deployment, plan *platform.Plan) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("d-%s", d.Name),
			Namespace: d.Namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{v1.ResourceStorage: plan.Spec.Storage},
			},
		},
	}
}

func (c *AppManagerController) cleanBuilds(ns, appName string) error {
	// skip non-platform resources
	if !draft.NewNamespaceMetadata(ns).IsValid() {
		return nil
	}
	return c.kclient.Core().Pods(ns).DeleteCollection(
		&metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", platform.AnnotationApp, appName)},
	)
}

// addDefaultRoute add a service and ingress for a given deployment.
func (c *AppManagerController) addDefaultRoute(d *v1beta1.Deployment) error {
	if len(c.defaultDomain) == 0 || d.Labels["kolihub.io/type"] != "app" {
		return nil
	}

	ownerRefs := []metav1.OwnerReference{{
		APIVersion: v1beta1.SchemeGroupVersion.String(),
		Kind:       "Deployment",
		Controller: func(b bool) *bool { return &b }(true),
		Name:       d.GetName(),
		UID:        d.GetUID(),
	}}
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       d.Namespace,
			Name:            d.Name,
			Labels:          map[string]string{"kolihub.io/type": "app"},
			OwnerReferences: ownerRefs,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       "http",
				Port:       80,
				Protocol:   "TCP",
				TargetPort: intstr.FromInt(5000),
			}},
			Selector: map[string]string{
				"kolihub.io/name": d.Name,
				"kolihub.io/type": "app",
			},
		},
	}
	if _, err := c.kclient.Core().Services(d.Namespace).Create(svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	ing := &v1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: d.Namespace,
			Name:      d.Name,
			// Kong Ingress claim subdomains from koli-system
			Annotations:     map[string]string{"kolihub.io/parent": platform.SystemNamespace},
			Labels:          map[string]string{"kolihub.io/type": "app"},
			OwnerReferences: ownerRefs,
		},
		Spec: v1beta1.IngressSpec{Rules: []v1beta1.IngressRule{{
			Host: fmt.Sprintf("%s-%s.%s", d.Name, d.Namespace, c.defaultDomain),
			IngressRuleValue: v1beta1.IngressRuleValue{HTTP: &v1beta1.HTTPIngressRuleValue{
				Paths: []v1beta1.HTTPIngressPath{{
					Backend: v1beta1.IngressBackend{
						ServiceName: d.Name,
						ServicePort: intstr.FromInt(80),
					},
					Path: "/",
				}},
			}},
		}}},
	}
	_, err := c.kclient.Extensions().Ingresses(d.Namespace).Create(ing)
	return err
}
