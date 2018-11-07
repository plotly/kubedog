package tracker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

type PodFeed interface {
	Added() error
	Succeeded() error
	Failed() error
	ContainerLogChunk(*ContainerLogChunk) error
	ContainerError(ContainerError) error
}

type LogLine struct {
	Timestamp string
	Data      string
}

type ContainerLogChunk struct {
	ContainerName string
	LogLines      []LogLine
}

type ContainerError struct {
	Message       string
	ContainerName string
}

func TrackPod(name, namespace string, kube kubernetes.Interface, feed PodFeed, opts Options) error {
	errorChan := make(chan error, 0)
	doneChan := make(chan struct{}, 0)

	parentContext := opts.ParentContext
	if parentContext == nil {
		parentContext = context.Background()
	}
	ctx, cancel := watchtools.ContextWithOptionalTimeout(parentContext, opts.Timeout)
	defer cancel()

	pod := NewPodTracker(ctx, name, namespace, kube)

	go func() {
		err := pod.Track()
		if err != nil {
			errorChan <- err
		} else {
			doneChan <- struct{}{}
		}
	}()

	for {
		select {
		case chunk := <-pod.ContainerLogChunk:
			if debug() {
				fmt.Printf("Pod `%s` container `%s` log chunk:\n", pod.ResourceName, chunk.ContainerName)
				for _, line := range chunk.LogLines {
					fmt.Printf("[%s] %s\n", line.Timestamp, line.Data)
				}
			}

			err := feed.ContainerLogChunk(chunk)
			if err == StopTrack {
				return nil
			}
			if err != nil {
				return err
			}

		case containerError := <-pod.ContainerError:
			if debug() {
				fmt.Printf("Pod's `%s` container error: %#v", pod.ResourceName, containerError)
			}

			err := feed.ContainerError(containerError)
			if err != nil {
				return err
			}
			if err == StopTrack {
				return nil
			}

		case <-pod.Added:
			if debug() {
				fmt.Printf("Pod `%s` added\n", pod.ResourceName)
			}

			err := feed.Added()
			if err == StopTrack {
				return nil
			}
			if err != nil {
				return err
			}

		case <-pod.Succeeded:
			if debug() {
				fmt.Printf("Pod `%s` succeeded\n", pod.ResourceName)
			}

			err := feed.Succeeded()
			if err != nil {
				return err
			}
			if err == StopTrack {
				return nil
			}

		case <-pod.Failed:
			if debug() {
				fmt.Printf("Pod `%s` failed\n", pod.ResourceName)
			}

			err := feed.Failed()
			if err != nil {
				return err
			}
			if err == StopTrack {
				return nil
			}

		case err := <-errorChan:
			return err

		case <-doneChan:
			return nil
		}
	}
}

type PodTracker struct {
	Tracker

	Added             chan struct{}
	Succeeded         chan struct{}
	Failed            chan struct{}
	ContainerLogChunk chan *ContainerLogChunk
	ContainerError    chan ContainerError

	State                           TrackerState
	ContainerTrackerStates          map[string]TrackerState
	ProcessedContainerLogTimestamps map[string]time.Time
	TrackedContainers               []string

	lastObject     *corev1.Pod
	objectAdded    chan *corev1.Pod
	objectModified chan *corev1.Pod
	objectDeleted  chan *corev1.Pod
	containerDone  chan string
	errors         chan error
}

func NewPodTracker(ctx context.Context, name, namespace string, kube kubernetes.Interface) *PodTracker {
	return &PodTracker{
		Tracker: Tracker{
			Kube:         kube,
			Namespace:    namespace,
			ResourceName: name,
			Context:      ctx,
		},

		Added:             make(chan struct{}, 0),
		Succeeded:         make(chan struct{}, 0),
		Failed:            make(chan struct{}, 0),
		ContainerError:    make(chan ContainerError, 0),
		ContainerLogChunk: make(chan *ContainerLogChunk, 1000),

		State: Initial,
		ContainerTrackerStates:          make(map[string]TrackerState),
		ProcessedContainerLogTimestamps: make(map[string]time.Time),
		TrackedContainers:               make([]string, 0),

		objectAdded:    make(chan *corev1.Pod, 0),
		objectModified: make(chan *corev1.Pod, 0),
		objectDeleted:  make(chan *corev1.Pod, 0),
		errors:         make(chan error, 0),
		containerDone:  make(chan string, 10),
	}
}

func (pod *PodTracker) Track() error {
	err := pod.runInformer()
	if err != nil {
		return err
	}

	for {
		select {
		case containerName := <-pod.containerDone:
			trackedContainers := make([]string, 0)
			for _, name := range pod.TrackedContainers {
				if name != containerName {
					trackedContainers = append(trackedContainers, name)
				}
			}
			pod.TrackedContainers = trackedContainers

			done, err := pod.handlePodState(pod.lastObject)
			if err != nil {
				return err
			}
			if done {
				return nil
			}

		case object := <-pod.objectAdded:
			pod.lastObject = object

			switch pod.State {
			case Initial:
				pod.State = ResourceAdded
				pod.Added <- struct{}{}

				err := pod.runContainersTrackers()
				if err != nil {
					return err
				}
			}

			done, err := pod.handlePodState(object)
			if err != nil {
				return err
			}
			if done {
				return nil
			}

		case object := <-pod.objectModified:
			pod.lastObject = object

			done, err := pod.handlePodState(object)
			if err != nil {
				return err
			}
			if done {
				return nil
			}

		case <-pod.objectDeleted:
			if debug() {
				fmt.Printf("Pod `%s` resource gone: stop tracking\n", pod.ResourceName)
			}
			return nil

		case <-pod.Context.Done():
			return ErrTrackTimeout

		case err := <-pod.errors:
			return err
		}
	}
}

func (pod *PodTracker) handlePodState(object *corev1.Pod) (done bool, err error) {
	err = pod.handleContainersState(object)
	if err != nil {
		return false, err
	}

	if len(pod.TrackedContainers) == 0 {
		if object.Status.Phase == corev1.PodSucceeded {
			pod.Succeeded <- struct{}{}
			done = true
		} else if object.Status.Phase == corev1.PodFailed {
			pod.Failed <- struct{}{}
			done = true
		}
	}

	return
}

func (pod *PodTracker) handleContainersState(object *corev1.Pod) error {
	allContainerStatuses := make([]corev1.ContainerStatus, 0)
	for _, cs := range object.Status.InitContainerStatuses {
		allContainerStatuses = append(allContainerStatuses, cs)
	}
	for _, cs := range object.Status.ContainerStatuses {
		allContainerStatuses = append(allContainerStatuses, cs)
	}

	for _, cs := range allContainerStatuses {
		oldState := pod.ContainerTrackerStates[cs.Name]

		if cs.State.Waiting != nil {
			pod.ContainerTrackerStates[cs.Name] = ContainerWaiting

			switch cs.State.Waiting.Reason {
			case "ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff":
				pod.ContainerError <- ContainerError{
					ContainerName: cs.Name,
					Message:       fmt.Sprintf("%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message),
				}
			}
		}
		if cs.State.Running != nil {
			pod.ContainerTrackerStates[cs.Name] = ContainerRunning
		}
		if cs.State.Terminated != nil {
			pod.ContainerTrackerStates[cs.Name] = ContainerTerminated
		}

		if oldState != pod.ContainerTrackerStates[cs.Name] {
			if debug() {
				fmt.Printf("Pod `%s` container `%s` state changed %#v -> %#v\n", pod.ResourceName, cs.Name, oldState, pod.ContainerTrackerStates[cs.Name])
			}
		}
	}

	return nil
}

func (pod *PodTracker) followContainerLogs(containerName string) error {
	req := pod.Kube.Core().
		Pods(pod.Namespace).
		GetLogs(pod.ResourceName, &corev1.PodLogOptions{
			Container:  containerName,
			Timestamps: true,
			Follow:     true,
		})

	readCloser, err := req.Stream()
	if err != nil {
		return err
	}
	defer readCloser.Close()

	chunkBuf := make([]byte, 1024*64)
	lineBuf := make([]byte, 0, 1024*4)

	for {
		n, err := readCloser.Read(chunkBuf)

		if n > 0 {
			chunkLines := make([]LogLine, 0)
			for i := 0; i < n; i++ {
				bt := chunkBuf[i]

				if bt == '\n' {
					line := string(lineBuf)
					lineBuf = lineBuf[:0]

					lineParts := strings.SplitN(line, " ", 2)
					if len(lineParts) == 2 {
						chunkLines = append(chunkLines, LogLine{Timestamp: lineParts[0], Data: lineParts[1]})
					}

					continue
				}

				lineBuf = append(lineBuf, bt)
			}

			pod.ContainerLogChunk <- &ContainerLogChunk{
				ContainerName: containerName,
				LogLines:      chunkLines,
			}
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return err
		}

		select {
		case <-pod.Context.Done():
			return ErrTrackTimeout
		default:
		}
	}

	return nil
}

func (pod *PodTracker) trackContainer(containerName string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			state := pod.ContainerTrackerStates[containerName]

			switch state {
			case ContainerRunning, ContainerTerminated:
				return pod.followContainerLogs(containerName)
			case Initial, ContainerWaiting:
			default:
				return fmt.Errorf("unknown Pod's `%s` Container `%s` tracker state `%s`", pod.ResourceName, containerName, state)
			}

		case <-pod.Context.Done():
			return ErrTrackTimeout
		}
	}
}

func (pod *PodTracker) runContainersTrackers() error {
	podManifest, err := pod.Kube.Core().
		Pods(pod.Namespace).
		Get(pod.ResourceName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	allContainersNames := make([]string, 0)
	for _, containerConf := range podManifest.Spec.InitContainers {
		allContainersNames = append(allContainersNames, containerConf.Name)
	}
	for _, containerConf := range podManifest.Spec.Containers {
		allContainersNames = append(allContainersNames, containerConf.Name)
	}
	for i := range allContainersNames {
		containerName := allContainersNames[i]

		pod.ContainerTrackerStates[containerName] = Initial
		pod.TrackedContainers = append(pod.TrackedContainers, containerName)

		go func() {
			if debug() {
				fmt.Printf("Starting to track Pod's `%s` container `%s`\n", pod.ResourceName, containerName)
			}

			err := pod.trackContainer(containerName)
			if err != nil {
				pod.errors <- err
			}

			if debug() {
				fmt.Printf("Done tracking Pod's `%s` container `%s`\n", pod.ResourceName, containerName)
			}

			pod.containerDone <- containerName
		}()
	}

	return nil
}

func (pod *PodTracker) runInformer() error {
	tweakListOptions := func(options metav1.ListOptions) metav1.ListOptions {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", pod.ResourceName).String()
		return options
	}
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return pod.Kube.Core().Pods(pod.Namespace).List(tweakListOptions(options))
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return pod.Kube.Core().Pods(pod.Namespace).Watch(tweakListOptions(options))
		},
	}

	go func() {
		_, err := watchtools.UntilWithSync(pod.Context, lw, &corev1.Pod{}, nil, func(e watch.Event) (bool, error) {
			if debug() {
				fmt.Printf("Pod `%s` informer event: %#v\n", pod.ResourceName, e.Type)
			}

			var object *corev1.Pod

			if e.Type != watch.Error {
				var ok bool
				object, ok = e.Object.(*corev1.Pod)
				if !ok {
					return true, fmt.Errorf("expected %s to be a *corev1.Pod, got %T", pod.ResourceName, e.Object)
				}
			}

			if e.Type == watch.Added {
				pod.objectAdded <- object
			} else if e.Type == watch.Modified {
				pod.objectModified <- object
			} else if e.Type == watch.Deleted {
				pod.objectDeleted <- object
			}

			return false, nil
		})

		if err != nil {
			pod.errors <- err
		}

		if debug() {
			fmt.Printf("Pod `%s` informer done\n", pod.ResourceName)
		}
	}()

	return nil
}