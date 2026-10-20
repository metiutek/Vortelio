package runtime

import (
	"os"
	"sync"
)

// GPU arbitration. Chat (llama-server), image, video and audio engines share
// one GPU; on a small card two of them resident at once means an out-of-memory
// failure or a silent fall back to the CPU. Before using the GPU each consumer
// calls AcquireGPU(kind), which stops every resident of another kind. Each
// kind registers how to free its memory with RegisterGPUEvictor.
//
// VORTELIO_GPU_SHARE=1 keeps everything resident (large GPUs).

var (
	gpuMu       sync.Mutex
	gpuEvictors = map[string][]func(){}
	gpuOwner    string
)

// RegisterGPUEvictor registers fn as the way to free the GPU memory held by
// consumers of kind ("chat", "image", "video", "audio", "stt").
func RegisterGPUEvictor(kind string, fn func()) {
	gpuMu.Lock()
	gpuEvictors[kind] = append(gpuEvictors[kind], fn)
	gpuMu.Unlock()
}

// AcquireGPU frees the GPU from every other kind of consumer before kind
// loads a model. Cheap when kind already owns it.
func AcquireGPU(kind string) {
	if os.Getenv("VORTELIO_GPU_SHARE") == "1" {
		return
	}
	gpuMu.Lock()
	if gpuOwner == kind {
		gpuMu.Unlock()
		return
	}
	var evict []func()
	for k, fns := range gpuEvictors {
		if k != kind {
			evict = append(evict, fns...)
		}
	}
	gpuOwner = kind
	gpuMu.Unlock()
	for _, fn := range evict {
		fn()
	}
}

// GPUOwner reports which kind of consumer last took the GPU.
func GPUOwner() string {
	gpuMu.Lock()
	defer gpuMu.Unlock()
	return gpuOwner
}
