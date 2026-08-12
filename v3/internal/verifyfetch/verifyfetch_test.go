package verifyfetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestParseS3URIRequiresCanonicalObjectLocation(t *testing.T) {
	valid, err := ParseS3URI("s3://agw-artifacts/runs/run-1/patch.diff")
	if err != nil || valid.Bucket != "agw-artifacts" || valid.Key != "runs/run-1/patch.diff" {
		t.Fatalf("valid location=%#v err=%v", valid, err)
	}
	for _, raw := range []string{
		"https://objects.example/runs/run/patch.diff",
		"s3://agw-artifacts/runs/run/patch.diff?signature=secret",
		"s3://user:pass@agw-artifacts/runs/run/patch.diff",
		"s3://agw-artifacts/runs/../patch.diff",
		"s3://agw-artifacts/runs/%2e%2e/patch.diff",
		"s3://127.0.0.1/runs/run/patch.diff",
		"s3://agw-artifacts/runs//patch.diff",
	} {
		if _, err := ParseS3URI(raw); err == nil {
			t.Fatalf("ParseS3URI(%q) unexpectedly accepted", raw)
		}
	}
}

func TestValidateStoreConfigRejectsUnsafeEndpointsAndBounds(t *testing.T) {
	base := StoreConfig{Endpoint: "https://objects.example", Region: "us-east-1", Bucket: "agw-artifacts", MaxObjectBytes: objectstore.GeneralMaxObjectBytes}
	if err := ValidateStoreConfig(base); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"http://objects.example",
		"https://127.0.0.1",
		"https://[::1]",
		"https://objects.example/path",
		"https://user:pass@objects.example",
		"https://objects.example/?token=secret",
	} {
		candidate := base
		candidate.Endpoint = endpoint
		if err := ValidateStoreConfig(candidate); err == nil {
			t.Fatalf("unsafe endpoint %q unexpectedly accepted", endpoint)
		}
	}
	for _, max := range []int64{0, -1, PatchFetchMaxObjectBytes + 1} {
		candidate := base
		candidate.MaxObjectBytes = max
		if max == 0 {
			candidate.MaxObjectBytes = 0
		}
		if err := ValidateStoreConfig(candidate); max == 0 && err != nil {
			t.Fatalf("zero max should select default: %v", err)
		} else if max != 0 && err == nil {
			t.Fatalf("max %d unexpectedly accepted", max)
		}
	}
}

func TestPatchFetchLimitCanConstructGetterAtBoundary(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	getter, err := NewS3Getter(context.Background(), StoreConfig{
		Endpoint:       "https://objects.example",
		Region:         "us-east-1",
		Bucket:         "agw-artifacts",
		MaxObjectBytes: PatchFetchMaxObjectBytes,
	}, Credentials{AccessKeyID: "access", SecretAccessKey: "secret"})
	if err != nil {
		t.Fatalf("NewS3Getter() rejected the documented patch-fetch boundary: %v", err)
	}
	if getter == nil || getter.max != PatchFetchMaxObjectBytes {
		t.Fatalf("getter limit=%v, want %d", getter, PatchFetchMaxObjectBytes)
	}
}

func TestValidateCredentialsRejectsAmbiguousValues(t *testing.T) {
	valid := Credentials{AccessKeyID: "access", SecretAccessKey: "secret"}
	if err := ValidateCredentials(valid); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []Credentials{
		{},
		{AccessKeyID: "access\n", SecretAccessKey: "secret"},
		{AccessKeyID: "access", SecretAccessKey: " secret"},
		{AccessKeyID: "access", SecretAccessKey: "secret", SessionToken: strings.Repeat("x", MaxSessionTokenBytes+1)},
	} {
		if err := ValidateCredentials(candidate); !errors.Is(err, ErrInvalidSecret) {
			t.Fatalf("credentials=%#v err=%v, want ErrInvalidSecret", candidate, err)
		}
	}
}

func TestReadResponseEnforcesDeclaredAndActualLength(t *testing.T) {
	makeOutput := func(declared int64, body string) *s3.GetObjectOutput {
		return &s3.GetObjectOutput{ContentLength: aws.Int64(declared), Body: io.NopCloser(bytes.NewBufferString(body))}
	}
	data, err := readResponse(makeOutput(5, "hello"), 5, 10)
	if err != nil || string(data) != "hello" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	for _, output := range []*s3.GetObjectOutput{
		makeOutput(4, "hello"),
		makeOutput(5, "hell"),
		makeOutput(5, "hello!"),
		makeOutput(11, strings.Repeat("x", 11)),
		{Body: io.NopCloser(bytes.NewBufferString("hello"))},
	} {
		if _, err := readResponse(output, 5, 10); err == nil {
			t.Fatalf("output=%#v unexpectedly accepted", output)
		}
	}
}

func TestHTTPClientRejectsRedirects(t *testing.T) {
	client := safeHTTPClient()
	if client == nil || client.CheckRedirect == nil {
		t.Fatal("safe client does not install redirect policy")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error=%v", err)
	}
}
