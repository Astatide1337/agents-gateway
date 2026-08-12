package argoworkflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	runSecretManagedByLabel   = "agents.astatide.com/managed-by"
	runSecretManagedByValue   = "agw-run-secret"
	runSecretPhaseLabel       = "agents.astatide.com/credential-phase"
	runSecretUIDLabel         = "agents.astatide.com/run-uid"
	runSecretDigestAnnotation = "agents.astatide.com/spec-digest"
)

// CleanupWorkResources is retained as a trusted controller-side cleanup
// helper for callers that explicitly provide a Kubernetes client. The Argo
// onExit template does not call it: lifecycle pods are tokenless and have no
// Secret RBAC, while the main AgentRun controller owns per-run credential
// deletion and Sandbox/PVC retention.
//
// The operation is intentionally idempotent. A missing object is success, an
// object with any foreign identity is a hard error, and a retry after a
// successful delete is therefore safe.
func CleanupWorkResources(ctx context.Context, kube client.Client, request Request) error {
	if ctx == nil || kube == nil {
		return ErrProducerUnavailable
	}
	if err := validateRequest(request); err != nil {
		return err
	}
	secretName := workload.WorkSecretName(request.RunUID)
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: request.Namespace, Name: secretName}
	if err := kube.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get work Secret for cleanup: %w", err)
	}
	if err := validateCleanupSecret(secret, request); err != nil {
		return err
	}
	// resourceNames cannot safely scope a generated per-run Secret. The
	// authenticated GET validates its owner/digest, and this UID precondition
	// closes the name-reuse race between validation and deletion.
	uid := secret.UID
	if uid == "" {
		return fmt.Errorf("%w: work Secret UID is missing", ErrProducerConflict)
	}
	if err := kube.Delete(ctx, secret, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete work Secret: %w", err)
	}
	return nil
}

func validateCleanupSandbox(work *v1beta1.Sandbox, request Request) error {
	if work == nil || work.Namespace != request.Namespace || work.Name == "" || work.UID == "" || len(work.OwnerReferences) != 1 {
		return fmt.Errorf("%w: work Sandbox identity is not owned by this run", ErrProducerConflict)
	}
	owner := work.OwnerReferences[0]
	if !matchesRunOwner(owner, request) || owner.Kind != "AgentRun" {
		return fmt.Errorf("%w: work Sandbox owner is foreign", ErrProducerConflict)
	}
	name, err := sandbox.ChildName(types.UID(request.RunUID), sandbox.RoleWork)
	if err != nil || work.Name != name {
		return fmt.Errorf("%w: work Sandbox name is not deterministic", ErrProducerConflict)
	}
	labels := work.Labels
	annotations := work.Annotations
	if labels[sandbox.RunUIDLabelKey] != request.RunUID || labels[sandbox.RoleLabelKey] != string(sandbox.RoleWork) || labels[sandbox.SpecDigestLabelKey] != cleanupDigestLabel(request.SpecDigest) || annotations[sandbox.SpecDigestAnnotationKey] != request.SpecDigest || !canonical.ValidDigest(annotations[sandbox.SandboxSpecFingerprintAnnotationKey]) {
		return fmt.Errorf("%w: work Sandbox ownership or digest is foreign", ErrProducerConflict)
	}
	return nil
}

func validateCleanupSecret(secret *corev1.Secret, request Request) error {
	if secret == nil || secret.Namespace != request.Namespace || secret.Name != workload.WorkSecretName(request.RunUID) || secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable || len(secret.OwnerReferences) != 1 {
		return fmt.Errorf("%w: work Secret identity is not owned by this run", ErrProducerConflict)
	}
	owner := secret.OwnerReferences[0]
	if !matchesRunOwner(owner, request) || owner.Kind != "AgentRun" {
		return fmt.Errorf("%w: work Secret owner is foreign", ErrProducerConflict)
	}
	if secret.Labels[runSecretManagedByLabel] != runSecretManagedByValue || secret.Labels[runSecretPhaseLabel] != "work" || secret.Labels[runSecretUIDLabel] != request.RunUID || secret.Annotations[runSecretDigestAnnotation] != request.SpecDigest {
		return fmt.Errorf("%w: work Secret ownership or digest is foreign", ErrProducerConflict)
	}
	return nil
}

func matchesRunOwner(owner metav1.OwnerReference, request Request) bool {
	return owner.APIVersion == "agents.astatide.com/v1alpha1" && owner.Name == request.RunName && owner.UID == types.UID(request.RunUID) && owner.Controller != nil && *owner.Controller && owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion
}

func cleanupDigestLabel(digest string) string {
	value := strings.TrimPrefix(digest, "sha256:")
	if len(value) > 63 {
		return value[:63]
	}
	return value
}
