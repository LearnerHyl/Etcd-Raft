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

import (
	"fmt"
	"sort"
	"strings"
)

// Progress represents a follower’s progress in the view of the leader. Leader
// maintains progresses of all followers, and sends entries to the follower
// based on its progress.
// Progress表示leader视图中follower的进度。Leader维护所有follower的progress状态，并根
// 据其progress状态向follower发送日志条目。
//
// NB(tbg): Progress is basically a state machine whose transitions are mostly
// strewn around `*raft.raft`. Additionally, some fields are only used when in a
// certain State. All of this isn't ideal.
// NB(tbg): Progress基本上是一个状态机，其转换大部分分布在`*raft.raft`中。此外，某些字段仅在特定状态下使用。
// 这一切都不是理想的。
type Progress struct {
	// Match is the index up to which the follower's log is known to match the
	// leader's.
	Match uint64
	// Next is the log index of the next entry to send to this follower. All
	// entries with indices in (Match, Next) interval are already in flight.
	// Next是要发送给这个follower的下一个entry的日志索引。所有索引在（Match，Next）区间的entry已经在被发送中。
	//
	// Invariant: 0 <= Match < Next.
	Next uint64

	// State defines how the leader should interact with the follower.
	//
	// When in StateProbe, leader sends at most one replication message
	// per heartbeat interval. It also probes actual progress of the follower.
	//
	// When in StateReplicate, leader optimistically increases next
	// to the latest entry sent after sending replication message. This is
	// an optimized state for fast replicating log entries to the follower.
	//
	// When in StateSnapshot, leader should have sent out snapshot
	// before and stops sending any replication message.
	//
	// State定义了leader应该如何与follower进行交互。
	//
	// 当处于StateProbe状态时，leader每个心跳间隔最多发送一个复制消息。它还会探测follower的实际进度。
	//
	// 当处于StateReplicate状态时，leader会在发送复制消息后，乐观地增加next到最新发送的entry。
	// 这是一个用于快速复制日志条目到follower的优化状态。
	//
	// 当处于StateSnapshot状态时，leader应该在发出快照之前停止发送任何复制消息。
	// 并且在目标follower成功应用快照之前，leader不会再给它发送任何日志复制消息。
	//
	State StateType

	// PendingSnapshot is used in StateSnapshot and tracks the last index of the
	// leader at the time at which it realized a snapshot was necessary. This
	// matches the index in the MsgSnap message emitted from raft.
	// PendingSnapshot用于StateSnapshot状态，它跟踪了leader在意识到需要快照时的最后索引。
	// 这与raft发送的MsgSnap消息中的索引匹配。
	//
	// While there is a pending snapshot, replication to the follower is paused.
	// The follower will transition back to StateReplicate if the leader
	// receives an MsgAppResp from it that reconnects the follower to the
	// leader's log (such an MsgAppResp is emitted when the follower applies a
	// snapshot). It may be surprising that PendingSnapshot is not taken into
	// account here, but consider that complex systems may delegate the sending
	// of snapshots to alternative datasources (i.e. not the leader). In such
	// setups, it is difficult to manufacture a snapshot at a particular index
	// requested by raft and the actual index may be ahead or behind. This
	// should be okay, as long as the snapshot allows replication to resume.
	// 当有一个pending snapshot时，对follower的复制被暂停。如果leader从follower那里接收到一个
	// MsgAppResp，这个MsgAppResp重新连接了follower到leader的日志（当follower应用了一个快照时，会发出这样的MsgAppResp），
	// 那么follower将会回到StateReplicate状态。这里可能会有些令人惊讶，因为PendingSnapshot在这里没有被考虑进去，
	// 但是请考虑到复杂的系统可能会将发送快照的工作委托给替代数据源（即不是leader）。
	// 在这种设置中，很难在raft请求的特定索引处制造一个快照，实际的索引可能在前面或后面。
	// 只要快照允许复制恢复，这应该是可以的。
	//
	// The follower will transition to StateProbe if ReportSnapshot is called on
	// the leader; if SnapshotFinish is passed then PendingSnapshot becomes the
	// basis for the next attempt to append. In practice, the first mechanism is
	// the one that is relevant in most cases. However, if this MsgAppResp is
	// lost (fallible network) then the second mechanism ensures that in this
	// case the follower does not erroneously remain in StateSnapshot.
	// 如果在leader上调用了ReportSnapshot，follower将会转换到StateProbe状态；如果传递了SnapshotFinish，
	// 那么PendingSnapshot将成为下一次追加的基础。实际上，在大多数情况下，第一种机制是最相关的。
	// 但是，如果这个MsgAppResp丢失了（网络是有错误的），那么第二种机制确保在这种情况下，
	// follower不会错误地保持在StateSnapshot状态。
	PendingSnapshot uint64

	// RecentActive is true if the progress is recently active. Receiving any messages
	// from the corresponding follower indicates the progress is active.
	// RecentActive can be reset to false after an election timeout.
	// This is always true on the leader.
	// 如果progress最近活跃，则RecentActive为true。从相应的follower接收到任何消息都表示progress是活跃的。
	// 在选举超时后，RecentActive可以被重置为false。这在leader上始终为true。
	RecentActive bool

	// MsgAppFlowPaused is used when the MsgApp flow to a node is throttled. This
	// happens in StateProbe, or StateReplicate with saturated Inflights. In both
	// cases, we need to continue sending MsgApp once in a while to guarantee
	// progress, but we only do so when MsgAppFlowPaused is false (it is reset on
	// receiving a heartbeat response), to not overflow the receiver. See
	// IsPaused().
	// MsgAppFlowPaused在MsgApp流向一个节点被限制时使用。这发生在StateProbe状态下，或者在Inflights饱和的StateReplicate状态下。
	// 在这两种情况下，我们需要不时地继续发送MsgApp以保证进度，但是只有当MsgAppFlowPaused为false时（在接收到心跳响应时重置），
	// 我们才会这样做，以避免接收者端流量过大。参见IsPaused()。
	MsgAppFlowPaused bool

	// Inflights is a sliding window for the inflight messages.
	// Each inflight message contains one or more log entries.
	// The max number of entries per message is defined in raft config as MaxSizePerMsg.
	// Thus inflight effectively limits both the number of inflight messages
	// and the bandwidth each Progress can use.
	// When inflights is Full, no more message should be sent.
	// When a leader sends out a message, the index of the last
	// entry should be added to inflights. The index MUST be added
	// into inflights in order.
	// When a leader receives a reply, the previous inflights should
	// be freed by calling inflights.FreeLE with the index of the last
	// received entry.
	// Inflights是一个滑动窗口，用于存储inflight消息。每个inflight消息包含一个或多个日志条目。
	// 每个message中的最大条目数在raft配置中定义为MaxSizePerMsg。因此，inflight有效地限制了inflight消息的数量和
	// 每个Progress可以使用的带宽。当inflights满时，不应该再发送更多的消息。当leader发送消息时，最后一个entry的索引应该被添加到inflights中。
	// 必须按顺序将索引添加到inflights中。
	// 当leader接收到一个回复时，之前的inflights应该通过调用inflights.FreeLE来释放，参数是最后接收到的entry的索引。
	Inflights *Inflights

	// IsLearner is true if this progress is tracked for a learner.
	// 如果这个progress是作为一个Learner角色被跟踪的，那么IsLearner为true。
	// Learner用于在learner节点上进行日志复制。Learner节点不会参与选举，不会成为leader。
	// 用于使集群成员变更过程更加平滑，避免在添加或删除节点时造成集群不可用。
	IsLearner bool
}

// ResetState moves the Progress into the specified State, resetting MsgAppFlowPaused,
// PendingSnapshot, and Inflights.
func (pr *Progress) ResetState(state StateType) {
	pr.MsgAppFlowPaused = false
	pr.PendingSnapshot = 0
	pr.State = state
	pr.Inflights.reset()
}

// BecomeProbe transitions into StateProbe. Next is reset to Match+1 or,
// optionally and if larger, the index of the pending snapshot.
// BecomeProbe 转换为StateProbe。Next被重置为Match+1，或者（如果更大）为pending snapshot的索引。
func (pr *Progress) BecomeProbe() {
	// If the original state is StateSnapshot, progress knows that
	// the pending snapshot has been sent to this peer successfully, then
	// probes from pendingSnapshot + 1.
	// 如果原始状态是StateSnapshot，progress知道pending snapshot已经成功地发送给了这个peer，
	if pr.State == StateSnapshot {
		pendingSnapshot := pr.PendingSnapshot
		pr.ResetState(StateProbe)
		pr.Next = max(pr.Match+1, pendingSnapshot+1)
	} else {
		pr.ResetState(StateProbe)
		pr.Next = pr.Match + 1
	}
}

// BecomeReplicate transitions into StateReplicate, resetting Next to Match+1.
func (pr *Progress) BecomeReplicate() {
	pr.ResetState(StateReplicate)
	pr.Next = pr.Match + 1
}

// BecomeSnapshot moves the Progress to StateSnapshot with the specified pending
// snapshot index.
func (pr *Progress) BecomeSnapshot(snapshoti uint64) {
	pr.ResetState(StateSnapshot)
	pr.PendingSnapshot = snapshoti
}

// UpdateOnEntriesSend updates the progress on the given number of consecutive
// entries being sent in a MsgApp, with the given total bytes size, appended at
// log indices >= pr.Next.
//
// Must be used with StateProbe or StateReplicate.
func (pr *Progress) UpdateOnEntriesSend(entries int, bytes uint64) {
	switch pr.State {
	case StateReplicate:
		if entries > 0 {
			pr.Next += uint64(entries)
			pr.Inflights.Add(pr.Next-1, bytes)
		}
		// If this message overflows the in-flights tracker, or it was already full,
		// consider this message being a probe, so that the flow is paused.
		pr.MsgAppFlowPaused = pr.Inflights.Full()
	case StateProbe:
		// TODO(pavelkalinnikov): this condition captures the previous behaviour,
		// but we should set MsgAppFlowPaused unconditionally for simplicity, because any
		// MsgApp in StateProbe is a probe, not only non-empty ones.
		if entries > 0 {
			pr.MsgAppFlowPaused = true
		}
	default:
		panic(fmt.Sprintf("sending append in unhandled state %s", pr.State))
	}
}

// MaybeUpdate is called when an MsgAppResp arrives from the follower, with the
// index acked by it. The method returns false if the given n index comes from
// an outdated message. Otherwise it updates the progress and returns true.
// MaybeUpdate在leader接收到来自follower的MsgAppResp时被调用，其中包含了follower确认的index。
// 若给定的index n来自一个过时的消息，该方法返回false。否则，它会更新progress并返回true。
func (pr *Progress) MaybeUpdate(n uint64) bool {
	if n <= pr.Match {
		return false
	}
	pr.Match = n
	pr.Next = max(pr.Next, n+1) // invariant: Match < Next
	pr.MsgAppFlowPaused = false // 告诉Leader该follower可以继续发送MsgApp消息
	return true
}

// MaybeDecrTo adjusts the Progress to the receipt of a MsgApp rejection. The
// arguments are the index of the append message rejected by the follower, and
// the hint that we want to decrease to.
// MaybeDecrTo 调整Progress内容以适应MsgApp被拒绝的情况。rejected是被follower拒绝的append消息的索引，
// 即prevLogIndex；matchHint是我们想要减少到的索引，即我们通过findConflictByTerm找到的冲突索引。
//
// Rejections can happen spuriously as messages are sent out of order or
// duplicated. In such cases, the rejection pertains to an index that the
// Progress already knows were previously acknowledged, and false is returned
// without changing the Progress.
// Rejections 可能会在消息被无序发送或重复发送时发生。在这种情况下，rejections涉及到Progress已经知道的先前被确认的索引，
// 这种情况下返回false而不改变Progress。
//
// If the rejection is genuine, Next is lowered sensibly, and the Progress is
// cleared for sending log entries.
// 如果rejection是真实的，Next会被合理地降低，并且Progress会被清除以发送日志条目。
func (pr *Progress) MaybeDecrTo(rejected, matchHint uint64) bool {
	if pr.State == StateReplicate {
		// The rejection must be stale if the progress has matched and "rejected"
		// is smaller than "match".
		// 如果progress已经匹配并且rejected小于match，那么rejection必须是过时的。
		if rejected <= pr.Match {
			return false
		}
		// Directly decrease next to match + 1.
		//
		// TODO(tbg): why not use matchHint if it's larger?
		pr.Next = pr.Match + 1
		return true
	}

	// The rejection must be stale if "rejected" does not match next - 1. This
	// is because non-replicating followers are probed one entry at a time.
	// The check is a best effort assuming message reordering is rare.
	// 如果rejected不匹配next - 1，那么rejection必须是过时的。这是因为处于non-replicating状态的
	// follower是一次一个entry地被探测的。在假设消息重排序很少的情况下，这个检查是最好的努力。
	if pr.Next-1 != rejected {
		return false
	}

	pr.Next = max(min(rejected, matchHint+1), pr.Match+1)
	pr.MsgAppFlowPaused = false
	return true
}

// IsPaused returns whether sending log entries to this node has been throttled.
// This is done when a node has rejected recent MsgApps, is currently waiting
// for a snapshot, or has reached the MaxInflightMsgs limit. In normal
// operation, this is false. A throttled node will be contacted less frequently
// until it has reached a state in which it's able to accept a steady stream of
// log entries again.
// IsPaused返回是否向这个节点发送日志条目已经被限制。当一个节点拒绝了最近的MsgApps，因为当前正在等待快照，
// 或者已经达到了MaxInflightMsgs限制时，会发生这种情况。在正常操作中，这是false。被限制的节点将会被更少地联系，
// 直到它达到了一个可以再次接受稳定的日志条目流的状态。
func (pr *Progress) IsPaused() bool {
	switch pr.State {
	case StateProbe:
		return pr.MsgAppFlowPaused
	case StateReplicate:
		return pr.MsgAppFlowPaused
	case StateSnapshot:
		return true
	default:
		panic("unexpected state")
	}
}

func (pr *Progress) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "%s match=%d next=%d", pr.State, pr.Match, pr.Next)
	if pr.IsLearner {
		fmt.Fprint(&buf, " learner")
	}
	if pr.IsPaused() {
		fmt.Fprint(&buf, " paused")
	}
	if pr.PendingSnapshot > 0 {
		fmt.Fprintf(&buf, " pendingSnap=%d", pr.PendingSnapshot)
	}
	if !pr.RecentActive {
		fmt.Fprint(&buf, " inactive")
	}
	if n := pr.Inflights.Count(); n > 0 {
		fmt.Fprintf(&buf, " inflight=%d", n)
		if pr.Inflights.Full() {
			fmt.Fprint(&buf, "[full]")
		}
	}
	return buf.String()
}

// ProgressMap is a map of *Progress.
type ProgressMap map[uint64]*Progress

// String prints the ProgressMap in sorted key order, one Progress per line.
func (m ProgressMap) String() string {
	ids := make([]uint64, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	var buf strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&buf, "%d: %s\n", id, m[id])
	}
	return buf.String()
}
