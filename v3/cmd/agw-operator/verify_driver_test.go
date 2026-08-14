package main

import (
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVerifyPhaseAdapterFailsClosedWithoutDriver(t *testing.T) {
	adapter := verifyPhaseAdapter{store: objectstore.Config{Bucket: "agw-artifacts"}}
	if _, err := adapter.Verify(nil, nil, resolved.Snapshot{}); err == nil {
		t.Fatal("unconfigured verification adapter did not fail closed")
	}
}

func TestVerificationStartTimeUsesPersistedTimestamp(t *testing.T) {
	first := time.Date(2026, 8, 13, 0, 20, 0, 0, time.UTC)
	run := &v1alpha1.AgentRun{Status: v1alpha1.AgentRunStatus{
		VerificationStartedAt: func() *metav1.Time {
			value := metav1.NewTime(first)
			return &value
		}(),
	}}
	second := first.Add(10 * time.Minute)
	if got := verificationStartTime(run, func() time.Time { return second }); !got.Equal(first) {
		t.Fatalf("verification start = %s, want persisted %s", got, first)
	}
}
