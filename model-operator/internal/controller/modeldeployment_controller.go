package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	modelv1 "github.com/yourusername/model-operator/api/v1"
)

const (
	finalizerName = "models.hpe.io/cleanup"
)

// ModelDeploymentReconciler reconciles ModelDeployment objects
type ModelDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is the main control loop.
// It is called whenever a ModelDeployment CR is created, updated, or deleted,
// and also when any owned resource (PVC, Job, Deployment) changes.
func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// ── Fetch the CR ──────────────────────────────────────────────────────────
	md := &modelv1.ModelDeployment{}
	if err := r.Get(ctx, req.NamespacedName, md); err != nil {
		// Object deleted before we got here — nothing to do
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling ModelDeployment",
		"name", md.Name,
		"phase", md.Status.Phase,
		"model", md.Spec.ModelName)

	// ── Handle deletion ───────────────────────────────────────────────────────
	if !md.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, md)
	}

	// ── Ensure finalizer is present ───────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(md, finalizerName) {
		controllerutil.AddFinalizer(md, finalizerName)
		if err := r.Update(ctx, md); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Requeue so the rest of reconcile runs with the finalizer in place
		return ctrl.Result{Requeue: true}, nil
	}

	// ── STATE MACHINE ─────────────────────────────────────────────────────────
	// Phase 1: PVC — just ensure it exists (binding happens when Job pod is scheduled)
	if _, err := r.reconcilePVC(ctx, md); err != nil {
		return ctrl.Result{}, r.setFailed(ctx, md, fmt.Sprintf("PVC error: %v", err))
	}

	// Phase 2: Download Job — create and wait for completion
	jobDone, err := r.reconcileDownloadJob(ctx, md)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, md, fmt.Sprintf("Download Job error: %v", err))
	}
	if !jobDone {
		log.Info("Waiting for download Job to complete")
		r.setPhase(ctx, md, modelv1.PhaseDownloading, "Downloading model weights into PVC") //nolint
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Phase 3: Inference Deployment
	if err := r.reconcileDeployment(ctx, md); err != nil {
		return ctrl.Result{}, r.setFailed(ctx, md, fmt.Sprintf("Deployment error: %v", err))
	}

	// ── Update final status ───────────────────────────────────────────────────
	if err := r.reconcileFinalStatus(ctx, md); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue every 30s to keep ReadyReplicas count fresh
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ── Phase 1: PVC ─────────────────────────────────────────────────────────────

func (r *ModelDeploymentReconciler) reconcilePVC(ctx context.Context, md *modelv1.ModelDeployment) (bool, error) {
	pvcName := md.Name + "-models"
	pvc := &corev1.PersistentVolumeClaim{}

	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: md.Namespace}, pvc)
	if errors.IsNotFound(err) {
		// PVC doesn't exist yet — create it
		storageClass := md.Spec.StorageClass
		if storageClass == "" {
			storageClass = "standard"
		}
		desired := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: md.Namespace,
				Labels:    r.labels(md),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				// kind doesn't support ReadWriteMany, so use ReadWriteOnce for demo.
				// In HPE AIE with GreenLake File Storage: ReadWriteMany
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				StorageClassName: &storageClass,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(md.Spec.StorageSize),
					},
				},
			},
		}
		// Set owner reference so PVC is deleted with the ModelDeployment
		if err := ctrl.SetControllerReference(md, desired, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("creating PVC: %w", err)
		}
		// Update status with PVC name
		md.Status.PVCName = pvcName
		_ = r.Status().Update(ctx, md)
		return false, nil // Just created — wait for it to bind
	}
	if err != nil {
		return false, fmt.Errorf("getting PVC: %w", err)
	}

	// PVC exists — check if it's Bound
	return pvc.Status.Phase == corev1.ClaimBound, nil
}

// ── Phase 2: Download Job ─────────────────────────────────────────────────────

func (r *ModelDeploymentReconciler) reconcileDownloadJob(ctx context.Context, md *modelv1.ModelDeployment) (bool, error) {
	jobName := md.Name + "-download"
	job := &batchv1.Job{}

	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: md.Namespace}, job)
	if errors.IsNotFound(err) {
		// Job doesn't exist — create it
		desired := r.buildDownloadJob(md, jobName)
		if err := ctrl.SetControllerReference(md, desired, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("creating download Job: %w", err)
		}
		md.Status.DownloadJobName = jobName
		_ = r.Status().Update(ctx, md)
		return false, nil // Just created — wait for completion
	}
	if err != nil {
		return false, fmt.Errorf("getting Job: %w", err)
	}

	// Job exists — inspect conditions
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return true, nil // ✅ Done
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return false, fmt.Errorf("download Job %s failed", jobName)
		}
	}
	return false, nil // Still running
}

func (r *ModelDeploymentReconciler) buildDownloadJob(md *modelv1.ModelDeployment, jobName string) *batchv1.Job {
	pvcName := md.Name + "-models"
	// In production this runs: huggingface-cli download <modelName> --local-dir /mnt/models
	// For the demo we simulate with a sleep to show the state machine working
	downloadCmd := fmt.Sprintf(
		`echo "=== Model Operator Demo ===" && \
		 echo "Downloading model: %s" && \
		 echo "Target: /mnt/models/%s" && \
		 sleep 15 && \
		 echo "Download complete!" && \
		 ls /mnt/models`,
		md.Spec.ModelName,
		md.Spec.ModelName,
	)

	backoffLimit := int32(3)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: md.Namespace,
			Labels:    r.labels(md),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: r.labels(md),
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "downloader",
							Image:   "busybox:1.36",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{downloadCmd},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "model-cache",
									MountPath: "/mnt/models",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "model-cache",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}
}

// ── Phase 3: Inference Deployment ────────────────────────────────────────────

func (r *ModelDeploymentReconciler) reconcileDeployment(ctx context.Context, md *modelv1.ModelDeployment) error {
	deployName := md.Name + "-inference"
	pvcName := md.Name + "-models"
	deploy := &appsv1.Deployment{}

	err := r.Get(ctx, types.NamespacedName{Name: deployName, Namespace: md.Namespace}, deploy)

	// Build the desired Deployment
	replicas := md.Spec.Replicas
	// NOTE: nvidia.com/gpu will keep pods Pending on kind (no GPU nodes).
	// That's expected — the quota and spec are what matter for the demo.
	// Remove the GPU resource request below if you want pods to actually run on kind.
	gpuQty := resource.MustParse(fmt.Sprintf("%d", md.Spec.GPUPerPod))

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: md.Namespace,
			Labels:    r.labels(md),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":   "model-inference",
					"model": sanitizeName(md.Spec.ModelName),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "model-inference",
						"model": sanitizeName(md.Spec.ModelName),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "inference-server",
							Image: "busybox:1.36",
							// In production: NVIDIA NIM container image
							Command: []string{"/bin/sh", "-c"},
							Args: []string{
								fmt.Sprintf(
									`echo "=== Inference Server ===" && \
									 echo "Serving model: %s" && \
									 echo "GPU count: %d" && \
									 echo "Model path: /mnt/models/%s" && \
									 sleep infinity`,
									md.Spec.ModelName,
									md.Spec.GPUPerPod,
									md.Spec.ModelName,
								),
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									// Comment out the GPU line below to run on kind without GPU nodes
									"nvidia.com/gpu": gpuQty,
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "model-cache",
									MountPath: "/mnt/models",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "model-cache",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
									ReadOnly:  true,
								},
							},
						},
					},
				},
			},
		},
	}

	if errors.IsNotFound(err) {
		if err := ctrl.SetControllerReference(md, desired, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating Deployment: %w", err)
		}
		md.Status.DeploymentName = deployName
		_ = r.Status().Update(ctx, md)
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting Deployment: %w", err)
	}

	// Deployment exists — patch replicas if changed
	patch := client.MergeFrom(deploy.DeepCopy())
	deploy.Spec.Replicas = &replicas
	if err := r.Patch(ctx, deploy, patch); err != nil {
		return fmt.Errorf("patching Deployment replicas: %w", err)
	}
	return nil
}

// ── Status Helpers ─────────────────────────────────────────────────────────────

func (r *ModelDeploymentReconciler) reconcileFinalStatus(ctx context.Context, md *modelv1.ModelDeployment) error {
	// Count ready inference pods
	podList := &corev1.PodList{}
	_ = r.List(ctx, podList,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{
			"app":   "model-inference",
			"model": sanitizeName(md.Spec.ModelName),
		},
	)
	readyCount := int32(0)
	for _, pod := range podList.Items {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				readyCount++
			}
		}
	}

	md.Status.Phase = modelv1.PhaseReady
	md.Status.ReadyReplicas = readyCount
	md.Status.Message = fmt.Sprintf("Model %s is serving (%d/%d replicas ready)",
		md.Spec.ModelName, readyCount, md.Spec.Replicas)

	// Set standard condition
	r.setCondition(md, "Ready", metav1.ConditionTrue, "ModelReady",
		fmt.Sprintf("Model %s is deployed and serving", md.Spec.ModelName))

	return r.Status().Update(ctx, md)
}

func (r *ModelDeploymentReconciler) setPhase(ctx context.Context, md *modelv1.ModelDeployment, phase, message string) error {
	md.Status.Phase = phase
	md.Status.Message = message
	return r.Status().Update(ctx, md)
}

func (r *ModelDeploymentReconciler) setFailed(ctx context.Context, md *modelv1.ModelDeployment, message string) error {
	md.Status.Phase = modelv1.PhaseFailed
	md.Status.Message = message
	r.setCondition(md, "Ready", metav1.ConditionFalse, "Error", message)
	_ = r.Status().Update(ctx, md)
	return fmt.Errorf(message)
}

func (r *ModelDeploymentReconciler) setCondition(md *modelv1.ModelDeployment, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	newCond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: md.Generation,
	}
	for i, c := range md.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				md.Status.Conditions[i] = newCond
			}
			return
		}
	}
	md.Status.Conditions = append(md.Status.Conditions, newCond)
}

// ── Deletion Handler ──────────────────────────────────────────────────────────

func (r *ModelDeploymentReconciler) handleDeletion(ctx context.Context, md *modelv1.ModelDeployment) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(md, finalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("Running cleanup for deleted ModelDeployment", "name", md.Name)
	// Child resources (PVC, Job, Deployment) are auto-deleted by K8s via owner references.
	// The finalizer just gives us a chance to do extra cleanup (e.g. deregister from a model registry).
	log.Info("Cleanup complete — removing finalizer")

	controllerutil.RemoveFinalizer(md, finalizerName)
	if err := r.Update(ctx, md); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ── Watches ───────────────────────────────────────────────────────────────────

// SetupWithManager wires up the controller and declares which resources to watch
func (r *ModelDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&modelv1.ModelDeployment{}).
		Owns(&corev1.PersistentVolumeClaim{}). // triggers reconcile when PVC changes (e.g. Bound)
		Owns(&batchv1.Job{}).                  // triggers reconcile when Job completes
		Owns(&appsv1.Deployment{}).            // triggers reconcile when Deployment changes
		Complete(r)
}

// ── Utilities ─────────────────────────────────────────────────────────────────

func (r *ModelDeploymentReconciler) labels(md *modelv1.ModelDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "model-operator",
		"app.kubernetes.io/instance": md.Name,
		"models.hpe.io/model-name":   sanitizeName(md.Spec.ModelName),
	}
}

// sanitizeName replaces slashes and dots with dashes for use in K8s names/labels
func sanitizeName(name string) string {
	result := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '/' || c == '.' || c == '_' {
			result[i] = '-'
		} else {
			result[i] = c
		}
	}
	return string(result)
}
