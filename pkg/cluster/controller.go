package cluster

import (
	"context"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	clusterinformerv1 "open-cluster-management.io/api/client/cluster/informers/externalversions/cluster/v1"
	clusterlisterv1 "open-cluster-management.io/api/client/cluster/listers/cluster/v1"
	workclientv1 "open-cluster-management.io/api/client/work/clientset/versioned/typed/work/v1"
	workinformerv1 "open-cluster-management.io/api/client/work/informers/externalversions/work/v1"
	worklisterv1 "open-cluster-management.io/api/client/work/listers/work/v1"
)

// clusterController reconciles instances of ManagedCluster on the hub.
type clusterController struct {
	workclient    workclientv1.WorkV1Interface
	clusterLister clusterlisterv1.ManagedClusterLister
	workLister    worklisterv1.ManifestWorkLister
	cache         resourceapply.ResourceCache
	eventRecorder events.Recorder
}

// NewHubClusterController creates a new hub cluster controller
func NewHubClusterController(
	workclient workclientv1.WorkV1Interface,
	clusterInformer clusterinformerv1.ManagedClusterInformer,
	workInformer workinformerv1.ManifestWorkInformer,
	recorder events.Recorder) factory.Controller {
	c := &clusterController{
		workclient:    workclient,
		clusterLister: clusterInformer.Lister(),
		workLister:    workInformer.Lister(),
		cache:         resourceapply.NewResourceCache(),
		eventRecorder: recorder.WithComponentSuffix("hub-cluster-controller"),
	}
	return factory.New().
		WithFilteredEventsInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetName()
			},
			func(obj interface{}) bool {
				accessor, err := meta.Accessor(obj)
				if err != nil {
					return false
				}
				// enqueue all managed cluster except for local-cluster and hoh=disabled
				if accessor.GetLabels()["hoh"] == "disabled" || accessor.GetName() == "local-cluster" {
					return false
				} else {
					return true
				}
			}, clusterInformer.Informer()).
		WithFilteredEventsInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetNamespace()
			},
			func(obj interface{}) bool {
				accessor, err := meta.Accessor(obj)
				if err != nil {
					return false
				}
				// only enqueue when the hoh=enabled managed cluster is changed
				if accessor.GetName() == accessor.GetNamespace()+"-"+HOH_HUB_CLUSTER_SUBSCRIPTION ||
					accessor.GetName() == accessor.GetNamespace()+"-"+HOH_HUB_CLUSTER_MCH {
					return true
				}
				return false
			}, workInformer.Informer()).
		WithSync(c.sync).
		ToController("HubClusterController", recorder)
}

func (c *clusterController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	managedClusterName := syncCtx.QueueKey()
	klog.V(2).Infof("Reconciling hub cluster for %s", managedClusterName)
	managedCluster, err := c.clusterLister.Get(managedClusterName)
	if errors.IsNotFound(err) {
		// Spoke cluster not found, could have been deleted, delete manifestwork.
		// TODO: delete manifestwork
		return nil
	}
	if err != nil {
		return err
	}

	desiredSubscription := CreateSubManifestwork(managedClusterName)
	subscription, err := c.workLister.ManifestWorks(managedClusterName).Get(managedClusterName + "-" + HOH_HUB_CLUSTER_SUBSCRIPTION)
	if errors.IsNotFound(err) {
		klog.V(2).Infof("creating subscription manifestwork in %s namespace", managedClusterName)
		_, err := c.workclient.ManifestWorks(managedClusterName).
			Create(ctx, desiredSubscription, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}

	updated, err := EnsureManifestWork(subscription, desiredSubscription)
	if err != nil {
		return err
	}
	if updated {
		desiredSubscription.ObjectMeta.ResourceVersion = subscription.ObjectMeta.ResourceVersion
		_, err := c.workclient.ManifestWorks(managedClusterName).
			Update(ctx, desiredSubscription, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	// if the csv PHASE is Succeeded, then create mch manifestwork to install Hub
	for _, conditions := range subscription.Status.ResourceStatus.Manifests {
		if conditions.ResourceMeta.Kind == "Subscription" {
			for _, value := range conditions.StatusFeedbacks.Values {
				if value.Name == "state" && *value.Value.String == "AtLatestKnown" {
					//fetch user defined mch from annotation
					userDefinedMCH := ""
					if managedCluster.Annotations != nil {
						userDefinedMCH = managedCluster.Annotations["mch"]
					}

					desiredMCH, err := CreateMCHManifestwork(managedClusterName, userDefinedMCH)
					if err != nil {
						return err
					}
					mch, err := c.workLister.ManifestWorks(managedClusterName).Get(managedClusterName + "-" + HOH_HUB_CLUSTER_MCH)
					if errors.IsNotFound(err) {
						klog.V(2).Infof("creating mch manifestwork in %s namespace", managedClusterName)
						_, err := c.workclient.ManifestWorks(managedClusterName).
							Create(ctx, desiredMCH, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}
					if err != nil {
						return err
					}

					updated, err := EnsureManifestWork(mch, desiredMCH)
					if err != nil {
						return err
					}
					if updated {
						desiredMCH.ObjectMeta.ResourceVersion = mch.ObjectMeta.ResourceVersion
						_, err := c.workclient.ManifestWorks(managedClusterName).
							Update(ctx, desiredMCH, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
					return nil
				}
			}
		}
	}

	return nil
}
