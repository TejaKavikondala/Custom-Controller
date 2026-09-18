# Model Deployment Operator — Makefile
# Requires: kind, kubectl, go 1.21+
#
# Quick start:
#   make demo          → full end-to-end: cluster + CRD + operator + sample CR
#   make watch         → watch the CR status update in real time
#   make clean         → tear down everything

CLUSTER_NAME  ?= model-operator-demo
NAMESPACE     ?= default
SAMPLE_NAME   ?= llama3-demo

.PHONY: all build tidy kind-create kind-delete install-crd rbac run demo watch logs clean help

## ─────────────────────────────────────────────────────────────────────────────
## Build
## ─────────────────────────────────────────────────────────────────────────────

all: tidy build

tidy:
	@echo "→ go mod tidy"
	go mod tidy

build:
	@echo "→ go build ./..."
	go build ./...

## ─────────────────────────────────────────────────────────────────────────────
## Cluster management
## ─────────────────────────────────────────────────────────────────────────────

kind-create:
	@echo "→ Creating kind cluster '$(CLUSTER_NAME)'"
	kind create cluster --name $(CLUSTER_NAME)

kind-delete:
	@echo "→ Deleting kind cluster '$(CLUSTER_NAME)'"
	kind delete cluster --name $(CLUSTER_NAME)

## ─────────────────────────────────────────────────────────────────────────────
## Install CRD and RBAC
## ─────────────────────────────────────────────────────────────────────────────

install-crd:
	@echo "→ Installing ModelDeployment CRD"
	kubectl apply -f config/crd/modeldeployment.yaml
	@echo "→ Waiting for CRD to be established"
	kubectl wait --for=condition=Established crd/modeldeployments.models.hpe.io --timeout=30s

rbac:
	@echo "→ Applying RBAC (ClusterRole + ClusterRoleBinding)"
	kubectl apply -f config/rbac/role.yaml

## ─────────────────────────────────────────────────────────────────────────────
## Run the operator
## ─────────────────────────────────────────────────────────────────────────────

# Run the operator in the foreground (Ctrl+C to stop)
run: install-crd rbac
	@echo "→ Starting operator (press Ctrl+C to stop)"
	go run ./main.go

## ─────────────────────────────────────────────────────────────────────────────
## Apply sample CRs
## ─────────────────────────────────────────────────────────────────────────────

apply-sample:
	@echo "→ Applying sample ModelDeployment CR"
	kubectl apply -f config/samples/sample.yaml

delete-sample:
	@echo "→ Deleting sample ModelDeployment CR (finalizer will run cleanup)"
	kubectl delete -f config/samples/sample.yaml --ignore-not-found

## ─────────────────────────────────────────────────────────────────────────────
## Observation
## ─────────────────────────────────────────────────────────────────────────────

watch:
	@echo "→ Watching ModelDeployment '$(SAMPLE_NAME)' (Ctrl+C to stop)"
	kubectl get modeldeployment $(SAMPLE_NAME) -w

status:
	@echo "=== ModelDeployments ==="
	kubectl get modeldeployments -o wide
	@echo ""
	@echo "=== PVCs ==="
	kubectl get pvc -l "app.kubernetes.io/name=model-operator"
	@echo ""
	@echo "=== Jobs ==="
	kubectl get jobs -l "app.kubernetes.io/name=model-operator"
	@echo ""
	@echo "=== Deployments ==="
	kubectl get deployments -l "app.kubernetes.io/name=model-operator"
	@echo ""
	@echo "=== Pods ==="
	kubectl get pods -l "app.kubernetes.io/name=model-operator"

describe:
	kubectl describe modeldeployment $(SAMPLE_NAME)

logs:
	@echo "→ Tailing download job logs (may take ~15s to appear)"
	kubectl logs -l "app.kubernetes.io/instance=$(SAMPLE_NAME)" --all-containers --follow || true

## ─────────────────────────────────────────────────────────────────────────────
## Full demo workflow
## ─────────────────────────────────────────────────────────────────────────────

demo: kind-create install-crd rbac
	@echo ""
	@echo "╔══════════════════════════════════════════════════════════════╗"
	@echo "║          Model Deployment Operator — Demo Ready!            ║"
	@echo "╠══════════════════════════════════════════════════════════════╣"
	@echo "║                                                              ║"
	@echo "║  In a NEW terminal, run:                                     ║"
	@echo "║    make run                                                  ║"
	@echo "║                                                              ║"
	@echo "║  Then in another terminal, apply the sample CR:             ║"
	@echo "║    make apply-sample                                         ║"
	@echo "║                                                              ║"
	@echo "║  Watch the state machine progress:                           ║"
	@echo "║    make watch                                                ║"
	@echo "║    make status                                               ║"
	@echo "║                                                              ║"
	@echo "╚══════════════════════════════════════════════════════════════╝"

## ─────────────────────────────────────────────────────────────────────────────
## Cleanup
## ─────────────────────────────────────────────────────────────────────────────

clean: delete-sample kind-delete
	@echo "→ Cleanup complete"

## ─────────────────────────────────────────────────────────────────────────────
## Help
## ─────────────────────────────────────────────────────────────────────────────

help:
	@echo "Model Deployment Operator — Make Targets"
	@echo ""
	@echo "  make demo           Full setup: kind cluster + CRD + RBAC"
	@echo "  make run            Start the operator (foreground)"
	@echo "  make apply-sample   Apply the sample ModelDeployment CR"
	@echo "  make watch          Watch CR status in real time"
	@echo "  make status         Show all owned resources"
	@echo "  make describe       kubectl describe the sample CR"
	@echo "  make logs           Tail download job logs"
	@echo "  make clean          Delete kind cluster and all resources"
	@echo "  make build          go build ./..."
	@echo "  make tidy           go mod tidy"
