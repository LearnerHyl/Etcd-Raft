// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	"sync"

	pb "go.etcd.io/raft/v3/raftpb"
)

// ErrCompacted is returned by Storage.Entries/Compact when a requested
// index is unavailable because it predates the last snapshot.
var ErrCompacted = errors.New("requested index is unavailable due to compaction")

// ErrSnapOutOfDate is returned by Storage.CreateSnapshot when a requested
// index is older than the existing snapshot.
var ErrSnapOutOfDate = errors.New("requested index is older than the existing snapshot")

// ErrUnavailable is returned by Storage interface when the requested log entries
// are unavailable.
var ErrUnavailable = errors.New("requested entry at index is unavailable")

// ErrSnapshotTemporarilyUnavailable is returned by the Storage interface when the required
// snapshot is temporarily unavailable.
var ErrSnapshotTemporarilyUnavailable = errors.New("snapshot is temporarily unavailable")

// Storage is an interface that may be implemented by the application
// to retrieve log entries from storage.
//
// If any Storage method returns an error, the raft instance will
// become inoperable and refuse to participate in elections; the
// application is responsible for cleanup and recovery in this case.
type Storage interface {
	// TODO(tbg): split this into two interfaces, LogStorage and StateStorage.

	// InitialState returns the saved HardState and ConfState information.
	// IntialState返回保存的HardState和ConfState信息
	InitialState() (pb.HardState, pb.ConfState, error)

	// Entries returns a slice of consecutive log entries in the range [lo, hi),
	// starting from lo. The maxSize limits the total size of the log entries
	// returned, but Entries returns at least one entry if any.
	//
	// The caller of Entries owns the returned slice, and may append to it. The
	// individual entries in the slice must not be mutated, neither by the Storage
	// implementation nor the caller. Note that raft may forward these entries
	// back to the application via Ready struct, so the corresponding handler must
	// not mutate entries either (see comments in Ready struct).
	//
	// Since the caller may append to the returned slice, Storage implementation
	// must protect its state from corruption that such appends may cause. For
	// example, common ways to do so are:
	//  - allocate the slice before returning it (safest option),
	//  - return a slice protected by Go full slice expression, which causes
	//  copying on appends (see MemoryStorage).
	//
	// Returns ErrCompacted if entry lo has been compacted, or ErrUnavailable if
	// encountered an unavailable entry in [lo, hi).
	// 返回[lo, hi)范围内的连续日志条目的切片，从lo开始。maxSize限制返回的日志条目的总大小，但是如果有的话，Entries返回至少一个条目。
	// 调用Entries拥有返回的切片，并且可以追加到它。切片中的单个条目既不能由Storage实现修改，也不能由调用者修改。请注意，raft可能通过Ready结构将这些条目转发回应用程序，因此相应的处理程序也不能修改条目（请参阅Ready结构中的注释）。
	// 由于调用者可能追加到返回的切片，因此Storage实现必须保护其状态，以免这些追加可能导致的损坏。例如，保护其状态的常见方法是：
	//  - 在返回它之前分配切片的副本（最安全的选项），
	//  - 返回由Go完整切片表达式保护的切片，这会导致追加时的复制（请参见MemoryStorage）。
	// 如果条目lo已经被压缩，则返回ErrCompacted，如果在[lo, hi)中遇到不可用的条目，则返回ErrUnavailable。
	Entries(lo, hi, maxSize uint64) ([]pb.Entry, error)

	// Term returns the term of entry i, which must be in the range
	// [FirstIndex()-1, LastIndex()]. The term of the entry before
	// FirstIndex is retained for matching purposes even though the
	// rest of that entry may not be available.
	// 返回条目i的任期，它必须在[FirstIndex()-1, LastIndex()]范围内。即使该条目的其余部分可能不可用，也会保留FirstIndex之前的条目的任期以进行匹配。
	Term(i uint64) (uint64, error)
	// LastIndex returns the index of the last entry in the log.
	// LastIndex返回日志中最后一个条目的索引。
	LastIndex() (uint64, error)
	// FirstIndex returns the index of the first log entry that is
	// possibly available via Entries (older entries have been incorporated
	// into the latest Snapshot; if storage only contains the dummy entry the
	// first log entry is not available).
	// FirstIndex返回可能通过Entries获得的第一个日志条目的索引（较旧的条目已合并到最新的快照中；如果存储只包含虚拟条目，则第一个日志条目不可用）。
	FirstIndex() (uint64, error)
	// Snapshot returns the most recent snapshot.
	// If snapshot is temporarily unavailable, it should return ErrSnapshotTemporarilyUnavailable,
	// so raft state machine could know that Storage needs some time to prepare
	// snapshot and call Snapshot later.
	// Snapshot返回最近的快照。如果快照暂时不可用，它应该返回ErrSnapshotTemporarilyUnavailable，这样raft状态机就可以知道Storage需要一些时间来准备快照，并稍后调用Snapshot。
	Snapshot() (pb.Snapshot, error)
}

type inMemStorageCallStats struct {
	initialState, firstIndex, lastIndex, entries, term, snapshot int
}

// MemoryStorage implements the Storage interface backed by an
// in-memory array.
// MemoryStorage实现了由内存数组支持的Storage接口。
type MemoryStorage struct {
	// Protects access to all fields. Most methods of MemoryStorage are
	// run on the raft goroutine, but Append() is run on an application
	// goroutine.
	sync.Mutex

	hardState pb.HardState
	snapshot  pb.Snapshot
	// ents[i] has raft log position i+snapshot.Metadata.Index
	ents []pb.Entry // 日志条目

	callStats inMemStorageCallStats // 记录调用次数
}

// NewMemoryStorage creates an empty MemoryStorage.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		// When starting from scratch populate the list with a dummy entry at term zero.
		ents: make([]pb.Entry, 1),
	}
}

// InitialState implements the Storage interface.
func (ms *MemoryStorage) InitialState() (pb.HardState, pb.ConfState, error) {
	ms.callStats.initialState++
	return ms.hardState, ms.snapshot.Metadata.ConfState, nil
}

// SetHardState saves the current HardState.
func (ms *MemoryStorage) SetHardState(st pb.HardState) error {
	ms.Lock()
	defer ms.Unlock()
	ms.hardState = st
	return nil
}

// Entries implements the Storage interface.
func (ms *MemoryStorage) Entries(lo, hi, maxSize uint64) ([]pb.Entry, error) {
	ms.Lock()
	defer ms.Unlock()
	ms.callStats.entries++
	offset := ms.ents[0].Index
	if lo <= offset {
		return nil, ErrCompacted
	}
	if hi > ms.lastIndex()+1 {
		getLogger().Panicf("entries' hi(%d) is out of bound lastindex(%d)", hi, ms.lastIndex())
	}
	// only contains dummy entries.
	if len(ms.ents) == 1 {
		return nil, ErrUnavailable
	}

	ents := limitSize(ms.ents[lo-offset:hi-offset], entryEncodingSize(maxSize))
	// NB: use the full slice expression to limit what the caller can do with the
	// returned slice. For example, an append will reallocate and copy this slice
	// instead of corrupting the neighbouring ms.ents.
	return ents[:len(ents):len(ents)], nil
}

// Term implements the Storage interface.
func (ms *MemoryStorage) Term(i uint64) (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	ms.callStats.term++
	offset := ms.ents[0].Index
	if i < offset {
		return 0, ErrCompacted
	}
	if int(i-offset) >= len(ms.ents) {
		return 0, ErrUnavailable
	}
	return ms.ents[i-offset].Term, nil
}

// LastIndex implements the Storage interface.
func (ms *MemoryStorage) LastIndex() (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	ms.callStats.lastIndex++
	return ms.lastIndex(), nil
}

func (ms *MemoryStorage) lastIndex() uint64 {
	return ms.ents[0].Index + uint64(len(ms.ents)) - 1
}

// FirstIndex implements the Storage interface.
func (ms *MemoryStorage) FirstIndex() (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	ms.callStats.firstIndex++
	return ms.firstIndex(), nil
}

func (ms *MemoryStorage) firstIndex() uint64 {
	return ms.ents[0].Index + 1
}

// Snapshot implements the Storage interface.
func (ms *MemoryStorage) Snapshot() (pb.Snapshot, error) {
	ms.Lock()
	defer ms.Unlock()
	ms.callStats.snapshot++
	return ms.snapshot, nil
}

// ApplySnapshot overwrites the contents of this Storage object with
// those of the given snapshot.
// ApplySnapshot使用给定快照的内容覆盖此Storage对象的内容。
func (ms *MemoryStorage) ApplySnapshot(snap pb.Snapshot) error {
	ms.Lock()
	defer ms.Unlock()

	//handle check for old snapshot being applied
	msIndex := ms.snapshot.Metadata.Index
	snapIndex := snap.Metadata.Index
	if msIndex >= snapIndex {
		return ErrSnapOutOfDate
	}

	ms.snapshot = snap
	ms.ents = []pb.Entry{{Term: snap.Metadata.Term, Index: snap.Metadata.Index}}
	return nil
}

// CreateSnapshot makes a snapshot which can be retrieved with Snapshot() and
// can be used to reconstruct the state at that point.
// If any configuration changes have been made since the last compaction,
// the result of the last ApplyConfChange must be passed in.
// CreateSnapshot创建一个快照，可以使用Snapshot()检索，并且可以用于重建该点的状态。
// 如果自上次日志压缩以来进行了任何配置更改，则必须传递最后一次ApplyConfChange的结果。
func (ms *MemoryStorage) CreateSnapshot(i uint64, cs *pb.ConfState, data []byte) (pb.Snapshot, error) {
	ms.Lock()
	defer ms.Unlock()
	if i <= ms.snapshot.Metadata.Index {
		return pb.Snapshot{}, ErrSnapOutOfDate
	}

	offset := ms.ents[0].Index
	if i > ms.lastIndex() {
		getLogger().Panicf("snapshot %d is out of bound lastindex(%d)", i, ms.lastIndex())
	}

	ms.snapshot.Metadata.Index = i
	ms.snapshot.Metadata.Term = ms.ents[i-offset].Term
	if cs != nil {
		ms.snapshot.Metadata.ConfState = *cs
	}
	ms.snapshot.Data = data
	return ms.snapshot, nil
}

// Compact discards all log entries prior to compactIndex.
// It is the application's responsibility to not attempt to compact an index
// greater than raftLog.applied.
// Compact丢弃compactIndex之前的所有日志条目。
// 应用程序有责任不尝试压缩大于raftLog.applied的索引。
func (ms *MemoryStorage) Compact(compactIndex uint64) error {
	ms.Lock()
	defer ms.Unlock()
	offset := ms.ents[0].Index
	if compactIndex <= offset {
		return ErrCompacted
	}
	if compactIndex > ms.lastIndex() {
		getLogger().Panicf("compact %d is out of bound lastindex(%d)", compactIndex, ms.lastIndex())
	}

	i := compactIndex - offset
	// NB: allocate a new slice instead of reusing the old ms.ents. Entries in
	// ms.ents are immutable, and can be referenced from outside MemoryStorage
	// through slices returned by ms.Entries().
	ents := make([]pb.Entry, 1, uint64(len(ms.ents))-i)
	ents[0].Index = ms.ents[i].Index
	ents[0].Term = ms.ents[i].Term
	ents = append(ents, ms.ents[i+1:]...)
	ms.ents = ents
	return nil
}

// Append the new entries to storage.
// TODO (xiangli): ensure the entries are continuous and
// entries[0].Index > ms.entries[0].Index
func (ms *MemoryStorage) Append(entries []pb.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	ms.Lock()
	defer ms.Unlock()

	first := ms.firstIndex()
	last := entries[0].Index + uint64(len(entries)) - 1

	// shortcut if there is no new entry.
	if last < first {
		return nil
	}
	// truncate compacted entries
	if first > entries[0].Index {
		entries = entries[first-entries[0].Index:]
	}

	offset := entries[0].Index - ms.ents[0].Index
	switch {
	case uint64(len(ms.ents)) > offset:
		// NB: full slice expression protects ms.ents at index >= offset from
		// rewrites, as they may still be referenced from outside MemoryStorage.
		ms.ents = append(ms.ents[:offset:offset], entries...)
	case uint64(len(ms.ents)) == offset:
		ms.ents = append(ms.ents, entries...)
	default:
		getLogger().Panicf("missing log entry [last: %d, append at: %d]",
			ms.lastIndex(), entries[0].Index)
	}
	return nil
}
