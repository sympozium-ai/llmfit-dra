package modelclaim

// NvidiaDriverDomain scopes the attributes/capacity the NVIDIA-target CEL
// reads — the NVIDIA DRA driver's domain, not ours. llmfit-dra never
// prepares these devices; it only compiles fit into their vocabulary.
const NvidiaDriverDomain = "gpu.nvidia.com"

// DeviceClasses shipped by the NVIDIA DRA driver's chart. Naming one selects
// the NVIDIA translation backend, mirroring how cpu.llmfit.ai already
// switches FitCEL semantics: well-known class names carry meaning.
const (
	nvidiaGPUClass = "gpu.nvidia.com"
	nvidiaMIGClass = "mig.nvidia.com"
)

type backend int

const (
	backendLlmfit backend = iota
	backendNvidia
)

// backendFor picks the CEL compiler for a claim: NVIDIA when the claim names
// one of NVIDIA's shipped DeviceClasses or sets targetDriver explicitly
// (custom-class escape hatch), llmfit otherwise.
func backendFor(deviceClassName, targetDriver string) backend {
	if targetDriver == NvidiaDriverDomain {
		return backendNvidia
	}
	switch deviceClassName {
	case nvidiaGPUClass, nvidiaMIGClass:
		return backendNvidia
	}
	return backendLlmfit
}

// nvidiaBranchesFor maps the DeviceClass to which NVIDIA device types the
// generated CEL admits: mig.nvidia.com pins MIG partitions, gpu.nvidia.com
// pins full GPUs, and a custom class (reached via targetDriver) gets both
// branches OR'd — its own selector decides anything narrower.
func nvidiaBranchesFor(deviceClassName string) (mig, gpu bool) {
	switch deviceClassName {
	case nvidiaMIGClass:
		return true, false
	case nvidiaGPUClass:
		return false, true
	default:
		return true, true
	}
}
