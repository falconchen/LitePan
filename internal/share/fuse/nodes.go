//go:build fuse

package fuse

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"litepan/internal/domain"
	"litepan/internal/playback"
)

type cloudDir struct {
	fs.Inode
	b    *backend
	item domain.FileItem
}

var (
	_ fs.NodeLookuper  = (*cloudDir)(nil)
	_ fs.NodeReaddirer = (*cloudDir)(nil)
	_ fs.NodeGetattrer = (*cloudDir)(nil)
	_ fs.NodeOpendirer = (*cloudDir)(nil)
	_ fs.NodeMkdirer   = (*cloudDir)(nil)
	_ fs.NodeCreater   = (*cloudDir)(nil)
	_ fs.NodeUnlinker  = (*cloudDir)(nil)
	_ fs.NodeRmdirer   = (*cloudDir)(nil)
	_ fs.NodeRenamer   = (*cloudDir)(nil)
)

func (d *cloudDir) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillDirAttr(&out.Attr, d.b, d.item)
	return 0
}

func (d *cloudDir) Opendir(_ context.Context) syscall.Errno { return 0 }

func (d *cloudDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	items, err := d.b.deps.Files.List(ctx, d.b.accountID(), d.item.ID, false)
	if err != nil {
		return nil, errnoFor(err)
	}
	entries := make([]fuse.DirEntry, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		mode := fuse.S_IFREG
		if it.IsDir {
			mode = fuse.S_IFDIR
		}
		seen[it.Name] = struct{}{}
		entries = append(entries, fuse.DirEntry{
			Name: it.Name,
			Mode: uint32(mode),
			Ino:  fileIno(d.b.accountID(), it.ID),
		})
	}
	// 合并本会话内已创建、但网盘列表尚未可见的 staging 文件，保证刚写入的文件立即可见。
	for name, ch := range d.Children() {
		if _, ok := seen[name]; ok {
			continue
		}
		if st, ok := ch.Operations().(*stagingFile); ok && !st.superseded() {
			entries = append(entries, fuse.DirEntry{
				Name: name,
				Mode: fuse.S_IFREG,
				Ino:  ch.StableAttr().Ino,
			})
			seen[name] = struct{}{}
		}
	}
	return fs.NewListDirStream(entries), 0
}

func (d *cloudDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	// 先查本会话内存节点：刚创建、尚未在网盘列表可见的 staging 文件与新建目录优先命中。
	// 已上传完成（或已失效）的 staging 节点跳过，改走网盘列表；返回的新节点会覆盖旧目录项。
	if ch := d.GetChild(name); ch != nil {
		switch n := ch.Operations().(type) {
		case *stagingFile:
			if n.superseded() {
				d.dropStaleStaging(name, ch)
				break
			}
			var a fuse.AttrOut
			if errno := n.Getattr(ctx, nil, &a); errno != 0 {
				return nil, errno
			}
			out.Attr = a.Attr
			return ch, 0
		case *cloudDir:
			fillEntryAttr(out, d.b, n.item)
			return ch, 0
		case *cloudFile:
			fillEntryAttr(out, d.b, n.item)
			return ch, 0
		}
	}
	item, errno := d.lookupChildItem(ctx, name)
	if errno != 0 {
		return nil, errno
	}
	child := d.newChild(ctx, item)
	if child == nil {
		return nil, syscall.EIO
	}
	fillEntryAttr(out, d.b, item)
	return child, 0
}

// dropStaleStaging 把已失效的 staging 节点移出目录树，并取消其常驻，避免长期占用内存。
// 仅当该名字下仍是同一个节点时才移除，防止误删并发改名后放进来的新节点。
func (d *cloudDir) dropStaleStaging(name string, ch *fs.Inode) {
	if d.GetChild(name) != ch {
		return
	}
	if removed, _ := d.RmChild(name); removed {
		ch.ForgetPersistent()
	}
}

func (d *cloudDir) newChild(ctx context.Context, item domain.FileItem) *fs.Inode {
	ino := fileIno(d.b.accountID(), item.ID)
	if item.IsDir {
		return d.NewPersistentInode(ctx, &cloudDir{b: d.b, item: item}, fs.StableAttr{
			Ino:  ino,
			Mode: fuse.S_IFDIR,
		})
	}
	return d.NewPersistentInode(ctx, &cloudFile{b: d.b, item: item}, fs.StableAttr{
		Ino:  ino,
		Mode: fuse.S_IFREG,
	})
}

func (d *cloudDir) Mkdir(ctx context.Context, name string, _ uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	name, errno := normalizeChildName(name)
	if errno != 0 {
		return nil, errno
	}
	item, err := d.b.deps.Files.CreateFolder(ctx, d.b.accountID(), d.item.ID, name)
	if err != nil {
		return nil, errnoFor(err)
	}
	if item == nil {
		return nil, syscall.EIO
	}
	child := d.newChild(ctx, *item)
	if child == nil {
		return nil, syscall.EIO
	}
	fillEntryAttr(out, d.b, *item)
	return child, 0
}

func (d *cloudDir) Unlink(ctx context.Context, name string) syscall.Errno {
	name, errno := normalizeChildName(name)
	if errno != 0 {
		return errno
	}
	if ch := d.GetChild(name); ch != nil {
		if st, ok := ch.Operations().(*stagingFile); ok && !st.superseded() {
			return d.unlinkStaging(st)
		}
	}
	item, errno := d.lookupChildItem(ctx, name)
	if errno != 0 {
		return errno
	}
	if item.IsDir {
		return syscall.EISDIR
	}
	if err := d.b.deps.Files.DeleteFiles(ctx, d.b.accountID(), []string{item.ID}, d.item.ID); err != nil {
		return errnoFor(err)
	}
	return 0
}

func (d *cloudDir) Rmdir(ctx context.Context, name string) syscall.Errno {
	name, errno := normalizeChildName(name)
	if errno != 0 {
		return errno
	}
	if ch := d.GetChild(name); ch != nil {
		if dir, ok := ch.Operations().(*cloudDir); ok {
			if err := d.b.deps.Files.DeleteFiles(ctx, d.b.accountID(), []string{dir.item.ID}, d.item.ID); err != nil {
				return errnoFor(err)
			}
			return 0
		}
	}
	item, errno := d.lookupChildItem(ctx, name)
	if errno != 0 {
		return errno
	}
	if !item.IsDir {
		return syscall.ENOTDIR
	}
	if err := d.b.deps.Files.DeleteFiles(ctx, d.b.accountID(), []string{item.ID}, d.item.ID); err != nil {
		return errnoFor(err)
	}
	return 0
}

func (d *cloudDir) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	name, errno := normalizeChildName(name)
	if errno != 0 {
		return errno
	}
	newName, errno = normalizeChildName(newName)
	if errno != 0 {
		return errno
	}
	if flags != 0 {
		return syscall.EINVAL
	}
	targetDir, ok := newParent.(*cloudDir)
	if !ok {
		return syscall.EXDEV
	}
	if targetDir.b.mount == nil || d.b.mount == nil || targetDir.b.mount.ID != d.b.mount.ID {
		return syscall.EXDEV
	}
	// 目标名冲突检查（内存节点优先）。
	if ch := targetDir.GetChild(newName); ch != nil {
		if st, ok := ch.Operations().(*stagingFile); !ok || !st.superseded() {
			return syscall.EEXIST
		}
	}
	if _, found, errno := targetDir.lookupChildItemIfExists(ctx, newName); errno != 0 {
		return errno
	} else if found {
		return syscall.EEXIST
	}
	// 源优先取本会话内存节点：刚创建、尚未在网盘列表可见的 staging 文件与新建目录。
	if src := d.GetChild(name); src != nil {
		switch n := src.Operations().(type) {
		case *stagingFile:
			if !n.superseded() {
				return d.renameStaging(ctx, n, targetDir, newName)
			}
		case *cloudDir:
			return d.renameRemote(ctx, n.item, targetDir, newName)
		case *cloudFile:
			return d.renameRemote(ctx, n.item, targetDir, newName)
		}
	}
	item, errno := d.lookupChildItem(ctx, name)
	if errno != 0 {
		return errno
	}
	return d.renameRemote(ctx, item, targetDir, newName)
}

// renameStaging 处理本会话内刚创建、尚未在网盘列表可见的 staging 文件改名：
// 更新节点名，并联动修改未开始的上传任务目标名与目标目录。
func (d *cloudDir) renameStaging(ctx context.Context, st *stagingFile, targetDir *cloudDir, newName string) syscall.Errno {
	crossDir := targetDir.item.ID != d.item.ID
	newDisplay := ""
	if crossDir {
		newDisplay = targetDir.targetDisplayPath()
	}
	// 与 Release 中的投递互斥：改名要么先于投递（任务直接用新名），
	// 要么晚于投递（此时能读到 taskID 并联动任务）。
	st.opMu.Lock()
	defer st.opMu.Unlock()
	st.mu.Lock()
	oldName := st.name
	taskID := st.taskID
	uploads := st.uploads
	if !crossDir && oldName == newName {
		st.mu.Unlock()
		return 0
	}
	st.name = newName
	if crossDir {
		st.parentID = targetDir.item.ID
		st.displayPath = newDisplay
	}
	st.mu.Unlock()

	if taskID != "" && uploads != nil {
		newTarget := ""
		if crossDir {
			newTarget = targetDir.item.ID
		}
		renamed, err := uploads.RenameTask(ctx, taskID, newName, newTarget, newDisplay)
		if err != nil || !renamed {
			// 任务已开始传输时无法再改，网盘上会保留旧名。
			d.b.warn("FUSE staging rename 未能更新上传任务",
				"task_id", taskID,
				"old_name", oldName,
				"new_name", newName,
				"err", err,
			)
		}
	}
	return 0
}

// renameRemote 对网盘上已存在的文件或目录执行移动与改名。
func (d *cloudDir) renameRemote(ctx context.Context, item domain.FileItem, targetDir *cloudDir, newName string) syscall.Errno {
	if targetDir.item.ID == d.item.ID && item.Name == newName {
		return 0
	}
	if item.IsDir && targetDir.item.ID == item.ID {
		return syscall.EINVAL
	}
	if targetDir.item.ID != d.item.ID {
		if err := d.b.deps.Files.MoveFiles(ctx, d.b.accountID(), []string{item.ID}, targetDir.item.ID, d.item.ID); err != nil {
			return errnoFor(err)
		}
		if newName != item.Name {
			if err := d.b.deps.Files.RenameFile(ctx, d.b.accountID(), item.ID, newName, targetDir.item.ID); err != nil {
				if rollbackErr := d.b.deps.Files.MoveFiles(ctx, d.b.accountID(), []string{item.ID}, d.item.ID, targetDir.item.ID); rollbackErr != nil && d.b.deps.Log != nil {
					d.b.deps.Log.Error("FUSE move+rename 回滚失败",
						"account_id", d.b.accountID(),
						"file_id", item.ID,
						"source_parent_id", d.item.ID,
						"target_parent_id", targetDir.item.ID,
						"target_name", newName,
						"rename_err", err,
						"rollback_err", rollbackErr,
					)
				}
				return errnoFor(err)
			}
		}
		return 0
	}
	if err := d.b.deps.Files.RenameFile(ctx, d.b.accountID(), item.ID, newName, d.item.ID); err != nil {
		return errnoFor(err)
	}
	return 0
}

// unlinkStaging 删除本会话内刚创建、尚未在网盘列表可见的 staging 文件：
// 取消对应的上传任务并清理本地临时文件。
func (d *cloudDir) unlinkStaging(st *stagingFile) syscall.Errno {
	st.opMu.Lock()
	defer st.opMu.Unlock()
	st.mu.Lock()
	st.removed = true
	taskID := st.taskID
	uploads := st.uploads
	tempPath := st.tempPath
	st.mu.Unlock()
	if taskID != "" && uploads != nil {
		if _, err := uploads.Delete(context.Background(), taskID, false); err != nil {
			d.b.warn("FUSE staging unlink 取消上传任务失败",
				"task_id", taskID,
				"err", err,
			)
		}
	}
	if tempPath != "" {
		_ = os.Remove(tempPath)
	}
	return 0
}

func (d *cloudDir) lookupChildItem(ctx context.Context, name string) (domain.FileItem, syscall.Errno) {
	item, found, errno := d.lookupChildItemIfExists(ctx, name)
	if errno != 0 {
		return domain.FileItem{}, errno
	}
	if !found {
		return domain.FileItem{}, syscall.ENOENT
	}
	return item, 0
}

func (d *cloudDir) lookupChildItemIfExists(ctx context.Context, name string) (domain.FileItem, bool, syscall.Errno) {
	items, err := d.b.deps.Files.List(ctx, d.b.accountID(), d.item.ID, false)
	if err != nil {
		return domain.FileItem{}, false, errnoFor(err)
	}
	for _, it := range items {
		if it.Name == name {
			return it, true, 0
		}
	}
	return domain.FileItem{}, false, 0
}

func normalizeChildName(name string) (string, syscall.Errno) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", syscall.EINVAL
	}
	return name, 0
}

type cloudFile struct {
	fs.Inode
	b    *backend
	item domain.FileItem
}

var (
	_ fs.NodeGetattrer = (*cloudFile)(nil)
	_ fs.NodeOpener    = (*cloudFile)(nil)
	_ fs.NodeReader    = (*cloudFile)(nil)
	_ fs.FileReleaser  = (*remoteHandle)(nil)
)

func (f *cloudFile) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillFileAttr(&out.Attr, f.b, f.item)
	return 0
}

func (f *cloudFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	if f.b.deps.Playback == nil {
		return nil, 0, syscall.EIO
	}
	reader, err := f.b.deps.Playback.OpenRemoteReader(ctx, f.b.accountID(), f.item.ID, "")
	if err != nil {
		return nil, 0, errnoFor(err)
	}
	if reader.Size() > 0 {
		f.item.Size = reader.Size()
	}
	return &remoteHandle{reader: reader}, 0, 0
}

func (f *cloudFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h, ok := fh.(*remoteHandle)
	if !ok || h.reader == nil {
		return nil, syscall.EIO
	}
	var n int
	var err error
	if rc := f.b.deps.ReadCache; rc != nil && rc.Enabled(ctx) {
		n, err = rc.ReadAt(ctx, f.b.accountID(), f.item.ID, dest, off, h.reader.ReadAt)
	} else {
		n, err = h.reader.ReadAt(dest, off)
	}
	if n > 0 {
		return fuse.ReadResultData(dest[:n]), 0
	}
	if err == io.EOF {
		return fuse.ReadResultData(nil), 0
	}
	if err != nil {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(nil), 0
}

type remoteHandle struct {
	reader *playback.RemoteReader
}

func (h *remoteHandle) Release(_ context.Context) syscall.Errno {
	if h == nil || h.reader == nil {
		return 0
	}
	_ = h.reader.Close()
	h.reader = nil
	return 0
}

func fillDirAttr(out *fuse.Attr, b *backend, item domain.FileItem) {
	out.Mode = b.mount.DirMode & 0o7777
	out.Nlink = 2
	out.Uid = b.mount.UID
	out.Gid = b.mount.GID
	if !item.ModTime.IsZero() {
		sec := uint64(item.ModTime.Unix())
		out.Mtime = sec
		out.Atime = sec
		out.Ctime = sec
	} else {
		sec := uint64(time.Now().Unix())
		out.Mtime = sec
		out.Atime = sec
		out.Ctime = sec
	}
}

func fillFileAttr(out *fuse.Attr, b *backend, item domain.FileItem) {
	out.Mode = b.mount.FileMode & 0o7777
	out.Nlink = 1
	out.Uid = b.mount.UID
	out.Gid = b.mount.GID
	out.Size = uint64(item.Size)
	if !item.ModTime.IsZero() {
		sec := uint64(item.ModTime.Unix())
		out.Mtime = sec
		out.Atime = sec
		out.Ctime = sec
	}
	const bs = 4096
	out.Blksize = bs
	if item.Size > 0 {
		out.Blocks = (uint64(item.Size) + bs - 1) / bs
	}
}

func fillEntryAttr(out *fuse.EntryOut, b *backend, item domain.FileItem) {
	if item.IsDir {
		fillDirAttr(&out.Attr, b, item)
	} else {
		fillFileAttr(&out.Attr, b, item)
	}
}

func errnoFor(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if errors.Is(err, syscall.ENOSPC) {
		return syscall.ENOSPC
	}
	if errors.Is(err, syscall.EDQUOT) {
		return syscall.EDQUOT
	}
	if errors.Is(err, syscall.EFBIG) {
		return syscall.EFBIG
	}
	if ae, ok := domain.AsAppError(err); ok {
		switch ae.Code {
		case domain.CodeNotFound:
			return syscall.ENOENT
		case domain.CodePermissionDenied:
			return syscall.EACCES
		case domain.CodeValidation:
			return syscall.EINVAL
		}
	}
	return syscall.EIO
}
