/*
Copyright 2015 The Kubernetes Authors.

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

package controller

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"k8s.io/klog/v2"

	pool "gopkg.in/go-playground/pool.v3"
	apiv1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"

	"github.com/jcmoraisjr/haproxy-ingress/pkg/common/k8s"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/common/task"
)

const (
	updateInterval = 60 * time.Second
)

// StatusSync ...
type StatusSync interface {
	Run(stopCh <-chan struct{})
	Shutdown()
}

// statusSync keeps the status IP in each Ingress rule updated executing a periodic check
// in all the defined rules. To simplify the process leader election is used so the update
// is executed only in one node (Ingress controllers can be scaled to more than one)
// If the controller is running with the flag --publish-service (with a valid service)
// the IP address behind the service is used, if not the source is the IP/s of the node/s
type statusSync struct {
	ctx context.Context
	ic  *GenericController
	// pod contains runtime information about this pod
	pod *k8s.PodInfo
	//
	elector *leaderelection.LeaderElector
	// workqueue used to keep in sync the status IP/s
	// in the Ingress rules
	syncQueue *task.Queue
}

// Run starts the loop to keep the status in sync
func (s statusSync) Run(stopCh <-chan struct{}) {
	go s.elector.Run(context.Background())
	go wait.Forever(s.update, updateInterval)
	go s.syncQueue.Run(time.Second, stopCh)
	<-stopCh
}

func (s *statusSync) update() {
	// send a dummy object to the queue to force a sync
	s.syncQueue.Enqueue("sync status")
}

// Shutdown stop the sync. In case the instance is the leader it will remove the current IP
// if there is no other instances running.
func (s statusSync) Shutdown() {
	go s.syncQueue.Shutdown()
	// remove IP from Ingress
	if !s.elector.IsLeader() {
		return
	}

	if !s.ic.cfg.UpdateStatusOnShutdown {
		klog.Warningf("skipping update of status of Ingress rules")
		return
	}

	klog.Infof("updating status of Ingress rules (remove)")

	addrs, err := s.runningAddresses()
	if err != nil {
		klog.Errorf("error obtaining running IPs: %v", addrs)
		return
	}

	if len(addrs) > 1 {
		// leave the job to the next leader
		klog.Infof("leaving status update for next leader (%v)", len(addrs))
		return
	}

	if s.isRunningMultiplePods() {
		klog.V(2).Infof("skipping Ingress status update (multiple pods running - another one will be elected as master)")
		return
	}

	klog.Infof("removing address from ingress status (%v)", addrs)
	if err := s.updateStatus([]apiv1.LoadBalancerIngress{}); err != nil {
		klog.Errorf("cannot update status due to an error: %s", err.Error())
	}
}

func (s *statusSync) sync(key interface{}) error {
	if s.syncQueue.IsShuttingDown() {
		klog.V(2).Infof("skipping Ingress status update (shutting down in progress)")
		return nil
	}

	if !s.elector.IsLeader() {
		klog.V(2).Infof("skipping Ingress status update (I am not the current leader)")
		return nil
	}

	addrs, err := s.runningAddresses()
	if err != nil {
		return err
	}
	if err := s.updateStatus(sliceToStatus(addrs)); err != nil {
		return err
	}

	return nil
}

func (s statusSync) keyfunc(input interface{}) (interface{}, error) {
	return input, nil
}

// NewStatusSyncer returns a new Sync instance
func NewStatusSyncer(ic *GenericController) StatusSync {
	pod, err := k8s.GetPodDetails(ic.cfg.Client)
	if err != nil {
		klog.Exitf("unexpected error obtaining pod information: %v", err)
	}

	st := statusSync{
		ctx: context.Background(),
		pod: pod,
		ic:  ic,
		// StatusConfig: config,
	}
	st.syncQueue = task.NewCustomTaskQueue(st.sync, st.keyfunc)

	electionID := fmt.Sprintf("%v-%v", ic.cfg.ElectionID, ic.cfg.IngressClass)

	callbacks := leaderelection.LeaderCallbacks{
		OnStartedLeading: func(context.Context) {
			klog.V(2).Infof("I am the new status update leader")
		},
		OnStoppedLeading: func() {
			klog.V(2).Infof("I am not status update leader anymore")
		},
		OnNewLeader: func(identity string) {
			klog.Infof("new leader elected: %v", identity)
		},
	}

	broadcaster := record.NewBroadcaster()
	hostname, _ := os.Hostname()

	recorder := broadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{
		Component: "ingress-leader-elector",
		Host:      hostname,
	})

	lock := resourcelock.ConfigMapLock{
		ConfigMapMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: electionID},
		Client:        ic.cfg.Client.CoreV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      pod.Name,
			EventRecorder: recorder,
		},
	}

	ttl := 30 * time.Second
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          &lock,
		LeaseDuration: ttl,
		RenewDeadline: ttl / 2,
		RetryPeriod:   ttl / 4,
		Callbacks:     callbacks,
	})

	if err != nil {
		klog.Exitf("unexpected error starting leader election: %v", err)
	}

	st.elector = le
	return st
}

// runningAddresses returns a list of IP addresses and/or FQDN where the
// ingress controller is currently running
func (s *statusSync) runningAddresses() ([]string, error) {
	if s.ic.cfg.PublishService != "" {
		ns, name, _ := k8s.ParseNameNS(s.ic.cfg.PublishService)
		svc, err := s.ic.cfg.Client.CoreV1().Services(ns).Get(s.ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		addrs := []string{}
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			if ip.IP == "" {
				addrs = append(addrs, ip.Hostname)
			} else {
				addrs = append(addrs, ip.IP)
			}
		}
		addrs = append(addrs, svc.Spec.ExternalIPs...)

		return addrs, nil
	}

	// get information about all the pods running the ingress controller
	pods, err := s.ic.cfg.Client.CoreV1().Pods(s.pod.Namespace).List(s.ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(s.pod.Labels).String(),
	})
	if err != nil {
		return nil, err
	}

	addrs := []string{}
	for _, pod := range pods.Items {
		name := k8s.GetNodeIP(s.ic.cfg.Client, pod.Spec.NodeName, s.ic.cfg.UseNodeInternalIP)
		if !stringInSlice(name, addrs) {
			addrs = append(addrs, name)
		}
	}
	return addrs, nil
}

func (s *statusSync) isRunningMultiplePods() bool {
	pods, err := s.ic.cfg.Client.CoreV1().Pods(s.pod.Namespace).List(s.ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(s.pod.Labels).String(),
	})
	if err != nil {
		return false
	}

	return len(pods.Items) > 1
}

func stringInSlice(a string, slice []string) bool {
	for _, b := range slice {
		if b == a {
			return true
		}
	}
	return false
}

// sliceToStatus converts a slice of IP and/or hostnames to LoadBalancerIngress
func sliceToStatus(endpoints []string) []apiv1.LoadBalancerIngress {
	lbi := []apiv1.LoadBalancerIngress{}
	for _, ep := range endpoints {
		if net.ParseIP(ep) == nil {
			lbi = append(lbi, apiv1.LoadBalancerIngress{Hostname: ep})
		} else {
			lbi = append(lbi, apiv1.LoadBalancerIngress{IP: ep})
		}
	}

	sort.SliceStable(lbi, func(a, b int) bool {
		return lbi[a].IP < lbi[b].IP
	})

	return lbi
}

// updateStatus changes the status information of Ingress rules
// If the backend function CustomIngressStatus returns a value different
// of nil then it uses the returned value or the newIngressPoint values
func (s *statusSync) updateStatus(newIngressPoint []apiv1.LoadBalancerIngress) error {
	ings, err := s.ic.newctrl.GetIngressList()
	if err != nil {
		return err
	}

	p := pool.NewLimited(10)
	defer p.Close()

	batch := p.Batch()

	for _, ing := range ings {
		if !s.ic.newctrl.IsValidClass(ing) {
			continue
		}

		var callback func(*networking.Ingress) []apiv1.LoadBalancerIngress
		if s.ic.cfg.Backend != nil {
			callback = s.ic.cfg.Backend.UpdateIngressStatus
		} else {
			callback = func(*networking.Ingress) []apiv1.LoadBalancerIngress { return nil }
		}
		batch.Queue(runUpdate(s.ctx, ing, newIngressPoint, s.ic.cfg.Client, callback))
	}

	batch.QueueComplete()
	batch.WaitAll()

	return nil
}

func runUpdate(ctx context.Context, ing *networking.Ingress, status []apiv1.LoadBalancerIngress,
	client clientset.Interface,
	statusFunc func(*networking.Ingress) []apiv1.LoadBalancerIngress) pool.WorkFunc {
	return func(wu pool.WorkUnit) (interface{}, error) {
		if wu.IsCancelled() {
			return nil, nil
		}

		addrs := status
		ca := statusFunc(ing)
		if ca != nil {
			addrs = ca
		}
		sort.SliceStable(addrs, lessLoadBalancerIngress(addrs))

		curIPs := ing.Status.LoadBalancer.Ingress
		sort.SliceStable(curIPs, lessLoadBalancerIngress(curIPs))

		if ingressSliceEqual(addrs, curIPs) {
			klog.V(3).Infof("skipping update of Ingress %v/%v (no change)", ing.Namespace, ing.Name)
			return true, nil
		}

		ingClient := client.NetworkingV1().Ingresses(ing.Namespace)

		currIng, err := ingClient.Get(ctx, ing.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("unexpected error searching Ingress %v/%v: %w", ing.Namespace, ing.Name, err)
		}

		klog.Infof("updating Ingress %v/%v status to %v", currIng.Namespace, currIng.Name, addrs)
		currIng.Status.LoadBalancer.Ingress = addrs
		_, err = ingClient.UpdateStatus(ctx, currIng, metav1.UpdateOptions{})
		if err != nil {
			klog.Warningf("error updating ingress rule: %v", err)
		}

		return true, nil
	}
}

func lessLoadBalancerIngress(addrs []apiv1.LoadBalancerIngress) func(int, int) bool {
	return func(a, b int) bool {
		switch strings.Compare(addrs[a].Hostname, addrs[b].Hostname) {
		case -1:
			return true
		case 1:
			return false
		}
		return addrs[a].IP < addrs[b].IP
	}
}

func ingressSliceEqual(lhs, rhs []apiv1.LoadBalancerIngress) bool {
	if len(lhs) != len(rhs) {
		return false
	}

	for i := range lhs {
		if lhs[i].IP != rhs[i].IP {
			return false
		}
		if lhs[i].Hostname != rhs[i].Hostname {
			return false
		}
	}
	return true
}
