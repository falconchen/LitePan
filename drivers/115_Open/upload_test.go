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

	// 不填任何配置时应当回落到默认值
	d := &Driver{}
	if got := d.singlePartLimit(); got != 10*mb {
		t.Fatalf("默认分片阈值 = %d, want %d", got, 10*mb)
	}
	if got := d.uploadPartSize(); got != 5*mb {
		t.Fatalf("默认分片大小 = %d, want %d", got, 5*mb)
	}
	if shouldUseMultipart(10*mb, d.singlePartLimit()) {
		t.Fatal("10 MiB 应当走单片上传")
	}
	if !shouldUseMultipart(10*mb+1, d.singlePartLimit()) {
		t.Fatal("超过 10 MiB 应当走分片上传")
	}

	// 用户自定义
	custom := &Driver{add: Addition{SinglePartLimitMB: "64", UploadPartSizeMB: "16"}}
	if got := custom.singlePartLimit(); got != 64*mb {
		t.Fatalf("自定义分片阈值 = %d, want %d", got, 64*mb)
	}
	if got := custom.uploadPartSize(); got != 16*mb {
		t.Fatalf("自定义分片大小 = %d, want %d", got, 16*mb)
	}

	// 非法输入回落默认值
	for _, bad := range []flexString{"", "0", "-3", "abc"} {
		bd := &Driver{add: Addition{SinglePartLimitMB: bad, UploadPartSizeMB: bad}}
		if got := bd.singlePartLimit(); got != 10*mb {
			t.Fatalf("输入 %q 时分片阈值 = %d, want 默认 %d", bad, got, 10*mb)
		}
		if got := bd.uploadPartSize(); got != 5*mb {
			t.Fatalf("输入 %q 时分片大小 = %d, want 默认 %d", bad, got, 5*mb)
		}
	}

	// 超出范围要夹到边界，不能直接透传给 OSS
	tooBig := &Driver{add: Addition{SinglePartLimitMB: "999999", UploadPartSizeMB: "999999"}}
	if got := tooBig.singlePartLimit(); got != int64(maxSinglePartLimitMB)*mb {
		t.Fatalf("过大的分片阈值 = %d, want 夹到 %d", got, int64(maxSinglePartLimitMB)*mb)
	}
	if got := tooBig.uploadPartSize(); got != int64(maxUploadPartSizeMB)*mb {
		t.Fatalf("过大的分片大小 = %d, want 夹到 %d", got, int64(maxUploadPartSizeMB)*mb)
	}

	// 期望片大小能用时原样返回
	if got := calculateOSSPartSize(10*mb+1, 5*mb); got != 5*mb {
		t.Fatalf("calculateOSSPartSize = %d, want %d", got, 5*mb)
	}
	// 片数超过 OSS 上限时必须自动放大片大小
	tooLarge := int64(defaultUploadPartSize)*maxOSSUploadParts + 1
	if got := calculateOSSPartSize(tooLarge, defaultUploadPartSize); (tooLarge+got-1)/got > maxOSSUploadParts {
		t.Fatalf("片数超出 OSS 上限: size=%d partSize=%d", tooLarge, got)
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
