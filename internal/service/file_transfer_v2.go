package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"letshare-server/internal/model"
)

const (
	FileTransferV2BlockSize       = 256 * 1024
	FileTransferV2MaxFramePayload = 16 * 1024
	FileTransferV2MaxCredit       = 8 * 1024 * 1024
	fileTransferV2DefaultCredit   = 1 * 1024 * 1024
	// Persisted resumable and terminal receipts remain available for one day.
	fileTransferV2TaskTTL          = 24 * time.Hour
	fileTransferV2PerTaskSpool     = 64 * 1024 * 1024
	fileTransferV2GlobalSpool      = 512 * 1024 * 1024
	fileTransferV2MaxPerTaskSpool  = 512 * 1024 * 1024
	fileTransferV2MaxGlobalSpool   = 4 * 1024 * 1024 * 1024
	fileTransferV2MaxTasks         = 1024
	fileTransferV2MaxPendingFrames = 65536
	fileTransferV2MaxBlockFrames   = 1024
	fileTransferV2MaxTaskMetadata  = 1 * 1024 * 1024
	fileTransferV2MaxMetadata      = 128 * 1024 * 1024
)

var (
	ErrFileTransferV2NotFound = errors.New("file v2 transfer not found")
	ErrFileTransferV2Quota    = errors.New("file v2 relay spool quota exceeded; retry after receiver checkpoint")
	ErrFileTransferV2Frames   = errors.New("too many pending LSF2 fragments; resend the block with fewer, larger frames")
)

type FileTransferV2Session struct {
	Manifest   model.FileTransferV2Manifest `json:"manifest"`
	From       string                       `json:"from"`
	To         string                       `json:"to"`
	Room       string                       `json:"room"`
	Status     string                       `json:"status"`
	Checkpoint [][]int                      `json:"checkpoint,omitempty"`
	Blocks     map[int]string               `json:"blocks,omitempty"`
	ExpiresAt  time.Time                    `json:"expiresAt"`
	UpdatedAt  time.Time                    `json:"updatedAt"`
}

type v2BlockAssembly struct {
	total  int
	hash   string
	pieces map[int][]byte
	bytes  int64
	frames int
}

type v2TaskSpoolUsage struct {
	committedBytes int64
	pendingBytes   int64
}

// FileTransferV2Service persists task metadata and complete relay blocks below
// the existing private relay spool root. Partially received frames are bounded
// in memory and can be retransmitted after process restart.
type FileTransferV2Service struct {
	mu            sync.Mutex
	base          *FileTransferService
	root          string
	loaded        bool
	tasks         map[string]*FileTransferV2Session
	pending       map[string]map[int]*v2BlockAssembly
	taskSpool     map[string]v2TaskSpoolUsage
	spoolBytes    int64
	pendingFrames int
	metadataBytes int64
	perTaskMax    int64
	globalMax     int64
}

func NewFileTransferV2Service(base *FileTransferService) *FileTransferV2Service {
	root := filepath.Join(os.TempDir(), "letshare-relay-spool", "v2")
	if base != nil && base.spoolDir != "" {
		root = filepath.Join(base.spoolDir, "v2")
	}
	return &FileTransferV2Service{
		base: base, root: root,
		tasks:      make(map[string]*FileTransferV2Session),
		pending:    make(map[string]map[int]*v2BlockAssembly),
		taskSpool:  make(map[string]v2TaskSpoolUsage),
		perTaskMax: boundedV2SpoolEnv("LETSHARE_FILE_V2_SPOOL_PER_TRANSFER", fileTransferV2PerTaskSpool, fileTransferV2MaxPerTaskSpool),
		globalMax:  boundedV2SpoolEnv("LETSHARE_FILE_V2_SPOOL_GLOBAL", fileTransferV2GlobalSpool, fileTransferV2MaxGlobalSpool),
	}
}

func boundedV2SpoolEnv(name string, fallback, maximum int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || value < 1 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func (s *FileTransferV2Service) SetRoot(root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded && strings.TrimSpace(root) != "" {
		s.root = root
	}
}

func (s *FileTransferV2Service) taskDir(transferID string) string {
	sum := sha256.Sum256([]byte(transferID))
	return filepath.Join(s.root, hex.EncodeToString(sum[:]))
}

func (s *FileTransferV2Service) blockPath(transferID string, index int) string {
	return filepath.Join(s.taskDir(transferID), "blocks", fmt.Sprintf("block_%08d.bin", index))
}

func (s *FileTransferV2Service) loadLocked() error {
	if s.loaded {
		return nil
	}
	s.tasks = make(map[string]*FileTransferV2Session)
	s.pending = make(map[string]map[int]*v2BlockAssembly)
	s.taskSpool = make(map[string]v2TaskSpoolUsage)
	s.spoolBytes = 0
	s.pendingFrames = 0
	s.metadataBytes = 0
	if err := os.MkdirAll(s.root, relaySpoolDirPerm); err != nil {
		return fmt.Errorf("create file v2 spool root: %w", err)
	}
	if err := os.Chmod(s.root, relaySpoolDirPerm); err != nil {
		return fmt.Errorf("secure file v2 spool root: %w", err)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("read file v2 spool root: %w", err)
	}
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			_ = os.Remove(filepath.Join(s.root, entry.Name()))
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		taskMetadataPath := filepath.Join(dir, "task.json")
		metadataInfo, statErr := os.Stat(taskMetadataPath)
		if statErr != nil || !metadataInfo.Mode().IsRegular() {
			_ = os.RemoveAll(dir)
			continue
		}
		if metadataInfo.Size() > fileTransferV2MaxTaskMetadata {
			_ = os.RemoveAll(dir)
			continue
		}
		if len(s.tasks) >= fileTransferV2MaxTasks || s.metadataBytes+metadataInfo.Size() > fileTransferV2MaxMetadata {
			s.resetLoadLocked()
			return errors.New("file v2 task metadata quota exceeded")
		}
		data, readErr := os.ReadFile(taskMetadataPath)
		if readErr != nil {
			_ = os.RemoveAll(dir)
			continue
		}
		var task FileTransferV2Session
		if json.Unmarshal(data, &task) != nil || !IsValidFileTransferV2ID(task.Manifest.TransferID) || filepath.Base(s.taskDir(task.Manifest.TransferID)) != entry.Name() {
			_ = os.RemoveAll(dir)
			continue
		}
		if validateFileTransferV2Manifest(task.Manifest) != nil || task.From == "" || task.To == "" || task.From == task.To || task.Room == "" || !validV2Status(task.Status) || validateV2RangesLimit(task.Checkpoint, task.Manifest.TotalBlocks, task.Manifest.TotalBlocks) != nil {
			_ = os.RemoveAll(dir)
			continue
		}
		if !task.ExpiresAt.After(now) {
			_ = os.RemoveAll(dir)
			continue
		}
		if task.Blocks == nil {
			task.Blocks = make(map[int]string)
		}
		blocksDir := filepath.Join(dir, "blocks")
		if err := os.MkdirAll(blocksDir, relaySpoolDirPerm); err != nil {
			s.resetLoadLocked()
			return fmt.Errorf("create file v2 block directory: %w", err)
		}
		if err := os.Chmod(blocksDir, relaySpoolDirPerm); err != nil {
			s.resetLoadLocked()
			return fmt.Errorf("secure file v2 block directory: %w", err)
		}
		blockEntries, err := os.ReadDir(blocksDir)
		if err != nil {
			s.resetLoadLocked()
			return fmt.Errorf("read file v2 block directory: %w", err)
		}
		validNames := make(map[string]bool, len(task.Blocks))
		for idx, hash := range task.Blocks {
			path := s.blockPath(task.Manifest.TransferID, idx)
			info, statErr := os.Stat(path)
			expectedLength := expectedV2BlockLength(task, idx)
			if idx < 0 || idx >= task.Manifest.TotalBlocks || !isV2SHA256(hash) || statErr != nil || !info.Mode().IsRegular() || info.Size() != int64(expectedLength) {
				delete(task.Blocks, idx)
				_ = os.Remove(path)
				continue
			}
			blockData, readErr := os.ReadFile(path)
			if readErr != nil {
				delete(task.Blocks, idx)
				_ = os.Remove(path)
				continue
			}
			blockHash := sha256.Sum256(blockData)
			if !strings.EqualFold(hex.EncodeToString(blockHash[:]), hash) {
				delete(task.Blocks, idx)
				_ = os.Remove(path)
				continue
			}
			validNames[filepath.Base(path)] = true
			s.spoolBytes += info.Size()
			usage := s.taskSpool[task.Manifest.TransferID]
			usage.committedBytes += info.Size()
			s.taskSpool[task.Manifest.TransferID] = usage
		}
		for _, blockEntry := range blockEntries {
			if !validNames[blockEntry.Name()] {
				_ = os.RemoveAll(filepath.Join(blocksDir, blockEntry.Name()))
			}
		}
		if len(s.tasks) >= fileTransferV2MaxTasks {
			s.resetLoadLocked()
			return errors.New("file v2 task metadata quota exceeded")
		}
		s.tasks[task.Manifest.TransferID] = &task
		s.metadataBytes += int64(len(data))
		if err := s.persistLocked(&task); err != nil {
			s.resetLoadLocked()
			return err
		}
	}
	if len(s.tasks) > fileTransferV2MaxTasks {
		s.resetLoadLocked()
		return errors.New("file v2 task metadata quota exceeded")
	}
	s.loaded = true
	return nil
}

func (s *FileTransferV2Service) resetLoadLocked() {
	s.tasks = make(map[string]*FileTransferV2Session)
	s.pending = make(map[string]map[int]*v2BlockAssembly)
	s.taskSpool = make(map[string]v2TaskSpoolUsage)
	s.spoolBytes = 0
	s.pendingFrames = 0
	s.metadataBytes = 0
}

func (s *FileTransferV2Service) persistLocked(task *FileTransferV2Session) error {
	dir := s.taskDir(task.Manifest.TransferID)
	if err := os.MkdirAll(dir, relaySpoolDirPerm); err != nil {
		return fmt.Errorf("create file v2 transfer directory: %w", err)
	}
	if err := os.Chmod(dir, relaySpoolDirPerm); err != nil {
		return fmt.Errorf("secure file v2 transfer directory: %w", err)
	}
	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("encode file v2 transfer: %w", err)
	}
	if len(data) > fileTransferV2MaxTaskMetadata {
		return errors.New("file v2 task metadata quota exceeded")
	}
	taskPath := filepath.Join(dir, "task.json")
	oldSize := int64(0)
	if info, statErr := os.Stat(taskPath); statErr == nil {
		oldSize = info.Size()
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect file v2 task metadata: %w", statErr)
	}
	newTotal := s.metadataBytes - oldSize + int64(len(data))
	if newTotal > fileTransferV2MaxMetadata {
		return errors.New("file v2 global task metadata quota exceeded")
	}
	tmp := filepath.Join(dir, "task.json.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, relaySpoolFilePerm)
	if err != nil {
		return fmt.Errorf("create file v2 task temporary: %w", err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write file v2 task: %w", err)
	}
	if err := os.Rename(tmp, taskPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit file v2 task: %w", err)
	}
	s.metadataBytes = newTotal
	return nil
}

func (s *FileTransferV2Service) Create(manifest model.FileTransferV2Manifest, from, to, room string, isPro bool) error {
	if err := validateFileTransferV2Manifest(manifest); err != nil {
		return err
	}
	manifest.FileHash = strings.ToLower(manifest.FileHash)
	if s.base == nil {
		return errors.New("file transfer policy unavailable")
	}
	if !isPro && manifest.Size > NonProSizeLimit {
		return fmt.Errorf("file exceeds non-PRO limit of %d bytes", NonProSizeLimit)
	}
	if manifest.Size > s.base.maxFileSize {
		return fmt.Errorf("file exceeds configured maximum of %d bytes", s.base.maxFileSize)
	}
	if from == "" || to == "" || from == to || room == "" {
		return errors.New("file v2 transfer requires distinct participants and a room")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if err := s.cleanupExpiredLocked(time.Now()); err != nil {
		return err
	}
	if old := s.tasks[manifest.TransferID]; old != nil {
		if old.From == from && old.To == to && old.Room == room && old.Manifest == manifest && old.Status != "cancelled" && old.Status != "rejected" {
			return nil
		}
		return errors.New("transfer id already belongs to another task")
	}
	if len(s.tasks) >= fileTransferV2MaxTasks {
		return errors.New("file v2 task metadata quota exceeded; retry after older tasks expire")
	}
	now := time.Now()
	task := &FileTransferV2Session{
		Manifest: manifest, From: from, To: to, Room: room,
		Status: "offered", Blocks: make(map[int]string),
		ExpiresAt: now.Add(fileTransferV2TaskTTL), UpdatedAt: now,
	}
	if err := s.persistLocked(task); err != nil {
		return err
	}
	s.tasks[manifest.TransferID] = task
	s.taskSpool[manifest.TransferID] = v2TaskSpoolUsage{}
	return nil
}

func validateFileTransferV2Manifest(m model.FileTransferV2Manifest) error {
	if !IsValidFileTransferV2ID(m.TransferID) {
		return errors.New("invalid transferId")
	}
	if m.Name == "" || len(m.Name) > 1024 || !utf8.ValidString(m.Name) || hasV2ControlRune(m.Name) {
		return errors.New("invalid file name")
	}
	if m.Size < 0 || m.Size > 3*1024*1024*1024 {
		return errors.New("file size is outside the supported range")
	}
	if m.BlockSize != FileTransferV2BlockSize {
		return errors.New("unsupported blockSize")
	}
	blocks := int64(1)
	if m.Size > 0 {
		blocks = (m.Size + int64(m.BlockSize) - 1) / int64(m.BlockSize)
	}
	if int64(m.TotalBlocks) != blocks {
		return errors.New("totalBlocks does not match size and blockSize")
	}
	if m.Type == "" || len(m.Type) > 255 || !utf8.ValidString(m.Type) || hasV2ControlRune(m.Type) || m.LastModified < 0 || m.LastModified > maxSafeJSInteger || m.CreatedAt < 0 || m.CreatedAt > maxSafeJSInteger {
		return errors.New("invalid manifest metadata")
	}
	hash, err := hex.DecodeString(m.FileHash)
	if err != nil || len(hash) != sha256.Size {
		return errors.New("fileHash must be a SHA-256 hex digest")
	}
	return nil
}

const maxSafeJSInteger int64 = 1<<53 - 1

func expectedV2BlockLength(task FileTransferV2Session, index int) int {
	if index < 0 || index >= task.Manifest.TotalBlocks || task.Manifest.Size == 0 {
		return 0
	}
	if index == task.Manifest.TotalBlocks-1 {
		return int(task.Manifest.Size - int64(index*FileTransferV2BlockSize))
	}
	return FileTransferV2BlockSize
}

func isV2SHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validV2Status(status string) bool {
	switch status {
	case "offered", "accepted", "transferring", "rejected", "cancelled", "completed":
		return true
	default:
		return false
	}
}

func isV2TerminalStatus(status string) bool {
	switch status {
	case "rejected", "cancelled", "completed":
		return true
	default:
		return false
	}
}

// fileTransferV2TransitionAllowed is the authoritative persisted state
// machine. Callers must not rely on a prior Get snapshot to authorize a
// transition: every transition is checked again while holding s.mu.
func fileTransferV2TransitionAllowed(from, to string) bool {
	if from == to {
		return from == "accepted" || from == "transferring" || isV2TerminalStatus(from)
	}
	switch from {
	case "offered":
		return to == "accepted" || to == "rejected" || to == "cancelled"
	case "accepted":
		return to == "transferring" || to == "completed" || to == "cancelled"
	case "transferring":
		return to == "completed" || to == "cancelled"
	default:
		return false
	}
}

func hasV2ControlRune(value string) bool {
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return true
		}
	}
	return false
}

func IsValidFileTransferV2ID(id string) bool {
	if len(id) < 8 || len(id) > 96 {
		return false
	}
	for _, ch := range id {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func (s *FileTransferV2Service) Get(transferID string) (*FileTransferV2Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	task := s.tasks[transferID]
	if task == nil || !task.ExpiresAt.After(time.Now()) {
		return nil, ErrFileTransferV2NotFound
	}
	snapshot := *task
	snapshot.Blocks = make(map[int]string, len(task.Blocks))
	for index, hash := range task.Blocks {
		snapshot.Blocks[index] = hash
	}
	snapshot.Checkpoint = cloneV2Ranges(task.Checkpoint)
	return &snapshot, nil
}

func (s *FileTransferV2Service) SetStatus(transferID, status string, clearBlocks bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	task := s.tasks[transferID]
	if task == nil {
		return ErrFileTransferV2NotFound
	}
	if !validV2Status(status) {
		return fmt.Errorf("invalid file v2 status %q", status)
	}
	if task.Status == status && isV2TerminalStatus(status) {
		// Terminal retries are idempotent receipts. In particular, a repeated
		// complete can be forwarded without re-running cleanup or touching the
		// receipt's expiry/status.
		return nil
	}
	if task.Status == "transferring" && status == "accepted" {
		// A reconnect or a second path may replay the receiver's accept after
		// the sender has already delivered relay blocks. Treat that accept as
		// an atomic no-op so it cannot downgrade the active task.
		if clearBlocks {
			return errors.New("nonterminal file v2 acceptance cannot clear relay payload")
		}
		return nil
	}
	if !fileTransferV2TransitionAllowed(task.Status, status) {
		return fmt.Errorf("invalid file v2 status transition %s -> %s", task.Status, status)
	}
	if clearBlocks != isV2TerminalStatus(status) {
		return errors.New("file v2 terminal transitions must clear relay payload")
	}
	oldStatus, oldBlocks := task.Status, task.Blocks
	if clearBlocks {
		task.Blocks = make(map[int]string)
	}
	task.Status = status
	touchFileTransferV2Task(task)
	if err := s.persistLocked(task); err != nil {
		task.Status, task.Blocks = oldStatus, oldBlocks
		return err
	}
	if clearBlocks {
		removedBytes := int64(0)
		for index := range oldBlocks {
			path := s.blockPath(transferID, index)
			if info, statErr := os.Stat(path); statErr == nil {
				if err := os.Remove(path); err == nil {
					removedBytes += info.Size()
				}
			} else if os.IsNotExist(statErr) {
				// Reconcile a block removed outside this process against the
				// previously loaded counter.
				removedBytes += int64(expectedV2BlockLength(*task, index))
			}
		}
		removedPending := int64(0)
		for _, pending := range s.pending[transferID] {
			removedPending += pending.bytes
			s.pendingFrames -= pending.frames
		}
		usage := s.taskSpool[transferID]
		usage.committedBytes = subtractV2Bytes(usage.committedBytes, removedBytes)
		usage.pendingBytes = subtractV2Bytes(usage.pendingBytes, removedPending)
		s.taskSpool[transferID] = usage
		s.spoolBytes = subtractV2Bytes(s.spoolBytes, removedBytes+removedPending)
		delete(s.pending, transferID)
	}
	return nil
}

func (s *FileTransferV2Service) Checkpoint(transferID string, ranges [][]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	task := s.tasks[transferID]
	if task == nil {
		return ErrFileTransferV2NotFound
	}
	if task.Status != "offered" && task.Status != "accepted" && task.Status != "transferring" {
		return fmt.Errorf("file v2 checkpoint is not allowed in %s state", task.Status)
	}
	if err := validateV2Ranges(ranges, task.Manifest.TotalBlocks); err != nil {
		return err
	}
	previous, previousBlocks := task.Checkpoint, task.Blocks
	mergedRanges := mergeV2Ranges(previous, ranges)
	task.Checkpoint = mergedRanges
	task.Blocks = make(map[int]string, len(previousBlocks))
	for index, hash := range previousBlocks {
		if v2RangeContains(mergedRanges, index) {
			continue
		}
		task.Blocks[index] = hash
	}
	touchFileTransferV2Task(task)
	if err := s.persistLocked(task); err != nil {
		task.Checkpoint, task.Blocks = previous, previousBlocks
		return err
	}
	removedBytes, removedPending := int64(0), int64(0)
	var cleanupErr error
	for index := range previousBlocks {
		if v2RangeContains(mergedRanges, index) {
			path := s.blockPath(transferID, index)
			if info, statErr := os.Stat(path); statErr == nil {
				if err := os.Remove(path); err == nil {
					removedBytes += info.Size()
				} else {
					task.Blocks[index] = previousBlocks[index]
					if persistErr := s.persistLocked(task); persistErr != nil && cleanupErr == nil {
						cleanupErr = persistErr
					}
				}
			} else if os.IsNotExist(statErr) {
				removedBytes += int64(expectedV2BlockLength(*task, index))
			}
		}
	}
	if pending := s.pending[transferID]; pending != nil {
		for index, assembly := range pending {
			if v2RangeContains(mergedRanges, index) {
				removedPending += assembly.bytes
				s.pendingFrames -= assembly.frames
				delete(pending, index)
			}
		}
	}
	usage := s.taskSpool[transferID]
	usage.committedBytes = subtractV2Bytes(usage.committedBytes, removedBytes)
	usage.pendingBytes = subtractV2Bytes(usage.pendingBytes, removedPending)
	s.taskSpool[transferID] = usage
	s.spoolBytes = subtractV2Bytes(s.spoolBytes, removedBytes+removedPending)
	return cleanupErr
}

func subtractV2Bytes(current, released int64) int64 {
	if released >= current {
		return 0
	}
	return current - released
}

// AddFragment assembles one logical block. Complete blocks are atomically
// spooled and returned for forwarding; incomplete frames are never ACKed.
func (s *FileTransferV2Service) AddFragment(transferID string, index, offset, totalLength int, hash string, payload []byte) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, false, err
	}
	task := s.tasks[transferID]
	if task == nil {
		return nil, false, ErrFileTransferV2NotFound
	}
	if !task.ExpiresAt.After(time.Now()) || task.Status == "cancelled" || task.Status == "rejected" || task.Status == "completed" {
		return nil, false, errors.New("file v2 transfer is not active")
	}
	if task.Status != "accepted" && task.Status != "transferring" {
		return nil, false, errors.New("file v2 transfer has not been accepted")
	}
	if index < 0 || index >= task.Manifest.TotalBlocks || offset < 0 || len(payload) > FileTransferV2MaxFramePayload || offset > totalLength || len(payload) > totalLength-offset || (len(payload) == 0 && totalLength > 0) {
		return nil, false, errors.New("invalid LSF2 block bounds")
	}
	expected := expectedV2BlockLength(*task, index)
	if totalLength != expected || (totalLength == 0 && (offset != 0 || len(payload) != 0)) {
		return nil, false, errors.New("LSF2 totalLength does not match manifest")
	}
	decodedHash, err := hex.DecodeString(hash)
	if err != nil || len(decodedHash) != sha256.Size {
		return nil, false, errors.New("LSF2 hash must be a SHA-256 hex digest")
	}
	// The receiver's durable checkpoint is authoritative. Late retries for an
	// already committed block must never be replayed or re-spooled.
	if v2RangeContains(task.Checkpoint, index) {
		return nil, false, nil
	}
	if prior, ok := task.Blocks[index]; ok {
		if !strings.EqualFold(prior, hash) {
			return nil, false, errors.New("LSF2 block conflicts with committed block")
		}
		if offset != 0 {
			return nil, false, nil
		}
		data, readErr := os.ReadFile(s.blockPath(transferID, index))
		if readErr != nil {
			return nil, false, fmt.Errorf("read spooled LSF2 block: %w", readErr)
		}
		return data, true, nil
	}
	byIndex := s.pending[transferID]
	if byIndex == nil {
		byIndex = make(map[int]*v2BlockAssembly)
		s.pending[transferID] = byIndex
	}
	assembly := byIndex[index]
	if assembly == nil {
		assembly = &v2BlockAssembly{total: totalLength, hash: strings.ToLower(hash), pieces: make(map[int][]byte)}
		byIndex[index] = assembly
	} else if assembly.total != totalLength || !strings.EqualFold(assembly.hash, hash) {
		return nil, false, errors.New("LSF2 block frame metadata changed")
	}
	newFragment := true
	if prior, ok := assembly.pieces[offset]; ok {
		if !bytes.Equal(prior, payload) {
			return nil, false, errors.New("conflicting duplicate LSF2 frame")
		}
		newFragment = false
	} else {
		for existingOffset, piece := range assembly.pieces {
			if offset < existingOffset+len(piece) && existingOffset < offset+len(payload) {
				return nil, false, errors.New("overlapping LSF2 frame payload")
			}
		}
	}
	if newFragment {
		if assembly.frames >= fileTransferV2MaxBlockFrames || s.pendingFrames >= fileTransferV2MaxPendingFrames {
			s.discardAssemblyLocked(transferID, index, assembly)
			return nil, false, ErrFileTransferV2Frames
		}
		usage := s.taskSpool[transferID]
		newBytes := int64(len(payload))
		if usage.committedBytes+usage.pendingBytes+newBytes > s.perTaskMax || s.spoolBytes+newBytes > s.globalMax {
			// A rejected first fragment must not leave an empty per-block
			// assembly behind. Existing partial assemblies remain available for
			// a safe retry after the receiver frees spool space.
			if assembly.frames == 0 {
				s.discardAssemblyLocked(transferID, index, assembly)
			}
			return nil, false, ErrFileTransferV2Quota
		}
		assembly.pieces[offset] = append([]byte(nil), payload...)
		assembly.bytes += newBytes
		assembly.frames++
		s.pendingFrames++
		s.spoolBytes += newBytes
		usage.pendingBytes += newBytes
		s.taskSpool[transferID] = usage
	}
	if totalLength == 0 {
		return s.commitBlockLocked(task, index, assembly, nil)
	}
	pieces := make([]int, 0, len(assembly.pieces))
	for start := range assembly.pieces {
		pieces = append(pieces, start)
	}
	sort.Ints(pieces)
	covered := 0
	for _, start := range pieces {
		if start != covered {
			return nil, false, nil
		}
		covered += len(assembly.pieces[start])
	}
	if covered != totalLength {
		return nil, false, nil
	}
	block := make([]byte, totalLength)
	for _, start := range pieces {
		copy(block[start:], assembly.pieces[start])
	}
	return s.commitBlockLocked(task, index, assembly, block)
}

func (s *FileTransferV2Service) commitBlockLocked(task *FileTransferV2Session, index int, assembly *v2BlockAssembly, block []byte) ([]byte, bool, error) {
	actualHash := sha256.Sum256(block)
	if !strings.EqualFold(hex.EncodeToString(actualHash[:]), assembly.hash) {
		s.discardAssemblyLocked(task.Manifest.TransferID, index, assembly)
		return nil, false, errors.New("LSF2 logical block hash mismatch")
	}
	blocksDir := filepath.Join(s.taskDir(task.Manifest.TransferID), "blocks")
	if err := os.MkdirAll(blocksDir, relaySpoolDirPerm); err != nil {
		return nil, false, fmt.Errorf("create LSF2 block spool: %w", err)
	}
	if err := os.Chmod(blocksDir, relaySpoolDirPerm); err != nil {
		return nil, false, fmt.Errorf("secure LSF2 block spool: %w", err)
	}
	path := s.blockPath(task.Manifest.TransferID, index)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, relaySpoolFilePerm)
	if err != nil {
		return nil, false, fmt.Errorf("create LSF2 block temporary: %w", err)
	}
	if _, err = f.Write(block); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return nil, false, fmt.Errorf("commit LSF2 block: %w", err)
	}
	previousStatus := task.Status
	task.Blocks[index] = strings.ToLower(assembly.hash)
	task.Status = "transferring"
	touchFileTransferV2Task(task)
	if err := s.persistLocked(task); err != nil {
		delete(task.Blocks, index)
		task.Status = previousStatus
		_ = os.Remove(path)
		s.discardAssemblyLocked(task.Manifest.TransferID, index, assembly)
		return nil, false, err
	}
	delete(s.pending[task.Manifest.TransferID], index)
	s.pendingFrames -= assembly.frames
	usage := s.taskSpool[task.Manifest.TransferID]
	usage.pendingBytes = subtractV2Bytes(usage.pendingBytes, assembly.bytes)
	usage.committedBytes += int64(len(block))
	s.taskSpool[task.Manifest.TransferID] = usage
	return block, true, nil
}

func (s *FileTransferV2Service) discardAssemblyLocked(transferID string, index int, assembly *v2BlockAssembly) {
	if pending := s.pending[transferID]; pending != nil {
		delete(pending, index)
		if len(pending) == 0 {
			delete(s.pending, transferID)
		}
	}
	usage := s.taskSpool[transferID]
	usage.pendingBytes = subtractV2Bytes(usage.pendingBytes, assembly.bytes)
	s.taskSpool[transferID] = usage
	s.spoolBytes = subtractV2Bytes(s.spoolBytes, assembly.bytes)
	s.pendingFrames -= assembly.frames
}

func (s *FileTransferV2Service) CleanupExpired(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	return s.cleanupExpiredLocked(now)
}

func (s *FileTransferV2Service) cleanupExpiredLocked(now time.Time) error {
	for transferID, task := range s.tasks {
		if task.ExpiresAt.After(now) {
			continue
		}
		blockBytes := v2DirectoryBlockBytes(filepath.Join(s.taskDir(transferID), "blocks"))
		metadataBytes := int64(0)
		if info, statErr := os.Stat(filepath.Join(s.taskDir(transferID), "task.json")); statErr == nil {
			metadataBytes = info.Size()
		}
		pendingBytes := int64(0)
		for _, pending := range s.pending[transferID] {
			pendingBytes += pending.bytes
			s.pendingFrames -= pending.frames
		}
		if err := os.RemoveAll(s.taskDir(transferID)); err != nil {
			s.recountSpoolBytesLocked()
			return fmt.Errorf("remove expired file v2 task: %w", err)
		}
		s.spoolBytes = subtractV2Bytes(s.spoolBytes, blockBytes+pendingBytes)
		s.metadataBytes -= metadataBytes
		delete(s.pending, transferID)
		delete(s.taskSpool, transferID)
		delete(s.tasks, transferID)
	}
	return nil
}

func v2DirectoryBlockBytes(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func (s *FileTransferV2Service) recountSpoolBytesLocked() {
	total := int64(0)
	metadataTotal := int64(0)
	s.pendingFrames = 0
	s.taskSpool = make(map[string]v2TaskSpoolUsage, len(s.tasks))
	for transferID := range s.tasks {
		usage := v2TaskSpoolUsage{committedBytes: v2DirectoryBlockBytes(filepath.Join(s.taskDir(transferID), "blocks"))}
		total += usage.committedBytes
		if info, err := os.Stat(filepath.Join(s.taskDir(transferID), "task.json")); err == nil {
			metadataTotal += info.Size()
		}
		for _, pending := range s.pending[transferID] {
			usage.pendingBytes += pending.bytes
			s.pendingFrames += pending.frames
		}
		total += usage.pendingBytes
		s.taskSpool[transferID] = usage
	}
	s.spoolBytes = total
	s.metadataBytes = metadataTotal
}

func validateV2Ranges(ranges [][]int, total int) error {
	return validateV2RangesLimit(ranges, total, 256)
}

func validateV2RangesLimit(ranges [][]int, total, maxRanges int) error {
	if len(ranges) > maxRanges {
		return errors.New("too many checkpoint ranges")
	}
	lastEnd := -1
	for _, r := range ranges {
		if len(r) != 2 || r[0] < 0 || r[1] < r[0] || r[1] >= total || r[0] <= lastEnd {
			return errors.New("invalid or overlapping checkpoint ranges")
		}
		lastEnd = r[1]
	}
	return nil
}

func ValidateFileTransferV2Ranges(ranges [][]int, total int) error {
	return validateV2Ranges(ranges, total)
}

func mergeV2Ranges(current, incoming [][]int) [][]int {
	// Both inputs are validated, sorted, disjoint interval lists. Merge the
	// ranges directly so a single large interval costs O(1) regardless of its
	// covered block count.
	merged := make([][]int, 0, len(current)+len(incoming))
	left, right := 0, 0
	for left < len(current) || right < len(incoming) {
		var next []int
		if right >= len(incoming) || (left < len(current) && current[left][0] <= incoming[right][0]) {
			next = current[left]
			left++
		} else {
			next = incoming[right]
			right++
		}
		if len(next) != 2 {
			continue
		}
		if len(merged) == 0 || next[0] > merged[len(merged)-1][1]+1 {
			merged = append(merged, []int{next[0], next[1]})
			continue
		}
		if next[1] > merged[len(merged)-1][1] {
			merged[len(merged)-1][1] = next[1]
		}
	}
	return merged
}

func touchFileTransferV2Task(task *FileTransferV2Session) {
	now := time.Now()
	task.UpdatedAt = now
	task.ExpiresAt = now.Add(fileTransferV2TaskTTL)
}

func v2RangeContains(ranges [][]int, index int) bool {
	for _, r := range ranges {
		if len(r) == 2 && index >= r[0] && index <= r[1] {
			return true
		}
	}
	return false
}

func cloneV2Ranges(ranges [][]int) [][]int {
	if ranges == nil {
		return nil
	}
	copyRanges := make([][]int, len(ranges))
	for i, r := range ranges {
		copyRanges[i] = append([]int(nil), r...)
	}
	return copyRanges
}
