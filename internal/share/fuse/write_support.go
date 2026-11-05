//go:build fuse

package fuse

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"litepan/internal/domain"
	"litepan/internal/upload"
)

type stagingFile struct {
	fs.Inode
	b        *backend
	name     string
	tempPath string
	size     int64
	modTime  time.Time
	mu       sync.RWMutex
	// opMu 串行化"投递上传任务""改名""删除""上传完成收尾"四类状态迁移，
	// 保证改名要么发生在投递前（任务直接用新名），要么能看到 taskID 并联动任务。
	// 持有 opMu 时不得向内核发通知（NotifyEntry 等），否则可能与内核目录锁互等。
	opMu sync.Mutex
	// parentID/displayPath 记录上传目标目录，rename 跨目录时同步更新，投递任务时以最新值为准。
	parentID    string
	displayPath string
	// uploads 与 taskID 在 Release 投递上传任务后写入，供改名/删除时联动任务。
	uploads UploadManager
	taskID  string
	// removed：已被 unlink；dropped：未能投递上传，临时文件已清理；
	// uploaded：上传成功，节点已让位给网盘文件，remote 记录对应的网盘条目。
	removed  bool
	dropped  bool
	uploaded bool
	remote   domain.FileItem
}

var (
	_ fs.NodeGetattrer = (*stagingFile)(nil)
	_ fs.NodeOpener    = (*stagingFile)(nil)
	_ fs.FileWriter    = (*stagingUploadHandle)(nil)
	_ fs.FileFlusher   = (*stagingUploadHandle)(nil)
	_ fs.FileFsyncer   = (*stagingUploadHandle)(nil)
	_ fs.FileReleaser  = (*stagingUploadHandle)(nil)
	_ fs.FileReader    = (*localStagingReadHandle)(nil)
	_ fs.FileReleaser  = (*localStagingReadHandle)(nil)
	_ fs.FileReader    = (*stagingRemoteReadHandle)(nil)
	_ fs.FileReleaser  = (*stagingRemoteReadHandle)(nil)
)

func (d *cloudDir) Create(ctx context.Context, name string, flags uint32, _ uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	name, errno := normalizeChildName(name)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	if d.b.deps.Uploads == nil {
		return nil, nil, 0, syscall.EROFS
	}
	if _, found, errno := d.lookupChildItemIfExists(ctx, name); errno != 0 {
		return nil, nil, 0, errno
	} else if found {
		return nil, nil, 0, syscall.EEXIST
	}
	tmp, tmpPath, cleanup, untrack, err := createFuseTempFile(d.b.deps.Uploads, name)
	if err != nil {
		return nil, nil, 0, errnoFor(err)
	}
	node := &stagingFile{
		b:           d.b,
		name:        name,
		tempPath:    tmpPath,
		modTime:     time.Now(),
		parentID:    d.item.ID,
		displayPath: d.targetDisplayPath(),
	}
	child := d.NewPersistentInode(ctx, node, fs.StableAttr{
		Ino:  fileIno(d.b.accountID(), "pending:"+tmpPath),
		Mode: fuse.S_IFREG,
	})
	if child == nil {
		cleanup()
		_ = tmp.Close()
		return nil, nil, 0, syscall.EIO
	}
	fillEntryAttr(out, d.b, domain.FileItem{Name: name, Size: 0, ModTime: node.modTime})
	return child, &stagingUploadHandle{
		node:              node,
		uploads:           d.b.deps.Uploads,
		accountID:         d.b.accountID(),
		parentID:          d.item.ID,
		targetDisplayPath: d.targetDisplayPath(),
		fileName:          name,
		tempPath:          tmpPath,
		file:              tmp,
		cleanup:           cleanup,
		untrack:           untrack,
		flags:             flags,
	}, 0, 0
}

func (d *cloudDir) targetDisplayPath() string {
	return strings.Trim(d.Path(nil), "/")
}

func (f *stagingFile) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.mu.RLock()
	item := domain.FileItem{
		Name:    f.name,
		Size:    f.size,
		ModTime: f.modTime,
	}
	f.mu.RUnlock()
	fillFileAttr(&out.Attr, f.b, item)
	return 0
}

func (f *stagingFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	f.mu.RLock()
	tempPath := f.tempPath
	f.mu.RUnlock()
	if tempPath != "" {
		file, err := os.Open(tempPath)
		if err == nil {
			return &localStagingReadHandle{file: file}, 0, 0
		}
		if !os.IsNotExist(err) {
			return nil, 0, syscall.EIO
		}
	}
	// 临时文件已不在：上传成功后被上传任务清理。内核可能仍缓存着这个 staging
	// inode（目录项缓存未过期或通知尚未生效），此时回退为按网盘文件打开。
	item, ok := f.resolveRemoteItem(ctx)
	if !ok {
		return nil, 0, syscall.ENOENT
	}
	return openStagingRemote(ctx, f.b, item, flags)
}

// superseded 表示该 staging 节点已不再代表本地待上传内容：已上传成功、
// 已删除、投递失败，或上传任务已结束且临时文件被清理。此时应以网盘为准。
func (f *stagingFile) superseded() bool {
	f.mu.RLock()
	done := f.uploaded || f.removed || f.dropped
	taskID := f.taskID
	tempPath := f.tempPath
	f.mu.RUnlock()
	if done {
		return true
	}
	if taskID == "" || tempPath == "" {
		return false
	}
	_, err := os.Stat(tempPath)
	return os.IsNotExist(err)
}

// resolveRemoteItem 找到 staging 文件上传后对应的网盘条目：优先用上传结果，
// 其次（例如任务失败后被手动重试成功）按当前名字到目标目录里查。
func (f *stagingFile) resolveRemoteItem(ctx context.Context) (domain.FileItem, bool) {
	f.mu.RLock()
	remote := f.remote
	taskID := f.taskID
	removed := f.removed
	name := f.name
	parentID := f.parentID
	f.mu.RUnlock()
	if remote.ID != "" {
		return remote, true
	}
	if removed || taskID == "" || f.b == nil || f.b.deps.Files == nil {
		return domain.FileItem{}, false
	}
	items, err := f.b.deps.Files.List(ctx, f.b.accountID(), parentID, false)
	if err != nil {
		return domain.FileItem{}, false
	}
	for _, it := range items {
		if it.Name == name && !it.IsDir {
			f.mu.Lock()
			f.remote = it
			f.mu.Unlock()
			return it, true
		}
	}
	return domain.FileItem{}, false
}

// watchUpload 等待上传任务结束；成功后让 staging 节点让位给网盘文件。
// 任务失败、暂停或被删除时直接返回：临时文件仍在（失败不清理），本地读不受影响；
// 之后若任务被手动重试成功，由 Lookup/Open 中的 superseded 检查兜底。
func (f *stagingFile) watchUpload(uploads UploadManager, taskID string) {
	task, err := uploads.Wait(context.Background(), taskID)
	if err != nil || task == nil {
		return
	}
	if task.Status != upload.StatusSuccess && task.Status != upload.StatusSkipped {
		return
	}
	f.finishUpload(task)
}

// finishUpload 记录网盘条目并把 staging 节点从父目录摘除，再通知内核丢弃
// 对应目录项，使下一次 Lookup 走网盘列表得到 cloudFile。
func (f *stagingFile) finishUpload(task *upload.Task) {
	if task == nil {
		return
	}
	f.opMu.Lock()
	f.mu.Lock()
	if f.removed || f.uploaded || task.TaskID == "" || task.TaskID != f.taskID {
		f.mu.Unlock()
		f.opMu.Unlock()
		return
	}
	f.uploaded = true
	f.remote = remoteItemFromTask(task, f.name, f.size)
	nodeName := f.name
	nodeParentID := f.parentID
	f.mu.Unlock()

	if resultName, _ := task.Result["file_name"].(string); resultName != "" && resultName != nodeName {
		// 任务开始传输后才发生的改名无法联动，网盘上仍是旧名。
		f.b.warn("FUSE staging 上传完成时文件名已变化，网盘保留上传时的名字",
			"task_id", task.TaskID,
			"uploaded_name", resultName,
			"current_name", nodeName,
		)
	}
	if resultParent, _ := task.Result["parent_id"].(string); resultParent != "" && nodeParentID != "" && resultParent != nodeParentID {
		f.b.warn("FUSE staging 上传完成时所在目录已变化，网盘保留上传时的目录",
			"task_id", task.TaskID,
			"uploaded_parent_id", resultParent,
			"current_parent_id", nodeParentID,
		)
	}

	name, parent := f.Parent()
	detached := false
	if parent != nil && parent.GetChild(name) == f.EmbeddedInode() {
		detached, _ = parent.RmChild(name)
	}
	if detached {
		// 已脱离目录树，不再需要常驻；内核仍持有引用时由 Forget 回收。
		f.ForgetPersistent()
	}
	f.opMu.Unlock()

	if detached {
		// ENOENT 表示内核本就没缓存该目录项，忽略即可。
		_ = parent.NotifyEntry(name)
	}
}

func remoteItemFromTask(task *upload.Task, name string, size int64) domain.FileItem {
	item := domain.FileItem{Name: name, Size: size, ModTime: time.Now()}
	if task == nil || task.Result == nil {
		return item
	}
	if id, ok := task.Result["file_id"].(string); ok {
		item.ID = id
	}
	switch v := task.Result["size"].(type) {
	case int64:
		if v > 0 {
			item.Size = v
		}
	case int:
		if v > 0 {
			item.Size = int64(v)
		}
	case float64:
		if v > 0 {
			item.Size = int64(v)
		}
	}
	return item
}

func (f *stagingFile) updateSize(size int64) {
	f.mu.Lock()
	if size > f.size {
		f.size = size
	}
	f.modTime = time.Now()
	f.mu.Unlock()
}

func (f *stagingFile) updateFromStat(st os.FileInfo) {
	if st == nil {
		return
	}
	f.mu.Lock()
	f.size = st.Size()
	f.modTime = st.ModTime()
	f.mu.Unlock()
}

type stagingUploadHandle struct {
	node              *stagingFile
	uploads           UploadManager
	accountID         int64
	parentID          string
	targetDisplayPath string
	fileName          string
	tempPath          string
	file              *os.File
	cleanup           func()
	untrack           func()
	flags             uint32
	queued            bool
	released          bool
	mu                sync.Mutex
}

func (h *stagingUploadHandle) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	file := h.file
	h.mu.Unlock()
	if file == nil {
		return 0, syscall.EBADF
	}
	n, err := file.WriteAt(data, off)
	if n > 0 && h.node != nil {
		h.node.updateSize(off + int64(n))
	}
	if err != nil {
		return uint32(n), errnoFor(err)
	}
	return uint32(n), 0
}

// Flush 在每个文件描述符 close 时都会被调用（dup/fork 出来的副本各一次），
// 例如 `cmd | gzip > file` 时 shell 会先关闭自己那份 fd，此时文件还是空的。
// 因此这里只落盘并刷新大小，真正投递上传放到最后一个 fd 关闭时的 Release。
// 取舍：Release 的返回值内核不会交给用户态，投递任务失败只能记日志，
// close() 不再报告投递错误（上传本身原本就是异步的，close 也拿不到上传结果）。
func (h *stagingUploadHandle) Flush(_ context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return syscall.EBADF
	}
	if h.flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		return syscall.EBADF
	}
	if err := h.file.Sync(); err != nil {
		return errnoFor(err)
	}
	st, err := h.file.Stat()
	if err != nil {
		return errnoFor(err)
	}
	if h.node != nil {
		h.node.updateFromStat(st)
	}
	return 0
}

func (h *stagingUploadHandle) Fsync(_ context.Context, _ uint32) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return syscall.EBADF
	}
	if err := h.file.Sync(); err != nil {
		return errnoFor(err)
	}
	return 0
}

// Release 在最后一个引用该句柄的 fd 关闭后调用，此时内容已写完，投递上传任务。
func (h *stagingUploadHandle) Release(_ context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return 0
	}
	h.released = true
	if h.file == nil {
		h.discardLocked()
		return 0
	}
	syncErr := h.file.Sync()
	st, statErr := h.file.Stat()
	closeErr := h.file.Close()
	h.file = nil
	if err := firstErr(syncErr, statErr, closeErr); err != nil {
		h.logWarn("FUSE staging 文件落盘失败，放弃上传", "err", err)
		h.discardLocked()
		return errnoFor(err)
	}
	if h.flags&syscall.O_ACCMODE == syscall.O_RDONLY || h.uploads == nil {
		h.discardLocked()
		return 0
	}
	if h.node != nil {
		h.node.updateFromStat(st)
	}
	h.queueLocked(st.Size())
	return 0
}

func (h *stagingUploadHandle) queueLocked(size int64) {
	node := h.node
	fileName := h.fileName
	parentID := h.parentID
	displayPath := h.targetDisplayPath
	if node != nil {
		node.opMu.Lock()
		defer node.opMu.Unlock()
		node.mu.RLock()
		removed := node.removed
		// 若在 close 前已发生 rename，以节点当前名与目录为准，避免上传任务使用旧名。
		if node.name != "" {
			fileName = node.name
		}
		if node.parentID != "" {
			parentID = node.parentID
		}
		if node.displayPath != "" {
			displayPath = node.displayPath
		}
		node.mu.RUnlock()
		if removed {
			h.discardLocked()
			return
		}
	}
	task, err := h.uploads.Create(context.Background(), upload.CreateParams{
		AccountID:         h.accountID,
		FileName:          fileName,
		DisplayName:       fileName,
		TargetPath:        parentID,
		TargetDisplayPath: displayPath,
		LocalPath:         h.tempPath,
		TotalBytes:        size,
		ConflictPolicy:    "overwrite",
	})
	if err != nil {
		h.logWarn("FUSE staging 投递上传任务失败", "file", fileName, "err", err)
		h.discardLocked()
		return
	}
	h.queued = true
	if h.untrack != nil {
		h.untrack()
		h.untrack = nil
	}
	h.cleanup = nil
	if node == nil || task == nil || task.TaskID == "" {
		return
	}
	node.mu.Lock()
	node.uploads = h.uploads
	node.taskID = task.TaskID
	node.mu.Unlock()
	go node.watchUpload(h.uploads, task.TaskID)
}

// discardLocked 清理未投递的临时文件，并标记节点已失效。
func (h *stagingUploadHandle) discardLocked() {
	if h.queued {
		return
	}
	if h.cleanup != nil {
		h.cleanup()
		h.cleanup = nil
	}
	if h.node != nil {
		h.node.mu.Lock()
		h.node.dropped = true
		h.node.mu.Unlock()
	}
}

func (h *stagingUploadHandle) logWarn(msg string, args ...any) {
	if h.node == nil {
		return
	}
	h.node.b.warn(msg, append(args, "temp_path", h.tempPath)...)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type localStagingReadHandle struct {
	file *os.File
}

func (h *localStagingReadHandle) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if h.file == nil {
		return nil, syscall.EBADF
	}
	n, err := h.file.ReadAt(dest, off)
	if n > 0 {
		return fuse.ReadResultData(dest[:n]), 0
	}
	if err == nil || err == io.EOF {
		return fuse.ReadResultData(nil), 0
	}
	if os.IsNotExist(err) {
		return nil, syscall.ENOENT
	}
	return nil, syscall.EIO
}

func (h *localStagingReadHandle) Release(_ context.Context) syscall.Errno {
	if h.file != nil {
		_ = h.file.Close()
		h.file = nil
	}
	return 0
}

// stagingRemoteReadHandle 用于临时文件已被清理后、按网盘文件读取 staging 节点。
type stagingRemoteReadHandle struct {
	file   *cloudFile
	handle *remoteHandle
}

func openStagingRemote(ctx context.Context, b *backend, item domain.FileItem, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if b == nil || item.ID == "" {
		return nil, 0, syscall.ENOENT
	}
	cf := &cloudFile{b: b, item: item}
	fh, fuseFlags, errno := cf.Open(ctx, flags)
	if errno != 0 {
		return nil, 0, errno
	}
	rh, ok := fh.(*remoteHandle)
	if !ok {
		return nil, 0, syscall.EIO
	}
	return &stagingRemoteReadHandle{file: cf, handle: rh}, fuseFlags, 0
}

func (h *stagingRemoteReadHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if h.file == nil || h.handle == nil {
		return nil, syscall.EBADF
	}
	return h.file.Read(ctx, h.handle, dest, off)
}

func (h *stagingRemoteReadHandle) Release(ctx context.Context) syscall.Errno {
	if h.handle == nil {
		return 0
	}
	errno := h.handle.Release(ctx)
	h.handle = nil
	return errno
}

func createFuseTempFile(uploads UploadManager, fileName string) (*os.File, string, func(), func(), error) {
	dir := uploads.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", nil, nil, err
	}
	safeName := filepath.Base(fileName)
	if safeName == "" || safeName == "." {
		safeName = "upload.bin"
	}
	f, err := os.CreateTemp(dir, "fuse_*_"+safeName)
	if err != nil {
		return nil, "", nil, nil, err
	}
	path := filepath.Clean(f.Name())
	untrack := func() {}
	if registry := uploads.TempRegistry(); registry != nil {
		untrack = registry.Track(path)
	}
	cleanup := func() {
		untrack()
		_ = os.Remove(path)
	}
	return f, path, cleanup, untrack, nil
}
