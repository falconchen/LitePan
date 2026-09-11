package dav

import (
	"context"
	"os"

	"golang.org/x/net/webdav"

	"litepan/internal/driver"
)

// 注意：WebDAV 的 PUT 由 Server.servePut 直接处理（见 server.go 的方法分发），
// 不会走到这里。本文件这条路径留给 webdav.Handler 内部可能触发的写入。
// 大文件转异步上传的逻辑只在 servePut 里实现，别在这边重复一份。

func (fs *FileSystem) openUpload(ctx context.Context, name string, flag int) (webdav.File, error) {
	exclusive := flag&os.O_EXCL != 0
	plan, err := fs.planUpload(ctx, name, exclusive)
	if err != nil {
		return nil, err
	}
	if plan.noop {
		return &noopUpload{}, nil
	}
	tmp, tmpPath, _, release, err := fs.createWebDAVTempFile(plan.fileName)
	if err != nil {
		return nil, err
	}
	return &uploadHandle{
		fs:        fs,
		ctx:       ctx,
		accountID: plan.accountID,
		parentID:  plan.parentID,
		fileName:  plan.fileName,
		tmpPath:   tmpPath,
		file:      tmp,
		release:   release,
	}, nil
}

func (fs *FileSystem) createWebDAVTempFile(fileName string) (*os.File, string, func(), func(), error) {
	return createWebDAVTempFile(fs.dataDir, fileName, fs.tempRegistry)
}

func (u *uploadHandle) Close() error {
	if u.closed {
		return nil
	}
	u.closed = true
	if u.file != nil {
		_ = u.file.Close()
	}
	if u.release != nil {
		defer u.release()
	}
	req := driver.LocalUploadRequest{
		LocalPath:      u.tmpPath,
		FileName:       u.fileName,
		ParentID:       u.parentID,
		ConflictPolicy: "overwrite",
	}
	if times, ok := uploadTimesFromContext(u.ctx); ok {
		req.ModTime = times.ModTime
		req.CreateTime = times.CreateTime
	}
	_, err := u.fs.files.UploadLocal(u.ctx, u.accountID, req)
	return err
}
