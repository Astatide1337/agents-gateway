package retention

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

// KubernetesEventSink turns bounded retention reason codes into Events on the
// corresponding AgentRun. A missing run is benign during namespace teardown;
// the structured controller log and metrics remain authoritative.
type KubernetesEventSink struct {
	Reader   client.Reader
	Recorder record.EventRecorder
}

func (s KubernetesEventSink) Record(ctx context.Context, run Run, reason, message string) {
	if s.Reader == nil || s.Recorder == nil || ctx == nil || run.Namespace == "" || run.Name == "" {
		return
	}
	object := &v1alpha1.AgentRun{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Name}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		return
	}
	if types.UID(run.UID) != object.UID {
		return
	}
	if len(reason) == 0 || len(reason) > 128 {
		return
	}
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		message = message[:512]
	}
	eventType := corev1.EventTypeNormal
	if strings.Contains(strings.ToLower(message), "missing") || strings.Contains(strings.ToLower(message), "conflict") || strings.Contains(strings.ToLower(message), "error") {
		eventType = corev1.EventTypeWarning
	}
	s.Recorder.Event(object, eventType, reason, message)
}

var _ EventSink = KubernetesEventSink{}
