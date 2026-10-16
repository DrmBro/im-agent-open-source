// Package drafts 保存仅供本机显式恢复的加密草稿快照，不自动恢复或删除快照。
package drafts

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"im-agent/internal/vault"
)

const (
	maxTextBytes    = 64 << 10
	maxFileBytes    = 128 << 10
	maxBindingBytes = 1024
	// 现有 vault 使用 AES-GCM：12 字节 nonce 和 16 字节认证标签。
	cipherOverhead = 28
)

// 所有实例共用锁，避免同一进程中不同 New 调用并发创建主密钥或更新文件。
// 跨进程约定：仅后端写入；桌面进程只能调用 List、Read。
var journalMu sync.Mutex

// Record 仅包含元数据；不得向此结构添加原文、密文或底层错误。
type Record struct {
	ID        string    `json:"id"`
	EventID   string    `json:"event_id"`
	ThreadID  string    `json:"thread_id"`
	State     string    `json:"state"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type snapshot struct {
	Record
	Version    int    `json:"version"`
	Ciphertext []byte `json:"ciphertext"`
}

// Journal 的创建不访问文件、钥匙串或数据库；配置错误延迟到 API 返回。
type Journal struct {
	root    string
	account string
	initErr error
}

// New 使用绝对、清理后的 stateDir 派生独立固定的钥匙串账号。
// 移动目录会改变账号；本包不迁移密钥，也不在密钥丢失时重建已有快照的密钥。
func New(stateDir string) *Journal {
	j := &Journal{}
	if strings.TrimSpace(stateDir) == "" || strings.ContainsRune(stateDir, 0) {
		j.initErr = errors.New("草稿状态目录无效")
		return j
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		j.initErr = errors.New("无法确定草稿状态目录")
		return j
	}
	stateDir = filepath.Clean(stateDir)
	digest := sha256.Sum256([]byte(stateDir))
	j.root = filepath.Join(stateDir, "desktop-drafts")
	j.account = "desktop-drafts-v1-" + hex.EncodeToString(digest[:])
	return j
}

// Save 精确保留原文（包括空字符串、纯空白及首尾空格和换行），不裁剪或归一化。
// 不去重；每次成功调用都产生独立快照。仅成功返回后，调用方才可移走原草稿。
// 返回错误不代表文件必定不存在；同步失败时可能保留已发布的加密备份。
func (j *Journal) Save(eventID, threadID, text string) (Record, error) {
	journalMu.Lock()
	defer journalMu.Unlock()
	if !validBinding(eventID) || !validBinding(threadID) {
		return Record{}, errors.New("草稿事件或会话标识无效")
	}
	if len(text) > maxTextBytes || !utf8.ValidString(text) {
		return Record{}, errors.New("草稿必须为有效 UTF-8 文本且不超过 64 KiB")
	}
	if err := checkPublishSupport(); err != nil {
		return Record{}, err
	}
	dir, err := j.openDir(true)
	if err != nil {
		return Record{}, err
	}
	defer dir.close()
	entries, err := dir.file.ReadDir(-1)
	if err != nil {
		return Record{}, errors.New("无法检查已有草稿快照，不会创建新密钥")
	}
	// 访问钥匙串前再次检查路径，避免接受期间出现的祖先符号链接。
	if err := dir.check(); err != nil {
		return Record{}, err
	}
	// 包括 restored、未知文件和崩溃遗留临时文件；不根据元数据或解密结果猜测可清理性。
	v := vault.New(j.account)
	if err := v.Unlock(len(entries) != 0); err != nil {
		return Record{}, errors.New("草稿凭证库不可用；已有快照需要原主密钥，不会降级明文")
	}
	id, err := randomID()
	if err != nil {
		return Record{}, err
	}
	now := time.Now().UTC()
	s := snapshot{Record: Record{
		ID: id, EventID: eventID, ThreadID: threadID, State: "saved", Reason: "snapshot_saved",
		CreatedAt: now, UpdatedAt: now,
	}, Version: 1}
	s.Ciphertext, err = v.Seal(snapshotScope(s.Record), text)
	if err != nil {
		return Record{}, errors.New("草稿快照加密失败，不会降级明文")
	}
	if err := dir.publish(s, nil); err != nil {
		return Record{}, err
	}
	return s.Record, nil
}

// Mark 仅更新状态和原因代码，不访问钥匙串，不删除或重加密原始密文。
// 原因为空或为最多 64 字节的 ASCII 代码：[a-z][a-z0-9_]*。
// 调用方只能传固定业务代码，禁止传原文、错误消息或其他动态敏感内容。
func (j *Journal) Mark(id, state, reason string) error {
	journalMu.Lock()
	defer journalMu.Unlock()
	if !validID(id) || !validState(state) || !validReason(reason) {
		return errors.New("草稿标识、状态或原因代码无效")
	}
	if err := checkPublishSupport(); err != nil {
		return err
	}
	dir, err := j.openDir(false)
	if err != nil {
		return err
	}
	defer dir.close()
	s, info, err := dir.load(id)
	if err != nil {
		return err
	}
	s.State, s.Reason = state, reason
	updatedAt := time.Now().UTC()
	// 本机时钟回拨时仍保持元数据时间不倒退。
	if updatedAt.Before(s.UpdatedAt) {
		updatedAt = s.UpdatedAt
	}
	s.UpdatedAt = updatedAt
	return dir.publish(s, info)
}

// List 只读取元数据，不解密、不创建目录或主密钥。按创建时间升序，时间相同按 ID 排序。
// 损坏文件、未知文件及遗留临时文件不会阻断其它记录；返回可用记录及固定中文警告。
// 调用方须同时展示记录和警告；异常文件保持不变，不自动晋升、合并、删除或解密恢复。
func (j *Journal) List() ([]Record, error) {
	journalMu.Lock()
	defer journalMu.Unlock()
	dir, err := j.openDir(false)
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.close()
	entries, err := dir.file.ReadDir(-1)
	// ReadDir 返回部分目录项和错误时，也保留其中所有可读的正式记录。
	warning := err != nil
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || !validID(strings.TrimSuffix(name, ".json")) {
			warning = true
			continue
		}
		s, _, err := dir.load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			warning = true
			continue
		}
		records = append(records, s.Record)
	}
	sort.Slice(records, func(a, b int) bool {
		if records[a].CreatedAt.Equal(records[b].CreatedAt) {
			return records[a].ID < records[b].ID
		}
		return records[a].CreatedAt.Before(records[b].CreatedAt)
	})
	if warning {
		return records, errors.New("部分草稿快照无法列出或读取，或存在未完成写入；已返回可用记录，异常文件保持不变，不会自动恢复或清理")
	}
	return records, nil
}

// Read 仅供本机用户显式查看；所有状态都可读取，始终 Unlock(true)，不创建目录或新钥匙。
func (j *Journal) Read(id string) (string, error) {
	journalMu.Lock()
	defer journalMu.Unlock()
	if !validID(id) {
		return "", errors.New("草稿标识无效")
	}
	dir, err := j.openDir(false)
	if err != nil {
		return "", err
	}
	defer dir.close()
	s, _, err := dir.load(id)
	if err != nil {
		return "", err
	}
	if err := dir.check(); err != nil {
		return "", err
	}
	v := vault.New(j.account)
	if err := v.Unlock(true); err != nil {
		return "", errors.New("草稿凭证库不可用；请恢复或解锁原主密钥，不会创建新密钥")
	}
	text, err := v.Open(snapshotScope(s.Record), s.Ciphertext)
	if err != nil {
		return "", errors.New("草稿快照解密失败；请检查原主密钥及快照完整性")
	}
	if len(text) > maxTextBytes || !utf8.ValidString(text) {
		return "", errors.New("草稿快照文本格式或大小无效")
	}
	return text, nil
}

// 使用 JSON 数组编码绑定，避免简单拼接引入分隔符歧义。
func snapshotScope(r Record) string {
	data, _ := json.Marshal([4]string{"desktop-drafts/v1", r.ID, r.EventID, r.ThreadID})
	return string(data)
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validBinding(value string) bool {
	if len(value) == 0 || len(value) > maxBindingBytes || !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

func validState(state string) bool {
	switch state {
	case "saved", "suspended", "retained", "restored":
		return true
	}
	return false
}

func validReason(reason string) bool {
	if len(reason) > 64 {
		return false
	}
	for i, c := range reason {
		if !(c >= 'a' && c <= 'z' || i > 0 && (c >= '0' && c <= '9' || c == '_')) {
			return false
		}
	}
	return true
}

func randomID() (string, error) {
	var buf [16]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return "", errors.New("无法生成草稿快照随机标识")
	}
	return hex.EncodeToString(buf[:]), nil
}

type journalDir struct {
	path string
	file *os.File
	root *os.Root
	info os.FileInfo
}

func (d *journalDir) close() {
	d.root.Close()
	d.file.Close()
}

func (j *Journal) openDir(create bool) (*journalDir, error) {
	if j == nil || j.initErr != nil || j.root == "" {
		return nil, errors.New("草稿状态目录配置无效")
	}
	// 首次创建前也检查 stateDir 及其祖先，不能等到跟随链接写入后才拒绝。
	if err := checkDirectoryPath(j.root); err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(j.root), 0700); err != nil {
			return nil, errors.New("无法准备草稿状态目录")
		}
		if err := checkDirectoryPath(j.root); err != nil {
			return nil, err
		}
		if err := os.Mkdir(j.root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, errors.New("无法创建草稿快照目录")
		}
		if err := checkDirectoryPath(j.root); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(j.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errMissing
	}
	if err != nil || !safeDir(info) {
		return nil, errors.New("草稿目录必须为非符号链接的 0700 目录")
	}
	file, err := os.Open(j.root)
	if err != nil {
		return nil, errors.New("无法打开草稿快照目录")
	}
	opened, err := file.Stat()
	if err != nil || !safeDir(opened) || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("草稿快照目录在打开时发生变化")
	}
	root, err := os.OpenRoot(j.root)
	if err != nil {
		file.Close()
		return nil, errors.New("无法建立草稿快照目录边界")
	}
	d := &journalDir{path: j.root, file: file, root: root, info: opened}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, rootInfo) || d.check() != nil {
		d.close()
		return nil, errors.New("草稿快照目录在打开时发生变化")
	}
	if create {
		// 同步父目录，确保首次创建的 desktop-drafts 目录项已持久化。
		parent, err := os.Open(filepath.Dir(j.root))
		if err != nil {
			d.close()
			return nil, errors.New("无法同步草稿状态目录")
		}
		err = parent.Sync()
		closeErr := parent.Close()
		if err != nil || closeErr != nil {
			d.close()
			return nil, errors.New("无法同步草稿状态目录")
		}
	}
	return d, nil
}

// 保持 errors.Is(..., os.ErrNotExist) 语义，同时不暴露路径和系统错误。
var errMissing = missingError{}

type missingError struct{}

func (missingError) Error() string        { return "草稿快照目录或记录不存在" }
func (missingError) Is(target error) bool { return target == os.ErrNotExist }

func safeDir(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0700
}

func safeFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && info.Size() > 0 && info.Size() <= maxFileBytes
}

// checkDirectoryPath 从文件系统根向下检查绝对路径的已有目录分量，拒绝任何
// 符号链接（包括悬空链接）或非目录；遇到缺失分量即返回，由调用方按原规则
// 创建目录或报告不存在。不能用 EvalSymlinks 将链接静默转换成可接受路径。
// 这是创建前后及操作前的校验，不是祖先目录的 fd 锚定：无法消除恶意进程在
// 检查间隙替换祖先目录的 TOCTOU；要求状态目录及祖先不被不可信进程并发改名。
// 快照发布仍使用现有目录 fd，不增加绝对路径发布回退。
func checkDirectoryPath(path string) error {
	var paths []string
	for {
		paths = append(paths, path)
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	for i := len(paths) - 1; i >= 0; i-- {
		info, err := os.Lstat(paths[i])
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return errors.New("无法检查草稿状态目录及其祖先")
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("草稿状态目录及其祖先必须为非符号链接的目录")
		}
	}
	return nil
}

func (d *journalDir) check() error {
	if err := checkDirectoryPath(d.path); err != nil {
		return err
	}
	info, err := os.Lstat(d.path)
	if err != nil || !safeDir(info) || !os.SameFile(d.info, info) {
		return errors.New("草稿快照目录发生变化，已停止操作")
	}
	return nil
}

func (d *journalDir) load(id string) (snapshot, os.FileInfo, error) {
	var s snapshot
	if !validID(id) {
		return s, nil, errors.New("草稿标识无效")
	}
	if err := d.check(); err != nil {
		return s, nil, err
	}
	name := id + ".json"
	info, err := d.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil, errMissing
	}
	if err != nil || !safeFile(info) {
		return s, nil, errors.New("草稿文件必须为大小受限、非符号链接的 0600 普通文件")
	}
	f, err := d.root.Open(name)
	if err != nil {
		return s, nil, errors.New("无法读取草稿快照文件")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !safeFile(opened) || !os.SameFile(info, opened) {
		return s, nil, errors.New("草稿快照文件在打开时发生变化")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(data) > maxFileBytes {
		return s, nil, errors.New("草稿快照读取失败或文件超过大小限制")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return snapshot{}, nil, errors.New("草稿快照文件格式无效，已保留原文件")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || s.Version != 1 || s.ID != id ||
		!validBinding(s.EventID) || !validBinding(s.ThreadID) || !validState(s.State) || !validReason(s.Reason) ||
		s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) ||
		len(s.Ciphertext) < cipherOverhead || len(s.Ciphertext) > maxTextBytes+cipherOverhead {
		return snapshot{}, nil, errors.New("草稿快照元数据或密文格式无效，已保留原文件")
	}
	return s, opened, nil
}

// previous 非空时仅替换已读取的记录；为空时用硬链接原子发布，绝不覆盖同名文件。
func (d *journalDir) publish(s snapshot, previous os.FileInfo) error {
	data, err := json.Marshal(s)
	if err != nil || len(data) > maxFileBytes {
		return errors.New("无法编码草稿快照或文件超过大小限制")
	}
	if err := d.check(); err != nil {
		return err
	}
	tempID, err := randomID()
	if err != nil {
		return err
	}
	tempName := ".pending-" + tempID + ".tmp"
	f, err := d.root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("无法创建草稿快照临时文件")
	}
	// 未发布时仅清理本次创建的临时文件；一旦发布，失败时保留可用备份。
	published := false
	defer func() {
		f.Close()
		if !published {
			d.root.Remove(tempName)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return errors.New("无法设置草稿快照文件权限")
	}
	if n, err := f.Write(data); err != nil || n != len(data) {
		return errors.New("草稿快照写入失败，原草稿不得移走")
	}
	if err := f.Sync(); err != nil {
		return errors.New("草稿快照同步失败，原草稿不得移走")
	}
	written, err := f.Stat()
	if err != nil || !safeFile(written) {
		return errors.New("草稿快照临时文件属性无效")
	}
	if err := f.Close(); err != nil {
		return errors.New("草稿快照关闭失败，原草稿不得移走")
	}
	if err := d.check(); err != nil {
		return err
	}
	temporary, err := d.root.Lstat(tempName)
	if err != nil || !safeFile(temporary) || !os.SameFile(written, temporary) {
		return errors.New("草稿快照临时文件发生变化，已停止发布")
	}
	name := s.ID + ".json"
	if previous != nil {
		current, err := d.root.Lstat(name)
		if err != nil || !safeFile(current) || !os.SameFile(previous, current) {
			return errors.New("草稿快照已发生变化，未更新状态")
		}
	}
	// 两端均锚定已打开且与 os.Root 核对一致的目录 fd，不重新解析绝对路径。
	if err := publishAt(d.file, tempID, s.ID, previous != nil); err != nil {
		return err
	}
	published = true
	if err := d.check(); err != nil {
		return err
	}
	committed, err := d.root.Lstat(name)
	if err != nil || !safeFile(committed) || !os.SameFile(written, committed) {
		return errors.New("草稿快照在发布时发生变化；请保留原草稿")
	}
	if err := d.file.Sync(); err != nil {
		return errors.New("草稿快照目录同步失败，文件可能已保存；请保留原草稿")
	}
	if previous == nil {
		if err := d.check(); err != nil {
			return err
		}
		if err := d.root.Remove(tempName); err != nil {
			return errors.New("草稿快照已保存但临时文件清理失败；请保留原草稿")
		}
		if err := d.file.Sync(); err != nil {
			return errors.New("草稿快照已保存但目录同步未完成；请保留原草稿")
		}
	}
	return nil
}
