// Copyright 2019 The etcd Authors
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

package quorum

import (
	"math"
	"strconv"
)

// Index is a Raft log position.
type Index uint64

func (i Index) String() string {
	if i == math.MaxUint64 {
		return "∞"
	}
	return strconv.FormatUint(uint64(i), 10)
}

// AckedIndexer allows looking up a commit index for a given ID of a voter
// from a corresponding MajorityConfig.
// AckedIndexer允许从相应的MajorityConfig中查找给定ID的投票者的提交索引。
type AckedIndexer interface {
	AckedIndex(voterID uint64) (idx Index, found bool)
}

type mapAckIndexer map[uint64]Index

func (m mapAckIndexer) AckedIndex(id uint64) (Index, bool) {
	idx, ok := m[id]
	return idx, ok
}

// VoteResult indicates the outcome of a vote.
//
//go:generate stringer -type=VoteResult
type VoteResult uint8

const (
	// VotePending indicates that the decision of the vote depends on future
	// votes, i.e. neither "yes" or "no" has reached quorum yet.
	// VotePending表示投票的决定取决于未来的投票，即目前还不能确定是否已经达到“是”或“否”的法定人数。
	VotePending VoteResult = 1 + iota
	// VoteLost indicates that the quorum has voted "no".
	// VoteLost表示法定人数投票“否”。
	VoteLost
	// VoteWon indicates that the quorum has voted "yes".
	// VoteWon表示法定人数投票“是”。
	VoteWon
)
