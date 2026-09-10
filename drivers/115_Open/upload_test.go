package pan115open

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUploadThresholdAndPartSize(t *testing.T) {
	const mb = int64(1024 * 1024)
	if got := singlePartUploadLimit; got != 10*mb {
		t.Fatalf("singlePartUploadLimit = %d, want %d", got, 10*mb)
	}
	if shouldUseMultipart(10 * mb) {
		t.Fatal("10 MiB should use single-part upload")
	}
	if !shouldUseMultipart(10*mb + 1) {
		t.Fatal("a file larger than 10 MiB should use multipart upload")
	}
	if got := calculateOSSPartSize(10*mb + 1); got != 5*mb {
		t.Fatalf("calculateOSSPartSize = %d, want %d", got, 5*mb)
	}
	tooLarge := int64(defaultUploadPartSize)*maxOSSUploadParts + 1
	if got := calculateOSSPartSize(tooLarge); (tooLarge+got-1)/got > maxOSSUploadParts {
		t.Fatalf("part count exceeds OSS limit: size=%d partSize=%d", tooLarge, got)
	}
}

func TestOSSUploadPartRetriesOnce(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "part-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const content = "retry-body"
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}

	attempts := 0
	d := &Driver{uploadClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != content {
			t.Fatalf("attempt %d body = %q, want %q", attempts, body, content)
		}
		status := http.StatusInternalServerError
		header := make(http.Header)
		if attempts == 2 {
			status = http.StatusOK
			header.Set("ETag", `"etag-2"`)
		}
		return &http.Response{
			StatusCode: status,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}}
	token := ossTokenData{
		AccessKeyID: "id", AccessKeySecret: "secret", SecurityToken: "token", Endpoint: "https://oss.example.test",
	}
	etag, _, err := d.ossUploadPartWithRetry(context.Background(), token, "bucket", "object", "upload-id", 1, f, int64(len(content)), 0, int64(len(content)), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if etag != "etag-2" {
		t.Fatalf("etag = %q, want %q", etag, "etag-2")
	}
}
