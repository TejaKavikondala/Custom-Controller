# Model Deployment Operator

A production-pattern Kubernetes operator that manages the full lifecycle of LLM deployments — from storage provisioning through model download to inference serving.

Built with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) in Go. Designed to demonstrate the same architecture as HPE AI Essentials' MLIS component.

---

## Architecture

```
kubectl apply -f sample.yaml
        │
        ▼
┌──────────────────────────────────────────────────────────────────┐
│                    Reconcile Loop                                 │
│                                                                   │
│  CR Created                                                       │
│       │                                                           │
│       ▼                                                           │
│  Phase 1: Provisioning ──────────────────────────────────────    │
│       │   Create PVC (standard StorageClass)                      │
│       │   Watch for PVC.Status.Phase == Bound                     │
│       │   RequeueAfter: 5s                                        │
│       │                                                           │
│       ▼  (PVC Bound)                                              │
│  Phase 2: Downloading ───────────────────────────────────────    │
│       │   Create busybox Job (simulates: huggingface-cli download)│
│       │   Watch for Job condition: Complete                       │
│       │   RequeueAfter: 5s                                        │
│       │                                                           │
│       ▼  (Job Complete)                                           │
│  Phase 3: Ready ─────────────────────────────────────────────    │
│           Create/patch Deployment (inference server pods)         │
│           Count Ready pods → update status.readyReplicas         │
│           RequeueAfter: 30s                                       │
└──────────────────────────────────────────────────────────────────┘
```

### Kubernetes patterns demonstrated

| Pattern | Where |
|---------|-------|
| Custom Resource Definition (CRD) | `api/v1/types.go`, `config/crd/` |
| Reconcile loop | `internal/controller/modeldeployment_controller.go` |
| State machine (3 phases) | `Reconcile()` function |
| Owner references + garbage collection | `ctrl.SetControllerReference(...)` |
| Finalizers (deletion lifecycle) | `handleDeletion()` |
| Status conditions (`metav1.Condition`) | `setCondition()` |
| `Owns()` watches (event-driven) | `SetupWithManager()` |
| Leader election (HA) | `main.go --leader-elect` |
| Health probes | `/healthz`, `/readyz` endpoints |

---

## Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| Go | 1.21+ | https://go.dev/dl |
| kind | any | `brew install kind` or https://kind.sigs.k8s.io |
| kubectl | any | `brew install kubectl` |

---

## Quick Start

### 1 — Set up the cluster

```bash
make demo
```

This creates a `kind` cluster, installs the CRD, and applies RBAC. It prints next steps.

### 2 — Start the operator (Terminal A)

```bash
make run
```

You'll see structured logs. The operator is now watching for `ModelDeployment` CRs.

### 3 — Apply the sample CR (Terminal B)

```bash
make apply-sample
```

### 4 — Watch the state machine

```bash
# Watch status.phase change: Provisioning → Downloading → Ready
make watch

# Full picture: PVC, Job, Deployment, Pods
make status

# Human-readable description with conditions
make describe

# Download job logs (~15s into the demo)
make logs
```

### 5 — Clean up

```bash
make clean
```

---

## Expected timeline

```
t=0s    CR applied → finalizer added → PVC created
t=1s    Phase: Provisioning (waiting for PVC to bind)
t=3s    PVC binds on kind → Phase: Downloading → Job created
t=5s    Job starts (busybox:1.36 pulls from Docker Hub)
t=20s   Job completes (sleep 15 simulation done)
t=25s   Phase: Ready → Deployment created (1 replica)
        ⚠️  Pods stay Pending on kind — nvidia.com/gpu not satisfiable
            That's expected. The operator logic is the demo, not the GPU.
```

> **To get pods to actually run on kind**: In `internal/controller/modeldeployment_controller.go`, remove or comment out the `"nvidia.com/gpu": gpuQty` line in `reconcileDeployment()`. Run `make run` again. Pods will schedule and reach Running state.

---

## Project structure

```
model-operator/
├── main.go                          # Manager setup, scheme registration
├── go.mod
├── api/
│   └── v1/
│       ├── types.go                 # CRD types + manual DeepCopy methods
│       └── register.go              # Scheme registration (AddToScheme)
├── internal/
│   └── controller/
│       └── modeldeployment_controller.go   # Reconciler — state machine
└── config/
    ├── crd/
    │   └── modeldeployment.yaml     # CRD manifest (apply this to K8s)
    ├── rbac/
    │   └── role.yaml                # ClusterRole + ClusterRoleBinding
    └── samples/
        └── sample.yaml              # Example ModelDeployment CRs
```

---

## The CRD

```yaml
apiVersion: models.hpe.io/v1
kind: ModelDeployment
metadata:
  name: llama3-demo
spec:
  modelName: meta-llama/Meta-Llama-3-8B-Instruct
  replicas: 1
  storageSize: 10Gi
  gpuPerPod: 1
  storageClass: standard   # kind; use GreenLake FS class on HPE AIE
```

### Status fields updated by the operator

```yaml
status:
  phase: Ready               # Provisioning | Downloading | Ready | Failed
  readyReplicas: 1
  message: "Model meta-llama/Meta-Llama-3-8B-Instruct is serving (1/1 replicas ready)"
  pvcName: llama3-demo-models
  downloadJobName: llama3-demo-download
  deploymentName: llama3-demo-inference
  conditions:
    - type: Ready
      status: "True"
      reason: ModelReady
      lastTransitionTime: "2024-01-15T10:30:00Z"
```

---

## Production differences (vs this demo)

| Demo | Production (HPE AIE / MLIS) |
|------|-----------------------------|
| `busybox:1.36` downloader | `kserve/huggingfaceserver` with `huggingface-cli download` |
| `ReadWriteOnce` PVC | `ReadWriteMany` (GreenLake File Storage, shared across replicas) |
| `sleep 15` download sim | Actual model weight download (minutes for 8B+ models) |
| `busybox` inference server | NVIDIA NIM container, vLLM, or TGI |
| `nvidia.com/gpu` → Pending | Real GPU nodes with device plugin |
| Local kubeconfig | In-cluster RBAC via ServiceAccount |
| Single cluster | Multi-cluster with cross-namespace model cache |

The operator **pattern** (CRD → controller → state machine → owned resources) is identical in both environments.

---

## Key points

**" Operator's reconcile loop."**
> The reconcile function implements a 3-phase state machine. Each phase creates a resource and re-queues until that resource reaches its terminal state — PVC Bound, Job Complete, Deployment pods Ready. Owner references ensure automatic garbage collection when the CR is deleted.

**"How does your operator handle crashes or restarts?"**
> The reconcile function is fully idempotent — it reads the current state from the API server on every call, checks whether each resource already exists, and only creates what's missing. If the operator restarts mid-download, the next reconcile sees the existing Job and continues waiting for it.

**"What's the difference between an operator and a controller?"**
> Every operator contains a controller, but not every controller is an operator. An operator encodes domain-specific operational knowledge — in this case, the sequence: provision storage, download weights, then serve. A bare controller just reacts to state changes without encoding that domain logic.

**"Why finalizers?"**
> Owner references handle cascading deletion of child resources. The finalizer gives us a hook to do additional cleanup before K8s removes the CR itself — for example, deregistering the model from a model registry or flushing billing records.
