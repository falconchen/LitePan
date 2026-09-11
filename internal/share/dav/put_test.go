package dav

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"litepan/internal/upload"
)

func TestCreateUploadTaskQueuesSmallWebDAVFile(t *testing.T) {
	startupGate := make(chan struct{})
	manager := upload.NewManager(upload.Options{
		DataDir:     t.TempDir(),
		StartupGate: startupGate,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Stop(ctx); err != nil {
			t.Errorf("停止上传管理器: %v", err)
		}
	})

	localPath := filepath.Join(t.TempDir(), "small.bin")
	if err := os.WriteFile(localPath, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{fs: &FileSystem{uploads: manager}, log: slog.Default()}
	task, err := server.createUploadTask(&uploadPlan{
		accountID:   1,
		accountName: "测试账号",
		driverType:  "mock",
		parentID:    "0",
		fileName:    "small.bin",
	}, localPath, 5)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatal("小文件也应创建 WebDAV 上传任务")
	}
	if task.SourceType != upload.SourceTypeWebDAV {
		t.Fatalf("source_type = %q，期望 %q", task.SourceType, upload.SourceTypeWebDAV)
	}
}

func TestFileItemFromUploadTaskUsesCompletedResult(t *testing.T) {
	item := fileItemFromUploadTask(&upload.Task{Result: map[string]any{
		"file_id":   "remote-id",
		"file_name": "remote.bin",
		"size":      int64(12),
	}}, "fallback.bin", 5)
	if item.ID != "remote-id" || item.Name != "remote.bin" || item.Size != 12 {
		t.Fatalf("file item = %+v", item)
	}
}
