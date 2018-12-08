package follow

import (
	"fmt"

	"github.com/flant/kubedog/pkg/log"
	"github.com/flant/kubedog/pkg/tracker"
	"k8s.io/client-go/kubernetes"
)

func TrackStatefulSet(name, namespace string, kube kubernetes.Interface, opts tracker.Options) error {
	feed := &tracker.ControllerFeedProto{
		AddedFunc: func(ready bool) error {
			if ready {
				fmt.Printf("sts/%s appears to be ready\n", name)
			} else {
				fmt.Printf("sts/%s added\n", name)
			}
			return nil
		},
		ReadyFunc: func() error {
			fmt.Printf("sts/%s become READY\n", name)
			return nil
		},
		FailedFunc: func(reason string) error {
			fmt.Printf("sts/%s FAIL: %s\n", name, reason)
			return nil
		},
		AddedPodFunc: func(pod tracker.ReplicaSetPod) error {
			fmt.Printf("+ sts/%s %s\n", name, pod.Name)
			return nil
		},
		PodErrorFunc: func(podError tracker.ReplicaSetPodError) error {
			fmt.Printf("sts/%s %s %s error: %s\n", name, podError.PodName, podError.ContainerName, podError.Message)
			return nil
		},
		PodLogChunkFunc: func(chunk *tracker.ReplicaSetPodLogChunk) error {
			log.SetLogHeader(fmt.Sprintf("sts/%s %s %s:", name, chunk.PodName, chunk.ContainerName))
			for _, line := range chunk.LogLines {
				fmt.Println(line.Data)
			}
			return nil
		},
	}

	return tracker.TrackStatefulSet(name, namespace, kube, feed, opts)
}
