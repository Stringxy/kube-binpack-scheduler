package priority

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
	"math"
	"sigs.k8s.io/scheduler-plugins/apis/config"
)

const (
	Name = "Binpack"
)

type Binpack struct {
	handle framework.Handle
	weight priorityWeight
}

type priorityWeight struct {
	BinPackingCPU    int64
	BinPackingMemory int64
}

// resourceToWeightMap contains resource name and weight.

var defaultWeight = priorityWeight{1, 1}
var _ framework.ScorePlugin = &Binpack{}

// New initializes a new plugin and returns it.
func New(ctx context.Context, bpArgs runtime.Object, h framework.Handle) (framework.Plugin, error) {
	resToWeightMap := defaultWeight
	// Update values from args, if specified.
	if bpArgs != nil {
		args, ok := bpArgs.(*config.BinPackArgs)
		if !ok {
			return nil, fmt.Errorf("want args to be of type BinPackArgs, got %T", bpArgs)
		}
		if args.BinPackingCPU <= 0 {
			args.BinPackingCPU = 1
		}
		if args.BinPackingMemory <= 0 {
			args.BinPackingMemory = 1
		}
		resToWeightMap = priorityWeight{args.BinPackingCPU, args.BinPackingMemory}
	}

	return &Binpack{
		handle: h,
		weight: resToWeightMap,
	}, nil
}

// Name returns name of the plugin.
func (pl *Binpack) Name() string {
	return Name
}

// Score invoked at the score extension point.
func (pl *Binpack) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	logger := klog.FromContext(ctx)
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	return BinPackingScore(pod, nodeInfo, pl.weight, logger)
}

// ScoreExtensions of the Score plugin.
func (binpack *Binpack) ScoreExtensions() framework.ScoreExtensions {
	return binpack
}

// BinPackingScore use the best fit polices during scheduling.
// Goals:
// - Schedule Jobs using BestFit Policy using Resource Bin Packing Priority Function
// - Reduce Fragmentation of scarce resources on the Cluster
func BinPackingScore(pod *v1.Pod, nodeInfo *framework.NodeInfo, weight priorityWeight, logger klog.Logger) (int64, *framework.Status) {
	score := int64(0)
	cpuRequest := calculatePodResourceRequest(pod, corev1.ResourceCPU)
	cpuAllo := nodeInfo.Allocatable.MilliCPU
	memRequest := calculatePodResourceRequest(pod, corev1.ResourceMemory)
	memAllo := nodeInfo.Allocatable.Memory

	if cpuRequest > 0 {
		resourceScore, err := ResourceBinPackingScore(cpuRequest, cpuAllo, weight.BinPackingCPU)

		if err != nil {
			return 0, framework.NewStatus(framework.Error, fmt.Sprintf("pod %s/%s cannot binpack node %s: cpuRequest is %s, need %f, allocatable %f",
				pod.Namespace, pod.Name, nodeInfo.GetName(), err.Error(), cpuRequest, cpuAllo))
		}

		logger.V(5).Info("pod %s/%s on node %s cpuRequest need %f, allocatable %f, score %f",
			pod.Namespace, pod.Name, nodeInfo.GetName(), cpuRequest, cpuAllo, resourceScore)

		score += resourceScore
	}

	if memRequest > 0 {
		resourceScore, err := ResourceBinPackingScore(memRequest, memAllo, weight.BinPackingMemory)

		if err != nil {
			return 0, framework.NewStatus(framework.Error, fmt.Sprintf("pod %s/%s cannot binpack node %s: memRequest is %s, need %f, allocatable %f",
				pod.Namespace, pod.Name, nodeInfo.GetName(), err.Error(), memRequest, memAllo))
		}

		logger.V(5).Info("pod %s/%s on node %s memRequest need %f, allocatable %f, score %f",
			pod.Namespace, pod.Name, nodeInfo.GetName(), memRequest, memAllo, resourceScore)

		score += resourceScore
	}
	return score, nil
}

// calculatePodResourceRequest returns the total non-zero requests. If Overhead is defined for the pod and the
// PodOverhead feature is enabled, the Overhead is added to the result.
// podResourceRequest = max(sum(podSpec.Containers), podSpec.InitContainers) + overHead
func calculatePodResourceRequest(pod *v1.Pod, resource v1.ResourceName) int64 {
	var podRequest int64
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		qty := schedutil.GetRequestForResource(resource, &container.Resources.Requests, true)
		podRequest += qty.Value()
	}

	for i := range pod.Spec.InitContainers {
		initContainer := &pod.Spec.InitContainers[i]
		qty := schedutil.GetRequestForResource(resource, &initContainer.Resources.Requests, true)
		if value := qty.Value(); podRequest < value {
			podRequest = value
		}
	}

	// If Overhead is being utilized, add to the total requests for the pod
	if pod.Spec.Overhead != nil {
		if quantity, found := pod.Spec.Overhead[resource]; found {
			podRequest += quantity.Value()
		}
	}

	return podRequest
}

// ResourceBinPackingScore calculate the binpack score for resource with provided info
func ResourceBinPackingScore(requested, capacity, weight int64) (int64, error) {
	if capacity == 0 || weight == 0 {
		return 0, nil
	}
	if requested > capacity {
		return 0, fmt.Errorf("not enough")
	}

	score := requested * weight / capacity
	return score, nil
}

// NormalizeScore invoked after scoring all nodes.
func (binpack *Binpack) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Find highest and lowest scores.
	var highest int64 = -math.MaxInt64
	var lowest int64 = math.MaxInt64
	for _, nodeScore := range scores {
		if nodeScore.Score > highest {
			highest = nodeScore.Score
		}
		if nodeScore.Score < lowest {
			lowest = nodeScore.Score
		}
	}

	// Transform the highest to lowest score range to fit the framework's min to max node score range.
	oldRange := highest - lowest
	newRange := framework.MaxNodeScore - framework.MinNodeScore
	for i, nodeScore := range scores {
		if oldRange == 0 {
			scores[i].Score = framework.MinNodeScore
		} else {
			scores[i].Score = ((nodeScore.Score - lowest) * newRange / oldRange) + framework.MinNodeScore
		}
	}

	return nil
}
