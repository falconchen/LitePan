//go:build fuse

package fuse

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"litepan/internal/domain"
	"litepan/internal/upload"
)

type recordingUploadManager struct {
	mu        sync.Mutex
	tempDir   string
	taskID    string
	createErr error
	// waitTask 为 nil 时 Wait 返回"任务不存在"，模拟监听方拿不到结果。
	waitTask *upload.Task
	params   []upload.CreateParams
	renamed  []string
	deleted  []string
}

func (m *recordingUploadManager) Create(_ context.Context, params upload.CreateParams) (*upload.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return nil, m.createErr
	}
	m.params = append(m.params, params)
	id := m.taskID
	if id == "" {
		id = "task-1"
	}
	return &upload.Task{TaskID: id}, nil
}

func (m *recordingUploadManager) Wait(_ context.Context, _ string) (*upload.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waitTask == nil {
		return nil, errors.New("上传任务不存在")
	}
	task := *m.waitTask
	return &task, nil
}

func (m *recordingUploadManager) TempDir() string { return m.tempDir }

func (m *recordingUploadManager) TempRegistry() *upload.TempRegistry { return nil }

func (m *recordingUploadManager) RenameTask(_ context.Context, taskID, newName, _ string, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renamed = append(m.renamed, taskID+":"+newName)
	return true, nil
}

func (m *recordingUploadManager) Delete(_ context.Context, taskID string, _ bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, taskID)
	return true, nil
}

func (m *recordingUploadManager) createdParams() []upload.CreateParams {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]upload.CreateParams(nil), m.params...)
}

// newTestStagingHandle 构造一个写句柄及其临时文件，等价于 cloudDir.Create 的产物。
func newTestStagingHandle(t *testing.T, manager *recordingUploadManager, node *stagingFile, name string) *stagingUploadHandle {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "staging-*")
	if err != nil {
		t.Fatalf("创建临时文件失败: %v", err)
	}
	path := tmp.Name()
	if node != nil {
		node.name = name
		node.tempPath = path
		if node.parentID == "" {
			node.parentID = "root"
		}
	}
	return &stagingUploadHandle{
		node:      node,
		uploads:   manager,
		accountID: 1,
		parentID:  "root",
		fileName:  name,
		tempPath:  path,
		file:      tmp,
		cleanup:   func() { _ = os.Remove(path) },
		flags:     syscall.O_WRONLY,
	}
}

// newTestMountRoot 用 go-fuse 的 NewNodeFS 初始化一棵不挂载到内核的节点树，
// 这样可以真实地增删子节点；没有内核连接时 NotifyEntry 返回 ENOSYS。
func newTestMountRoot(t *testing.T) *cloudDir {
	t.Helper()
	root := &cloudDir{
		b:    &backend{deps: Deps{}, mount: &domain.FuseMount{ID: 1, AccountID: 1}},
		item: domain.FileItem{ID: "root", IsDir: true},
	}
	_ = fs.NewNodeFS(root, nil)
	return root
}

func attachStaging(t *testing.T, root *cloudDir, st *stagingFile, name string) *fs.Inode {
	t.Helper()
	child := root.NewPersistentInode(context.Background(), st, fs.StableAttr{
		Ino:  fileIno(1, "pending:"+name),
		Mode: fuse.S_IFREG,
	})
	if !root.AddChild(name, child, false) {
		t.Fatalf("挂入 staging 节点 %q 失败", name)
	}
	return child
}

func successTask(taskID, name string, size int64) *upload.Task {
	return &upload.Task{
		TaskID: taskID,
		Status: upload.StatusSuccess,
		Result: map[string]any{
			"file_id":   "remote-" + name,
			"parent_id": "root",
			"file_name": name,
			"size":      size,
		},
	}
}

func TestStagingUploadReleaseQueuesEmptyFile(t *testing.T) {
	manager := &recordingUploadManager{tempDir: t.TempDir()}
	handle := newTestStagingHandle(t, manager, &stagingFile{}, "empty.txt")

	if errno := handle.Flush(context.Background()); errno != 0 {
		t.Fatalf("空文件 Flush 不应失败，errno=%v", errno)
	}
	if n := len(manager.createdParams()); n != 0 {
		t.Fatalf("Flush 不应投递上传任务，实际 %d 个", n)
	}
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release 不应失败，errno=%v", errno)
	}
	params := manager.createdParams()
	if len(params) != 1 {
		t.Fatalf("期望创建一个上传任务，实际 %d", len(params))
	}
	if params[0].TotalBytes != 0 {
		t.Fatalf("空文件任务大小应为 0，实际 %d", params[0].TotalBytes)
	}
}

// 模拟 `cmd | gzip > file`：shell 先关闭自己那份 fd（触发 Flush，此时文件为空），
// 随后 gzip 写入内容并关闭最后一个 fd（Flush + Release）。只应上传一次，且是完整内容。
func TestStagingUploadFlushOnDupFdDoesNotQueueEmptyFile(t *testing.T) {
	manager := &recordingUploadManager{tempDir: t.TempDir()}
	node := &stagingFile{}
	handle := newTestStagingHandle(t, manager, node, "backup.sql.gz")
	ctx := context.Background()

	if errno := handle.Flush(ctx); errno != 0 {
		t.Fatalf("第一次 Flush 不应失败，errno=%v", errno)
	}
	payload := []byte("compressed-backup-data")
	if n, errno := handle.Write(ctx, payload[:10], 0); errno != 0 || n != 10 {
		t.Fatalf("写入第一段失败 n=%d errno=%v", n, errno)
	}
	if errno := handle.Flush(ctx); errno != 0 {
		t.Fatalf("中途 Flush 不应失败，errno=%v", errno)
	}
	if n, errno := handle.Write(ctx, payload[10:], 10); errno != 0 || int(n) != len(payload)-10 {
		t.Fatalf("写入第二段失败 n=%d errno=%v", n, errno)
	}
	if errno := handle.Flush(ctx); errno != 0 {
		t.Fatalf("最后 Flush 不应失败，errno=%v", errno)
	}
	if n := len(manager.createdParams()); n != 0 {
		t.Fatalf("最后一个 fd 关闭前不应投递任务，实际 %d 个", n)
	}
	if errno := handle.Release(ctx); errno != 0 {
		t.Fatalf("Release 不应失败，errno=%v", errno)
	}

	params := manager.createdParams()
	if len(params) != 1 {
		t.Fatalf("期望恰好一个上传任务，实际 %d", len(params))
	}
	if params[0].TotalBytes != int64(len(payload)) {
		t.Fatalf("任务大小应为 %d，实际 %d", len(payload), params[0].TotalBytes)
	}
	got, err := os.ReadFile(params[0].LocalPath)
	if err != nil {
		t.Fatalf("投递后临时文件应保留给上传任务: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("临时文件内容不完整: %q", got)
	}
	if node.taskID != "task-1" {
		t.Fatalf("节点应记录任务 ID，实际 %q", node.taskID)
	}
	if node.size != int64(len(payload)) {
		t.Fatalf("节点大小应为 %d，实际 %d", len(payload), node.size)
	}
}

func TestStagingUploadReleaseUsesRenamedName(t *testing.T) {
	manager := &recordingUploadManager{tempDir: t.TempDir()}
	dir := &cloudDir{
		b:    &backend{deps: Deps{}, mount: &domain.FuseMount{ID: 1, AccountID: 1}},
		item: domain.FileItem{ID: "root", IsDir: true},
	}
	node := &stagingFile{b: dir.b}
	handle := newTestStagingHandle(t, manager, node, ".photo.jpg.part")

	// rsync 式写法：写完未关闭前就改成正式名。
	if errno := dir.renameStaging(context.Background(), node, dir, "photo.jpg"); errno != 0 {
		t.Fatalf("renameStaging 不应失败，errno=%v", errno)
	}
	if len(manager.renamed) != 0 {
		t.Fatalf("投递前改名不应调用 RenameTask，实际 %v", manager.renamed)
	}
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release 不应失败，errno=%v", errno)
	}
	params := manager.createdParams()
	if len(params) != 1 || params[0].FileName != "photo.jpg" {
		t.Fatalf("上传任务应使用改名后的文件名，实际 %+v", params)
	}
}

func TestStagingUploadReleaseSkipsUnlinkedFile(t *testing.T) {
	manager := &recordingUploadManager{tempDir: t.TempDir()}
	dir := &cloudDir{b: &backend{deps: Deps{}, mount: &domain.FuseMount{ID: 1, AccountID: 1}}}
	node := &stagingFile{b: dir.b}
	handle := newTestStagingHandle(t, manager, node, "scratch.tmp")
	if _, errno := handle.Write(context.Background(), []byte("x"), 0); errno != 0 {
		t.Fatalf("写入失败 errno=%v", errno)
	}

	if errno := dir.unlinkStaging(node); errno != 0 {
		t.Fatalf("unlinkStaging 不应失败，errno=%v", errno)
	}
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release 不应失败，errno=%v", errno)
	}
	if n := len(manager.createdParams()); n != 0 {
		t.Fatalf("已删除的文件不应上传，实际 %d 个任务", n)
	}
	if !node.superseded() {
		t.Fatal("已删除的 staging 节点应视为失效")
	}
}

func TestStagingUploadReleaseCreateFailureCleansTemp(t *testing.T) {
	manager := &recordingUploadManager{tempDir: t.TempDir(), createErr: errors.New("上传服务正在停止")}
	node := &stagingFile{}
	handle := newTestStagingHandle(t, manager, node, "a.txt")

	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release 不应失败，errno=%v", errno)
	}
	if _, err := os.Stat(handle.tempPath); !os.IsNotExist(err) {
		t.Fatalf("投递失败时应清理临时文件，stat err=%v", err)
	}
	if !node.superseded() {
		t.Fatal("投递失败的 staging 节点应视为失效")
	}
}

func TestStagingSupersededAfterTempCleanup(t *testing.T) {
	tmp := t.TempDir() + "/data"
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	st := &stagingFile{tempPath: tmp}
	if st.superseded() {
		t.Fatal("尚未投递任务的节点不应失效")
	}
	st.taskID = "task-1"
	if st.superseded() {
		t.Fatal("任务进行中、临时文件仍在时不应失效")
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatalf("删除临时文件失败: %v", err)
	}
	if !st.superseded() {
		t.Fatal("任务已投递且临时文件被清理后应失效")
	}
}

// 上传成功后 staging 节点应从目录树摘除，下次 Lookup 走网盘；
// 内核仍缓存旧 inode 时，Open 应回退到网盘文件而不是 ENOENT。
func TestStagingFinishUploadDetachesNode(t *testing.T) {
	root := newTestMountRoot(t)
	tmp := t.TempDir() + "/data"
	if err := os.WriteFile(tmp, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	st := &stagingFile{b: root.b, name: "a.jpg", tempPath: tmp, size: 5, parentID: "root", taskID: "task-1"}
	attachStaging(t, root, st, "a.jpg")

	// 上传任务按 file_on_success 清理临时文件。
	if err := os.Remove(tmp); err != nil {
		t.Fatalf("删除临时文件失败: %v", err)
	}
	st.finishUpload(successTask("task-1", "a.jpg", 5))

	if root.GetChild("a.jpg") != nil {
		t.Fatal("上传成功后 staging 节点应从父目录移除")
	}
	if !st.superseded() {
		t.Fatal("上传成功后 staging 节点应视为失效")
	}
	if st.remote.ID != "remote-a.jpg" || st.remote.Size != 5 {
		t.Fatalf("应记录网盘条目，实际 %+v", st.remote)
	}
	// 测试环境没有 Playback，回退到网盘时得到 EIO；关键是不再返回 ENOENT。
	if _, _, errno := st.Open(context.Background(), syscall.O_RDONLY); errno == syscall.ENOENT {
		t.Fatal("临时文件被清理后 Open 应回退到网盘文件，而不是 ENOENT")
	}
}

func TestStagingFinishUploadIgnoresOtherTaskAndRemovedNode(t *testing.T) {
	root := newTestMountRoot(t)
	st := &stagingFile{b: root.b, name: "a.jpg", tempPath: t.TempDir() + "/data", parentID: "root", taskID: "task-1"}
	attachStaging(t, root, st, "a.jpg")

	st.finishUpload(successTask("task-other", "a.jpg", 1))
	if root.GetChild("a.jpg") == nil {
		t.Fatal("其它任务的结果不应影响该节点")
	}

	st.removed = true
	st.finishUpload(successTask("task-1", "a.jpg", 1))
	if root.GetChild("a.jpg") == nil {
		t.Fatal("已删除的节点交由 Unlink 处理，不应在此再次摘除")
	}
}

// 完整流程：Release 投递任务并启动监听，任务成功后节点自动让位给网盘文件。
func TestStagingReleaseDetachesNodeAfterUploadSuccess(t *testing.T) {
	root := newTestMountRoot(t)
	manager := &recordingUploadManager{tempDir: t.TempDir(), waitTask: successTask("task-1", "a.jpg", 3)}
	st := &stagingFile{b: root.b, parentID: "root"}
	handle := newTestStagingHandle(t, manager, st, "a.jpg")
	attachStaging(t, root, st, "a.jpg")
	if _, errno := handle.Write(context.Background(), []byte("abc"), 0); errno != 0 {
		t.Fatalf("写入失败 errno=%v", errno)
	}
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release 不应失败，errno=%v", errno)
	}

	deadline := time.Now().Add(5 * time.Second)
	for root.GetChild("a.jpg") != nil {
		if time.Now().After(deadline) {
			t.Fatal("上传成功后 staging 节点未被移除")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStagingReleaseKeepsNodeWhenUploadFails(t *testing.T) {
	root := newTestMountRoot(t)
	manager := &recordingUploadManager{tempDir: t.TempDir(), waitTask: &upload.Task{TaskID: "task-1", Status: upload.StatusFailed}}
	st := &stagingFile{b: root.b, parentID: "root"}
	handle := newTestStagingHandle(t, manager, st, "a.jpg")
	attachStaging(t, root, st, "a.jpg")
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release 不应失败，errno=%v", errno)
	}

	// 给监听协程留出执行时间；失败时节点与临时文件都应保留，可继续本地读。
	time.Sleep(100 * time.Millisecond)
	if root.GetChild("a.jpg") == nil {
		t.Fatal("上传失败时不应移除 staging 节点")
	}
	if st.superseded() {
		t.Fatal("上传失败、临时文件仍在时节点不应失效")
	}
	fh, _, errno := st.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("上传失败后仍应能本地读，errno=%v", errno)
	}
	if r, ok := fh.(fs.FileReleaser); ok {
		_ = r.Release(context.Background())
	}
}

func TestStagingRenameUpdatesNodeAndTask(t *testing.T) {
	manager := &recordingUploadManager{tempDir: t.TempDir()}
	mount := &domain.FuseMount{ID: 1, AccountID: 1}
	dir := &cloudDir{
		b: &backend{deps: Deps{}, mount: mount},
		item: domain.FileItem{
			ID:    "parent-id",
			Name:  "dir",
			IsDir: true,
		},
	}
	st := &stagingFile{
		b:           dir.b,
		name:        "movie.tmp",
		tempPath:    t.TempDir() + "/movie.tmp",
		uploads:     manager,
		taskID:      "task-1",
		parentID:    "parent-id",
		displayPath: "dir",
	}

	if errno := dir.renameStaging(context.Background(), st, dir, "movie.mp4"); errno != 0 {
		t.Fatalf("renameStaging 不应失败，errno=%v", errno)
	}
	if st.name != "movie.mp4" {
		t.Fatalf("节点名应更新为 movie.mp4，实际 %q", st.name)
	}
	if len(manager.renamed) != 1 || manager.renamed[0] != "task-1:movie.mp4" {
		t.Fatalf("应联动更新上传任务名，实际 %v", manager.renamed)
	}
}

func TestStagingUnlinkCancelsTask(t *testing.T) {
	manager := &recordingUploadManager{tempDir: t.TempDir()}
	mount := &domain.FuseMount{ID: 1, AccountID: 1}
	dir := &cloudDir{b: &backend{deps: Deps{}, mount: mount}}
	st := &stagingFile{
		b:        dir.b,
		name:     "movie.tmp",
		tempPath: t.TempDir() + "/movie.tmp",
		uploads:  manager,
		taskID:   "task-1",
	}

	if errno := dir.unlinkStaging(st); errno != 0 {
		t.Fatalf("unlinkStaging 不应失败，errno=%v", errno)
	}
	if len(manager.deleted) != 1 || manager.deleted[0] != "task-1" {
		t.Fatalf("应取消上传任务，实际 %v", manager.deleted)
	}
}
