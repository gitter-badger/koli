package controller

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	platform "kolihub.io/koli/pkg/apis/core/v1alpha1"
	"kolihub.io/koli/pkg/apis/core/v1alpha1/draft"
	clientset "kolihub.io/koli/pkg/clientset"
	koliutil "kolihub.io/koli/pkg/util"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	extensions "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// keys required in a deployment annotation for creating a new release
var requiredKeys = []string{platform.AnnotationGitRemote}

// ReleaseController controller
type ReleaseController struct {
	kclient    kubernetes.Interface
	clientset  clientset.CoreInterface
	releaseInf cache.SharedIndexInformer
	dpInf      cache.SharedIndexInformer

	queue    *TaskQueue
	recorder record.EventRecorder
}

// NewReleaseController creates a new ReleaseController
func NewReleaseController(releaseInf, dpInf cache.SharedIndexInformer, sysClient clientset.CoreInterface, kclient kubernetes.Interface) *ReleaseController {
	r := &ReleaseController{
		kclient:    kclient,
		clientset:  sysClient,
		releaseInf: releaseInf,
		dpInf:      dpInf,
		recorder:   newRecorder(kclient, "apps-controller"),
	}
	r.queue = NewTaskQueue("release", r.syncHandler)
	r.dpInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    r.addDeployment,
		UpdateFunc: r.updateDeployment,
	})
	return r
}

func (r *ReleaseController) addDeployment(obj interface{}) {
	dp := obj.(*extensions.Deployment)
	glog.V(2).Infof("add-deployment - %s/%s", dp.Namespace, dp.Name)
	r.queue.Add(dp)
}

func (r *ReleaseController) updateDeployment(o, n interface{}) {
	old := o.(*extensions.Deployment)
	new := n.(*extensions.Deployment)
	if old.ResourceVersion == new.ResourceVersion {
		return
	}

	statusGen := new.Status.ObservedGeneration
	// ref: https://github.com/kubernetes/kubernetes/issues/34363#issuecomment-263336109
	// observedGeneration is intended as a way for an observer to determine how up to date the status reported by the primary controller
	// for the resource is. No more, no less. That needs to be combined with the usual resourceVersion-based optimistic concurrency
	// mechanism to ensure that controllers don't act upon stale data, and with leader-election sequence numbers in the case of HA.
	if old.Generation != new.Generation {
		glog.V(2).Infof("update-deployment(%d) - %s/%s - new generation, queueing ...", statusGen, new.Namespace, new.Name)
		r.queue.Add(new)
	}
	// updating a deployment triggers this function serveral times.
	// a deployment must be queued only when every generation status is synchronized -
	// when the generation and ObservedGeneration are equal for each resource object (new and old)
	// if old.Generation == new.Generation && old.Status.ObservedGeneration == statusGen {
	// 	glog.Infof("update-deployment(%d) - %s/%s - resource on sync, queueing ...", statusGen, new.Namespace, new.Name)
	// 	r.queue.add(new)
	// }
}

// Run the controller.
func (r *ReleaseController) Run(workers int, stopc <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()
	// make sure the work queue is shutdown which will trigger workers to end
	defer r.queue.shutdown()

	glog.Info("Starting Release controller...")

	if !cache.WaitForCacheSync(stopc, r.releaseInf.HasSynced, r.dpInf.HasSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		// runWorker will loop until "something bad" happens.
		// The .Until will then rekick the worker after one second
		go r.queue.run(time.Second, stopc)
		// go wait.Until(r.runWorker, time.Second, stopc)
	}

	// wait until we're told to stop
	<-stopc
	glog.Info("Shutting down Release controller...")
}

func (r *ReleaseController) syncHandler(key string) error {
	obj, exists, err := r.dpInf.GetStore().GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		glog.V(2).Infof("%s - release doesn't exists", key)
		return nil
	}

	dp := obj.(*extensions.Deployment)
	// TODO: change to namespace meta!
	if !draft.NewNamespaceMetadata(dp.Namespace).IsValid() {
		glog.V(2).Infof("%s - noop, it's not a valid namespace", key)
		return nil
	}

	if dp.Annotations["kolihub.io/build"] != "true" {
		glog.V(4).Infof("%s - noop, isn't a build action", key)
		return nil
	}

	// The release doesn't exists, create/build a new one!
	if err := validateRequiredKeys(dp); err != nil {
		r.recorder.Event(dp, v1.EventTypeWarning, "MissingAnnotationKey", err.Error())
		return fmt.Errorf("ValidateRequiredKeys [%s]", err)
	}

	gitSha, _ := koliutil.NewSha(dp.Annotations[platform.AnnotationGitCommitID])
	dpRevision := dp.Name
	// dpRevision := fmt.Sprintf("%s-v%s", dp.Name, dp.Annotations[platform.AnnotationBuildRevision])
	// if len(dpRevision) == 0 {
	// 	// Try to find the last revision
	// 	revisions := []int{}
	// 	cache.ListAllByNamespace(r.releaseInf.GetIndexer(), dp.Name, labels.Everything(), func(obj interface{}) {
	// 		rel := obj.(*platform.Release)
	// 		rev := rel.BuildRevision()
	// 		if rev > 0 {
	// 			revisions = append(revisions, rel.BuildRevision())
	// 		}
	// 	})
	// 	lastRevision := getLastBuildRevision(revisions)
	// 	glog.Infof("%s - last revision [%v]", key, lastRevision)
	// 	dpRevision = strconv.Itoa(lastRevision + 1)
	// }
	autoDeploy := false
	// Deploy after the build
	if dp.Annotations[platform.AnnotationAutoDeploy] == "true" {
		glog.V(2).Infof("%s - autodeploy turned on.", key)
		autoDeploy = true
	}
	sourceType := platform.SourceType(dp.Annotations[platform.AnnotationBuildSource])
	release := &platform.Release{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Release",
			APIVersion: platform.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      dpRevision,
			Namespace: dp.Namespace,
			// Useful for filtering
			Labels: map[string]string{"kolihub.io/deploy": dp.Name},
		},
		Spec: platform.ReleaseSpec{
			// BuildRevision: dp.Annotations[platform.AnnotationBuildRevision],
			GitRemote:     dp.Annotations[platform.AnnotationGitRemote],
			GitBranch:     dp.Annotations[platform.AnnotationGitBranch],
			GitRepository: dp.Annotations[platform.AnnotationGitRepository],
			HeadCommit: platform.HeadCommit{
				ID:        dp.Annotations[platform.AnnotationGitCommitID],
				Author:    dp.Annotations[platform.AnnotationGitAuthorName],
				AvatarURL: dp.Annotations[platform.AnnotationGitAuthorAvatar],
				Compare:   dp.Annotations[platform.AnnotationGitCompare],
				Message:   dp.Annotations[platform.AnnotationGitCommitMessage],
				URL:       dp.Annotations[platform.AnnotationGitCommitURL],
			},
			// AuthToken:     dp.Annotations[spec.KoliPrefix("authtoken")],
			// GitRevision:   gitSha.Full(),
			AutoDeploy: autoDeploy,
			DeployName: dp.Name,
			Build:      true, // Always build a new release!
			Source:     sourceType,
		},
	}
	if gitSha != nil {
		// Useful for filtering
		release.Labels[platform.AnnotationGitRevision] = gitSha.Short()
		// release.Spec.GitRevision = gitSha.Full()
	}
	_, err = r.clientset.Release(release.Namespace).Create(release)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed creating new release: %s", err)
	}
	if err == nil {
		ref := release.Spec.HeadCommit.ID
		if len(ref) == 0 {
			ref = release.Spec.GitBranch
		}
		msg := fmt.Sprintf("Created release with revision '%s' from '%s'", ref, sourceType)
		r.recorder.Event(release, v1.EventTypeNormal, "Created", msg)
		glog.Infof("%s - new release created '%s'", key, release.Name)
	}

	// We need to update the 'build' key to false, otherwise the build will be triggered again.
	// TODO: Need other strategy for dealing with this kind of scenario
	deactivateBuild := activateBuildPayload(false)
	_, err = r.kclient.Extensions().Deployments(dp.Namespace).Patch(dp.Name, types.StrategicMergePatchType, deactivateBuild)
	if err != nil {
		glog.Warningf("%s - failed deactivating build from deployment [%s]", key, err)
	}
	return nil
}

func validateRequiredKeys(dp *extensions.Deployment) error {
	for _, key := range requiredKeys {
		_, ok := dp.Annotations[key]
		if !ok {
			return fmt.Errorf("Missing required key '%s'", key)
		}
	}
	return nil
}

func activateBuildPayload(activate bool) []byte {
	build := "false"
	if activate {
		build = "true"
	}
	payload := fmt.Sprintf(`{"metadata": {"annotations": {"%s": "%s"}}}`, platform.AnnotationBuild, build)
	return []byte(payload)
}

func getLastBuildRevision(revisions []int) int {
	var n int
	for _, v := range revisions {
		if v > n {
			n = v
		}
	}
	return n
}
