package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/capturecontroller"
	agwcontroller "github.com/Astatide1337/agents-gateway/v3/internal/controller"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

type capturePhaseAdapter struct {
	driver *capturecontroller.Driver
	image  string
}

func (a capturePhaseAdapter) Capture(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (agwcontroller.CaptureOutcome, error) {
	plan, err := a.plan(run, snapshot)
	if err != nil {
		return agwcontroller.CaptureOutcome{}, err
	}
	if _, err := a.driver.Ensure(ctx, plan); err != nil {
		return agwcontroller.CaptureOutcome{}, err
	}
	result, err := a.driver.Capture(ctx, plan, snapshot, run.Status.BaseSHA)
	if err != nil {
		switch {
		case errors.Is(err, capturecontroller.ErrRunning), errors.Is(err, capturecontroller.ErrMissing):
			return agwcontroller.CaptureOutcome{Pending: true}, nil
		case errors.Is(err, capturecontroller.ErrFailed):
			return agwcontroller.CaptureOutcome{Failed: true, Reason: "networkless capture Job failed"}, nil
		default:
			return agwcontroller.CaptureOutcome{}, err
		}
	}
	manifestRef := result.Validated.ManifestArtifact()
	if manifestRef == nil || manifestRef.Digest != result.ManifestArtifact.Digest || manifestRef.URI != result.ManifestArtifact.URI {
		return agwcontroller.CaptureOutcome{}, errors.New("capture result is missing its immutable manifest reference")
	}
	return agwcontroller.CaptureOutcome{Artifact: result.Artifact, ManifestArtifact: result.ManifestArtifact, Validated: result.Validated}, nil
}

func (a capturePhaseAdapter) Cleanup(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) error {
	plan, err := a.plan(run, snapshot)
	if err != nil {
		// No capture resource can exist before the work child reference exists.
		if run.Status.WorkSandboxRef == nil {
			return nil
		}
		return err
	}
	return a.driver.Cleanup(ctx, plan)
}

func (a capturePhaseAdapter) plan(run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (capture.Plan, error) {
	if a.driver == nil || run == nil || run.Status.WorkSandboxRef == nil || run.Status.BaseSHA == "" {
		return capture.Plan{}, errors.New("capture phase is missing its work Sandbox identity")
	}
	claim, err := workload.WorkspaceClaimName(run.Status.WorkSandboxRef.Name)
	if err != nil {
		return capture.Plan{}, err
	}
	return capture.Build(snapshot, run.Status.BaseSHA, capture.Options{
		Image: a.image, WorkspaceClaimName: claim, MaxPatchBytes: snapshot.Spec.Limits.MaxPatchBytes,
	})
}

type podLogReader struct{ client kubernetes.Interface }

func (r podLogReader) ReadCaptureLogs(ctx context.Context, namespace, podName, containerName string, maxBytes int64) ([]byte, error) {
	if r.client == nil || maxBytes <= 0 || namespace == "" || podName == "" || containerName != capturecontroller.CaptureContainer {
		return nil, errors.New("capture log request is invalid")
	}
	stream, err := r.client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName, Follow: false, Timestamps: false,
	}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open bounded capture logs: %w", err)
	}
	defer stream.Close()
	body, err := io.ReadAll(io.LimitReader(stream, maxBytes+1))
	if err != nil {
		return nil, errors.New("read bounded capture logs")
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("capture logs exceeded the configured bound")
	}
	return body, nil
}
