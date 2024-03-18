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

	pb "go.etcd.io/raft/v3/raftpb"
	"go.etcd.io/raft/v3/tracker"
)

// ErrStepLocalMsg is returned when try to step a local raft message
// 节点拒绝处理通过网络接收到的本地消息，即RawNode.step()不处理本地消息。
var ErrStepLocalMsg = errors.New("raft: cannot step raft local message")

// ErrStepPeerNotFound is returned when try to step a response message
// but there is no peer found in raft.trk for that node.
// 尝试处理响应消息时，如果在raft.trk中找不到对等方，则返回ErrStepPeerNotFound。
var ErrStepPeerNotFound = errors.New("raft: cannot step as peer not found")

// RawNode is a thread-unsafe Node.
// The methods of this struct correspond to the methods of Node and are described
// more fully there.
// RawNode是一个线程不安全的Node。
// 该结构的方法对应于Node的方法，并在那里有更详细的描述。
type RawNode struct {
	raft               *raft
	asyncStorageWrites bool

	// Mutable fields.
	prevSoftSt     *SoftState
	prevHardSt     pb.HardState
	stepsOnAdvance []pb.Message
}

// NewRawNode instantiates a RawNode from the given configuration.
// NewRawNode从给定的配置实例化一个RawNode。
//
// See Bootstrap() for bootstrapping an initial state; this replaces the former
// 'peers' argument to this method (with identical behavior). However, It is
// recommended that instead of calling Bootstrap, applications bootstrap their
// state manually by setting up a Storage that has a first index > 1 and which
// stores the desired ConfState as its InitialState.
// 有关引导初始状态的信息，请参见Bootstrap（）；这将替换此方法的以前的“peers”参数（具有相同的行为）。
// 但是，建议应用程序不要调用Bootstrap，而是通过设置具有fistIndex> 1的Storage，
// 并将所需的ConfState存储为其InitialState来手动引导其状态。
func NewRawNode(config *Config) (*RawNode, error) {
	r := newRaft(config)
	rn := &RawNode{
		raft: r,
	}
	rn.asyncStorageWrites = config.AsyncStorageWrites
	ss := r.softState()
	rn.prevSoftSt = &ss
	rn.prevHardSt = r.hardState()
	return rn, nil
}

// Tick advances the internal logical clock by a single tick.
// Tick通过单个时钟周期推进内部逻辑时钟。
func (rn *RawNode) Tick() {
	rn.raft.tick()
}

// TickQuiesced advances the internal logical clock by a single tick without
// performing any other state machine processing. It allows the caller to avoid
// periodic heartbeats and elections when all of the peers in a Raft group are
// known to be at the same state. Expected usage is to periodically invoke Tick
// or TickQuiesced depending on whether the group is "active" or "quiesced".
// TickQuiesced通过单个时钟周期推进内部逻辑时钟，而不执行任何其他状态机处理。
// 当知道Raft组中的所有对等方处于相同状态时，它允许调用者避免定期心跳和选举。
// 预期的用法是定期调用Tick或TickQuiesced，具体调用哪一个方法取决于该raft group是“活动”还是“静止”。
//
// WARNING: Be very careful about using this method as it subverts the Raft
// state machine. You should probably be using Tick instead.
// 警告：在使用此方法时要非常小心，因为它会破坏Raft状态机。您可能应该使用Tick。
//
// DEPRECATED: This method will be removed in a future release.
// 已弃用：此方法将在将来的版本中删除。
func (rn *RawNode) TickQuiesced() {
	rn.raft.electionElapsed++
}

// Campaign causes this RawNode to transition to candidate state.
// Campaign导致此RawNode转换为候选人状态。
func (rn *RawNode) Campaign() error {
	return rn.raft.Step(pb.Message{
		Type: pb.MsgHup,
	})
}

// Propose proposes data be appended to the raft log.
// Propose建议将数据附加到raft日志。
func (rn *RawNode) Propose(data []byte) error {
	return rn.raft.Step(pb.Message{
		Type: pb.MsgProp,
		From: rn.raft.id,
		Entries: []pb.Entry{
			{Data: data},
		}})
}

// ProposeConfChange proposes a config change. See (Node).ProposeConfChange for
// details.
// ProposeConfChange提议配置更改。有关详细信息，请参见（Node）。ProposeConfChange。
func (rn *RawNode) ProposeConfChange(cc pb.ConfChangeI) error {
	m, err := confChangeToMsg(cc)
	if err != nil {
		return err
	}
	return rn.raft.Step(m)
}

// ApplyConfChange applies a config change to the local node. The app must call
// this when it applies a configuration change, except when it decides to reject
// the configuration change, in which case no call must take place.
// ApplyConfChange将配置更改应用于本地节点。应用程序必须在应用配置更改时调用此方法，
// 除非它决定拒绝配置更改，在这种情况下不得进行任何调用。
func (rn *RawNode) ApplyConfChange(cc pb.ConfChangeI) *pb.ConfState {
	cs := rn.raft.applyConfChange(cc.AsV2())
	return &cs
}

// Step advances the state machine using the given message.
// Step使用给定的消息推进状态机。
func (rn *RawNode) Step(m pb.Message) error {
	// Ignore unexpected local messages receiving over network.
	// 忽略通过网络接收到的意外本地消息。
	if IsLocalMsg(m.Type) && !IsLocalMsgTarget(m.From) {
		return ErrStepLocalMsg
	}
	if IsResponseMsg(m.Type) && !IsLocalMsgTarget(m.From) && rn.raft.trk.Progress[m.From] == nil {
		return ErrStepPeerNotFound
	}
	return rn.raft.Step(m)
}

// Ready returns the outstanding work that the application needs to handle. This
// includes appending and applying entries or a snapshot, updating the HardState,
// and sending messages. The returned Ready() *must* be handled and subsequently
// passed back via Advance().
// Ready返回应用程序需要处理的未完成工作。这包括附加和应用条目或快照，更新HardState和发送消息。
// 返回的Ready() *必须*被处理，并随后通过Advance()传递回来。
func (rn *RawNode) Ready() Ready {
	rd := rn.readyWithoutAccept()
	rn.acceptReady(rd)
	return rd
}

// readyWithoutAccept returns a Ready. This is a read-only operation, i.e. there
// is no obligation that the Ready must be handled.
// readyWithoutAccept返回一个Ready。这是一个只读操作，即没有义务必须处理这个Ready消息。
func (rn *RawNode) readyWithoutAccept() Ready {
	r := rn.raft

	rd := Ready{
		Entries:          r.raftLog.nextUnstableEnts(),
		CommittedEntries: r.raftLog.nextCommittedEnts(rn.applyUnstableEntries()),
		Messages:         r.msgs,
	}
	if softSt := r.softState(); !softSt.equal(rn.prevSoftSt) {
		// Allocate only when SoftState changes.
		escapingSoftSt := softSt
		rd.SoftState = &escapingSoftSt
	}
	if hardSt := r.hardState(); !isHardStateEqual(hardSt, rn.prevHardSt) {
		rd.HardState = hardSt
	}
	if r.raftLog.hasNextUnstableSnapshot() {
		rd.Snapshot = *r.raftLog.nextUnstableSnapshot()
	}
	if len(r.readStates) != 0 {
		rd.ReadStates = r.readStates
	}
	rd.MustSync = MustSync(r.hardState(), rn.prevHardSt, len(rd.Entries))

	if rn.asyncStorageWrites {
		// If async storage writes are enabled, enqueue messages to
		// local storage threads, where applicable.
		// 如果启用了异步存储写入，则将消息排队到本地存储线程（如果适用）。
		// 等待node统一处理这些消息即可
		if needStorageAppendMsg(r, rd) {
			m := newStorageAppendMsg(r, rd)
			rd.Messages = append(rd.Messages, m)
		}
		if needStorageApplyMsg(rd) {
			m := newStorageApplyMsg(r, rd)
			rd.Messages = append(rd.Messages, m)
		}
	} else {
		// If async storage writes are disabled, immediately enqueue
		// msgsAfterAppend to be sent out. The Ready struct contract
		// mandates that Messages cannot be sent until after Entries
		// are written to stable storage.
		// 如果禁用了异步存储写入，则立即将msgsAfterAppend中的消息依次入队到rd.Messages中，并等待发送。
		// Ready结构合同要求在将条目写入稳定存储后，才能发送Messages中的消息。
		for _, m := range r.msgsAfterAppend {
			if m.To != r.id {
				rd.Messages = append(rd.Messages, m)
			}
		}
	}

	return rd
}

// MustSync returns true if the hard state and count of Raft entries indicate
// that a synchronous write to persistent storage is required.
// 如果HardState和Raft条目的计数表明需要对持久存储进行同步写入，则MustSync返回true。
func MustSync(st, prevst pb.HardState, entsnum int) bool {
	// Persistent state on all servers:
	// (Updated on stable storage before responding to RPCs)
	// currentTerm
	// votedFor
	// log entries[]
	// 在所有服务器上的持久状态：
	// (在响应RPC之前更新到稳定存储,这意味着在回复任何RPC之前，这些状态必须写入稳定存储)
	// currentTerm
	// votedFor
	// log entries[]
	return entsnum != 0 || st.Vote != prevst.Vote || st.Term != prevst.Term
}

func needStorageAppendMsg(r *raft, rd Ready) bool {
	// Return true if log entries, hard state, or a snapshot need to be written
	// to stable storage. Also return true if any messages are contingent on all
	// prior MsgStorageAppend being processed.
	// 如果需要将日志条目、硬状态或快照写入稳定存储，则返回true。
	// 如果任何消息取决于所有之前的且正在被处理的MsgStorageAppend，则也返回true。
	return len(rd.Entries) > 0 ||
		!IsEmptyHardState(rd.HardState) ||
		!IsEmptySnap(rd.Snapshot) ||
		len(r.msgsAfterAppend) > 0
}

func needStorageAppendRespMsg(r *raft, rd Ready) bool {
	// Return true if raft needs to hear about stabilized entries or an applied
	// snapshot. See the comment in newStorageAppendRespMsg, which explains why
	// we check hasNextOrInProgressUnstableEnts instead of len(rd.Entries) > 0.
	// 如果raft需要了解稳定的条目或已应用的快照，则返回true。
	// 请参见newStorageAppendRespMsg中的注释，该注释解释了为什么我们检查hasNextOrInProgressUnstableEnts而不是len(rd.Entries) > 0。
	return r.raftLog.hasNextOrInProgressUnstableEnts() ||
		!IsEmptySnap(rd.Snapshot)
}

// newStorageAppendMsg creates the message that should be sent to the local
// append thread to instruct it to append log entries, write an updated hard
// state, and apply a snapshot. The message also carries a set of responses
// that should be delivered after the rest of the message is processed. Used
// with AsyncStorageWrites.
// newStorageAppendMsg创建应发送到本地append线程的消息，以指示其附加日志条目，写入更新的硬状态并应用快照。
// 该消息还携带一组响应，应在处理该消息的其余部分之后传递。与AsyncStorageWrites一起使用。
func newStorageAppendMsg(r *raft, rd Ready) pb.Message {
	m := pb.Message{
		Type:    pb.MsgStorageAppend,
		To:      LocalAppendThread,
		From:    r.id,
		Entries: rd.Entries,
	}
	if !IsEmptyHardState(rd.HardState) {
		// If the Ready includes a HardState update, assign each of its fields
		// to the corresponding fields in the Message. This allows clients to
		// reconstruct the HardState and save it to stable storage.
		// 如果Ready包含HardState更新，请将其各个字段分配给Message中的相应字段。
		// 这允许客户端重建HardState并将其保存到稳定存储。
		//
		// If the Ready does not include a HardState update, make sure to not
		// assign a value to any of the fields so that a HardState reconstructed
		// from them will be empty (return true from raft.IsEmptyHardState).
		// 如果Ready不包含HardState更新，请确保不为任何字段分配值，
		// 以便从中重建的HardState将为空（从raft.IsEmptyHardState返回true）。
		m.Term = rd.Term
		m.Vote = rd.Vote
		m.Commit = rd.Commit
	}
	if !IsEmptySnap(rd.Snapshot) {
		snap := rd.Snapshot
		m.Snapshot = &snap
	}
	// Attach all messages in msgsAfterAppend as responses to be delivered after
	// the message is processed, along with a self-directed MsgStorageAppendResp
	// to acknowledge the entry stability.
	// 将msgsAfterAppend中的所有消息附加为响应，以便在处理消息后传递给下层raft层，
	// 这会与一个自我定向的MsgStorageAppendResp一起，以确认条目的稳定性。
	//
	// NB: it is important for performance that MsgStorageAppendResp message be
	// handled after self-directed MsgAppResp messages on the leader (which will
	// be contained in msgsAfterAppend). This ordering allows the MsgAppResp
	// handling to use a fast-path in r.raftLog.term() before the newly appended
	// entries are removed from the unstable log.
	// 注意：对于性能来说，MsgStorageAppendResp消息在领导者上的自我定向的MsgAppResp消息之后
	// 处理是很重要的（这些消息将包含在msgsAfterAppend中）。这种顺序允许MsgAppResp
	// 在新的附加条目从不稳定日志中删除之前，使用r.raftLog.term()中的快速路径。
	m.Responses = r.msgsAfterAppend
	if needStorageAppendRespMsg(r, rd) {
		m.Responses = append(m.Responses, newStorageAppendRespMsg(r, rd))
	}
	return m
}

// newStorageAppendRespMsg creates the message that should be returned to node
// after the unstable log entries, hard state, and snapshot in the current Ready
// (along with those in all prior Ready structs) have been saved to stable
// storage.
// newStorageAppendRespMsg创建应返回给节点的消息，
// 该消息在当前Ready（以及所有先前的Ready结构中）中的不稳定日志条目、硬状态和快照已保存到稳定存储之后，
// 该消息应该被返回给节点。
func newStorageAppendRespMsg(r *raft, rd Ready) pb.Message {
	m := pb.Message{
		Type: pb.MsgStorageAppendResp,
		To:   r.id,
		From: LocalAppendThread,
		// Dropped after term change, see below.
		Term: r.Term,
	}
	if r.raftLog.hasNextOrInProgressUnstableEnts() {
		// If the raft log has unstable entries, attach the last index and term of the
		// append to the response message. This (index, term) tuple will be handed back
		// and consulted when the stability of those log entries is signaled to the
		// unstable. If the (index, term) match the unstable log by the time the
		// response is received (unstable.stableTo), the unstable log can be truncated.
		// 如果raft日志有不稳定的条目，请将附加的最后索引和term附加到响应消息中。
		// 当这些日志条目的稳定性被标记为不稳定时，这个(index, term)元组将被交还并咨询。
		// 如果(index, term)与响应接收时的不稳定日志匹配(unstable.stableTo)，则可以截断不稳定日志。
		//
		// However, with just this logic, there would be an ABA problem[^1] that could
		// lead to the unstable log and the stable log getting out of sync temporarily
		// and leading to an inconsistent view. Consider the following example with 5
		// nodes, A B C D E:
		// 然而，仅有这个逻辑，会有一个ABA问题[^1]，可能导致不稳定的日志和稳定的日志暂时不同步，
		// 并导致一个不一致的视图。考虑以下示例，其中有5个节点，A B C D E：
		//
		//  1. A is the leader. A是leader。
		//  2. A proposes some log entries but only B receives these entries.
		//     A提议了一些日志条目，但只有B收到了这些条目。
		//  3. B gets the Ready and the entries are appended asynchronously.
		//     B收到Ready并异步附加条目。
		//  4. A crashes and C becomes leader after getting a vote from D and E.
		//     A崩溃，C在从D和E获得投票后成为leader。
		//  5. C proposes some log entries and B receives these entries, overwriting the
		//     previous unstable log entries that are in the process of being appended.
		//     The entries have a larger term than the previous entries but the same
		//     indexes. It begins appending these new entries asynchronously.
		//     C提议了一些日志条目，B收到了这些条目，覆盖了正在附加的先前不稳定的日志条目。
		//     这些条目的term大于先前的条目，但索引相同。它开始异步附加这些新条目。
		//  6. C crashes and A restarts and becomes leader again after getting the vote
		//     from D and E.
		//     C崩溃，A重新启动并在从D和E获得投票后再次成为leader。
		//  7. B receives the entries from A which are the same as the ones from step 2,
		//     overwriting the previous unstable log entries that are in the process of
		//     being appended from step 5. The entries have the original terms and
		//     indexes from step 2. Recall that log entries retain their original term
		//     numbers when a leader replicates entries from previous terms. It begins
		//     appending these new entries asynchronously.
		//     B收到了A的条目，这些条目与步骤2中的条目相同，覆盖了正在从步骤5中附加的先前不稳定的日志条目。
		//     这些条目具有步骤2中的原始term和索引。回想一下，当领导者复制先前term的条目时，日志条目会保留其原始term编号。
		//     它开始异步附加这些新条目。
		//  8. The asynchronous log appends from the first Ready complete and stableTo
		//     is called.
		//     第一个Ready的异步日志附加完成，并调用stableTo。
		//  9. However, the log entries from the second Ready are still in the
		//     asynchronous append pipeline and will overwrite (in stable storage) the
		//     entries from the first Ready at some future point. We can't truncate the
		//     unstable log yet or a future read from Storage might see the entries from
		//     step 5 before they have been replaced by the entries from step 7.
		//     Instead, we must wait until we are sure that the entries are stable and
		//     that no in-progress appends might overwrite them before removing entries
		//     from the unstable log.
		//     然而，第二个Ready中的日志条目仍然在异步附加管道中，并且将在将来的某个时间点覆盖（在稳定存储中）第一个Ready中的条目。
		//     我们还不能截断不稳定的日志，否则Storage中的未来读取可能会在步骤7的日志被写入到Storage之前看到步骤5中写入的日志。(意味着步骤7的异步写入还没有完成)
		//     相反，我们必须等到我们可以确定条目是稳定的，并且没有正在进行的附加操作会覆盖它们，然后才能从不稳定的日志中删除条目。

		//
		// To prevent these kinds of problems, we also attach the current term to the
		// MsgStorageAppendResp (above). If the term has changed by the time the
		// MsgStorageAppendResp if returned, the response is ignored and the unstable
		// log is not truncated. The unstable log is only truncated when the term has
		// remained unchanged from the time that the MsgStorageAppend was sent to the
		// time that the MsgStorageAppendResp is received, indicating that no-one else
		// is in the process of truncating the stable log.
		// 为了防止这些问题，我们还将当前term附加到MsgStorageAppendResp（上面）。
		// 如果在MsgStorageAppendResp返回时term发生了变化，则会忽略响应，并且不会截断不稳定的日志。
		// 只有当term从发送MsgStorageAppend到接收MsgStorageAppendResp的时间保持不变时，才会截断不稳定的日志，
		// 这表明没有其他人正在截断稳定的日志。
		//
		// However, this replaces a correctness problem with a liveness problem. If we
		// only attempted to truncate the unstable log when appending new entries but
		// also occasionally dropped these responses, then quiescence of new log entries
		// could lead to the unstable log never being truncated.
		// 然而，这用一个存活问题替换了一个正确性问题。如果我们只在附加新条目时尝试截断不稳定的日志，
		// 但也偶尔丢弃这些响应，那么新日志条目的静止可能会导致不稳定的日志永远不会被截断。
		//
		// To combat this, we attempt to truncate the log on all MsgStorageAppendResp
		// messages where the unstable log is not empty, not just those associated with
		// entry appends. This includes MsgStorageAppendResp messages associated with an
		// updated HardState, which occur after a term change.
		// 为了解决这个问题，我们不仅仅在append条目时尝试截断日志，而是在所有MsgStorageAppendResp消息上尝试截断日志，
		// 其中不稳定的日志不为空。这些消息包括与更新的HardState相关的MsgStorageAppendResp消息，这些消息发生在term更改后。
		//
		// In other words, we set Index and LogTerm in a block that looks like:
		// 换句话说，我们在一个块中设置Index和LogTerm，看起来像：
		//
		//  if r.raftLog.hasNextOrInProgressUnstableEnts() { ... }
		//
		// not like:
		//
		//  if len(rd.Entries) > 0 { ... }
		//
		// To do so, we attach r.raftLog.lastIndex() and r.raftLog.lastTerm(), not the
		// (index, term) of the last entry in rd.Entries. If rd.Entries is not empty,
		// these will be the same. However, if rd.Entries is empty, we still want to
		// attest that this (index, term) is correct at the current term, in case the
		// MsgStorageAppend that contained the last entry in the unstable slice carried
		// an earlier term and was dropped.
		// 为此，我们附加r.raftLog.lastIndex()和r.raftLog.lastTerm()，而不是rd.Entries中最后一个条目的(index, term)。
		// 如果rd.Entries不为空，这些将是相同的。然而，如果rd.Entries为空，我们仍然希望在当前term证明这个(index, term)是正确的，
		// 以防一个MsgStorageAppend消息包含了unstable slice中最后一个条目、却由于携带了一个较早的term而被丢弃。
		//
		// A MsgStorageAppend with a new term is emitted on each term change. This is
		// the same condition that causes MsgStorageAppendResp messages with earlier
		// terms to be ignored. As a result, we are guaranteed that, assuming a bounded
		// number of term changes, there will eventually be a MsgStorageAppendResp
		// message that is not ignored. This means that entries in the unstable log
		// which have been appended to stable storage will eventually be truncated and
		// dropped from memory.
		// 每次term更改时都会发出一个新term的MsgStorageAppend。这是导致忽略具有较早term的MsgStorageAppendResp消息的相同条件。
		// 因此，我们保证，假设有一个有界数量的term更改，最终将有一个不被忽略的MsgStorageAppendResp消息。
		// 这意味着已附加到稳定存储的不稳定日志中的条目最终将被截断并从内存中删除。
		//
		// [^1]: https://en.wikipedia.org/wiki/ABA_problem
		last := r.raftLog.lastEntryID()
		m.Index = last.index
		m.LogTerm = last.term
	}
	if !IsEmptySnap(rd.Snapshot) {
		snap := rd.Snapshot
		m.Snapshot = &snap
	}
	return m
}

func needStorageApplyMsg(rd Ready) bool     { return len(rd.CommittedEntries) > 0 }
func needStorageApplyRespMsg(rd Ready) bool { return needStorageApplyMsg(rd) }

// newStorageApplyMsg creates the message that should be sent to the local
// apply thread to instruct it to apply committed log entries. The message
// also carries a response that should be delivered after the rest of the
// message is processed. Used with AsyncStorageWrites.
// newStorageApplyMsg创建应发送到本地apply线程的消息，以指示其应用已提交的日志条目。
// 该消息还携带一个响应, 在处理消息的其余部分之后再传递。与AsyncStorageWrites一起使用。
func newStorageApplyMsg(r *raft, rd Ready) pb.Message {
	ents := rd.CommittedEntries
	return pb.Message{
		Type:    pb.MsgStorageApply,
		To:      LocalApplyThread,
		From:    r.id,
		Term:    0, // committed entries don't apply under a specific term
		Entries: ents,
		Responses: []pb.Message{
			newStorageApplyRespMsg(r, ents),
		},
	}
}

// newStorageApplyRespMsg creates the message that should be returned to node
// after the committed entries in the current Ready (along with those in all
// prior Ready structs) have been applied to the local state machine.
// newStorageApplyRespMsg创建应返回给节点的消息，在当前Ready（以及所有先前的Ready结构中）中的已提交的条目已经
// 应用到本地状态机之后，该消息应该被返回给节点。
func newStorageApplyRespMsg(r *raft, ents []pb.Entry) pb.Message {
	return pb.Message{
		Type:    pb.MsgStorageApplyResp,
		To:      r.id,
		From:    LocalApplyThread,
		Term:    0, // committed entries don't apply under a specific term
		Entries: ents,
	}
}

// acceptReady is called when the consumer of the RawNode has decided to go
// ahead and handle a Ready. Nothing must alter the state of the RawNode between
// this call and the prior call to Ready().
// acceptReady在RawNode的使用者决定继续处理Ready时调用。
// 在此调用和之前对Ready()的调用之间，不得更改RawNode的状态。
// 意思是当一个节点正在处理Ready的时候，不允许其他的goroutine去修改RawNode的状态。
func (rn *RawNode) acceptReady(rd Ready) {
	if rd.SoftState != nil {
		rn.prevSoftSt = rd.SoftState
	}
	if !IsEmptyHardState(rd.HardState) {
		rn.prevHardSt = rd.HardState
	}
	if len(rd.ReadStates) != 0 {
		rn.raft.readStates = nil
	}
	if !rn.asyncStorageWrites {
		if len(rn.stepsOnAdvance) != 0 {
			rn.raft.logger.Panicf("two accepted Ready structs without call to Advance")
		}
		for _, m := range rn.raft.msgsAfterAppend {
			if m.To == rn.raft.id {
				rn.stepsOnAdvance = append(rn.stepsOnAdvance, m)
			}
		}
		if needStorageAppendRespMsg(rn.raft, rd) {
			m := newStorageAppendRespMsg(rn.raft, rd)
			rn.stepsOnAdvance = append(rn.stepsOnAdvance, m)
		}
		if needStorageApplyRespMsg(rd) {
			m := newStorageApplyRespMsg(rn.raft, rd.CommittedEntries)
			rn.stepsOnAdvance = append(rn.stepsOnAdvance, m)
		}
	}
	rn.raft.msgs = nil
	rn.raft.msgsAfterAppend = nil
	rn.raft.raftLog.acceptUnstable()
	if len(rd.CommittedEntries) > 0 {
		ents := rd.CommittedEntries
		index := ents[len(ents)-1].Index
		rn.raft.raftLog.acceptApplying(index, entsSize(ents), rn.applyUnstableEntries())
	}
}

// applyUnstableEntries returns whether entries are allowed to be applied once
// they are known to be committed but before they have been written locally to
// stable storage.
// applyUnstableEntries: 若存在已知被提交、但是还未写入到稳定存储的条目，是否允许应用这些条目。
// 这与异步存储写入有关，如果异步存储写入被禁用，意味着允许应用这些条目。否则，不允许应用这些条目。
func (rn *RawNode) applyUnstableEntries() bool {
	return !rn.asyncStorageWrites
}

// HasReady called when RawNode user need to check if any Ready pending.
// HasReady在RawNode用户需要检查是否有任何Ready挂起时调用。
func (rn *RawNode) HasReady() bool {
	// TODO(nvanbenschoten): order these cases in terms of cost and frequency.
	r := rn.raft
	if softSt := r.softState(); !softSt.equal(rn.prevSoftSt) {
		return true
	}
	if hardSt := r.hardState(); !IsEmptyHardState(hardSt) && !isHardStateEqual(hardSt, rn.prevHardSt) {
		return true
	}
	if r.raftLog.hasNextUnstableSnapshot() {
		return true
	}
	if len(r.msgs) > 0 || len(r.msgsAfterAppend) > 0 {
		return true
	}
	if r.raftLog.hasNextUnstableEnts() || r.raftLog.hasNextCommittedEnts(rn.applyUnstableEntries()) {
		return true
	}
	if len(r.readStates) != 0 {
		return true
	}
	return false
}

// Advance notifies the RawNode that the application has applied and saved progress in the
// last Ready results.
// Advance通知RawNode：应用程序已经在最后的Ready结果中应用并保存了进度。
//
// NOTE: Advance must not be called when using AsyncStorageWrites. Response messages from
// the local append and apply threads take its place.
// 注意：在使用AsyncStorageWrites时，不得调用Advance。local append和apply线程的响应消息会代替Advance。
func (rn *RawNode) Advance(_ Ready) {
	// The actions performed by this function are encoded into stepsOnAdvance in
	// acceptReady. In earlier versions of this library, they were computed from
	// the provided Ready struct. Retain the unused parameter for compatibility.
	// 此函数执行的操作已编码到acceptReady中的stepsOnAdvance中。
	// 在此库的早期版本中，它们是从提供的Ready结构中计算出来的。为了兼容性，保留未使用的参数。
	if rn.asyncStorageWrites {
		rn.raft.logger.Panicf("Advance must not be called when using AsyncStorageWrites")
	}
	for i, m := range rn.stepsOnAdvance {
		_ = rn.raft.Step(m)
		rn.stepsOnAdvance[i] = pb.Message{}
	}
	rn.stepsOnAdvance = rn.stepsOnAdvance[:0]
}

// Status returns the current status of the given group. This allocates, see
// BasicStatus and WithProgress for allocation-friendlier choices.
// Status返回给定的Raft Group的当前状态。这会分配内存，有关分配更友好的选择，请参见BasicStatus和WithProgress。
func (rn *RawNode) Status() Status {
	status := getStatus(rn.raft)
	return status
}

// BasicStatus returns a BasicStatus. Notably this does not contain the
// Progress map; see WithProgress for an allocation-free way to inspect it.
// BasicStatus返回一个BasicStatus。特别是，它不包含Progress map；请参见WithProgress，以无分配的方式检查它。
func (rn *RawNode) BasicStatus() BasicStatus {
	return getBasicStatus(rn.raft)
}

// ProgressType indicates the type of replica a Progress corresponds to.
// ProgressType表示Progress对应的副本的类型。
type ProgressType byte

const (
	// ProgressTypePeer accompanies a Progress for a regular peer replica.
	// ProgressTypePeer伴随着一个常规对等体副本的Progress。
	ProgressTypePeer ProgressType = iota
	// ProgressTypeLearner accompanies a Progress for a learner replica.
	// ProgressTypeLearner伴随着一个学习者副本的Progress。
	ProgressTypeLearner
)

// WithProgress is a helper to introspect the Progress for this node and its
// peers.
// WithProgress是一个帮助函数，用于检查此节点及其对等体的Progress。
func (rn *RawNode) WithProgress(visitor func(id uint64, typ ProgressType, pr tracker.Progress)) {
	rn.raft.trk.Visit(func(id uint64, pr *tracker.Progress) {
		typ := ProgressTypePeer
		if pr.IsLearner {
			typ = ProgressTypeLearner
		}
		p := *pr
		p.Inflights = nil
		visitor(id, typ, p)
	})
}

// ReportUnreachable reports the given node is not reachable for the last send.
// ReportUnreachable报告最后一次发送的给定节点不可达。
func (rn *RawNode) ReportUnreachable(id uint64) {
	_ = rn.raft.Step(pb.Message{Type: pb.MsgUnreachable, From: id})
}

// ReportSnapshot reports the status of the sent snapshot.
// ReportSnapshot报告发送的快照的状态。
func (rn *RawNode) ReportSnapshot(id uint64, status SnapshotStatus) {
	rej := status == SnapshotFailure

	_ = rn.raft.Step(pb.Message{Type: pb.MsgSnapStatus, From: id, Reject: rej})
}

// TransferLeader tries to transfer leadership to the given transferee.
// TransferLeader尝试将领导权转移到给定的接收者。
func (rn *RawNode) TransferLeader(transferee uint64) {
	_ = rn.raft.Step(pb.Message{Type: pb.MsgTransferLeader, From: transferee})
}

// ForgetLeader forgets a follower's current leader, changing it to None.
// See (Node).ForgetLeader for details.
// ForgetLeader忘记跟随者的当前领导者，将其更改为None。
func (rn *RawNode) ForgetLeader() error {
	return rn.raft.Step(pb.Message{Type: pb.MsgForgetLeader})
}

// ReadIndex requests a read state. The read state will be set in ready.
// Read State has a read index. Once the application advances further than the read
// index, any linearizable read requests issued before the read request can be
// processed safely. The read state will have the same rctx attached.
// ReadIndex请求一个read state。read state将在ready中设置。
// Read State具有一个read index。一旦应用程序超过了read index，任何在read请求之前发出的可线性化读请求都可以安全地处理。
// read state将附加相同的rctx。
func (rn *RawNode) ReadIndex(rctx []byte) {
	_ = rn.raft.Step(pb.Message{Type: pb.MsgReadIndex, Entries: []pb.Entry{{Data: rctx}}})
}
