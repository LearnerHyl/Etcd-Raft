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

package tracker

// StateType is the state of a tracked follower.
type StateType uint64

/**
 * 这里总结一下Leader对于处于不同状态的follower的不同行为：
 * 1. StateProbe：Leader定期发送Append请求探测follower的lastIndex，以找到合适的prevIndex ，
 *    其实本质上与StateReplicate是一样的，当follower的lastIndex被探测到，正确的PrevLogIndex和PrevLogTerm被找到后，
 *    follower就可以进入StateReplicate状态。Leader不会给处于StateProbe状态的follower发送日志流，只会逐步探测其lastIndex。
 *    详情可见pr.MsgAppFlowPaused参数的解释。
 * 2. StateReplicate：Leader会发送Append请求，follower会积极的接收要追加到其日志中的日志条目。
 * 3. StateSnapshot：Leader会发送InstallSnapshot请求，follower需要一个完整的快照来返回到StateReplicate状态。
 *    在follower应用完快照并回复Leader之前，Leader不会给其发送Append请求。
 */
const (
	// StateProbe indicates a follower whose last index isn't known. Such a
	// follower is "probed" (i.e. an append sent periodically) to narrow down
	// its last index. In the ideal (and common) case, only one round of probing
	// is necessary as the follower will react with a hint. Followers that are
	// probed over extended periods of time are often offline.
	// StateProbe意味着当前follower的lastIndex值是未知的。这样的follower会被“探测”（即定期发送一个append请求）
	// 以缩小其lastIndex的范围。在理想情况下（也是常见情况），只需要一轮探测就足够了，
	// 因为follower会返回一个提示。如果follower长时间被探测，通常是因为follower处于离线状态。
	StateProbe StateType = iota
	// StateReplicate is the state steady in which a follower eagerly receives
	// log entries to append to its log.
	// StateReplicate是follower的稳定状态，follower会积极地接收要追加到其日志中的日志条目。
	StateReplicate
	// StateSnapshot indicates a follower that needs log entries not available
	// from the leader's Raft log. Such a follower needs a full snapshot to
	// return to StateReplicate.
	// StateSnapshot意味着follower需要的日志条目在leader的Raft日志中是不可用的。
	// 这样的follower需要一个完整的快照来返回到StateReplicate状态。
	StateSnapshot
)

var prstmap = [...]string{
	"StateProbe",
	"StateReplicate",
	"StateSnapshot",
}

func (st StateType) String() string { return prstmap[st] }
