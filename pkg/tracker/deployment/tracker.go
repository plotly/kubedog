package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/flant/kubedog/pkg/tracker"
	"github.com/flant/kubedog/pkg/tracker/debug"
	"github.com/flant/kubedog/pkg/tracker/event"
	"github.com/flant/kubedog/pkg/tracker/pod"
	"github.com/flant/kubedog/pkg/tracker/replicaset"
	"github.com/flant/kubedog/pkg/utils"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	watchtools "k8s.io/client-go/tools/watch"
)

type DeploymentStatus struct {
	Pods map[string]pod.PodStatus

	appsv1.DeploymentStatus
	DesiredReplicas int32

	IsFailed     bool
	FailedReason string

	ReadyStatus tracker.ReadyStatus
}

func NewDeploymentStatus(readyStatus tracker.ReadyStatus, isFailed bool, failedReason string, kubeSpec appsv1.DeploymentSpec, kubeStatus appsv1.DeploymentStatus, podsStatuses map[string]pod.PodStatus) DeploymentStatus {
	res := DeploymentStatus{
		DeploymentStatus: kubeStatus,
		DesiredReplicas:  *kubeSpec.Replicas,
		Pods:             make(map[string]pod.PodStatus),
		ReadyStatus:      readyStatus,
		IsFailed:         isFailed,
		FailedReason:     failedReason,
	}
	for k, v := range podsStatuses {
		res.Pods[k] = v
	}
	return res
}

type Tracker struct {
	tracker.Tracker
	LogsFromTime time.Time

	CurrentReady bool

	State                 string
	Conditions            []string
	FinalDeploymentStatus appsv1.DeploymentStatus
	NewReplicaSetName     string
	knownReplicaSets      map[string]*appsv1.ReplicaSet
	lastObject            *appsv1.Deployment
	readyStatus           tracker.ReadyStatus
	failedReason          string
	podStatuses           map[string]pod.PodStatus

	Added           chan bool
	Ready           chan bool
	Failed          chan string
	EventMsg        chan string
	AddedReplicaSet chan replicaset.ReplicaSet
	AddedPod        chan replicaset.ReplicaSetPod
	PodLogChunk     chan *replicaset.ReplicaSetPodLogChunk
	PodError        chan replicaset.ReplicaSetPodError
	StatusReport    chan DeploymentStatus

	resourceAdded         chan *appsv1.Deployment
	resourceModified      chan *appsv1.Deployment
	resourceDeleted       chan *appsv1.Deployment
	resourceFailed        chan string
	replicaSetAdded       chan *appsv1.ReplicaSet
	replicaSetModified    chan *appsv1.ReplicaSet
	replicaSetDeleted     chan *appsv1.ReplicaSet
	podAdded              chan *corev1.Pod
	podDone               chan string
	errors                chan error
	podStatusesReport     chan map[string]pod.PodStatus
	replicaSetPodLogChunk chan *replicaset.ReplicaSetPodLogChunk
	replicaSetPodError    chan replicaset.ReplicaSetPodError

	TrackedPods []string
}

func NewTracker(ctx context.Context, name, namespace string, kube kubernetes.Interface, opts tracker.Options) *Tracker {
	if debug.Debug() {
		fmt.Printf("> deployment.NewTracker\n")
	}
	return &Tracker{
		Tracker: tracker.Tracker{
			Kube:             kube,
			Namespace:        namespace,
			FullResourceName: fmt.Sprintf("deploy/%s", name),
			ResourceName:     name,
			Context:          ctx,
		},

		LogsFromTime: opts.LogsFromTime,

		Added:           make(chan bool, 0),
		Ready:           make(chan bool, 1),
		Failed:          make(chan string, 1),
		EventMsg:        make(chan string, 1),
		AddedReplicaSet: make(chan replicaset.ReplicaSet, 10),
		AddedPod:        make(chan replicaset.ReplicaSetPod, 10),
		PodLogChunk:     make(chan *replicaset.ReplicaSetPodLogChunk, 1000),
		PodError:        make(chan replicaset.ReplicaSetPodError, 0),
		StatusReport:    make(chan DeploymentStatus, 100),
		//PodReady:        make(chan bool, 1),

		knownReplicaSets: make(map[string]*appsv1.ReplicaSet),
		podStatuses:      make(map[string]pod.PodStatus),
		TrackedPods:      make([]string, 0),

		//PodError: make(chan PodError, 0),
		resourceAdded:         make(chan *appsv1.Deployment, 1),
		resourceModified:      make(chan *appsv1.Deployment, 1),
		resourceDeleted:       make(chan *appsv1.Deployment, 1),
		resourceFailed:        make(chan string, 1),
		replicaSetAdded:       make(chan *appsv1.ReplicaSet, 1),
		replicaSetModified:    make(chan *appsv1.ReplicaSet, 1),
		replicaSetDeleted:     make(chan *appsv1.ReplicaSet, 1),
		podAdded:              make(chan *corev1.Pod, 1),
		podDone:               make(chan string, 1),
		errors:                make(chan error, 0),
		podStatusesReport:     make(chan map[string]pod.PodStatus),
		replicaSetPodLogChunk: make(chan *replicaset.ReplicaSetPodLogChunk, 1000),
		replicaSetPodError:    make(chan replicaset.ReplicaSetPodError, 1),
	}
}

// Track starts tracking of deployment rollout process.
// watch only for one deployment resource with name d.ResourceName within the namespace with name d.Namespace
// Watcher can wait for namespace creation and then for deployment creation
// watcher receives added event if deployment is started
// watch is infinite by default
// there is option StopOnAvailable — if true, watcher stops after deployment has available status
// you can define custom stop triggers using custom implementation of ControllerFeed.
func (d *Tracker) Track() (err error) {
	if debug.Debug() {
		fmt.Printf("> DeploymentTracker.Track()\n")
	}

	d.runDeploymentInformer()

	for {
		select {
		case object := <-d.resourceAdded:
			ready, err := d.handleDeploymentState(object)
			if err != nil {
				if debug.Debug() {
					fmt.Printf("handle deployment state error: %v", err)
				}
				return err
			}
			if debug.Debug() {
				fmt.Printf("deployment `%s` initial ready state: %v\n", d.ResourceName, ready)
			}

			switch d.State {
			case "":
				d.State = "Started"
				d.Added <- ready
			}

			d.runReplicaSetsInformer()
			d.runPodsInformer()
			d.runEventsInformer(object)

		case object := <-d.resourceModified:
			ready, err := d.handleDeploymentState(object)
			if err != nil {
				return err
			}
			if ready {
				d.Ready <- true
			}

		case <-d.resourceDeleted:
			d.lastObject = nil
			d.StatusReport <- DeploymentStatus{}

			d.State = "Deleted"
			d.Failed <- "resource deleted"

		case reason := <-d.resourceFailed:
			d.State = "Failed"
			d.failedReason = reason

			if d.lastObject != nil {
				d.StatusReport <- NewDeploymentStatus(d.readyStatus, (d.State == "Failed"), d.failedReason, d.lastObject.Spec, d.lastObject.Status, d.podStatuses)
			}
			d.Failed <- reason

		case rs := <-d.replicaSetAdded:
			if debug.Debug() {
				fmt.Printf("rs/%s added\n", rs.Name)
			}

			d.knownReplicaSets[rs.Name] = rs

			rsNew, err := utils.IsReplicaSetNew(d.lastObject, d.knownReplicaSets, rs.Name)
			if err != nil {
				return err
			}

			d.AddedReplicaSet <- replicaset.ReplicaSet{
				Name:  rs.Name,
				IsNew: rsNew,
			}

		case rs := <-d.replicaSetModified:
			if debug.Debug() {
				fmt.Printf("rs/%s modified\n", rs.Name)
			}

			d.knownReplicaSets[rs.Name] = rs

		case rs := <-d.replicaSetDeleted:
			delete(d.knownReplicaSets, rs.Name)

		case pod := <-d.podAdded:
			if debug.Debug() {
				fmt.Printf("po/%s added\n", pod.Name)
			}

			rsName := utils.GetPodReplicaSetName(pod)
			rsNew, err := utils.IsReplicaSetNew(d.lastObject, d.knownReplicaSets, rsName)
			if err != nil {
				return err
			}

			rsPod := replicaset.ReplicaSetPod{
				Name: pod.Name,
				ReplicaSet: replicaset.ReplicaSet{
					Name:  rsName,
					IsNew: rsNew,
				},
			}

			d.AddedPod <- rsPod

			err = d.runPodTracker(pod.Name, rsName)
			if err != nil {
				return err
			}

		case podName := <-d.podDone:
			trackedPods := make([]string, 0)
			for _, name := range d.TrackedPods {
				if name != podName {
					trackedPods = append(trackedPods, name)
				}
			}
			d.TrackedPods = trackedPods

		case podStatuses := <-d.podStatusesReport:
			for podName, podStatus := range podStatuses {
				d.podStatuses[podName] = podStatus
			}
			if d.lastObject != nil {
				d.StatusReport <- NewDeploymentStatus(d.readyStatus, (d.State == "Failed"), d.failedReason, d.lastObject.Spec, d.lastObject.Status, d.podStatuses)
			}

		case rsChunk := <-d.replicaSetPodLogChunk:
			rsNew, err := utils.IsReplicaSetNew(d.lastObject, d.knownReplicaSets, rsChunk.ReplicaSet.Name)
			if err != nil {
				return err
			}
			rsChunk.ReplicaSet.IsNew = rsNew
			d.PodLogChunk <- rsChunk

		case rsPodError := <-d.replicaSetPodError:
			rsNew, err := utils.IsReplicaSetNew(d.lastObject, d.knownReplicaSets, rsPodError.ReplicaSet.Name)
			if err != nil {
				return err
			}
			rsPodError.ReplicaSet.IsNew = rsNew
			d.PodError <- rsPodError

		case <-d.Context.Done():
			return tracker.ErrTrackInterrupted

		case err := <-d.errors:
			return err
		}
	}

	return err
}

// runDeploymentInformer watch for deployment events
func (d *Tracker) runDeploymentInformer() {
	client := d.Kube

	tweakListOptions := func(options metav1.ListOptions) metav1.ListOptions {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", d.ResourceName).String()
		return options
	}
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return client.AppsV1().Deployments(d.Namespace).List(tweakListOptions(options))
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return client.AppsV1().Deployments(d.Namespace).Watch(tweakListOptions(options))
		},
	}

	go func() {
		_, err := watchtools.UntilWithSync(d.Context, lw, &appsv1.Deployment{}, nil, func(e watch.Event) (bool, error) {
			if debug.Debug() {
				fmt.Printf("    deploy/%s event: %#v\n", d.ResourceName, e.Type)
			}

			var object *appsv1.Deployment

			if e.Type != watch.Error {
				var ok bool
				object, ok = e.Object.(*appsv1.Deployment)
				if !ok {
					return true, fmt.Errorf("expected %s to be a *extension.Deployment, got %T", d.ResourceName, e.Object)
				}
			}

			switch e.Type {
			case watch.Added:
				d.resourceAdded <- object
			case watch.Modified:
				d.resourceModified <- object
			case watch.Deleted:
				d.resourceDeleted <- object
			case watch.Error:
				err := fmt.Errorf("deployment error: %v", e.Object)
				//d.errors <- err
				return true, err
			}

			return false, nil
		})

		if err != nil {
			d.errors <- err
		}

		if debug.Debug() {
			fmt.Printf("      deploy/%s informer DONE\n", d.ResourceName)
		}
	}()

	return
}

// runReplicaSetsInformer watch for deployment events
func (d *Tracker) runReplicaSetsInformer() {
	if d.lastObject == nil {
		// This shouldn't happen!
		// TODO add error
		return
	}

	rsInformer := replicaset.NewReplicaSetInformer(&d.Tracker, utils.ControllerAccessor(d.lastObject))
	rsInformer.WithChannels(d.replicaSetAdded, d.replicaSetModified, d.replicaSetDeleted, d.errors)
	rsInformer.Run()

	return
}

// runDeploymentInformer watch for deployment events
func (d *Tracker) runPodsInformer() {
	if d.lastObject == nil {
		// This shouldn't happen!
		// TODO add error
		return
	}

	podsInformer := pod.NewPodsInformer(&d.Tracker, utils.ControllerAccessor(d.lastObject))
	podsInformer.WithChannels(d.podAdded, d.errors)
	podsInformer.Run()

	return
}

func (d *Tracker) runPodTracker(podName, rsName string) error {
	errorChan := make(chan error, 0)
	doneChan := make(chan struct{}, 0)

	podTracker := pod.NewTracker(d.Context, podName, d.Namespace, d.Kube)
	if !d.LogsFromTime.IsZero() {
		podTracker.LogsFromTime = d.LogsFromTime
	}
	d.TrackedPods = append(d.TrackedPods, podName)

	go func() {
		if debug.Debug() {
			fmt.Printf("Starting Deployment's `%s` Pod `%s` tracker. pod state: %v\n", d.ResourceName, podTracker.ResourceName, podTracker.State)
		}

		err := podTracker.Start()
		if err != nil {
			errorChan <- err
		} else {
			doneChan <- struct{}{}
		}

		if debug.Debug() {
			fmt.Printf("Done Deployment's `%s` Pod `%s` tracker\n", d.ResourceName, podTracker.ResourceName)
		}
	}()

	go func() {
		for {
			select {
			case chunk := <-podTracker.ContainerLogChunk:
				rsChunk := &replicaset.ReplicaSetPodLogChunk{
					PodLogChunk: &pod.PodLogChunk{
						ContainerLogChunk: chunk,
						PodName:           podTracker.ResourceName,
					},
					ReplicaSet: replicaset.ReplicaSet{
						Name: rsName,
					},
				}
				d.replicaSetPodLogChunk <- rsChunk

			case containerError := <-podTracker.ContainerError:
				podError := replicaset.ReplicaSetPodError{
					PodError: pod.PodError{
						ContainerError: containerError,
						PodName:        podTracker.ResourceName,
					},
					ReplicaSet: replicaset.ReplicaSet{
						Name: rsName,
					},
				}
				d.replicaSetPodError <- podError

			case msg := <-podTracker.EventMsg:
				d.EventMsg <- fmt.Sprintf("po/%s %s", podTracker.ResourceName, msg)
			case <-podTracker.Added:
			case <-podTracker.Succeeded:
			case <-podTracker.Failed:
			case <-podTracker.Ready:
			case podStatus := <-podTracker.StatusReport:
				d.podStatusesReport <- map[string]pod.PodStatus{podTracker.ResourceName: podStatus}
			case err := <-errorChan:
				d.errors <- err
				return
			case <-doneChan:
				d.podDone <- podTracker.ResourceName
				return
			}
		}
	}()

	return nil
}

// TODO get rid of previous object
func (d *Tracker) handleDeploymentState(object *appsv1.Deployment) (ready bool, err error) {
	if debug.Debug() {
		fmt.Printf("%s\n%s\n",
			getDeploymentStatus(d.Kube, d.lastObject, object),
			getReplicaSetsStatus(d.Kube, object))
	}

	if debug.Debug() {
		evList, err := utils.ListEventsForObject(d.Kube, object)
		if err != nil {
			return false, err
		}
		utils.DescribeEvents(evList)
	}

	prevReady := false
	newStatus := object.Status
	// calc new status
	if d.lastObject != nil {
		prevReady = d.CurrentReady
		d.readyStatus = utils.DeploymentReadyStatus(d.lastObject, &newStatus)
	} else {
		d.readyStatus = utils.DeploymentReadyStatus(object, &newStatus)
	}

	d.CurrentReady = d.readyStatus.IsReady
	d.lastObject = object

	d.StatusReport <- NewDeploymentStatus(d.readyStatus, (d.State == "Failed"), d.failedReason, d.lastObject.Spec, d.lastObject.Status, d.podStatuses)

	if prevReady == false && d.CurrentReady == true {
		d.FinalDeploymentStatus = newStatus
		ready = true
	}

	if ready && debug.Debug() {
		fmt.Printf("Deployment READY.\n")
	}

	return
}

// runEventsInformer watch for Deployment events
func (d *Tracker) runEventsInformer(resource interface{}) {
	//if d.lastObject == nil {
	//	return
	//}

	eventInformer := event.NewEventInformer(&d.Tracker, resource)
	eventInformer.WithChannels(d.EventMsg, d.resourceFailed, d.errors)
	eventInformer.Run()

	return
}
