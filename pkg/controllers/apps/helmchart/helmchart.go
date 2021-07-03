/*
Copyright 2021 The Clusternet Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package helmchart

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	appsapi "github.com/clusternet/clusternet/pkg/apis/apps/v1alpha1"
	clusternetClientSet "github.com/clusternet/clusternet/pkg/generated/clientset/versioned"
	appInformers "github.com/clusternet/clusternet/pkg/generated/informers/externalversions/apps/v1alpha1"
	appListers "github.com/clusternet/clusternet/pkg/generated/listers/apps/v1alpha1"
)

type SyncHandlerFunc func(chart *appsapi.HelmChart) error

// Controller is a controller that handle HelmChart
type Controller struct {
	ctx context.Context

	clusternetClient clusternetClientSet.Interface

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface

	helmChartLister appListers.HelmChartLister
	helmChartSynced cache.InformerSynced

	SyncHandler SyncHandlerFunc
}

func NewController(ctx context.Context, clusternetClient clusternetClientSet.Interface,
	helmChartInformer appInformers.HelmChartInformer, syncHandler SyncHandlerFunc) (*Controller, error) {
	if syncHandler == nil {
		return nil, fmt.Errorf("syncHandler must be set")
	}

	c := &Controller{
		ctx:              ctx,
		clusternetClient: clusternetClient,
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "helmChart"),
		helmChartLister:  helmChartInformer.Lister(),
		helmChartSynced:  helmChartInformer.Informer().HasSynced,
		SyncHandler:      syncHandler,
	}

	// Manage the addition/update of HelmChart
	helmChartInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addHelmChart,
		UpdateFunc: c.updateHelmChart,
		DeleteFunc: c.deleteHelmChart,
	})

	return c, nil
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("starting helmchart controller...")
	defer klog.Info("shutting down helmchart controller")

	// Wait for the caches to be synced before starting workers
	klog.V(5).Info("waiting for informer caches to sync")
	if !cache.WaitForCacheSync(stopCh, c.helmChartSynced) {
		return
	}

	klog.V(5).Infof("starting %d worker threads", workers)
	// Launch workers to process HelmChart resources
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *Controller) addHelmChart(obj interface{}) {
	chart := obj.(*appsapi.HelmChart)
	klog.V(4).Infof("adding HelmChart %q", klog.KObj(chart))
	c.enqueue(chart)
}

func (c *Controller) updateHelmChart(old, cur interface{}) {
	oldChart := old.(*appsapi.HelmChart)
	newChart := cur.(*appsapi.HelmChart)

	// Decide whether discovery has reported a spec change.
	if reflect.DeepEqual(oldChart.Spec, newChart.Spec) {
		klog.V(4).Infof("no updates on the spec of HelmChart %q, skipping syncing", oldChart.Name)
		return
	}

	klog.V(4).Infof("updating HelmChart %q", klog.KObj(oldChart))
	c.enqueue(newChart)
}

func (c *Controller) deleteHelmChart(obj interface{}) {
	chart, ok := obj.(*appsapi.HelmChart)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		_, ok = tombstone.Obj.(*appsapi.HelmChart)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a HelmChart %#v", obj))
			return
		}
	}
	klog.V(4).Infof("deleting HelmChart %q", klog.KObj(chart))
	c.enqueue(chart)
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// HelmChart resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("successfully synced HelmChart %q", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the HelmChart resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// If an error occurs during handling, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.

	// Convert the namespace/name string into a distinct namespace and name
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	klog.V(4).Infof("start processing HelmChart %q", key)
	// Get the HelmChart resource with this name
	chart, err := c.helmChartLister.HelmCharts(ns).Get(name)
	// The HelmChart resource may no longer exist, in which case we stop processing.
	if errors.IsNotFound(err) {
		klog.V(2).Infof("HelmChart %q has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	return c.SyncHandler(chart)
}

func (c *Controller) UpdateChartStatus(chart *appsapi.HelmChart, status *appsapi.HelmChartStatus) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance

	klog.V(5).Infof("try to update HelmChart %q status", chart.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		chart.Status = *status
		_, err := c.clusternetClient.AppsV1alpha1().HelmCharts(chart.Namespace).UpdateStatus(c.ctx, chart, metav1.UpdateOptions{})
		if err == nil {
			//TODO
			return nil
		}

		if updated, err := c.helmChartLister.HelmCharts(chart.Namespace).Get(chart.Name); err == nil {
			// make a copy so we don't mutate the shared cache
			chart = updated.DeepCopy()
		} else {
			utilruntime.HandleError(fmt.Errorf("error getting updated HelmChart %q from lister: %v", chart.Name, err))
		}
		return err
	})
}

// enqueue takes a HelmChart resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than HelmChart.
func (c *Controller) enqueue(chart *appsapi.HelmChart) {
	key, err := cache.MetaNamespaceKeyFunc(chart)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}