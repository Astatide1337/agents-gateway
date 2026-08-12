package criticworkload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

// CanonicalRecordBytes serializes controller-authored output metadata. The
// record is intentionally separate from the critic's CorroborationInput.
func CanonicalRecordBytes(record OutputRecord) ([]byte, error) {
	if err := validateRecordShape(record); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal record: %v", ErrInvalidRecord, err)
	}
	body, err := strictjson.Normalize(raw)
	if err != nil || len(body) > MaxRecordBytes || strictjson.ValidateObject(body) != nil {
		return nil, fmt.Errorf("%w: record exceeds canonical bound", ErrInvalidRecord)
	}
	return body, nil
}

// ParseCanonicalRecord strictly decodes an output record. It is intentionally
// called only after the source has authenticated the Kubernetes Job/Pod
// identity; parsing JSON is not producer authentication.
func ParseCanonicalRecord(body []byte) (OutputRecord, error) {
	var zero OutputRecord
	if len(body) == 0 || len(body) > MaxRecordBytes || strictjson.ValidateObject(body) != nil {
		return zero, ErrInvalidRecord
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var record OutputRecord
	if err := decoder.Decode(&record); err != nil {
		return zero, fmt.Errorf("%w: decode record: %v", ErrInvalidRecord, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return zero, ErrNonCanonical
	}
	canonicalBody, err := CanonicalRecordBytes(record)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonicalBody, body) {
		return zero, ErrNonCanonical
	}
	return record, nil
}

func validateRecordShape(record OutputRecord) error {
	if record.SchemaVersion != RecordSchemaVersion {
		return fmt.Errorf("%w: unsupported schemaVersion", ErrInvalidRecord)
	}
	if err := ValidateInput(record.Input); err != nil {
		return fmt.Errorf("%w: input: %v", ErrInvalidRecord, err)
	}
	inputDigest, err := InputDigest(record.Input)
	if err != nil || record.InputDigest != inputDigest {
		return fmt.Errorf("%w: input digest mismatch", ErrInvalidRecord)
	}
	bindingDigest, err := BindingDigest(record.Input)
	if err != nil || record.BindingDigest != bindingDigest {
		return fmt.Errorf("%w: binding digest mismatch", ErrInvalidRecord)
	}
	if !validPathSegment(record.Job.Namespace) || !validPathSegment(record.Job.Name) || !validPathSegment(record.Job.UID) || !validPathSegment(record.Pod.Namespace) || !validPathSegment(record.Pod.Name) || !validPathSegment(record.Pod.UID) {
		return fmt.Errorf("%w: Job or Pod identity is invalid", ErrInvalidRecord)
	}
	if record.Object.Key == "" || !safeURI(record.Object.URI) || !canonical.ValidDigest(record.Object.Digest) || record.Object.SizeBytes <= 0 || record.Object.SizeBytes > MaxOutputBytes || record.Object.Digest != record.Artifact.Digest || record.Object.URI != record.Artifact.URI || record.Object.SizeBytes != record.Artifact.SizeBytes {
		return fmt.Errorf("%w: object identity is invalid", ErrInvalidRecord)
	}
	if record.Artifact.Kind != InputKind || record.Artifact.Name != InputName || record.Artifact.MediaType != InputMediaType {
		return fmt.Errorf("%w: artifact type is invalid", ErrInvalidRecord)
	}
	return nil
}

// ValidateRecordForOutput verifies the record against the exact object ref,
// logical object key, Job, and Pod that an authenticated source observed.
func ValidateRecordForOutput(record OutputRecord, ref v1alpha1.ArtifactRef, key string, job JobIdentity, pod PodIdentity, maxBytes int64) error {
	if maxBytes <= 0 || maxBytes > MaxOutputBytes {
		return fmt.Errorf("%w: output bound is invalid", ErrInvalidRecord)
	}
	if err := validateRecordShape(record); err != nil {
		return err
	}
	if ref.Kind != InputKind || ref.Name != InputName || ref.MediaType != InputMediaType || ref.Digest != record.Artifact.Digest || ref.URI != record.Artifact.URI || ref.SizeBytes != record.Artifact.SizeBytes || ref.SizeBytes <= 0 || ref.SizeBytes > maxBytes {
		return ErrArtifactConflict
	}
	if record.Object.Key != key || record.Job != job || record.Pod != pod {
		return ErrOutputIdentity
	}
	if record.Input.Run.Namespace != job.Namespace || record.Input.Run.UID != stringsBeforeKey(key, "/critic/") {
		return ErrBindingConflict
	}
	return nil
}

// stringsBeforeKey extracts the run UID from the fixed logical output key.
// It is kept deliberately strict so a source cannot use a path traversal or a
// similarly named object as a producer binding.
func stringsBeforeKey(key, marker string) string {
	const prefix = "runs/"
	if len(key) <= len(prefix) || !bytes.HasPrefix([]byte(key), []byte(prefix)) {
		return ""
	}
	remainder := key[len(prefix):]
	index := bytes.Index([]byte(remainder), []byte(marker))
	if index <= 0 {
		return ""
	}
	return remainder[:index]
}
