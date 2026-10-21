package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func newV2TestService(t *testing.T, root string) *FileTransferV2Service {
	t.Helper()
	base := NewFileTransferService(nil, 3*1024*1024*1024, 64*1024)
	t.Cleanup(base.Shutdown)
	v2 := base.V2()
	v2.SetRoot(root)
	v2.perTaskMax = fileTransferV2PerTaskSpool
	v2.globalMax = fileTransferV2GlobalSpool
	return v2
}

func v2TestManifest(data []byte) model.FileTransferV2Manifest {
	hash := sha256.Sum256(data)
	blocks := 1
	if len(data) > 0 {
		blocks = (len(data) + FileTransferV2BlockSize - 1) / FileTransferV2BlockSize
	}
	return model.FileTransferV2Manifest{
		TransferID: "transfer_v2_test_01", Name: "résumé.bin", Size: int64(len(data)), Type: "application/octet-stream",
		BlockSize: FileTransferV2BlockSize, TotalBlocks: blocks, FileHash: hex.EncodeToString(hash[:]),
		CreatedAt: time.Now().UnixMilli(),
	}
}

func TestFileTransferV2DuplicateOfferAndManifestBounds(t *testing.T) {
	v2 := newV2TestService(t, filepath.Join(t.TempDir(), "spool"))
	manifest := v2TestManifest([]byte("data"))
	if err := v2.Create(manifest, "alice", "bob", "room", false); err != nil {
		t.Fatal(err)
	}
	if err := v2.Create(manifest, "alice", "bob", "room", false); err != nil {
		t.Fatalf("same offer was not idempotent: %v", err)
	}
	conflict := manifest
	conflict.Name = "other.bin"
	if err := v2.Create(conflict, "alice", "bob", "room", false); err == nil {
		t.Fatal("conflicting transfer ID was accepted")
	}
	if IsValidFileTransferV2ID("../escape001") || IsValidFileTransferV2ID("short") || !IsValidFileTransferV2ID(manifest.TransferID) {
		t.Fatal("transfer ID validator does not match the safe alphabet and length")
	}
	badName := manifest
	badName.Name = strings.Repeat("x", 1025)
	if err := validateFileTransferV2Manifest(badName); err == nil {
		t.Fatal("oversized UTF-8 filename accepted")
	}
	badName = manifest
	badName.Name = "bad\u0000name"
	if err := validateFileTransferV2Manifest(badName); err == nil {
		t.Fatal("filename control character accepted")
	}
}

func TestFileTransferV2TerminalTransitionsAndLateCheckpointAreRejected(t *testing.T) {
	v2 := newV2TestService(t, filepath.Join(t.TempDir(), "spool"))
	manifest := v2TestManifest([]byte("terminal guard"))
	if err := v2.Create(manifest, "alice", "bob", "room", false); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(manifest.TransferID, "cancelled", true); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", false); err == nil {
		t.Fatal("stale accept resurrected a cancelled transfer")
	}
	if err := v2.Checkpoint(manifest.TransferID, [][]int{{0, 0}}); err == nil {
		t.Fatal("late checkpoint mutated a cancelled terminal receipt")
	}
	if err := v2.SetStatus(manifest.TransferID, "completed", true); err == nil {
		t.Fatal("late complete overwrote a cancelled terminal receipt")
	}
	if err := v2.SetStatus(manifest.TransferID, "cancelled", true); err != nil {
		t.Fatalf("same-terminal cancel retry was not idempotent: %v", err)
	}
	cancelled, err := v2.Get(manifest.TransferID)
	if err != nil || cancelled.Status != "cancelled" || len(cancelled.Checkpoint) != 0 {
		t.Fatalf("cancelled receipt changed after late controls: task=%+v err=%v", cancelled, err)
	}

	completedManifest := manifest
	completedManifest.TransferID = "transfer_v2_done_01"
	if err := v2.Create(completedManifest, "alice", "bob", "room", false); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(completedManifest.TransferID, "accepted", false); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(completedManifest.TransferID, "completed", true); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(completedManifest.TransferID, "completed", true); err != nil {
		t.Fatalf("complete retry was not idempotent: %v", err)
	}
	if err := v2.SetStatus(completedManifest.TransferID, "cancelled", true); err == nil {
		t.Fatal("late cancel overwrote a completed terminal receipt")
	}
	if err := v2.Checkpoint(completedManifest.TransferID, [][]int{{0, 0}}); err == nil {
		t.Fatal("late checkpoint mutated a completed terminal receipt")
	}
	completed, err := v2.Get(completedManifest.TransferID)
	if err != nil || completed.Status != "completed" || len(completed.Checkpoint) != 0 || len(completed.Blocks) != 0 {
		t.Fatalf("completed receipt changed after late controls: task=%+v err=%v", completed, err)
	}
}

func TestFileTransferV2RepeatedAcceptancePreservesTransferringState(t *testing.T) {
	v2 := newV2TestService(t, filepath.Join(t.TempDir(), "spool"))
	data := []byte("accept replay while a relay path is active")
	manifest := v2TestManifest(data)
	if err := v2.Create(manifest, "alice", "bob", "room", false); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", false); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if _, committed, err := v2.AddFragment(manifest.TransferID, 0, 0, len(data), hex.EncodeToString(hash[:]), data); err != nil || !committed {
		t.Fatalf("could not persist relay block: committed=%v err=%v", committed, err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", false); err != nil {
		t.Fatalf("replayed acceptance failed while transferring: %v", err)
	}
	task, err := v2.Get(manifest.TransferID)
	if err != nil || task.Status != "transferring" || len(task.Blocks) != 1 {
		t.Fatalf("replayed acceptance downgraded or cleared active transfer: task=%+v err=%v", task, err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", true); err == nil {
		t.Fatal("replayed nonterminal acceptance cleared relay payload")
	}
}

func TestFileTransferV2SpoolCountersTrackCommitCheckpointAndRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	data := bytes.Repeat([]byte("counter-test-"), (FileTransferV2BlockSize+64*1024+12)/13)
	data = data[:FileTransferV2BlockSize+64*1024]
	manifest := v2TestManifest(data)
	first := newV2TestService(t, root)
	if err := first.Create(manifest, "alice", "bob", "room", true); err != nil {
		t.Fatal(err)
	}
	if err := first.SetStatus(manifest.TransferID, "accepted", false); err != nil {
		t.Fatal(err)
	}

	addBlock := func(v2 *FileTransferV2Service, index int, block []byte) {
		t.Helper()
		hash := sha256.Sum256(block)
		blockHash := hex.EncodeToString(hash[:])
		for offset := 0; offset < len(block); offset += FileTransferV2MaxFramePayload {
			end := offset + FileTransferV2MaxFramePayload
			if end > len(block) {
				end = len(block)
			}
			_, complete, err := v2.AddFragment(manifest.TransferID, index, offset, len(block), blockHash, block[offset:end])
			if err != nil {
				t.Fatalf("add block %d at offset %d: %v", index, offset, err)
			}
			if complete != (end == len(block)) {
				t.Fatalf("block %d complete=%v at offset %d", index, complete, offset)
			}
		}
	}
	block0 := data[:FileTransferV2BlockSize]
	addBlock(first, 0, block0)
	usage := first.taskSpool[manifest.TransferID]
	if usage.committedBytes != int64(len(block0)) || usage.pendingBytes != 0 || first.spoolBytes != int64(len(block0)) {
		t.Fatalf("committed accounting mismatch: usage=%+v global=%d", usage, first.spoolBytes)
	}
	block1 := data[FileTransferV2BlockSize:]
	block1Hash := sha256.Sum256(block1)
	if _, complete, err := first.AddFragment(manifest.TransferID, 1, 0, len(block1), hex.EncodeToString(block1Hash[:]), block1[:FileTransferV2MaxFramePayload]); err != nil || complete {
		t.Fatalf("partial second block complete=%v err=%v", complete, err)
	}
	usage = first.taskSpool[manifest.TransferID]
	if usage.committedBytes != int64(len(block0)) || usage.pendingBytes != FileTransferV2MaxFramePayload || first.spoolBytes != int64(len(block0)+FileTransferV2MaxFramePayload) {
		t.Fatalf("pending accounting mismatch: usage=%+v global=%d", usage, first.spoolBytes)
	}

	// Recovery reconstructs committed counters from durable files, then
	// applies quota without walking every already committed block per frame.
	recovered := newV2TestService(t, root)
	if _, err := recovered.Get(manifest.TransferID); err != nil {
		t.Fatal(err)
	}
	recovered.perTaskMax = int64(len(block0))
	if _, _, err := recovered.AddFragment(manifest.TransferID, 1, 0, len(block1), hex.EncodeToString(block1Hash[:]), block1[:FileTransferV2MaxFramePayload]); err != ErrFileTransferV2Quota {
		t.Fatalf("quota ignored recovered committed bytes: %v", err)
	}
	if err := recovered.Checkpoint(manifest.TransferID, [][]int{{0, 0}}); err != nil {
		t.Fatal(err)
	}
	usage = recovered.taskSpool[manifest.TransferID]
	if usage.committedBytes != 0 || usage.pendingBytes != 0 || recovered.spoolBytes != 0 {
		t.Fatalf("checkpoint did not release committed counters: usage=%+v global=%d", usage, recovered.spoolBytes)
	}
	addBlock(recovered, 1, block1)
	usage = recovered.taskSpool[manifest.TransferID]
	if usage.committedBytes != int64(len(block1)) || usage.pendingBytes != 0 || recovered.spoolBytes != int64(len(block1)) {
		t.Fatalf("second committed accounting mismatch: usage=%+v global=%d", usage, recovered.spoolBytes)
	}
	restarted := newV2TestService(t, root)
	if _, err := restarted.Get(manifest.TransferID); err != nil {
		t.Fatal(err)
	}
	usage = restarted.taskSpool[manifest.TransferID]
	if usage.committedBytes != int64(len(block1)) || usage.pendingBytes != 0 || restarted.spoolBytes != int64(len(block1)) {
		t.Fatalf("restart did not reconcile counters from disk: usage=%+v global=%d", usage, restarted.spoolBytes)
	}
}

func TestFileTransferV2MergeRangesKeepsLargeContiguousCheckpointCompressed(t *testing.T) {
	merged := mergeV2Ranges([][]int{{0, 10000}}, [][]int{{10001, 12287}})
	if len(merged) != 1 || merged[0][0] != 0 || merged[0][1] != 12287 {
		t.Fatalf("large contiguous checkpoint did not stay compressed: %v", merged)
	}
}

func TestFileTransferV2ConcurrentAcceptCancelAndCompleteCancelTransitions(t *testing.T) {
	v2 := newV2TestService(t, filepath.Join(t.TempDir(), "spool"))
	data := []byte("race")
	manifest := v2TestManifest(data)
	manifest.TransferID = "transfer_v2_race_01"
	if err := v2.Create(manifest, "alice", "bob", "room", false); err != nil {
		t.Fatal(err)
	}
	// Model both handlers having read an offered snapshot before racing their
	// service mutations. Cancellation may follow acceptance, but acceptance
	// cannot commit after cancellation wins the mutex.
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- v2.SetStatus(manifest.TransferID, "accepted", false) }()
	go func() { <-start; results <- v2.SetStatus(manifest.TransferID, "cancelled", true) }()
	close(start)
	errA, errB := <-results, <-results
	if errA != nil && errB != nil {
		t.Fatalf("both offered transitions failed: accept=%v cancel=%v", errA, errB)
	}
	afterCancelRace, err := v2.Get(manifest.TransferID)
	if err != nil || afterCancelRace.Status != "cancelled" {
		t.Fatalf("concurrent cancel did not leave a cancelled receipt: task=%+v err=%v", afterCancelRace, err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", false); err == nil {
		t.Fatal("post-race stale acceptance resurrected cancellation")
	}

	manifest.TransferID = "transfer_v2_race_02"
	if err := v2.Create(manifest, "alice", "bob", "room", false); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", false); err != nil {
		t.Fatal(err)
	}
	start = make(chan struct{})
	results = make(chan error, 2)
	go func() { <-start; results <- v2.SetStatus(manifest.TransferID, "completed", true) }()
	go func() { <-start; results <- v2.SetStatus(manifest.TransferID, "cancelled", true) }()
	close(start)
	errA, errB = <-results, <-results
	if (errA == nil) == (errB == nil) {
		t.Fatalf("exactly one competing terminal transition must win: complete=%v cancel=%v", errA, errB)
	}
	terminal, err := v2.Get(manifest.TransferID)
	if err != nil || (terminal.Status != "completed" && terminal.Status != "cancelled") {
		t.Fatalf("concurrent terminal transitions produced invalid receipt: task=%+v err=%v", terminal, err)
	}
	loser := "completed"
	if terminal.Status == "completed" {
		loser = "cancelled"
	}
	if err := v2.SetStatus(manifest.TransferID, loser, true); err == nil {
		t.Fatalf("late %s overwrote winning %s receipt", loser, terminal.Status)
	}
}

func TestFileTransferV2RestartKeepsCommittedBlockAndResendIsSingleBlock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	data := bytes.Repeat([]byte("v2-block-"), (FileTransferV2BlockSize+8)/9)
	data = data[:FileTransferV2BlockSize]
	manifest := v2TestManifest(data)
	first := newV2TestService(t, root)
	if err := first.Create(manifest, "alice", "bob", "room", true); err != nil {
		t.Fatal(err)
	}
	if err := first.SetStatus(manifest.TransferID, "accepted", false); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	blockHash := hex.EncodeToString(hash[:])
	for offset := 0; offset < len(data); offset += FileTransferV2MaxFramePayload {
		end := offset + FileTransferV2MaxFramePayload
		if end > len(data) {
			end = len(data)
		}
		_, complete, err := first.AddFragment(manifest.TransferID, 0, offset, len(data), blockHash, data[offset:end])
		if err != nil {
			t.Fatal(err)
		}
		if complete != (end == len(data)) {
			t.Fatalf("complete=%v at offset %d", complete, offset)
		}
	}
	blockFile := first.blockPath(manifest.TransferID, 0)
	if info, err := os.Stat(blockFile); err != nil || info.Size() != FileTransferV2BlockSize {
		t.Fatalf("committed block is not in the blocks directory: info=%v err=%v", info, err)
	}

	// A fresh service instance must recover metadata and the actual spooled bytes.
	restarted := newV2TestService(t, root)
	task, err := restarted.Get(manifest.TransferID)
	if err != nil || task.Blocks[0] != blockHash {
		t.Fatalf("restart lost committed block: task=%+v err=%v", task, err)
	}
	for offset := 0; offset < len(data); offset += FileTransferV2MaxFramePayload {
		end := offset + FileTransferV2MaxFramePayload
		if end > len(data) {
			end = len(data)
		}
		block, complete, err := restarted.AddFragment(manifest.TransferID, 0, offset, len(data), blockHash, data[offset:end])
		if err != nil {
			t.Fatal(err)
		}
		if offset == 0 {
			if !complete || !bytes.Equal(block, data) {
				t.Fatal("first retransmitted fragment did not replay exactly one committed block")
			}
		} else if complete || len(block) != 0 {
			t.Fatalf("duplicate fragment at offset %d replayed the whole block again", offset)
		}
	}
	if err := restarted.Checkpoint(manifest.TransferID, [][]int{{0, 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blockFile); !os.IsNotExist(err) {
		t.Fatalf("receiver checkpoint did not free block spool: %v", err)
	}
	if _, replayed, err := restarted.AddFragment(manifest.TransferID, 0, 0, len(data), blockHash, data[:FileTransferV2MaxFramePayload]); err != nil || replayed {
		t.Fatalf("late retry overrode receiver checkpoint: replayed=%v err=%v", replayed, err)
	}
	if restarted.spoolBytes != 0 {
		t.Fatalf("late committed retry consumed spool bytes: %d", restarted.spoolBytes)
	}
	checkpointed, err := newV2TestService(t, root).Get(manifest.TransferID)
	if err != nil || !v2RangeContains(checkpointed.Checkpoint, 0) {
		t.Fatalf("checkpoint did not survive restart: task=%+v err=%v", checkpointed, err)
	}
}

func TestFileTransferV2SpoolQuotaAndExpiryCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	v2 := newV2TestService(t, root)
	data := []byte("quota")
	manifest := v2TestManifest(data)
	if err := v2.Create(manifest, "alice", "bob", "room", false); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", false); err != nil {
		t.Fatal(err)
	}
	v2.globalMax = 2
	hash := sha256.Sum256(data)
	if _, _, err := v2.AddFragment(manifest.TransferID, 0, 0, len(data), hex.EncodeToString(hash[:]), data); err != ErrFileTransferV2Quota {
		t.Fatalf("quota error = %v, want recoverable quota error", err)
	}
	if len(v2.pending[manifest.TransferID]) != 0 || v2.pendingFrames != 0 || v2.spoolBytes != 0 {
		t.Fatalf("quota rejection retained empty assembly metadata: pending=%d frames=%d bytes=%d", len(v2.pending[manifest.TransferID]), v2.pendingFrames, v2.spoolBytes)
	}
	v2.globalMax = fileTransferV2GlobalSpool
	v2.mu.Lock()
	task := v2.tasks[manifest.TransferID]
	task.ExpiresAt = time.Now().Add(-time.Second)
	if err := v2.persistLocked(task); err != nil {
		v2.mu.Unlock()
		t.Fatal(err)
	}
	v2.mu.Unlock()
	if err := v2.CleanupExpired(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v2.taskDir(manifest.TransferID)); !os.IsNotExist(err) {
		t.Fatalf("expired task directory remains: %v", err)
	}
}

func TestFileTransferV2CheckpointPagesSurviveRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	first := newV2TestService(t, root)
	manifest := v2TestManifest(nil)
	manifest.Size = int64(FileTransferV2BlockSize * 1200)
	manifest.TotalBlocks = 1200
	if err := first.Create(manifest, "alice", "bob", "room", true); err != nil {
		t.Fatal(err)
	}
	ranges := make([][]int, 0, 600)
	for index := 0; index < 1200; index += 2 {
		ranges = append(ranges, []int{index, index})
	}
	for start := 0; start < len(ranges); start += 256 {
		end := start + 256
		if end > len(ranges) {
			end = len(ranges)
		}
		if err := first.Checkpoint(manifest.TransferID, ranges[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := newV2TestService(t, root).Get(manifest.TransferID)
	if err != nil || len(recovered.Checkpoint) != len(ranges) {
		t.Fatalf("cumulative checkpoint pages did not recover: ranges=%d err=%v", len(recovered.Checkpoint), err)
	}
}

func TestFileTransferV2PendingFragmentMetadataIsBoundedAndRecoverable(t *testing.T) {
	v2 := newV2TestService(t, filepath.Join(t.TempDir(), "spool"))
	data := bytes.Repeat([]byte{'a'}, FileTransferV2BlockSize)
	manifest := v2TestManifest(data)
	if err := v2.Create(manifest, "alice", "bob", "room", true); err != nil {
		t.Fatal(err)
	}
	if err := v2.SetStatus(manifest.TransferID, "accepted", false); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	blockHash := hex.EncodeToString(hash[:])
	for offset := 0; offset < fileTransferV2MaxBlockFrames; offset++ {
		if _, _, err := v2.AddFragment(manifest.TransferID, 0, offset, len(data), blockHash, data[offset:offset+1]); err != nil {
			t.Fatalf("fragment %d unexpectedly failed: %v", offset, err)
		}
	}
	if _, _, err := v2.AddFragment(manifest.TransferID, 0, fileTransferV2MaxBlockFrames, len(data), blockHash, data[fileTransferV2MaxBlockFrames:fileTransferV2MaxBlockFrames+1]); err != ErrFileTransferV2Frames {
		t.Fatalf("fragment metadata bound returned %v", err)
	}
	if v2.pendingFrames != 0 || v2.spoolBytes != 0 {
		t.Fatalf("rejected fragment storm did not release bounded assembly: frames=%d bytes=%d", v2.pendingFrames, v2.spoolBytes)
	}
	for offset := 0; offset < len(data); offset += FileTransferV2MaxFramePayload {
		end := offset + FileTransferV2MaxFramePayload
		if end > len(data) {
			end = len(data)
		}
		_, complete, err := v2.AddFragment(manifest.TransferID, 0, offset, len(data), blockHash, data[offset:end])
		if err != nil {
			t.Fatal(err)
		}
		if complete != (end == len(data)) {
			t.Fatalf("recoverable block resend complete=%v at offset %d", complete, offset)
		}
	}
}
