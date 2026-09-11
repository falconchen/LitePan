package dav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"litepan/internal/domain"
	"litepan/internal/driver"
	"litepan/internal/upload"
)

type uploadPlan struct {
	accountID   int64
	accountName string
	driverType  string
	parentID    string
	fileName    string
	existed     bool
	noop        bool
}

func (fs *FileSystem) planUpload(ctx context.Context, webPath string, exclusive bool) (*uploadPlan, error) {
	parsed := ParseWebDAVPath(webPath)
	if parsed.AccountName == "" || len(parsed.RelParts) == 0 {
		return nil, os.ErrPermission
	}
	if isMacOSMetadataPath(append([]string{parsed.AccountName}, parsed.RelParts...)) {
		return &uploadPlan{noop: true}, nil
	}
	acc, err := fs.resolver.accountByName(ctx, parsed.AccountName)
	if err != nil {
		return nil, err
	}
	fileName := parsed.RelParts[len(parsed.RelParts)-1]
	parentParts := parsed.RelParts[:len(parsed.RelParts)-1]
	parentID := "0"
	if len(parentParts) > 0 {
		parentItem, _, err := fs.resolver.resolveUnderAccount(ctx, acc.ID, parentParts)
		if err != nil {
			return nil, err
		}
		if !parentItem.IsDir {
			return nil, os.ErrInvalid
		}
		parentID = parentItem.ID
	}
	existed := false
	if cur, _, err := fs.resolver.resolveUnderAccount(ctx, acc.ID, parsed.RelParts); err == nil {
		if cur.IsDir {
			return nil, errUploadToCollection
		}
		existed = true
		if exclusive {
			return nil, os.ErrExist
		}
	}
	return &uploadPlan{
		accountID:   acc.ID,
		accountName: acc.Name,
		driverType:  acc.DriverType,
		parentID:    parentID,
		fileName:    fileName,
		existed:     existed,
	}, nil
}

var errUploadToCollection = errors.New("cannot overwrite a collection with PUT")

func (s *Server) servePut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	webPath := resourcePath(r)
	exclusive := r.Header.Get("If-None-Match") == "*"

	plan, err := s.fs.planUpload(ctx, webPath, exclusive)
	if err != nil {
		writeUploadErr(w, err)
		return
	}
	if plan.noop {
		w.WriteHeader(http.StatusCreated)
		return
	}

	tmp, tmpPath, untrack, release, err := createWebDAVTempFile(s.fs.dataDir, plan.fileName, s.fs.tempRegistry)
	if err != nil {
		s.log.Warn("webdav put temp file", "path", webPath, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// 临时文件若被上传任务接管，删除时机就归任务管（上传成功后按
	// CleanupLocalFileOnSuccess 处理），这里只能取消登记不能删。
	handedOff := false
	defer func() {
		if handedOff {
			untrack()
			return
		}
		release()
	}()

	if _, err := io.Copy(tmp, r.Body); err != nil {
		_ = tmp.Close()
		s.log.Warn("webdav put read body", "path", webPath, "err", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := tmp.Close(); err != nil {
		s.log.Warn("webdav put close temp", "path", webPath, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		s.log.Warn("webdav put stat temp", "path", webPath, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	parsed := ParseWebDAVPath(webPath)
	if info.Size() == 0 {
		if _, staging := stripWebDAVStagingSuffix(plan.fileName); staging {
			s.fs.resolver.rememberFile(ctx, plan.accountID, parsed.RelParts, plan.parentID, domain.FileItem{
				Name: plan.fileName,
				Size: 0,
			})
		}
		if plan.existed {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
		return
	}

	// WebDAV 文件统一交给上传任务队列，并等待任务真正完成后再向客户端应答。
	// 等待使用请求 context，但任务使用 Manager 自己的 context；即使客户端断开，
	// 后台上传也会继续执行。
	if task, err := s.createUploadTask(plan, tmpPath, info.Size()); err != nil {
		s.log.Warn("webdav 创建上传任务失败", "file", plan.fileName, "err", err)
		http.Error(w, "Unable to create upload task", http.StatusInternalServerError)
		return
	} else if task != nil {
		handedOff = true
		finished, err := s.fs.uploads.Wait(ctx, task.TaskID)
		if err != nil {
			// 客户端断开只结束等待，不能影响已接管临时文件的后台任务。
			if ctx.Err() != nil {
				s.log.Info("webdav 客户端断开，上传任务继续后台执行",
					"file", plan.fileName, "task_id", task.TaskID)
				return
			}
			s.log.Warn("webdav 等待上传任务失败",
				"file", plan.fileName, "task_id", task.TaskID, "err", err)
			http.Error(w, "Upload task unavailable", http.StatusInternalServerError)
			return
		}
		if finished.Status != upload.StatusSuccess && finished.Status != upload.StatusSkipped {
			errMsg := finished.Error
			if errMsg == "" {
				errMsg = "Upload failed"
			}
			s.log.Warn("webdav 上传任务未成功",
				"file", plan.fileName, "task_id", task.TaskID,
				"status", finished.Status, "err", finished.Error)
			http.Error(w, errMsg, http.StatusBadGateway)
			return
		}

		item := fileItemFromUploadTask(finished, plan.fileName, info.Size())
		s.fs.resolver.rememberFile(ctx, plan.accountID, parsed.RelParts, plan.parentID, item)
		if item.ID != "" {
			w.Header().Set("ETag", stableFileETag(item))
		}
		if plan.existed {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
		return
	}

	req := driver.LocalUploadRequest{
		LocalPath:      tmpPath,
		FileName:       plan.fileName,
		ParentID:       plan.parentID,
		ConflictPolicy: "overwrite",
	}
	if times, ok := uploadTimesFromContext(ctx); ok {
		req.ModTime = times.ModTime
		req.CreateTime = times.CreateTime
	}
	result, err := s.fs.files.UploadLocal(ctx, plan.accountID, req)
	if err != nil {
		s.log.Warn("webdav put upload", "path", webPath, "account", plan.accountID, "err", err)
		writeUploadErr(w, err)
		return
	}
	item := domain.FileItem{
		ID:     result.FileID,
		Name:   result.FileName,
		Size:   result.Size,
		IDKind: domain.IDStable,
	}
	if times, ok := uploadTimesFromContext(ctx); ok && times.ModTime != nil && !times.ModTime.IsZero() {
		item.ModTime = *times.ModTime
	} else {
		item.ModTime = time.Now()
	}
	s.fs.resolver.rememberFile(ctx, plan.accountID, parsed.RelParts, plan.parentID, item)
	w.Header().Set("ETag", stableFileETag(item))
	if plan.existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func writeUploadErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errUploadToCollection) {
		http.Error(w, err.Error(), http.StatusMethodNotAllowed)
		return
	}
	if errors.Is(err, os.ErrPermission) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if errors.Is(err, os.ErrExist) {
		http.Error(w, "File already exists", http.StatusPreconditionFailed)
		return
	}
	if errors.Is(err, os.ErrInvalid) {
		http.Error(w, "Parent path is not a collection", http.StatusConflict)
		return
	}
	if ae, ok := domain.AsAppError(err); ok {
		http.Error(w, ae.Message, ae.HTTPStatus())
		return
	}
	if os.IsNotExist(err) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	http.Error(w, "Upload failed", http.StatusConflict)
}

// createUploadTask 把 WebDAV 已完整接收的临时文件交给任务队列。返回 nil task
// 表示当前 Server 没有配置上传管理器，调用方应兼容性地退回同步上传。
func (s *Server) createUploadTask(plan *uploadPlan, tmpPath string, size int64) (*upload.Task, error) {
	if s.fs == nil || s.fs.uploads == nil {
		return nil, nil
	}
	task, err := s.fs.uploads.CreateServerLocalTask(context.Background(), upload.ServerLocalCreateParams{
		AccountID:        plan.accountID,
		AccountName:      plan.accountName,
		DriverType:       plan.driverType,
		FileName:         plan.fileName,
		DisplayName:      plan.fileName,
		SourceType:       upload.SourceTypeWebDAV,
		TargetPath:       plan.parentID,
		LocalPath:        tmpPath,
		CleanupLocalMode: upload.CleanupLocalFileOnSuccess,
		CleanupLocalPath: tmpPath,
		TotalBytes:       size,
		ConflictPolicy:   "overwrite",
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("webdav 文件转入上传队列",
		"file", plan.fileName, "size", size, "account", plan.accountName,
		"task_id", task.TaskID)
	return task, nil
}

func fileItemFromUploadTask(task *upload.Task, fallbackName string, fallbackSize int64) domain.FileItem {
	item := domain.FileItem{
		Name:    fallbackName,
		Size:    fallbackSize,
		ModTime: time.Now(),
	}
	if task == nil || task.Result == nil {
		return item
	}
	if id, ok := task.Result["file_id"].(string); ok {
		item.ID = id
		item.IDKind = domain.IDStable
	}
	if name, ok := task.Result["file_name"].(string); ok && name != "" {
		item.Name = name
	}
	if size, ok := task.Result["size"].(int64); ok {
		item.Size = size
	}
	return item
}
