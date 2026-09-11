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

	// 大文件转交上传任务队列，立刻应答，避免客户端等不住而取消请求。
	if s.enqueueUpload(plan, tmpPath, info.Size()) {
		handedOff = true
		// 让客户端随后的 PROPFIND 能看到这个文件；此刻它还在队列里，
		// 上传失败的话缓存会在下次刷新时被纠正。
		s.fs.resolver.rememberFile(ctx, plan.accountID, parsed.RelParts, plan.parentID, domain.FileItem{
			Name:    plan.fileName,
			Size:    info.Size(),
			ModTime: time.Now(),
		})
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

// asyncUploadThreshold 决定 WebDAV 上传走同步还是任务队列。
//
// WebDAV 的 PUT 语义是"返回 201 即表示已存好"，所以默认必须同步——客户端要
// 依据响应判断成败。但同步意味着整个 PUT 请求要挂到网盘上传结束为止：慢上行
// 链路下一个几百 MB 的文件要传好几分钟，客户端（或中间的反代）往往先超时
// 断开，请求 context 随之取消，连带把正在进行的网盘上传也掐断，白传。
//
// 超过该阈值的文件因此改为交给上传任务队列：立刻应答，上传在后台进行，可在
// 管理页看到进度，并获得重试与断点续传。代价是这时的 201 只表示"已接收并
// 排队"，真正失败客户端不会知道——所以阈值取得较大，只让那些同步几乎必然被
// 客户端超时掐断的大文件走这条路。
const asyncUploadThreshold = 64 << 20 // 64 MiB

// enqueueUpload 尝试把这次上传交给任务队列。返回 true 表示已接管，调用方应当
// 直接应答成功，并且不要删除临时文件。
//
// 任何一步不满足都返回 false 退回同步上传：同步总能给出确定的结果，比让
// 客户端收到一个失败的 PUT 要好。
func (s *Server) enqueueUpload(plan *uploadPlan, tmpPath string, size int64) bool {
	if s.fs == nil || s.fs.uploads == nil || size <= asyncUploadThreshold {
		return false
	}
	// 这里刻意用 context.Background() 而不是请求的 ctx：后者在客户端断开时
	// 会被取消，而建任务这一步不该受此影响。任务本身由 Manager 用自己的根
	// context 执行，同样不受请求生命周期约束。
	if _, err := s.fs.uploads.CreateServerLocalTask(context.Background(), upload.ServerLocalCreateParams{
		AccountID:        plan.accountID,
		AccountName:      plan.accountName,
		DriverType:       plan.driverType,
		FileName:         plan.fileName,
		DisplayName:      plan.fileName,
		TargetPath:       plan.parentID,
		LocalPath:        tmpPath,
		CleanupLocalMode: upload.CleanupLocalFileOnSuccess,
		CleanupLocalPath: tmpPath,
		TotalBytes:       size,
		ConflictPolicy:   "overwrite",
	}); err != nil {
		s.log.Warn("webdav 转上传任务失败，退回同步上传",
			"file", plan.fileName, "err", err)
		return false
	}
	s.log.Info("webdav 大文件转入上传队列",
		"file", plan.fileName, "size", size, "account", plan.accountName)
	return true
}
