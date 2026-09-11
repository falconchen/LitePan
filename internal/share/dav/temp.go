package dav

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"litepan/internal/upload"
)

// createWebDAVTempFile 返回临时文件、路径、以及两个收尾函数：
//
//	untrack  仅把路径从 TempRegistry 摘掉，文件保留
//	release  摘掉登记并删除文件
//
// 之所以要分开：临时文件若交给上传任务队列接管，删除的时机由任务决定
// （上传成功后按 CleanupLocalMode 处理），这里只能取消登记，不能删。
func createWebDAVTempFile(dataDir, fileName string, registry *upload.TempRegistry) (*os.File, string, func(), func(), error) {
	dir := upload.TempDir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", nil, nil, err
	}
	safeName := filepath.Base(fileName)
	if safeName == "" || safeName == "." {
		safeName = "upload.bin"
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, "", nil, nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("webdav_%s_%s", hex.EncodeToString(id[:]), safeName))
	f, err := os.Create(path)
	if err != nil {
		return nil, "", nil, nil, err
	}
	untrack := func() {}
	if registry != nil {
		untrack = registry.Track(path)
	}
	release := func() {
		untrack()
		_ = os.Remove(path)
	}
	return f, path, untrack, release, nil
}
