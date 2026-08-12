package retention

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type conditionalCreateS3Fake struct {
	err error
}

func (f conditionalCreateS3Fake) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return nil, nil
}

func (f conditionalCreateS3Fake) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return nil, f.err
}

func (f conditionalCreateS3Fake) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, nil
}

func (f conditionalCreateS3Fake) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return nil, nil
}

func TestS3ConditionalCreateConflictIsNotTreatedAsProvenExisting(t *testing.T) {
	apiErr := &smithy.GenericAPIError{Code: "ConditionalRequestConflict", Message: "concurrent request", Fault: smithy.FaultServer}
	storeErr := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusConflict}},
		Err:      apiErr,
	}
	store, err := NewS3InventoryWithClient(S3Config{Bucket: "agw-retention", Prefix: "agents-gateway/v3"}, conditionalCreateS3Fake{err: storeErr})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(context.Background(), "agents-gateway/v3/publish-effects/tombstones/"+testEffectHex+".json", []byte(`{"state":"succeeded"}`), "application/json")
	if err == nil || created {
		t.Fatalf("conditional conflict create = created %t, error %v; ambiguous 409 must not be treated as an existing fence", created, err)
	}
}

const testEffectHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
