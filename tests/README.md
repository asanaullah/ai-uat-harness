<!-- Assisted by Claude Opus -->
# Test Library

This directory contains the tests used to validate AI platforms on OpenShift. Each test is an Ansible playbook (`<test>.yml`) that starts the test's pods and other objects, and a Ginkgo test file (`<test>.go`) that gets compiled into a binary and run inside a pod. Batches in `batches/` list which tests run and in what order, and can set a test's mode or failure policy. The numbering below follows the original test library and has gaps where tests were removed.

## Categories

### Preflight
Platform prerequisites. Checks that must pass before GPU workloads run.

| Test | Description |
|------|-------------|
| [platform-check](#1-platform-check) | RBAC permissions, API groups, and operator CRD registration |
| [platform-check-virt](#18-platform-check-virt) | RBAC permissions, and OpenShift Virtualization API groups and CRD registration |
| [component](#3-component) | Node hardware and software validation |
| [iperf3](#4-iperf3) | Inter-node TCP bandwidth |
| [iperf3-virt](#14-iperf3-virt) | TCP bandwidth between a pod and a KubeVirt VM on the same node |
| [fio](#15-fio) | Storage throughput, IOPS and latency on the results PVC from a pod |
| [fio-virt](#16-fio-virt) | Storage throughput, IOPS and latency on a PVC attached to a KubeVirt VM as a disk |
| [rwx](#20-rwx) | Shared filesystem visibility across nodes on the RWX results claim |
| [negative](#21-negative) | Failure modes: over-requests wait in Pending, deletions complete |
| [vm-network](#22-vm-network) | Guest networking: reachability in and out of a KubeVirt VM |
| [vm-storage](#24-vm-storage) | Guest persistence: a CDI root disk survives a full VM stop and start |
| [vm-migration](#25-vm-migration) | Live migration: the VM moves nodes without rebooting |
| [vm-evictability](#26-vm-evictability) | Eviction: a plain pod yields, a VM's pod is guarded |
| [vm-secondary-ipam](#27-vm-secondary-ipam) | Secondary-network address exhaustion, legibility and release |
| [vm-memory-overhead](#28-vm-memory-overhead) | What a VM's memory costs the node, measured at three guest sizes |

### Operator
Model serving through cluster operators and CRDs.

| Test | Description |
|------|-------------|
| [kserve](#6-kserve) | KServe InferenceService deployment and inference validation |
| [inference-extended](#23-inference-extended) | Serving behavior: correctness, schema, determinism, streaming, errors, stop sequences |

### Fine-Tuning
Model fine-tuning through a training operator, and serving the result.

| Test | Description |
|------|-------------|
| [finetune-lora](#17-finetune-lora) | LoRA fine-tuning with Kubeflow Trainer, then serving the adapter |

### Single-GPU Performance
Inference benchmarks on a single GPU.

| Test | Description |
|------|-------------|
| [guidellm](#7-guidellm) | vLLM + guidellm benchmark sweeps |
| [inference-perf](#8-inference-perf) | vLLM + inference-perf benchmark sweeps |

### Multi-GPU Performance
Workloads that stress GPU-to-GPU communication fabrics.

| Test | Description |
|------|-------------|
| [chunked-prefill](#9-chunked-prefill) | NCCL over NVLink, tensor-parallel inference |
| [llm-d-local](#10-llm-d-local) | NIXL over NVLink, disaggregated prefill/decode |

### Interactive Environments

| Test | Description |
|------|-------------|
| [dev-env](#5-dev-env) | Jupyter notebook server with CUDA validation |

### Multi-Tenancy
Namespace isolation and cross-tenant performance impact. Isolation tests verify that network boundaries between namespaces are enforced. Contention tests deploy competing GPU workloads in a peer namespace and measure the performance impact on the project namespace under different load intensities.

| Test | Description |
|------|-------------|
| [ping](#11-ping) | Cross-namespace service connectivity |
| [metrics-isolation](#19-metrics-isolation) | Cross-namespace metrics readability |
| [peer-load-high](#12-peer-load-high) | Inference performance under high cross-namespace GPU load |
| [peer-load-low](#13-peer-load-low) | Inference performance under low cross-namespace GPU load |

## Test Dimensions

Every test sits somewhere on two dimensions:

- **Functional vs performance.** A functional test asks whether something works. A performance test measures how well it works and records the numbers for analysis after the run.
- **Horizontal vs vertical.** A horizontal test exercises a single layer of the AI stack on its own. A vertical test exercises several layers together, the way a real workload does.

A vertical test detects that something is wrong but cannot say which layer is responsible. A horizontal test locates the problem in its layer. The two are meant to be read together: when a vertical test gives an unexpected result, the horizontal tests for the layers it depends on show where to look.

```
                       Horizontal                     Vertical
                    (one layer alone)         (several layers together)
               +--------------------------+----------------------------+
               |  platform-check          |  dev-env                   |
               |  platform-check-virt     |                            |
  Functional   |  component               |  kserve                    |
  (does it     |  ping                    |  finetune-lora             |
   work?)      |  metrics-isolation       |  inference-extended        |
               |  rwx                     |                            |
               |  negative                |                            |
               |  vm-network              |                            |
               |  vm-storage              |                            |
               |  vm-migration            |                            |
               |  vm-evictability         |                            |
               |  vm-secondary-ipam       |                            |
               |  vm-memory-overhead      |                            |
               +--------------------------+----------------------------+
               |  iperf3                  |  guidellm                  |
  Performance  |  iperf3-virt             |  inference-perf            |
  (how well    |  fio                     |  chunked-prefill           |
   does it     |  fio-virt                |  llm-d-local               |
   work?)      |                          |  peer-load-high            |
               |                          |  peer-load-low             |
               +--------------------------+----------------------------+
```

## Tests


---

## 1. platform-check

### Purpose

This test checks whether the cluster has the right RBAC permissions, API groups, and operator CRDs in place before any workloads get deployed. On the RBAC side, it confirms that the test pod's ServiceAccount is properly locked down and should not be able to read secrets, create pods, or access node information. On the API group side, it confirms that the CRDs needed by later tests (Kubeflow Trainer, KServe) are actually installed. On the CRD side, it verifies that the operators required by the platform (RHOAI, GPU operator, NFD, Serverless, Service Mesh) have registered their custom resource definitions. Running this first means you find out about missing platform prerequisites before spending time on GPU-heavy tests that would just fail anyway.

### Architecture

```
checker [pass-fail, ephemeral]
    creates SelfSubjectAccessReviews (RBAC checks)
    queries API server discovery endpoint (API group and CRD checks)
```

This is a single ephemeral pod with no GPUs, no server, no service, and no sweep phase.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| checker | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `PERMISSION_CHECKS` env (JSON array of check objects) |

The list of checks to run is defined in the test playbook (`serverConfig.checks`) and passed to the pod as a JSON array through the `PERMISSION_CHECKS` environment variable. Each entry in the array has a `type` (`permission`, `apiGroup`, or `crd`) and an `expected` value (`"yes"` or `"no"`). The user can add, remove, or change the expected value for any check in the playbook to match their cluster's configuration. The test goes through each check, compares the actual result against the expected value, and fails if any of them do not match.

### Pass-fail assertions

The default checks included in the playbook are listed below, but these are fully configurable by the user.

**Permission checks** are expected to be denied, which confirms the pod is properly sandboxed:

1. Cannot `get` pods
2. Cannot `create` pods
3. Cannot `get` secrets
4. Cannot `get` nodes

**API group checks** confirm required CRDs are installed:

5. `trainer.kubeflow.org` is present
6. `serving.kserve.io` is present
7. `nonexistent.example.io` is absent (negative control)

**CRD checks** confirm required operators have registered their custom resource definitions:

8. `DataScienceCluster` in `datasciencecluster.opendatahub.io/v1` (RHOAI operator)
9. `DSCInitialization` in `dscinitialization.opendatahub.io/v1` (RHOAI operator)
10. `ClusterPolicy` in `nvidia.com/v1` (GPU operator)
11. `NodeFeatureDiscovery` in `nfd.openshift.io/v1` (NFD operator)
12. `KnativeServing` in `operator.knative.dev/v1beta1` (OpenShift Serverless)
13. `Istio` in `sailoperator.io/v1` (Service Mesh 3)

### Prerequisites

The in-cluster Kubernetes client must work from the pod (`rest.InClusterConfig()`). No special RBAC is needed. The default ServiceAccount is sufficient since all three check types use APIs that are available to any authenticated user: `SelfSubjectAccessReview` for permission checks, and the Discovery API for API group and CRD checks.

Note: operator health checks (e.g. verifying Deployment conditions, CR status fields, or DSC component conditions) were considered but would require cross-namespace read access to Deployments and custom resources, which conflicts with the sandbox design of this test. The CRD existence checks validate that operators are installed; actual operator health is validated implicitly by the downstream tests that depend on them.

### Design rationale

The permission checks are there to verify that the test pod is sandboxed. If any of them come back as allowed, the test fails because it means the ServiceAccount has more privileges than it should. This guards against accidentally running the test suite with an overly permissive ServiceAccount. The API group checks verify that the cluster has the CRDs that downstream tests depend on. For example, the llm-d test needs `inference.networking.k8s.io` for InferencePool resources. Finding out about a missing CRD here gives a much clearer error message than a cryptic failure partway through a later test. The `nonexistent.example.io` entry with expected `"no"` is a negative control that confirms the API group check logic is actually working and not just returning `"yes"` for everything. The CRD checks go one level deeper than API group checks by verifying that specific Kinds exist within an API group/version, which confirms the operator has registered its full CRD schema rather than just the API group being present.


---

## 3. component

### Purpose

This test checks whether a node's actual hardware and software configuration matches what the cluster specification says it should be. It looks at things like GPU count and model, NVLink connectivity, PCIe link width, CPU and memory specs, kernel version, power management settings, and FIPS compliance. The goal is to surface hardware faults or misconfigurations early, before the more expensive GPU-heavy tests run.

### Architecture

```
test-runner [pass-fail, ephemeral]
    reads cluster spec (embedded at compile time)
    probes nvidia-smi, /proc/*, /sys/*
```

This is a single ephemeral pod. There is no server, no service, and no sweep phase. It just runs the checks and exits.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| test-runner | `registry.redhat.io/ubi9/ubi:9.8` | All node GPUs | none | `NODE_NAME` env; cluster spec embedded via `//go:embed` |

### Pass-fail assertions

The assertions are grouped into three categories:

**Sanity checks** are hard failures that indicate broken or mismatched hardware:

1. GPU count matches cluster spec
2. GPU model matches cluster spec
3. NVLink width matches expected (e.g. NV6, NV12)
4. NVLink topology is all-to-all (every GPU has a direct NVLink path to every other)
5. PCIe link width matches expected (e.g. x16)
6. PCIe generation matches expected (e.g. Gen5)
7. CPU model matches cluster spec
8. CPU count matches cluster spec
9. Memory capacity is within 5% of expected
10. NUMA node count matches expected

**Ideal checks** cover settings that affect performance but are not outright failures:

11. CUDA driver version
12. GPU power limit (watts)
13. GPU persistence mode
14. Kernel version
15. Hugepages (2Mi) count
16. CPU frequency governor (e.g. `performance`)
17. CPU idle driver (e.g. `intel_idle`)
18. CPU idle governor (e.g. `menu`)
19. Only the expected C-states are enabled (e.g. POLL and C1 only)
20. Transparent hugepages setting (e.g. `madvise`)

**Compliance checks** cover regulatory requirements:

21. FIPS mode is enabled (this check is skipped if the cluster spec does not require it)

### Prerequisites

`nvidia-smi` must be available in the container, which is provided by the GPU device plugin. The cluster spec must be present at compile time since it is embedded into the Go binary via `//go:embed`. The pod must request all GPUs on the node so that it can see the full topology.

### Design rationale

The pod requests all of the node's GPUs because `nvidia-smi topo -m` only reports topology for GPUs that are visible to the container. Without requesting all of them, NVLink validation would be incomplete. The cluster spec is embedded at compile time rather than loaded from a file or configmap because the test pod does not have access to the host filesystem and should not depend on external configmaps being present. The memory check allows a 5% tolerance because the kernel reserves memory for page tables, device mappings, and other overhead, so `/proc/meminfo` always reports less than the physical DIMM capacity. NVLink topology validation currently only supports all-to-all because that is the topology used by current SXM-based GPU nodes (DGX, HGX). Other topologies like ring or tree would need different validation logic. The C-state check iterates through sysfs entries under `cpuidle/state*/disable` because deep C-states (C3 and beyond) add wake-up latency that can affect interrupt-driven workloads like RDMA transfers and GPU-to-GPU communication.


---

## 4. iperf3

### Purpose

This test measures TCP bandwidth between every pair of nodes in the cluster using iperf3. It validates that the pod network can sustain the throughput needed for inter-node communication such as NCCL allreduce over TCP or KV cache transfers without RDMA.

### Architecture

```
iperf-server [persistent, node 0 of pair]
    |
    +--- iperf-client [pass-fail, ephemeral, node 1 of pair, one pod per sweep entry]
```

The test runs in `pairs` mode by default, so for N nodes it produces N*(N-1) sequences covering every ordered pair (A->B and B->A are separate sequences). This catches asymmetric bandwidth issues caused by network topology or switch oversubscription.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| iperf-server | `networkstatic/iperf3:latest` | none | headless ClusterIP :5201 | `iperf3 -s`, 16 CPU and 32Gi requests and limits |
| iperf-client | `networkstatic/iperf3:latest` | none | none | `SERVER_HOST` and `SWEEP_COMMAND` env, 16 CPU and 32Gi requests and limits |

### Parameter sweep

The client step sweeps the number of parallel TCP streams. The base command is `iperf3 --client=<host> --time=10 --omit=2 --json`:

| ID | Parallel streams |
|----|------------------|
| `streams-1` | 1 |
| `streams-2` | 2 |
| `streams-4` | 4 |
| `streams-8` | 8 |
| `streams-16` | 16 |

### Pass-fail assertions

Each sweep entry runs both assertions:

1. TCP connection to server on port 5201 succeeds within 10 seconds
2. The sweep entry's iperf3 command runs successfully and measured bandwidth is greater than zero

The stream count and the bandwidth in Gbps are reported via Ginkgo `AddReportEntry`.

### Prerequisites

At least 2 nodes must be present in the cluster. The pod network must allow TCP connections on port 5201 between nodes. The iperf3 container image must be accessible from the cluster.

### Design rationale

`pairs` mode uses ordered pairs, so A->B and B->A are tested separately. Asymmetric bandwidth (due to network topology, switch oversubscription, or misconfigured QoS) would only show up if both directions are measured independently. The `--json` flag produces JSON output which is parsed directly for the `bits_per_second` value.

The sweep covers 1 to 16 parallel streams, and each run omits its first 2 seconds (`--omit=2`). A single stream is limited by one CPU core, especially on an overlay network with a small MTU, so the sweep shows both what one stream gets and where the link itself levels off.

The sweep is on the client step rather than a separate step because in `pairs` mode the server runs on the first node of the pair and the client (`nodeIndex: 1`) on the second, so the test has exactly two steps.


---

## 5. dev-env

### Purpose

This test deploys a Jupyter notebook server with all node GPUs and validates that the development environment is functional end-to-end. Rather than running diagnostics at startup, the validator pod creates a notebook on the server via the Jupyter Contents API, starts a kernel, executes CUDA diagnostic code through the Jupyter kernel websocket protocol, and then reads back the results. This proves the full interactive workflow works: notebook creation, kernel execution with GPU access, and file I/O through the Jupyter API.

### Architecture

```
notebook-server [persistent]
    |
    +--- validator [pass-fail, ephemeral]
             creates notebook via Contents API
             starts kernel, executes CUDA diagnostics via websocket
             reads result.txt via Contents API
             validates GPU count and NVLink against cluster spec
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| notebook-server | `quay.io/jschless/ml-dev-env:pytorch-2.9` | All node GPUs | headless ClusterIP :8888 | `jupyter lab --notebook-dir=/uat_workspace`, no auth, XSRF disabled, `HOME=/tmp` |
| validator | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `SERVER_URL`, `EXPECTED_GPU_COUNT`, `EXPECTED_NVLINK` env |

### Pass-fail assertions

1. Notebook created on server via Jupyter Contents API (`PUT /api/contents/gpu-diagnostics.ipynb`)
2. Kernel started and diagnostic code executed via websocket (`/api/kernels/<id>/channels`)
3. `result.txt` readable via Contents API (`GET /api/contents/result.txt`)
4. GPU count from CUDA matches cluster spec (`nvidia.com/gpu`)
5. NVLink width from `nvidia-smi topo -m` matches cluster spec (`nvlink`)
6. All GPU devices reported by CUDA (device list length matches expected count)

### Prerequisites

The `quay.io/jschless/ml-dev-env:pytorch-2.9` image must be accessible from the cluster. At least one GPU must be available for the notebook server to schedule. The cluster spec must include `nvlink` in the node's `componentValidation.sanity` section.

### Design rationale

The validator uses a minimal websocket client built from Go's standard library to avoid adding external dependencies. The notebook server runs with `HOME=/tmp` because the image's root filesystem is not writable under OpenShift's arbitrary UID assignment. XSRF protection is disabled (`--ServerApp.disable_check_xsrf=True`) because the validator pod communicates with the Jupyter API programmatically without browser cookies. Authentication is disabled (`--IdentityProvider.token=`) since the server is only accessible within the cluster network via a headless ClusterIP service. The diagnostic code writes `result.txt` to `/uat_workspace` (the PVC mount) using an absolute path because the kernel's working directory defaults to the user's home, not the notebook directory.


---

## 6. kserve

### Purpose

This test validates that the KServe operator can deploy and serve a model through the InferenceService CRD. It creates an InferenceService in RawDeployment mode with vLLM as the model server, waits for the endpoint to become available, and then runs health, model, and inference checks against it. This confirms the operator stack is functional before relying on it for production workloads.

### Architecture

```
isvc [resourceConfig, InferenceService CRD]
    |
    +--- test-runner [pass-fail, ephemeral]
             polls endpoint until ready (10 min timeout)
             health / models / inference checks
```

The InferenceService is created as a resource step. KServe processes the CRD and creates a Deployment and Service in RawDeployment mode. The test runner pod polls the KServe-created service until the model server is healthy, then runs the assertions.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| isvc | (KServe CRD, operator creates the pod) | 1 (configurable via `serverConfig.gpuCount`) | KServe-managed `{name}-predictor` :8080 | `--max-model-len=10000 --gpu-memory-utilization=0.6` |
| test-runner | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `SERVICE_URL`, `MODEL_NAME` env |

### Pass-fail assertions

1. Endpoint becomes available within 10 minutes (polls `/health` every 15s)
2. Model is loaded (`GET /v1/models` returns non-empty `data` array)
3. Inference request succeeds (`POST /v1/completions` returns valid response with choices)

### Prerequisites

The KServe operator must be installed and the `serving.kserve.io` API group must be available. The namespace must have RBAC permissions to create InferenceService resources. The models PVC must contain the model weights. At least one GPU must be available for the InferenceService pod to schedule.

### Design rationale

The test uses the `containers` predictor (inline container spec) rather than a separate ServingRuntime so the entire test is self-contained in a single CRD. RawDeployment mode is requested explicitly via the `serving.kserve.io/deploymentMode: RawDeployment` annotation, set through the resource step's `resourceConfig.annotations`, which the `uat_resource` role renders onto the InferenceService's `metadata.annotations`. The test is restricted to project mode because the InferenceService is a non-Pod resource whose spec the `uat_resource` role passes through as-is. The role cannot inject a `nodeSelector` since the path differs by resource kind (`spec.predictor.nodeSelector` for InferenceService vs `spec.template.spec.nodeSelector` for Deployment, etc.). In node mode, two "per-node" InferenceServices could land on the same node, defeating the purpose. The 10-minute polling timeout accounts for image pull time and model loading on first run.


---

## 7. guidellm

### Purpose

This test deploys a single-GPU vLLM inference server and benchmarks it using [guidellm](https://github.com/neuralmagic/guidellm). It first validates that the server starts up, loads the model, and can serve requests, then runs a set of parametric benchmarks that measure throughput and latency under different load profiles. This gives a baseline of single-GPU inference performance.

### Architecture

```
vllm-server [persistent]
    |
    +--- pass-fail [ephemeral]
    |
    +--- sweep [ephemeral, one pod per entry]
             short-burst
             sustained-load
             long-context
```

The vLLM server stays up for both the pass-fail and sweep phases. Each sweep entry runs as a separate pod against the same warm server.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| vllm-server | `nvcr.io/nvidia/vllm:26.03-py3` | 1 | headless ClusterIP :8000 | `--max-model-len=10000 --gpu-memory-utilization=0.6` |
| pass-fail | `ghcr.io/vllm-project/guidellm:v0.6.1` | none | none | `SERVER_URL` env |
| sweep | `ghcr.io/vllm-project/guidellm:v0.6.1` | none | none | `SERVER_URL`, `SWEEP_COMMAND` env |

### Pass-fail assertions

1. HTTP GET `/health` returns 200
2. HTTP GET `/v1/models` returns a non-empty `data` array

### Sweep entries

| ID | Rate | Duration | Prompt tokens | Output tokens |
|----|------|----------|---------------|---------------|
| short-burst | 4 req/s | 30s | 128 | 64 |
| sustained-load | 1 req/s | 120s | 256 | 128 |
| long-context | 1 req/s | 60s | 1024 | 512 |

The sweep step parses `benchmarks.json` from the guidellm output and reports throughput (tokens/s) and request latency (seconds) via Ginkgo `AddReportEntry`.

### Prerequisites

At least one GPU must be available for the server pod to schedule. The models PVC must be mounted at `/models` with the model weights present. The guidellm container image must be accessible from the cluster.

### Design rationale

`gpu-memory-utilization=0.6` leaves headroom to avoid OOM on nodes with smaller GPUs. `max-model-len=10000` caps the context window to reduce GPU memory usage, since this test is about measuring baseline inference performance rather than pushing context length limits. `HOME=/tmp` is set because vLLM writes cache files to `$HOME` and the container user may not have a writable home directory.


---

## 8. inference-perf

### Purpose

This test deploys a single-GPU vLLM inference server and benchmarks it using [inference-perf](https://github.com/kubernetes-sigs/inference-perf) from kubernetes-sigs. It validates that the server starts and loads the model, then runs parametric sweeps that measure throughput and latency under constant and Poisson arrival patterns at different rates with random token length distributions.

### Architecture

```
vllm-server [persistent]
    |
    +--- pass-fail [ephemeral]
    |
    +--- sweep [ephemeral, one pod per entry]
             constant-low
             constant-high
             poisson-burst
```

The vLLM server stays up for both the pass-fail and sweep phases. Each sweep entry runs as a separate pod against the same warm server.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| vllm-server | `nvcr.io/nvidia/vllm:26.03-py3` | 1 | headless ClusterIP :8000 | `--max-model-len=10000 --gpu-memory-utilization=0.6` |
| pass-fail | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none | `SERVER_URL` env |
| sweep | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none | `SERVER_URL`, `SWEEP_COMMAND` env |

### Pass-fail assertions

1. HTTP GET `/health` returns 200
2. HTTP GET `/v1/models` returns a non-empty `data` array

### Sweep entries

| ID | Load type | Rate | Duration | Workers | Data |
|----|-----------|------|----------|---------|------|
| constant-low | constant | 1 req/s | 30s | 4 | random (input: 64-256, mean=128; output: 32-128, mean=64) |
| constant-high | constant | 10 req/s | 60s | 4 | same |
| poisson-burst | poisson | 5 req/s | 60s | 4 | same |

The sweep step parses `summary_lifecycle_metrics.json` and asserts that there is at least one successful request and zero failures. It reports throughput (total and output tokens/s), request latency, TTFT, and TPOT via Ginkgo `AddReportEntry`.

### Prerequisites

At least one GPU must be available for the server pod to schedule. The models PVC must be mounted at `/models` with the model weights present. The inference-perf container image must be accessible from the cluster.

### Design rationale

`gpu-memory-utilization=0.6` leaves headroom to avoid OOM on nodes with smaller GPUs. `max-model-len=10000` caps the context window to reduce GPU memory usage. `HOME=/tmp` is set because vLLM writes cache files to `$HOME` and the container user may not have a writable home directory. inference-perf uses dot-separated flag names (`server.type`, `load.stages`, etc.) which the `uat_command` filter converts to `--server.type=vllm` style flags. `server.ignore_eos=true` forces the model to generate exactly the requested number of output tokens regardless of EOS token generation, which ensures consistent benchmark results across runs. `load.stages` is a JSON string within a YAML string, so watch for quoting issues when modifying the sweep entries.


---

## 9. chunked-prefill

### Purpose

This test deploys a single vLLM instance that uses all available GPUs under one tensor-parallel group with chunked prefill enabled. All GPUs work together in a single TP group where inter-GPU communication happens through NCCL allreduce over NVLink. This validates multi-GPU inference performance without the overhead of KV cache transfer between separate instances.

### Architecture

```
vllm-server [persistent]
    |
    +--- pass-fail [ephemeral]
    |
    +--- sweep [ephemeral]
             stress (ramped: 0.25 → 0.5 → 0.75 → 1 req/s)
```

The vLLM server stays up for both the pass-fail and sweep phases.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| vllm-server | `ghcr.io/llm-d/llm-d-cuda:v0.8.0` | All GPUs | headless ClusterIP :8000 | `--tensor-parallel-size=<all GPUs> --enable-chunked-prefill` |
| pass-fail | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `SERVER_URL`, `MODEL_NAME` env |
| sweep | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none | `SERVER_URL`, `SWEEP_COMMAND` env |

### Pass-fail assertions

1. Server is healthy (HTTP GET `/health` returns 200)
2. Model is loaded (HTTP GET `/v1/models` returns non-empty `data` array)
3. Server serves inference requests (POST `/v1/completions` returns valid response)

### Sweep entries

| ID | Load profile | Duration | Data |
|----|-------------|----------|------|
| stress | Ramped stages: 0.25 → 0.5 → 0.75 → 1 req/s | 4 × 30s | shared_prefix (500 groups × 1 prompt, 16000 system + 250 question tokens, 128 output tokens), worker_max_concurrency=1 |

### Prerequisites

All GPUs on the scheduling target must be available since the server requests all of them for tensor parallelism. The models PVC must be mounted at `/models` with the model weights present. Shared memory (`/dev/shm`) must support 16Gi for NCCL.

### Design rationale

Chunked prefill allows vLLM to interleave prefill and decode batches, which improves GPU utilization and reduces time-to-first-token under load. The `--enable-chunked-prefill` flag is passed as a bare flag (value `~` in YAML) since it takes no argument.


---

## 10. llm-d-local

### Purpose

This test validates KV cache transfer performance over NVLink using NIXL's `cuda_ipc` transport. It deploys a colocated prefill/decode disaggregated inference setup where both vLLM instances, the routing sidecar, and an nginx header injector all run inside a single pod. The GPUs are split evenly between prefill and decode so that each prefill GPU has a 1-to-1 transfer partner on the decode side. The `cuda_ipc` transport requires all containers to share an IPC namespace, which in Kubernetes means they must be in the same pod. Because of this constraint, there is no EPP (Endpoint Picker) or Envoy gateway. Instead, an nginx sidecar injects the `x-prefiller-host-port` header that the routing sidecar needs to orchestrate the P/D protocol.

### Architecture

```
pd-server [persistent, single pod with 4 containers]
    |
    |  main container: decode vLLM :8200 + prefill vLLM :8100
    |                  (two processes in one container, split by CUDA_VISIBLE_DEVICES)
    |
    |  sidecar: routing-sidecar :8000
    |           (orchestrates P/D protocol between prefill and decode)
    |
    |  sidecar: nginx header-injector :8080
    |           (injects x-prefiller-host-port header, serves as entry point)
    |
    +--- pass-fail [ephemeral]
    |
    +--- sweep [ephemeral]
             stress (ramped: 0.25 → 0.5 → 0.75 → 1 req/s)
```

Request flow: client → nginx :8080 → routing-sidecar :8000 → prefill :8100 computes KV cache → NIXL transfers KV over cuda_ipc/NVLink → decode :8200 generates tokens → response back to client.

Both prefill and decode run as separate processes in the same container, split by `CUDA_VISIBLE_DEVICES`. This is necessary because the Kubernetes NVIDIA device plugin isolates GPUs per container, and `cudaIpcOpenMemHandle` cannot map a remote GPU's memory if that GPU is not in the local CUDA context. With a single container owning all GPUs and using `CUDA_VISIBLE_DEVICES` to partition them, cuda_ipc can establish NVLink peer-to-peer mappings directly.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| pd-server (main) | `ghcr.io/llm-d/llm-d-cuda:v0.8.0` | All GPUs | headless ClusterIP :8080 | Prefill TP = g/2, Decode TP = g - g/2, UCX/NIXL env |
| routing-sidecar | `ghcr.io/llm-d/llm-d-router-disagg-sidecar:v0.9.0` | (shared) | :8000 | `--kv-connector=nixlv2` |
| header-injector | `docker.io/nginx:1.27-alpine` | (shared) | :8080 | Injects `x-prefiller-host-port: localhost:8100` |
| pass-fail | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `PREFILL_URL`, `DECODE_URL`, `GATEWAY_URL`, `MODEL_NAME`, `PREFILL_TP`, `DECODE_TP` |
| sweep | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none | `SERVER_URL` (nginx :8080), `SWEEP_COMMAND` |

### Pass-fail assertions

1. Prefill server is healthy, has model loaded, and serves inference
2. Decode server is healthy, has model loaded, and serves inference
3. Gateway (nginx → sidecar → P/D pipeline) routes disaggregated inference end-to-end
4. GPU split is reported (prefill TP and decode TP)

### Sweep entries

| ID | Load profile | Duration | Data |
|----|-------------|----------|------|
| stress | Ramped stages: 0.25 → 0.5 → 0.75 → 1 req/s | 4 × 30s | shared_prefix (500 groups × 1 prompt, 16000 system + 250 question tokens, 128 output tokens), worker_max_concurrency=1 |

### Prerequisites

At least 2 GPUs must be available (1 for prefill, rest for decode). The models PVC must be mounted at `/models` with the model weights present. Shared memory (`/dev/shm`) must support 16Gi for NCCL and NIXL IPC.

### Design rationale

Block size is computed from GPU memory using the formula `((mem + 2560) // 5120) * 64`. With cross-layer blocks enabled, NIXL can merge adjacent blocks into a single descriptor and transfer, so contiguous allocation is critical for bandwidth.

The stress test is specifically structured to minimize KV cache fragmentation, which is the main obstacle to achieving high transfer bandwidth. There are two sources of fragmentation. First, concurrent requests cause vLLM's block allocator to interleave block IDs across requests, which defeats NIXL descriptor merging and drops per-transfer bandwidth significantly. The stress test uses `worker_max_concurrency=1` to serialize requests and avoid this. Second, prefix caching causes fragmentation because when vLLM retains KV cache blocks for shared prefixes, new requests reuse those cached prefix blocks but allocate fresh blocks for the unique suffix in scattered free slots between cached entries. The stress test uses `num_groups=500, num_prompts_per_group=1` so that each request has a unique prefix and never gets a cache hit, keeping block allocation contiguous.

`UCX_RNDV_PIPELINE_SHM_ENABLE=no` disables the default behavior of staging GPU-to-GPU transfers through host shared memory, forcing direct cuda_ipc transfers over NVLink. NIXL side-channel ports must differ between prefill (5557) and decode (5558) since both bind to the pod IP.


---

## 11. ping

### Purpose

This test validates cross-namespace service connectivity by deploying simple HTTP servers in both the project and peer namespaces, then running connectivity checks from each side. A pod in the project namespace should be able to reach its own service but not the peer service by short name (namespace isolation). A pod in the peer namespace should be able to reach both its local peer service and the project service via cross-namespace FQDN.

### Architecture

```
project-server [persistent, project namespace]
    |
peer-server [persistent, peer namespace]
    |
    +--- project-check [pass-fail, ephemeral, project namespace]
    |        own service by short name ✓
    |        peer service by short name ✗
    |        peer service by FQDN ✗
    |
    +--- peer-check [pass-fail, ephemeral, peer namespace]
             own service by short name ✓
             project service by short name ✗
             project service by FQDN ✗
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| project-server | `registry.redhat.io/ubi9/ubi:9.8` | none | headless ClusterIP :8080 | `python3 -m http.server 8080` |
| peer-server | `registry.redhat.io/ubi9/ubi:9.8` | none | headless ClusterIP :8080 (peer namespace) | `python3 -m http.server 8080` |
| project-check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `OWN_URL`, `OTHER_SHORT_URL`, `OTHER_FQDN_URL` env |
| peer-check | `registry.redhat.io/ubi9/ubi:9.8` | none | none (peer namespace) | `OWN_URL`, `OTHER_SHORT_URL`, `OTHER_FQDN_URL` env |

### Pass-fail assertions

Both project-check and peer-check run the same three assertions with symmetric env vars:

1. HTTP GET to own service by short name returns 200 (service reachable in own namespace)
2. HTTP GET to other service by short name fails (DNS does not resolve across namespaces)
3. HTTP GET to other service by FQDN fails (network policy blocks cross-namespace traffic)

### Prerequisites

The peer namespace must be configured in the cluster config (`uat_peer_namespace`). Both namespaces must have the RBAC permissions to create pods and services. `serverConfig.projectNamespace` and `serverConfig.peerNamespace` are read from the cluster config.

### Design rationale

The test uses Python's built-in `http.server` module rather than nginx because it runs as non-root without any configuration, which works with OpenShift's default security context. Both checks run identical assertions with symmetric env vars: own service reachable, other service unreachable by short name (DNS scoping), other service unreachable by FQDN (network policy). This validates both DNS namespace isolation and network-level cross-namespace blocking.


---

## 12. peer-load-high

### Purpose

This test measures inference performance under high cross-namespace GPU contention. It deploys tensor-parallel vLLM servers with chunked prefill in both the project and peer namespaces on the same node, splitting the node's GPUs evenly between them (peer gets g/2, project gets g - g/2). It starts a sustained high-rate load generator (10 req/s for 10 minutes) against the peer server, then runs benchmark sweeps against the project server. This noisy-neighbor pattern reveals how inference latency and throughput degrade when a co-located multi-GPU workload is under heavy load.

### Architecture

```
peer-server [persistent, peer namespace]
    |
    +--- peer-load [persistent, peer namespace, 10 req/s × 600s]

project-server [persistent, project namespace]
    |
    +--- sweep [ephemeral, one pod per entry]
             constant-low
             constant-high
             poisson-burst
```

Both servers are pinned to the same node via nodeSelector (node mode only). The peer-load pod starts before the sweep and runs continuously throughout.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| peer-server | `nvcr.io/nvidia/vllm:26.03-py3` | g/2 | headless ClusterIP :8000 (peer namespace) | `--tensor-parallel-size=<g/2> --enable-chunked-prefill` |
| project-server | `nvcr.io/nvidia/vllm:26.03-py3` | g - g/2 | headless ClusterIP :8000 | `--tensor-parallel-size=<g - g/2> --enable-chunked-prefill` |
| peer-load | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none (peer namespace) | constant 10 req/s, 600s duration, 4 workers |
| sweep | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none | `SERVER_URL`, `SWEEP_COMMAND` env |

### Sweep entries

| ID | Load type | Rate | Duration | Workers | Data |
|----|-----------|------|----------|---------|------|
| constant-low | constant | 1 req/s | 30s | 4 | random (input: 64-256, mean=128; output: 32-128, mean=64) |
| constant-high | constant | 10 req/s | 60s | 4 | same |
| poisson-burst | poisson | 5 req/s | 60s | 4 | same |

Results are quantitative only (no pass-fail assertions). Throughput (total and output tokens/s), request latency, TTFT, and TPOT are reported via Ginkgo `AddReportEntry`.

### Prerequisites

All GPUs on the target node must be available since peer-server takes g/2 and project-server takes the remaining g - g/2. The peer namespace must be configured in the cluster config. The models PVC must be mounted at `/models` in both namespaces.

### Design rationale

The peer-load pod has no readinessProbe so it becomes Ready immediately after starting, which ensures the background load is running before the sweep pods launch. The 10 req/s rate with 4 workers creates sustained GPU pressure on the peer server without saturating it to the point of request failures. Node mode is required so both servers land on the same physical node, creating real GPU contention through shared PCIe bandwidth, NVLink fabric, memory controller, and power delivery. The GPU split uses `g // 2` for peer and `g - (g // 2)` for project, which on an odd GPU count gives the project server the extra GPU. Tensor parallelism with chunked prefill is enabled on both servers so each uses all its allocated GPUs in a single TP group, matching production-like multi-GPU serving configurations. Comparing these results against the chunked-prefill baseline (test 9) quantifies the performance impact of noisy-neighbor GPU workloads.


---

## 13. peer-load-low

### Purpose

This test is identical to peer-load-high but with a low-rate background load (1 req/s instead of 10 req/s) on the peer server. It provides a low-contention data point that, when compared against peer-load-high and the chunked-prefill baseline, reveals the relationship between neighbor load intensity and inference performance degradation.

### Architecture

```
peer-server [persistent, peer namespace]
    |
    +--- peer-load [persistent, peer namespace, 1 req/s × 600s]

project-server [persistent, project namespace]
    |
    +--- sweep [ephemeral, one pod per entry]
             constant-low
             constant-high
             poisson-burst
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| peer-server | `nvcr.io/nvidia/vllm:26.03-py3` | g/2 | headless ClusterIP :8000 (peer namespace) | `--tensor-parallel-size=<g/2> --enable-chunked-prefill` |
| project-server | `nvcr.io/nvidia/vllm:26.03-py3` | g - g/2 | headless ClusterIP :8000 | `--tensor-parallel-size=<g - g/2> --enable-chunked-prefill` |
| peer-load | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none (peer namespace) | constant 1 req/s, 600s duration, 4 workers |
| sweep | `quay.io/inference-perf/inference-perf:v0.6.1` | none | none | `SERVER_URL`, `SWEEP_COMMAND` env |

### Sweep entries

Same as peer-load-high. See [peer-load-high sweep entries](#12-peer-load-high).

### Prerequisites

Same as peer-load-high. See [peer-load-high prerequisites](#12-peer-load-high).

### Design rationale

At 1 req/s the peer server processes requests with minimal queuing, so the GPUs are mostly idle between requests. Any performance degradation observed in the project-side sweep at this load level points to fixed overhead from co-tenancy (shared NVLink fabric, memory controller, PCIe bus arbitration, GPU context switching) rather than sustained compute contention. This makes peer-load-low the control for peer-load-high's experiment.


---

## 14. iperf3-virt

### Purpose

This test measures TCP bandwidth between a pod and a KubeVirt virtual machine on the same node. Read next to `iperf3` run in node mode, which measures pod to pod on the same node, the difference is the cost of the virtualization layer on the network path.

### Architecture

```
server-vm [VirtualMachine, Fedora containerDisk, cloud-init installs and starts iperf3]
    |
server-svc [headless Service :5201 selecting the VM's launcher pod]
    |
    +--- iperf-client [pass-fail, ephemeral, same node, one pod per sweep entry]
```

### Infrastructure

| Name | Kind | Key config |
|------|------|------------|
| server-vm | `kubevirt.io/v1` VirtualMachine | `quay.io/containerdisks/fedora:latest`, 16 cores and 32Gi requests and limits, masquerade interface with multiqueue, pinned to the node |
| server-svc | Service | headless, port 5201, selects the VM by its template labels |
| iperf-client | Pod, `networkstatic/iperf3:latest` | `SERVER_HOST`, `READY_TIMEOUT_SECONDS` and `SWEEP_COMMAND` env, 16 CPU and 32Gi requests and limits |

### Parameter sweep

The base command is `iperf3 --client=<host> --time=10 --omit=2 --json`. Each stream count runs in both directions:

| ID | Direction | Parallel streams |
|----|-----------|------------------|
| `streams-1` to `streams-16` | pod to VM | 1, 2, 4, 8, 16 |
| `reverse-1` to `reverse-16` | VM to pod (`--reverse`) | 1, 2, 4, 8, 16 |

### Pass-fail assertions

Each sweep entry runs both assertions:

1. TCP connection to the VM on port 5201 succeeds within `readyTimeoutSeconds` (default 480)
2. The sweep entry's iperf3 command runs successfully and measured bandwidth is greater than zero

The time waited for the server, the direction, the stream count and the bandwidth in Gbps are reported via Ginkgo `AddReportEntry`.

### Prerequisites

OpenShift Virtualization must be installed and the node must be able to run VMs. The test namespace needs permission to create `virtualmachines`. The VM needs outbound access to the Fedora package mirrors to install iperf3 on first boot.

### Design rationale

The VM is a resource step, which the harness applies without waiting, so the client retries the connection until the guest has booted and installed iperf3. Only the first sweep entry pays that wait.

Both directions are measured because sending and receiving take different paths through the guest's virtual NIC. There is no VM to VM case: the pod to pod and pod to VM results already isolate the cost of one VM.

The VM and the pods in both this test and `iperf3` have the same CPU and memory requests and limits, one core per stream at the largest sweep entry, so the two tests can be compared directly.


---

## 15. fio

### Purpose

This test measures the storage behind the results PVC from a pod using fio: sequential throughput, random IOPS, and latency. It is the baseline for `fio-virt`.

### Architecture

```
fio-runner [pass-fail, ephemeral, one pod per sweep entry]
    builds fio once and caches the binary on the scratchpad
    runs the sweep entry against /uat_scratchpad/fio-data
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| fio-runner | `golang:1.26` | none | none | `FIO_VERSION` and `SWEEP_COMMAND` env, 16 CPU and 32Gi requests and limits |

### Parameter sweep

The base command is `fio --output-format=json --name=uat --directory=/uat_scratchpad/fio-data --size=1G --direct=1 --ioengine=psync --time_based --runtime=30 --group_reporting --unlink=1`:

| ID | Pattern | Block size | Jobs |
|----|---------|------------|------|
| `seq-write-1`, `seq-write-16` | sequential write | 1M | 1, 16 |
| `seq-read-1`, `seq-read-16` | sequential read | 1M | 1, 16 |
| `rand-write-1`, `rand-write-16` | random write | 4k | 1, 16 |
| `rand-read-1`, `rand-read-16` | random read | 4k | 1, 16 |

### Pass-fail assertions

Each sweep entry runs both assertions:

1. A fio binary for the pinned version exists on the scratchpad, or builds from source successfully
2. The sweep entry's fio command runs successfully and measured throughput is greater than zero

The pattern, block size, job count, throughput in MB/s, IOPS, and p50 and p99 completion latency in ms are reported via Ginkgo `AddReportEntry`. The full fio JSON is saved as `fio.json` in the entry's results folder.

### Prerequisites

The pod needs outbound access to GitHub to fetch the fio source. Each job uses 1GB on the results PVC while it runs, so the 16-job entries need 16GB free.

### Design rationale

fio is built from a pinned release tag so that this test and `fio-virt` run the identical version, without depending on a prebuilt fio image. The builder image already has the compiler, and the build takes seconds.

`--direct=1` bypasses the page cache, so the 1GB files measure the storage and not the node's memory. Each entry runs for a fixed 30 seconds over its files, so the footprint does not grow with the speed of the storage, and `--unlink=1` removes the files when the entry finishes.

The I/O engine is `psync` because io_uring is not available to pods and the builder image has no libaio. Each job therefore has one request in flight, and the job count is the degree of parallelism.


---

## 16. fio-virt

### Purpose

This test measures the same storage class when a KubeVirt VM uses it as a disk. It runs the same fio sweep as `fio` inside a VM, so the difference between the two is the cost of the VM's storage path.

### Architecture

```
server-vm [VirtualMachine, Fedora containerDisk, PVC attached as a disk and mounted at /uat_workspace,
           cloud-init builds fio and starts `fio --server`]
    |
server-svc [headless Service :8765 selecting the VM's launcher pod]
    |
    +--- fio-client [pass-fail, ephemeral, one pod per sweep entry]
             sends the sweep entry to the VM as a job file
             receives the results over the same connection
```

### Infrastructure

| Name | Kind | Key config |
|------|------|------------|
| server-vm | `kubevirt.io/v1` VirtualMachine | `quay.io/containerdisks/fedora:latest`, 16 cores and 32Gi requests and limits, `uat-project-storage-virt` as a virtio disk |
| server-svc | Service | headless, port 8765, selects the VM by its template labels |
| fio-client | Pod, `golang:1.26` | `FIO_VERSION`, `SERVER_HOST`, `READY_TIMEOUT_SECONDS` and `SWEEP_COMMAND` env, 16 CPU and 32Gi requests and limits |

### Parameter sweep

The same eight entries as `fio`, with `--client=<host>` added and `--directory=/uat_workspace`, the mount point of the disk inside the VM.

### Pass-fail assertions

Each sweep entry runs all three assertions:

1. A fio binary for the pinned version exists on the scratchpad, or builds from source successfully
2. TCP connection to the VM on port 8765 succeeds within `readyTimeoutSeconds` (default 480)
3. The sweep entry's fio job runs successfully on the VM and measured throughput is greater than zero

The time waited for the server and the same measurements as `fio` are reported via Ginkgo `AddReportEntry`. The full fio JSON is saved as `fio.json` in the entry's results folder.

### Prerequisites

OpenShift Virtualization must be installed and the test namespace needs permission to create `virtualmachines`. The `uat-project-storage-virt` PVC must exist. The VM and the client pod both need outbound access to GitHub, and the VM to the Fedora package mirrors.

### Design rationale

The VM cannot mount the harness folders, so the client pod drives it through fio's client/server mode: the VM does the I/O on its disk and returns the statistics to the client, which records them like any other test. Nothing is read from the VM's disk afterwards.

In that mode fio accepts the job only as a job file, so the test turns the sweep entry's options after `--client` into one.

The test supports serial and project modes but not node mode. The disk is a single image on one PVC, and node mode would start a VM on every node at once, all attaching it. Serial mode pins the VM and the client pod to one node at a time and runs the nodes one after another; `deploy.sh -n <node>` limits it to one node. Running `fio` in serial mode as well puts both tests on the same nodes in the same order. In project mode the scheduler picks the node.


---

## 17. finetune-lora

### Purpose

This test validates the fine-tuning path end to end: Kubeflow Trainer LoRA fine-tunes a base model, and a model server then loads and serves the result. It confirms that Kubeflow Trainer can run a multi-GPU training job, and that the adapter it produces is usable.

### Architecture

```
train-kubeflow [resourceConfig, TrainJob, 2 GPUs] --> adapter on /uat_scratchpad
    |
wait-adapters [ephemeral, waits for the adapter]
    |
vllm-server [persistent, 1 GPU, base model plus the adapter]
    |
    +--- query [pass-fail, ephemeral]
    +--- query-quant [quantitative, ephemeral]
```

The training job is a resource step, which the harness applies without waiting. `wait-adapters` is the gate: it blocks until the job has written its adapter to the scratchpad, so serving only starts after training has finished.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| train-kubeflow | `pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime` | 2 (`serverConfig.gpuCount`) | none | `trainer.kubeflow.org/v1alpha1` TrainJob on the `torch-distributed` ClusterTrainingRuntime (`serverConfig.trainRuntime`), `torchrun` with one rank per GPU |
| wait-adapters | `registry.redhat.io/ubi9/ubi:9.8` | none | none | waits up to 3300 seconds for the adapter's `adapter_config.json` |
| vllm-server | `vllm/vllm-openai:v0.6.6` | 1 | headless ClusterIP :8080 | `--enable-lora --max-loras=1`, the adapter loaded from the scratchpad |
| query | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `SERVER_URL`, model ids and `PROMPT` env |
| query-quant | `registry.redhat.io/ubi9/ubi:9.8` | none | none | same env as `query` |

The base model is `Qwen/Qwen2.5-3B-Instruct` from the models PVC, the dataset is `tatsu-lab/alpaca`, and the job trains for 25 steps at LoRA rank 16. All of these are set in `serverConfig`.

### Pass-fail assertions

1. The model server is healthy
2. The base model and the adapter are registered
3. Inference succeeds against the base model
4. Inference succeeds against the Kubeflow adapter

### Quantitative tests

One chat request is sent to each of the base model and the Kubeflow adapter. Each must return a non-empty response. Completion tokens, latency in seconds and throughput in tokens per second are reported per model via Ginkgo `AddReportEntry`.

### Prerequisites

Kubeflow Trainer must be installed, with the `trainer.kubeflow.org` API group available, the ClusterTrainingRuntime named in `serverConfig.trainRuntime` must exist, and the namespace needs permission to create TrainJobs. On RHOAI 3.5 the trainer component installs the `torch-distributed` runtime. The models PVC must contain the base model. The training pods install their Python dependencies with pip when they start and load the dataset from Hugging Face, so they need outbound access to both.

### Design rationale

The quantitative step records tokens, latency and throughput for the base model and the adapter.

The scratchpad is how the steps hand data to each other: the steps have no other way to pass the adapter from the training step to the serving step.

The training job uses a stock PyTorch image, with the training script embedded in the job and the dependencies installed when it starts.

The test supports project mode only.


---

## 18. platform-check-virt

### Purpose

This test is the `platform-check` for the virt tests. It checks that OpenShift Virtualization is installed: its API groups, the HyperConverged operator's CRD, and the KubeVirt, CDI and network add-ons CRDs that VMs depend on. It does not check for RHOAI, KServe or Kubeflow, because the virt tests run their workloads in their own VM images.

### Architecture

```
checker [pass-fail, ephemeral]
    creates SelfSubjectAccessReviews (RBAC checks)
    queries API server discovery endpoint (API group and CRD checks)
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| checker | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `PERMISSION_CHECKS` env (JSON array of check objects) |

The checks have the same format as in `platform-check`, except that a permission check can also have a `group` for resources outside the core API group.

### Pass-fail assertions

**Permission checks** are expected to be denied:

1. Cannot `get` pods
2. Cannot `create` pods
3. Cannot `get` secrets
4. Cannot `get` nodes
5. Cannot `create` `virtualmachines.kubevirt.io`

**API group checks:**

6. `kubevirt.io` is present
7. `subresources.kubevirt.io` is present (virt-api's aggregated API, used for console, VNC and guest agent access)
8. `cdi.kubevirt.io` is present
9. `nonexistent.example.io` is absent (negative control)

**CRD checks:**

10. `HyperConverged` in `hco.kubevirt.io/v1beta1` (OpenShift Virtualization operator)
11. `KubeVirt` in `kubevirt.io/v1`
12. `VirtualMachine` in `kubevirt.io/v1`
13. `VirtualMachineInstance` in `kubevirt.io/v1`
14. `CDI` in `cdi.kubevirt.io/v1beta1` (Containerized Data Importer)
15. `DataVolume` in `cdi.kubevirt.io/v1beta1`
16. `NetworkAddonsConfig` in `networkaddonsoperator.network.kubevirt.io/v1` (cluster network add-ons operator)

### Prerequisites

Same as `platform-check`: the default ServiceAccount is enough.

### Design rationale

As in `platform-check`, the CRD checks show that the operators are installed but not that they are healthy. A VM test that fails to start is what shows an unhealthy operator.


---

## 19. metrics-isolation

### Purpose

This test validates that metrics are isolated between namespaces. Both namespaces publish a canary metric through a PodMonitor to user workload monitoring, from where Thanos Querier serves every namespace as one shared pile. Nothing in that pipeline separates projects: the separation sits at the two doors a project can use. The tenancy port (9092) checks the identity's right to read the requested namespace and pins the query to that namespace; the web port (9091) only cluster monitors may use. The test tries every way the project namespace has to read the peer's canary, and each must fail or come back without it.

### Architecture

```
project-canary [persistent, project namespace]
    +--- project-podmonitor [PodMonitor, project namespace]
    |
peer-canary [persistent, peer namespace]
    +--- peer-podmonitor [PodMonitor, peer namespace]
    |
    |            user workload Prometheus (scrapes both, no separation)
    |            Thanos Querier (serves both, no separation)
    |
    +--- peer-check [pass-fail, ephemeral, peer namespace]
    |        own canary served at the source ✓
    |        tenancy port, own namespace, own token ✗ 403
    |
    +--- project-check [pass-fail, ephemeral, project namespace]
             own canary served at the source ✓
             reader may read own namespace ✓, may not read the peer's ✓
             tenancy port, own namespace, reader token: own canary ✓
             tenancy port, own namespace, reader token: peer's canary ✗
             tenancy port, peer namespace, reader token ✗ 403
             web port, reader token ✗ 403
             tenancy port, own namespace, own token ✗ 403
```

The reader is the identity the harness playbooks themselves run under: the runner service account with `-p`, the logged-in person without it. It holds the project's user role, which may read metrics there, and the peer's, which may not: a plain tenant, which is who the isolation must hold for. Its token is handed to the project check through a Secret that is torn down with the sequence.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| project-canary | `registry.redhat.io/ubi9/ubi:9.8` | none | headless ClusterIP :8080 | serves `uat_canary_metric{run="<id>"} 1` on `/metrics` |
| peer-canary | `registry.redhat.io/ubi9/ubi:9.8` | none | headless ClusterIP :8080 (peer namespace) | same canary, labeled for its PodMonitor |
| project-podmonitor | PodMonitor | none | none | selects `uat/canary=project` |
| peer-podmonitor | PodMonitor | none | none | selects `uat/canary=peer` (peer namespace) |
| peer-check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `METRIC_NAME`, `RUN_ID`, `CANARY_URL`, `THANOS_TENANCY_URL` env |
| project-check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | those plus `PEER_NAMESPACE`, `THANOS_WEB_URL`, `READER_TOKEN_FILE` env |

### Pass-fail assertions

**peer-check:**

1. Own canary serves the metric over HTTP (the canary exists at the source)
2. Tenancy port refuses the peer's own namespace to a plain tenant pod: 403

**project-check:**

3. Own canary serves the metric over HTTP
4. Reader may `get` `pods.metrics.k8s.io` in the project namespace (SelfSubjectAccessReview; the premise for reading through the tenancy port)
5. Reader may not `get` `pods.metrics.k8s.io` in the peer namespace (the premise for every refusal below)
6. Tenancy port returns the project's canary to the reader, polling until user workload monitoring has scraped it (the positive control every refusal is measured against)
7. Tenancy port, queried with the project's namespace as a project's Grafana would, never returns the peer's canary: without a namespace matcher, with an explicit peer matcher, with a regex matcher, and aggregated with `sum`, which must count exactly one series
8. Tenancy port refuses the peer's namespace to the reader: 403
9. Web port refuses the reader: 403 (the unpinned pile is for cluster monitors)
10. Tenancy port refuses a plain tenant pod even for its own namespace: 403 (the door checks rights, not just shape)

### Prerequisites

Both namespaces' roles may create PodMonitors (`uat-harness` chart), user workload monitoring is enabled on the cluster, and egress to `openshift-monitoring` is open as it is for every tenant namespace. The peer's canary being scraped is not directly observable from either namespace, since no identity available to the test may read the peer's metrics; the project-side positive control covers the scrape pipeline, and the peer's PodMonitor is created by the same mechanism.

### Design rationale

The canary metric carries the run id, so queries never match another run's leftovers. The source checks prove each canary exists before any refusal is trusted, and the reader premises (assertions 4 and 5) make the test fail loudly when it is run by an identity that is not a plain tenant, such as a cluster admin on a workstation, instead of reporting a vacuous pass. The query shapes in assertion 7 exist because the tenancy port's namespace pinning is the actual isolation mechanism; a pinning failure would return the peer's series to a fully authorized reader, which no amount of 403 checking would catch.


---

## 20. rwx

### Purpose

This test validates that the results claim's ReadWriteMany access is real across nodes. Two pods pinned to different nodes share the test's scratchpad directory: the writer writes a marker file, and the reader, on another node, reads it back and checks the content. A claim that only appears RWX, or a filesystem that is not actually shared, fails here.

### Architecture

```
writer [pass-fail, ephemeral, first node of the pair]
    writes marker-<node> to the scratchpad
    |
reader [pass-fail, ephemeral, second node of the pair]
    reads marker-<writer node> back, checks content, checks it is on a different node
```

Pairs mode runs one sequence per ordered node pair, so on a two-node cluster each direction (A to B, then B to A) is checked.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| writer | `registry.redhat.io/ubi9/ubi:9.8` | none | none | writes `/uat_scratchpad/marker-<node>` |
| reader | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `RUN_ID`, `WRITER_NODE`, `NODE`, `MARKER_DIR` env |

### Pass-fail assertions

1. The reader's node differs from the writer's (the data really crossed nodes)
2. The marker the writer left is readable and holds exactly `<run id> from <writer node>`

### Prerequisites

At least two nodes, since pairs mode places the writer and reader on different nodes. The scratchpad is a subdirectory of the same RWX claim the run results live on, created by the harness for every step.

### Design rationale

The scratchpad is used instead of a claim of the test's own because the project deliberately has no rights to create claims: the harness's pre-provisioned storage is the shared filesystem every step already mounts. fio measures how fast that filesystem is; this test checks that it is one filesystem.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `component-checks/rwx.sh`.

---

## 21. negative

### Purpose

This test validates the platform's failure modes. Two pods ask for more than any node has (one for twice the largest node's memory, one for one GPU more than the largest node's count, both derived from the cluster config's inventory so a request that is impossible on one cluster cannot become schedulable on a bigger one) and both must wait in `Pending` with `Unschedulable` as the reason, visible on the pod, instead of being silently queued or failing outright. Deleting them must then complete, rather than leaving them stuck in `Terminating`.

### Architecture

```
memory-hog [Pending, never scheduled]
gpu-hog [Pending, never scheduled]
    |
check [pass-fail, ephemeral]
    reads both pods: phase Pending, PodScheduled False, reason Unschedulable
    deletes both pods: they disappear
```

The GPU taint is tolerated on both pods, so the resource request itself is the only thing keeping them unscheduled.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| memory-hog | `registry.redhat.io/ubi9/ubi:9.8` | none | none | requests 2× the largest node's memory |
| gpu-hog | `registry.redhat.io/ubi9/ubi:9.8` | max+1 requested | none | requests one GPU more than the largest node's count |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `HOGS`, `READER_TOKEN_FILE` env |

### Pass-fail assertions

For each of the two over-requesting pods:

1. Phase is `Pending` (not `Running`, not `Failed`: the request was neither furtively satisfied nor rejected)
2. The `PodScheduled` condition is `False` with reason `Unschedulable`, so the failure is legible on the object
3. After deletion, the pod is gone within two minutes (deletion does not hang in `Terminating`)

### Prerequisites

None beyond pod rights. The check reads and deletes the pods as the reader identity (the one the playbooks run under), whose token reaches the check pod through a Secret torn down with the sequence.

### Design rationale

The hogs' requests are derived from the cluster config's node inventory (twice the largest `componentValidation.sanity.memory`, one more than the largest `nvidia.com/gpu` count) and the test fails loudly when a node's memory is not a plain `<number>Gi`, rather than deriving a request that might fit somewhere. Asserting the `Unschedulable` reason, not just the phase, is what makes this a failure-mode test rather than a scheduling one: a platform that hid the reason, or dropped the pod into a silent queue, would fail the second assertion. The check deletes the pods itself so that "deletion completes" is an assertion with a deadline, instead of something teardown does unobserved.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `component-checks/negative-tests.sh`.

---

## 22. vm-network

### Purpose

This test validates networking for a KubeVirt VM in both directions. A Fedora guest on the pod network serves a marker over HTTP through a Service, which a check pod in the namespace fetches (inbound). The guest also runs its own probes and publishes the results alongside the marker (outbound): cluster DNS and the API service, the outside services an AI workload pulls from (huggingface.co, github.com, quay.io and Docker Hub's registry) and ICMP to its gateway and to an external address.

### Architecture

```
vm [VirtualMachine, Fedora 41 containerDisk, persistent]
    cloud-init: HTTP server in the guest, marker file, probe run
    |
vm-service [headless Service on the VM's pod]
    |
check [pass-fail, ephemeral]
    fetches /marker through the service (inbound)
    fetches /probes.txt: every probe must report ok (outbound)
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| vm | `quay.io/containerdisks/fedora:41` | none | headless ClusterIP :8080 | 2 vCPU, 2Gi guest memory |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `VM_URL`, `RUN_ID`, `EXPECTED_PROBES` env |

### Pass-fail assertions

1. The guest's marker, served at `/marker`, holds exactly `<run id> from vm-network`: the guest booted, and the namespace reached into it through the Service
2. `/probes.txt` holds one line per probe (12) and every line starts with `ok`: the guest resolved cluster DNS and each external name, fetched the cluster API service and each external site, and answered pings from its gateway and from 8.8.8.8

### Prerequisites

OpenShift Virtualization installed and the RBAC to create VMs and Services. The external probes depend on the cluster's internet egress and on those services being up, which is deliberate: an AI workload that cannot reach a model hub or a container registry is broken in practice.

### Design rationale

The probes run inside the guest rather than from the check pod because the guest's network path (its masquerade, the node's DNS proxy, its gateway) is the thing under test; probing from a pod would only exercise the pod's path. Results are published as a plain file next to the marker so one HTTP fetch carries both directions, and the check compares the line count against the expected probe total, so a probe run that died partway fails the test instead of passing vacuously. A curl exit code counts as reachable regardless of HTTP status, because registries answer `401` to anonymous fetches and that still proves the path works.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `vm-testing/test-vm.sh`.

---

## 23. inference-extended

### Purpose

This test validates that a serving stack behaves, not just that it answers fast. Against a plain vLLM OpenAI server it checks: the model is listed, a factual question gets the right answer, both completion endpoints return the OpenAI response schema, temperature zero repeats itself, streaming chunks terminate with `[DONE]`, unknown models and malformed requests are refused with the right status codes, and stop sequences stay out of the output. Latency is measured and recorded, not asserted: the right budget depends on the workload, so the numbers land in the run artifacts to be read against the workload later.

### Architecture

```
vllm-server [persistent, 1 GPU]
    vLLM OpenAI server, granite-3.3-8b-instruct from the models PVC
    |
check [pass-fail, ephemeral]
    correctness, schema, determinism, streaming, errors, stop sequences
    records time-to-first-token and total time
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| vllm-server | `nvcr.io/nvidia/vllm:26.03-py3` | 1 | headless ClusterIP :8000 | granite-3.3-8b-instruct, max-model-len 10000 |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `SERVER_URL`, `MODEL_NAME` env |

### Pass-fail assertions

1. `/v1/models` lists the served model
2. "What is the capital of France?" answered with Paris
3. `/v1/completions` response carries id, object, choices with text and finish reason, and nonzero token usage
4. `/v1/chat/completions` response carries an assistant message with content and a finish reason
5. The same temperature-zero request twice gives identical answers
6. A streaming request delivers content chunks and ends with `data: [DONE]`
7. An unknown model is refused with 404; a request without messages with 400
8. A stop sequence never appears in the output, and the finish reason is `stop`

Recorded, not asserted: time-to-first-token and total response time, plus stream chunk counts.

### Prerequisites

One GPU and the granite model on the models PVC, the same server setup guidellm and inference-perf use.

### Design rationale

The benchmark tests tell you how fast the stack is; this one catches behavior bugs benchmarks race past: a broken chat template, nondeterministic sampling, a proxy that swallows `[DONE]`, an error path that returns 200. Determinism and content checks run at temperature zero with no concurrent load, the conditions under which repeatability is actually expected. Latency stays a recorded measurement because a budget that fits one workload fails another, and a subjective read against the workload is more honest than a threshold tuned to pass.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `inference/test-inference-extended.sh`.

---

## 24. vm-storage

### Purpose

This test validates that a VM's disk is real storage. The VM boots from a CDI DataVolume (a fresh PVC into which CDI imports the Fedora image) rather than an ephemeral containerDisk. A marker file written inside the guest on first boot must survive a full VM stop and start. The kernel's boot id, served alongside the marker, must change across the restart, proving the guest really rebooted so the surviving marker is persistence and not a regenerated file.

### Architecture

```
dv [DataVolume, 10Gi RWO]
    CDI imports quay.io/containerdisks/fedora:41 into a fresh PVC
    |
vm [VirtualMachine, persistent, root disk from the DataVolume]
    cloud-init: systemd web server in the guest (every boot),
    marker written once (first boot only), boot id rewritten each boot
    |
vm-service [headless Service on the VM's pod]
    |
check [pass-fail, ephemeral]
    boots: waits for the marker
    stops and starts the VM through the subresources (the reader identity)
    asserts: same marker after restart, different boot id
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| dv | CDI import of `quay.io/containerdisks/fedora:41` | none | none | 10Gi ReadWriteOnce, node pull method |
| vm | boots from the DataVolume | none | headless ClusterIP :8080 | 2 vCPU, 2Gi guest memory |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `VM_URL`, `VM_NAME`, `RUN_ID`, `READER_TOKEN_FILE` env |

### Pass-fail assertions

1. The guest boots from the volume and serves `<run id> from vm-storage` (import and boot completed)
2. After the check stops the VM, the guest stops serving
3. After the check starts it again, the marker holds exactly the same content: the disk persisted it
4. The boot id after the restart differs from before: the guest rebooted rather than resuming

Recorded: the time from step start to first successful fetch (import plus first boot).

### Prerequisites

OpenShift Virtualization with CDI, and the RBAC to create DataVolumes and to stop and start VMs through their subresources. The check drives the subresources as the reader identity (the one the playbooks run under), which is the same call `virtctl` makes.

### Design rationale

A containerDisk is thrown away with the pod, so booting from one proves nothing about storage; the DataVolume path is the one a real workload uses. The marker is written under a file-exists guard so only the first boot writes it, and the web server runs from a systemd unit so it starts on every boot while the marker does not regenerate. The boot id check exists because a stop that silently failed, or a VM that resumed instead of rebooting, would otherwise make the marker survival look like persistence.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `vm-testing/test-vm-storage.sh`.

---

## 25. vm-migration

### Purpose

This test validates live migration: a running VM moves to another node without rebooting. The disk is a ReadWriteMany DataVolume, reachable from both nodes. The check records the guest's boot id, the VMI's identity and node, and the heartbeat's baseline freshness; then it creates a VirtualMachineInstanceMigration (the object path, the same thing `virtctl migrate` does) and waits for it to succeed. The move counts only if the VMI kept its identity, the node changed, the marker on the disk survived, and the boot id never changed: a reboot would mean the migration was not live.

### Architecture

```
dv [DataVolume, 10Gi RWX]
    CDI imports quay.io/containerdisks/fedora:41 into a shared PVC
    |
vm [VirtualMachine, persistent, root disk from the DataVolume]
    cloud-init: systemd web server, marker (first boot only),
    boot id, heartbeat timestamp rewritten every 100ms
    |
vm-service [headless Service on the VM's pod]
    |
check [pass-fail, ephemeral]
    records: VMI uid and node, boot id, baseline heartbeat freshness
    creates the migration object (the reader identity), waits for Succeeded
    asserts: same uid, different node, same marker, same boot id
    records: migration duration, heartbeat pause estimate
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| dv | CDI import of `quay.io/containerdisks/fedora:41` | none | none | 10Gi ReadWriteMany, node pull method |
| vm | boots from the DataVolume | none | headless ClusterIP :8080 | 2 vCPU, 2Gi guest memory |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `VM_URL`, `VM_NAME`, `RUN_ID`, `READER_TOKEN_FILE` env |

### Pass-fail assertions

1. The guest boots from the shared volume and serves its marker
2. The migration object reaches phase `Succeeded`
3. The VMI keeps the same uid (it was migrated, not replaced) and lands on a different node
4. The marker survives the move unchanged
5. The boot id is unchanged: the guest never rebooted, which is what makes the migration live

Recorded, not asserted: the migration's duration, the heartbeat's loss of freshness during the move minus its baseline (the guest-visible pause estimate), the longest gap between heartbeat reads, and failed reads.

### Prerequisites

OpenShift Virtualization with CDI, at least two nodes the VM can move between, and the RBAC to create DataVolumes and VirtualMachineInstanceMigrations. The check creates the migration as the reader identity (the one the playbooks run under).

### Design rationale

The heartbeat is a timestamp file the guest rewrites every 100ms; its staleness, sampled from the check pod every 50ms, is the guest's own clock of how long it was frozen, so the pause estimate does not depend on the network's behavior. The baseline is measured before the migration starts and subtracted, which removes clock skew between the guest and the check pod. Timing is recorded rather than asserted for the same reason as every other measurement in this suite: the budget depends on the workload and the platform, and a subjective read against the run is more honest than a tuned threshold.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `vm-testing/test-vm-migration.sh`.

---

## 26. vm-evictability

### Purpose

This test validates the asymmetry between container pods and VM pods under eviction pressure. A plain pod asked to evict through the Eviction API yields: the request is accepted and the pod goes away. A VM whose eviction strategy is LiveMigrate does not: its launcher pod's eviction is refused, and the guest keeps running (same marker, same boot id), moving to a new launcher pod if the refusal triggers a live migration. On OpenShift Virtualization 4.22 the refusal comes from KubeVirt's eviction-interceptor admission webhook, whose denial is itself the signal to migrate; older versions guard the pod with a PodDisruptionBudget and answer 429. The test asserts the outcome, refuses-or-not plus the guest surviving, and records whichever mechanism's traces appear. The Eviction API is used directly rather than a node drain, so nothing outside the test's own objects is disturbed.

### Architecture

```
dv [DataVolume, 10Gi RWX] -> vm [VirtualMachine, evictionStrategy LiveMigrate]
    |                          guarded by a PodDisruptionBudget
    |
plain-pod [persistent, Guaranteed like the launcher, no budget]
    |
check [pass-fail, ephemeral]
    records both pods' QoS class and the namespace's disruption budgets
    evicts the plain pod: accepted, pod gone
    evicts the VM's launcher pod: refused (429), guest untouched
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| dv | CDI import of `quay.io/containerdisks/fedora:41` | none | none | 10Gi ReadWriteMany |
| vm | boots from the DataVolume | none | headless ClusterIP :8080 | 2 vCPU, 2Gi, evictionStrategy LiveMigrate |
| plain-pod | `registry.redhat.io/ubi9/ubi:9.8` | none | none | Guaranteed: 1 CPU, 1Gi |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `VM_URL`, `VM_NAME`, `PLAIN_POD`, `RUN_ID`, `READER_TOKEN_FILE` env |

### Pass-fail assertions

1. The guest boots from the shared volume and serves its marker
2. The VM's launcher pod exists, and both pods' QoS classes and the namespace's disruption budgets are recorded
3. Evicting the plain pod is accepted and the pod disappears (a container yields)
4. Evicting the VM's launcher pod is refused (the error is recorded, not assumed to be 429) and the guest still serves the same marker with the same boot id through whatever the refusal triggered (a VM does not yield)

Recorded: both pods' QoS classes and the disruption budgets present.

### Prerequisites

OpenShift Virtualization with CDI, and the RBAC to create pod evictions and read PodDisruptionBudgets. The check evicts as the reader identity (the one the playbooks run under).

### Design rationale

The plain pod is Guaranteed, matching the virt-launcher's QoS, so the difference in outcome comes from the VM's eviction protection and not from scheduling class. The refusal itself is the assertion, because protection that silently failed would let the eviction through and kill the guest, exactly the failure a shared node relies on never seeing. The refusal's transport is deliberately not asserted: this platform's webhook denial and an older version's 429 both mean the same thing, and the error text lands in the run artifacts either way. The source suite made the same choice for the same reason: the Eviction API measures evictability without cordoning a node or evicting anyone else's pods.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `vm-testing/test-vm-evictability.sh`.

---

## 27. vm-secondary-ipam

### Purpose

This test validates how a secondary network's address pool behaves at its limits. The pod network never runs out first (the kubelet refuses to schedule long before IPAM refuses to allocate) but a secondary network is hand-sized by definition and shared between VMs and containers. The test defines an OVN layer-2 network with six usable addresses (a documentation-range /29 that collides with nothing), fills it with claimant VMs until the next claim is refused, and checks: the refusal happens rather than being silently satisfied, its message names address exhaustion, a container draws from the same pool as the VMs, addresses come back on graceful delete, and a force-deleted launcher pod does not permanently shrink the pool. Everything is observed through what a tenant can see: the Multus network-status annotation on pods, and pod events.

### Architecture

```
check [pass-fail, ephemeral] creates and drives everything:
    ipam-<run>-net [NetworkAttachmentDefinition, OVN layer2, 6-address /29]
    ipam-<run>-c1..cN [CirrOS VMs, masquerade default + bridge secondary]
    ipam-<run>-container [plain pod annotated onto the same network]
    fill loop: one claimant at a time until refusal (max 12)
    container claimant refused by the full pool, then takes a freed address
    graceful delete: address reusable; force-delete: no leak, respawn served
```

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| claimant VMs | `quay.io/kubevirt/cirros-container-disk-demo` | none | none | 256Mi, two interfaces |
| container claimant | `registry.access.redhat.com/ubi9/ubi-minimal:latest` | none | none | annotated onto the network |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `TEST_RANGE`, `MAX_CLAIMANTS`, `CLAIM_TIMEOUT_SECONDS`, `VM_IMAGE`, `RUN_ID`, `READER_TOKEN_FILE` env |

### Pass-fail assertions

1. The secondary network is created by an ordinary project identity
2. Exactly the six usable addresses are granted: fewer means addresses were lost or reserved, more means the plugin hands out network or broadcast addresses
3. The seventh claim is refused rather than silently satisfied, and the refusal message names address exhaustion
4. A container is refused by the same pool the VMs filled (VMs and containers share it)
5. Deleting a VM frees an address the still-retrying container immediately takes
6. An address released by gracefully deleted objects is reusable by a new VM
7. After two force-deletes of a claimant's launcher pod, the respawned pod still receives an address: no permanent leak

Recorded: every granted address, the claimant index where refusal happened, and the refusal reasons.

### Prerequisites

OpenShift Virtualization and the RBAC to define secondary networks, create VMs and pods, and read events. The check drives everything as the reader identity (the one the playbooks run under).

### Design rationale

The allocation record is the Multus network-status annotation on the pod, not the VMI's interface status, because the VMI only learns its address from the guest agent, which CirrOS does not have, and the annotation is what a tenant can actually read. The OVN backend is used rather than a bridge plugin because it needs no node bridge, whose existence the check could not verify without cluster-scoped reads; the tenant-visible records work identically. Deliberately left out, as in the source suite: reading the IPAM plugin's cluster-scoped allocation objects: no namespace role can reach them, and "a user cannot audit allocations" is a finding, not a gap to grant around. Cleanup deletes claimants before the network, in that order, because a network deleted while claimants hold addresses leaves the plugin with allocations it cannot reconcile.


Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `vm-testing/test-vm-secondary-ipam.sh`.

---

## 28. vm-memory-overhead

### Purpose

This test measures what a VM's memory costs the node. The virt-launcher pod reserves the guest's memory plus an overhead of its own (qemu, the launcher container, virtio buffers) so a VM denies the node more than its spec says. Three VMs at 512Mi, 2Gi and 8Gi show the shape: the fixed part of the overhead amortizes as the guest grows. Two invariants are asserted: the launcher never reserves less than the guest was promised, and the guest never reports much less than it was promised. The multipliers themselves are recorded for capacity planning, not asserted.

### Architecture

```
vm-small / vm-medium / vm-large [persistent, 1 vCPU, 512Mi / 2Gi / 8Gi]
    Fedora containerDisk, cloud-init serves the guest's MemTotal over HTTP
    |
check [pass-fail, ephemeral]
    per VM: launcher pod's memory request and limit, guest's MemTotal
    asserts: request >= promise, reported >= 90% of promise
    records: request, limit, reported, overhead ratio per size
```

The check reaches each guest directly on its launcher pod's IP, so the test needs no Services.

### Infrastructure

| Name | Image | GPUs | Service | Key config |
|------|-------|------|---------|------------|
| vm-small | `quay.io/containerdisks/fedora:41` | none | none | 1 vCPU, 512Mi guest |
| vm-medium | `quay.io/containerdisks/fedora:41` | none | none | 1 vCPU, 2Gi guest |
| vm-large | `quay.io/containerdisks/fedora:41` | none | none | 1 vCPU, 8Gi guest |
| check | `registry.redhat.io/ubi9/ubi:9.8` | none | none | `VM_SPECS`, `RUN_ID`, `READER_TOKEN_FILE` env |

### Pass-fail assertions

For each of the three guest sizes:

1. The launcher pod's memory request, summed over its containers, is at least the guest's promised memory (a VM never under-reserves)

Recorded: the launcher's memory request and limit, the guest's own `MemTotal`, and the overhead ratio (request divided by promise) per size. The guest's view is recorded rather than asserted because the firmware and kernel carve-out grows with the guest (64 to 260MB observed across these sizes), so no single floor fits every size.

### Prerequisites

OpenShift Virtualization, and the RBAC to create VMs and read pods. The check reads the launcher pods as the reader identity (the one the playbooks run under).

### Design rationale

The guest's own view is read from inside the guest rather than from the VMI's status because the VMI learns its memory the same way (from the guest) and serving `/proc/meminfo` over HTTP needs no guest agent. The 90% floor on the guest's view leaves room for the firmware's small carve-out without accepting a guest that sees half its memory. The overhead ratio is a measurement, following this suite's rule for timings: what counts as too much overhead is a judgment against the platform and the workload, not a threshold tuned to pass.

Ported from the [open-science-test-cases](https://github.com/memalhot/open-science-test-cases) suite, `vm-testing/test-vm-memory-overhead.sh`.
