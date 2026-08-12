package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrInventoryIncomplete    = errors.New("retention: inventory is incomplete")
	ErrInventoryUnavailable   = errors.New("retention: inventory is unavailable")
	ErrInvalidInventoryConfig = errors.New("retention: invalid inventory configuration")
)

// ObjectInfo is the metadata required for an object-store deletion fence.
// The body is never downloaded during inventory, and an empty ETag is not
// eligible for an enforcing action.
type ObjectInfo struct {
	Key          string
	ETag         string
	LastModified time.Time
	SizeBytes    int64
}

// ObjectInventory is intentionally narrower than the immutable write store.
// List must report complete=false when a bounded page leaves more objects, and
// Delete must honor the supplied version/ETag fence.
type ObjectInventory interface {
	List(context.Context, string, int) ([]ObjectInfo, bool, error)
	Delete(context.Context, string, string) error
}

// ObjectBodyReader is required for ledger records. Metadata such as an S3
// size or LastModified value is never sufficient to authorize retirement.
type ObjectBodyReader interface {
	Get(context.Context, string) ([]byte, error)
}

type ObjectPairWriter interface {
	ObjectBodyReader
	Create(context.Context, string, []byte, string) (bool, error)
}

// KubernetesInventoryReader is the only Kubernetes read capability needed by
// retention. Keeping it as a narrow interface makes the inventory boundary
// testable and prevents accidental Get/Update use in the retention package.
type KubernetesInventoryReader interface {
	List(context.Context, client.ObjectList, ...client.ListOption) error
}

// KubernetesMetadataReader is used for resource kinds whose normal API
// representation contains credential material. Kubernetes' metadata client
// requests PartialObjectMetadataList, so retention can inventory ownership
// and labels without ever decoding Secret.data into the operator process.
// It is intentionally separate from KubernetesInventoryReader: a typed
// SecretList is not a safe implementation of this boundary.
type KubernetesMetadataReader interface {
	ListMetadata(context.Context, schema.GroupVersionResource, string, int) (*metav1.PartialObjectMetadataList, error)
}

type InventoryLimits struct {
	MaxRuns            int
	MaxResources       int
	MaxObjects         int
	MaxLedgerBodyBytes int64
}

func DefaultInventoryLimits() InventoryLimits {
	return InventoryLimits{MaxRuns: 256, MaxResources: 1024, MaxObjects: 4096, MaxLedgerBodyBytes: effects.MaxLedgerBodyBytes}
}

func (l InventoryLimits) normalized() (InventoryLimits, error) {
	if l.MaxRuns <= 0 || l.MaxResources <= 0 || l.MaxObjects <= 0 || l.MaxRuns > MaxInventoryItems || l.MaxResources > MaxInventoryItems || l.MaxObjects > MaxInventoryItems {
		return InventoryLimits{}, ErrInvalidInventoryConfig
	}
	if l.MaxLedgerBodyBytes == 0 {
		l.MaxLedgerBodyBytes = effects.MaxLedgerBodyBytes
	}
	if l.MaxLedgerBodyBytes <= 0 || l.MaxLedgerBodyBytes > effects.MaxLedgerBodyBytes {
		return InventoryLimits{}, ErrInvalidInventoryConfig
	}
	return l, nil
}

// KubernetesSource inventories only the configured run namespace. It lists
// complete bounded collections before returning anything to the planner; one
// list failure or continuation token makes the entire snapshot unusable.
type KubernetesSource struct {
	Reader       KubernetesInventoryReader
	Metadata     KubernetesMetadataReader
	Objects      ObjectInventory
	Namespace    string
	ObjectPrefix string
	LedgerPrefix string
	Limits       InventoryLimits
}

func (s *KubernetesSource) Collect(ctx context.Context) (Inventory, error) {
	if s == nil || s.Reader == nil || s.Metadata == nil || s.Objects == nil || ctx == nil || s.Namespace == "" {
		return Inventory{}, ErrInvalidInventoryConfig
	}
	limits, err := s.Limits.normalized()
	if err != nil {
		return Inventory{}, err
	}
	if err := validateObjectKey(strings.TrimSuffix(s.ObjectPrefix, "/"), false); err != nil {
		return Inventory{}, fmt.Errorf("%w: object prefix: %v", ErrInvalidInventoryConfig, err)
	}
	ledgerPrefix := s.LedgerPrefix
	if ledgerPrefix == "" {
		ledgerPrefix = DefaultPolicy().LedgerPrefix
	}
	if err := validateObjectKey(strings.TrimSuffix(ledgerPrefix, "/"), false); err != nil {
		return Inventory{}, fmt.Errorf("%w: ledger prefix: %v", ErrInvalidInventoryConfig, err)
	}

	runList := &v1alpha1.AgentRunList{}
	if err := listBounded(ctx, s.Reader, runList, s.Namespace, limits.MaxRuns); err != nil {
		return Inventory{}, fmt.Errorf("%w: AgentRuns: %v", ErrInventoryIncomplete, err)
	}
	if len(runList.Items) > limits.MaxRuns {
		return Inventory{}, fmt.Errorf("%w: AgentRuns exceeded bounded item limit", ErrInventoryIncomplete)
	}
	runs := make([]Run, 0, len(runList.Items))
	for index := range runList.Items {
		run, err := FromAgentRun(&runList.Items[index])
		if err != nil {
			return Inventory{}, fmt.Errorf("%w: AgentRun %s/%s: %v", ErrInventoryIncomplete, s.Namespace, runList.Items[index].Name, err)
		}
		runs = append(runs, run)
	}

	secretList, err := listMetadataBounded(ctx, s.Metadata, schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}, s.Namespace, limits.MaxResources)
	if err != nil {
		return Inventory{}, fmt.Errorf("%w: Secrets: %v", ErrInventoryIncomplete, err)
	}
	if len(secretList.Items) > limits.MaxResources {
		return Inventory{}, fmt.Errorf("%w: Secrets exceeded bounded item limit", ErrInventoryIncomplete)
	}
	pvcList, err := listMetadataBounded(ctx, s.Metadata, schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}, s.Namespace, limits.MaxResources)
	if err != nil {
		return Inventory{}, fmt.Errorf("%w: PersistentVolumeClaims: %v", ErrInventoryIncomplete, err)
	}
	if len(pvcList.Items) > limits.MaxResources {
		return Inventory{}, fmt.Errorf("%w: PersistentVolumeClaims exceeded bounded item limit", ErrInventoryIncomplete)
	}

	resources := make([]Resource, 0, len(secretList.Items)+len(pvcList.Items))
	for index := range secretList.Items {
		resources = append(resources, resourceFromObject(&secretList.Items[index], SecretKind))
	}
	for index := range pvcList.Items {
		resources = append(resources, resourceFromObject(&pvcList.Items[index], PersistentVolumeKind))
	}
	// The upstream Sandbox controller may not copy the full digest annotation
	// to a generated PVC. In that case the immutable status child reference is
	// the second half of the fence: owner name+UID+role and child spec digest
	// must all match the run being considered.
	for index := range resources {
		if resources[index].Kind == PersistentVolumeKind && resources[index].SpecDigest == "" {
			resources[index].SpecDigest = pvcChildSpecDigest(resources[index], runs)
		}
	}

	objects, complete, err := s.Objects.List(ctx, strings.TrimSuffix(s.ObjectPrefix, "/")+"/", limits.MaxObjects)
	if err != nil {
		return Inventory{}, fmt.Errorf("%w: object store: %v", ErrInventoryUnavailable, err)
	}
	if !complete {
		return Inventory{}, fmt.Errorf("%w: object store returned a bounded partial page", ErrInventoryIncomplete)
	}
	if len(objects) > limits.MaxObjects {
		return Inventory{}, fmt.Errorf("%w: object store exceeded bounded item limit", ErrInventoryIncomplete)
	}
	artifacts := make([]Artifact, 0, len(objects))
	ledger := make([]LedgerObject, 0)
	seenKeys := make(map[string]struct{}, len(objects))
	bodyReader, hasBodyReader := s.Objects.(ObjectBodyReader)
	for _, object := range objects {
		if _, exists := seenKeys[object.Key]; exists {
			return Inventory{}, fmt.Errorf("%w: duplicate object-store key %q", ErrInventoryIncomplete, object.Key)
		}
		if validateObjectKey(object.Key, false) != nil || !strings.HasPrefix(object.Key, strings.TrimSuffix(s.ObjectPrefix, "/")+"/") {
			return Inventory{}, fmt.Errorf("%w: object-store key is outside the configured prefix: %q", ErrInventoryIncomplete, object.Key)
		}
		seenKeys[object.Key] = struct{}{}
		if isLedgerObjectKey(s.ObjectPrefix, ledgerPrefix, object.Key) {
			if !hasBodyReader {
				return Inventory{}, fmt.Errorf("%w: ledger object store does not support bounded body reads", ErrInventoryIncomplete)
			}
			body, err := bodyReader.Get(ctx, object.Key)
			if err != nil {
				return Inventory{}, fmt.Errorf("%w: ledger object %q: %v", ErrInventoryIncomplete, object.Key, err)
			}
			if len(body) == 0 || int64(len(body)) > limits.MaxLedgerBodyBytes {
				return Inventory{}, fmt.Errorf("%w: ledger object %q exceeds bounded body limit", ErrInventoryIncomplete, object.Key)
			}
			ledger = append(ledger, LedgerObject{Key: object.Key, ETag: object.ETag, Body: append([]byte(nil), body...), SizeBytes: int64(len(body))})
			continue
		}
		if strings.HasPrefix(object.Key, strings.TrimSuffix(s.ObjectPrefix, "/")+"/runs/") && runUIDFromObjectKey(s.ObjectPrefix, object.Key) == "" {
			return Inventory{}, fmt.Errorf("%w: run-prefix object has no provable run identity: %q", ErrInventoryIncomplete, object.Key)
		}
		artifacts = append(artifacts, Artifact{
			RunUID:    runUIDFromObjectKey(s.ObjectPrefix, object.Key),
			Key:       object.Key,
			ETag:      object.ETag,
			CreatedAt: object.LastModified.UTC(),
			SizeBytes: object.SizeBytes,
		})
	}
	return Inventory{Runs: runs, Resources: resources, Artifacts: artifacts, Ledger: ledger}, nil
}

func listBounded(ctx context.Context, reader KubernetesInventoryReader, list client.ObjectList, namespace string, limit int) error {
	if limit <= 0 {
		return ErrInvalidInventoryConfig
	}
	if err := reader.List(ctx, list, &client.ListOptions{Namespace: namespace, Limit: int64(limit)}); err != nil {
		return err
	}
	meta, ok := list.(metav1.ListInterface)
	if !ok {
		return errors.New("list does not expose continuation metadata")
	}
	if meta.GetContinue() != "" {
		return ErrInventoryIncomplete
	}
	if remaining := meta.GetRemainingItemCount(); remaining != nil && *remaining > 0 {
		return ErrInventoryIncomplete
	}
	return nil
}

func listMetadataBounded(ctx context.Context, reader KubernetesMetadataReader, resource schema.GroupVersionResource, namespace string, limit int) (*metav1.PartialObjectMetadataList, error) {
	if limit <= 0 {
		return nil, ErrInvalidInventoryConfig
	}
	list, err := reader.ListMetadata(ctx, resource, namespace, limit)
	if err != nil {
		return nil, err
	}
	if list == nil {
		return nil, errors.New("metadata list is nil")
	}
	if list.GetContinue() != "" {
		return nil, ErrInventoryIncomplete
	}
	if remaining := list.GetRemainingItemCount(); remaining != nil && *remaining > 0 {
		return nil, ErrInventoryIncomplete
	}
	return list, nil
}

func resourceFromObject(object metav1.Object, kind string) Resource {
	labels := copyMap(object.GetLabels())
	annotations := copyMap(object.GetAnnotations())
	deleting := object.GetDeletionTimestamp() != nil && !object.GetDeletionTimestamp().IsZero()
	return Resource{
		Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()),
		SpecDigest: annotations[SpecDigestAnnotation], Labels: labels, Annotations: annotations,
		OwnerReferences: append([]OwnerReference(nil), object.GetOwnerReferences()...),
		Deleting:        deleting,
	}
}

func pvcChildSpecDigest(resource Resource, runs []Run) string {
	labelUID := resource.Labels[RunUIDLabelKey]
	if labelUID == "" {
		return ""
	}
	owner, err := controllerOwner(resource.OwnerReferences)
	if err != nil {
		return ""
	}
	for _, run := range runs {
		if run.UID != labelUID {
			continue
		}
		child, ok := workSandbox(run)
		if !ok || child.Name != owner.Name || child.UID != string(owner.UID) || child.SpecDigest == "" {
			continue
		}
		return child.SpecDigest
	}
	return ""
}

func runUIDFromObjectKey(prefix, key string) string {
	root := strings.TrimSuffix(prefix, "/") + "/runs/"
	if !strings.HasPrefix(key, root) {
		return ""
	}
	rest := strings.TrimPrefix(key, root)
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return ""
	}
	uid := rest[:slash]
	if !validUID(uid) {
		return ""
	}
	return uid
}

func copyMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
