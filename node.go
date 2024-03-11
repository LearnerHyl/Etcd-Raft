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
	"context"
	"errors"

	pb "go.etcd.io/raft/v3/raftpb"
)

type SnapshotStatus int

const (
	SnapshotFinish  SnapshotStatus = 1
	SnapshotFailure SnapshotStatus = 2
)

var (
	emptyState = pb.HardState{}

	// ErrStopped is returned by methods on Nodes that have been stopped.
	ErrStopped = errors.New("raft: stopped")
)

// SoftState provides state that is useful for logging and debugging.
// The state is volatile and does not need to be persisted to the WAL.
type SoftState struct {
	Lead      uint64 // must use atomic operations to access; keep 64-bit aligned.
	RaftState StateType
}

func (a *SoftState) equal(b *SoftState) bool {
	return a.Lead == b.Lead && a.RaftState == b.RaftState
}

// Ready encapsulates the entries and messages that are ready to read,
// be saved to stable storage, committed or sent to other peers.
// All fields in Ready are read-only.
type Ready struct {
	// The current volatile state of a Node.
	// SoftState will be nil if there is no update.
	// It is not required to consume or store SoftState.
	// 节点的当前易失性状态。如果没有更新，则SoftState将为nil。不需要消耗或存储SoftState。
	*SoftState

	// The current state of a Node to be saved to stable storage BEFORE
	// Messages are sent.
	//
	// HardState will be equal to empty state if there is no update.
	//
	// If async storage writes are enabled, this field does not need to be acted
	// on immediately. It will be reflected in a MsgStorageAppend message in the
	// Messages slice.
	// 节点的当前状态在消息发送之前保存到稳定存储中。如果没有更新，HardState将等于空状态。
	// 如果启用了异步存储写入，则不需要立即对此字段进行操作。它将在Messages切片中的MsgStorageAppend消息中反映出来。
	pb.HardState

	// ReadStates can be used for node to serve linearizable read requests locally
	// when its applied index is greater than the index in ReadState.
	// Note that the readState will be returned when raft receives msgReadIndex.
	// The returned is only valid for the request that requested to read.
	// 当节点的appliedIndex大于ReadState中的索引时，ReadStates可以用于节点本地提供线性读请求。
	// 请注意，当raft接收到msgReadIndex时，将返回readState。返回值仅对请求读取的请求有效。
	ReadStates []ReadState

	// Entries specifies entries to be saved to stable storage BEFORE
	// Messages are sent.
	//
	// If async storage writes are enabled, this field does not need to be acted
	// on immediately. It will be reflected in a MsgStorageAppend message in the
	// Messages slice.
	// 如果启用了async storage writes，则不需要立即对此字段进行操作。它将在Messages切片中的MsgStorageAppend消息中反映出来。
	Entries []pb.Entry

	// Snapshot specifies the snapshot to be saved to stable storage.
	//
	// If async storage writes are enabled, this field does not need to be acted
	// on immediately. It will be reflected in a MsgStorageAppend message in the
	// Messages slice.
	// Snapshot指定要保存到稳定存储中的快照。
	// 如果启用了async storage writes，则不需要立即对此字段进行操作。它将在Messages切片中的MsgStorageAppend消息中反映出来。
	Snapshot pb.Snapshot

	// CommittedEntries specifies entries to be committed to a
	// store/state-machine. These have previously been appended to stable
	// storage.
	//
	// If async storage writes are enabled, this field does not need to be acted
	// on immediately. It will be reflected in a MsgStorageApply message in the
	// Messages slice.
	// CommittedEntries指定要提交到存储/状态机的条目。这些条目先前已附加到稳定存储中。
	// 如果启用了async storage writes，则不需要立即对此字段进行操作。它将在Messages切片中的MsgStorageApply消息中反映出来。
	CommittedEntries []pb.Entry

	// Messages specifies outbound messages.
	//
	// If async storage writes are not enabled, these messages must be sent
	// AFTER Entries are appended to stable storage.
	//
	// If async storage writes are enabled, these messages can be sent
	// immediately as the messages that have the completion of the async writes
	// as a precondition are attached to the individual MsgStorage{Append,Apply}
	// messages instead.
	//
	// If it contains a MsgSnap message, the application MUST report back to raft
	// when the snapshot has been received or has failed by calling ReportSnapshot.
	// Messages指定该节点需要发送的消息。
	// 如果未启用async storage writes，则这些消息必须在Entries附加到稳定存储之后发送。
	// 如果启用了async storage writes，则这些消息可以立即发送，因为当消息MsgStorage{Append,Apply}附加到Messages中时，则意味着该异步写入已完成。
	Messages []pb.Message

	// MustSync indicates whether the HardState and Entries must be durably
	// written to disk or if a non-durable write is permissible.
	// MustSync指示是否必须将HardState和Entries耐久地写入磁盘，或者是否允许非耐久性写入。
	MustSync bool
}

func isHardStateEqual(a, b pb.HardState) bool {
	return a.Term == b.Term && a.Vote == b.Vote && a.Commit == b.Commit
}

// IsEmptyHardState returns true if the given HardState is empty.
func IsEmptyHardState(st pb.HardState) bool {
	return isHardStateEqual(st, emptyState)
}

// IsEmptySnap returns true if the given Snapshot is empty.
func IsEmptySnap(sp pb.Snapshot) bool {
	return sp.Metadata.Index == 0
}

// Node represents a node in a raft cluster.
type Node interface {
	// Tick increments the internal logical clock for the Node by a single tick. Election
	// timeouts and heartbeat timeouts are in units of ticks.
	// Tick方法通过一个时钟周期增加节点的内部逻辑时钟。选举超时和心跳超时的单位是时钟周期。
	Tick()
	// Campaign causes the Node to transition to candidate state and start campaigning to become leader.
	// Campaign方法导致节点转换为候选人状态，并开始竞选成为领导者。
	Campaign(ctx context.Context) error
	// Propose proposes that data be appended to the log. Note that proposals can be lost without
	// notice, therefore it is user's job to ensure proposal retries.
	// Propose方法建议将数据附加到日志中。请注意，propose可能会在不通知应用层的情况下丢失，因此用户需要确保propose重试机制。
	Propose(ctx context.Context, data []byte) error
	// ProposeConfChange proposes a configuration change. Like any proposal, the
	// configuration change may be dropped with or without an error being
	// returned. In particular, configuration changes are dropped unless the
	// leader has certainty that there is no prior unapplied configuration
	// change in its log.
	//
	// The method accepts either a pb.ConfChange (deprecated) or pb.ConfChangeV2
	// message. The latter allows arbitrary configuration changes via joint
	// consensus, notably including replacing a voter. Passing a ConfChangeV2
	// message is only allowed if all Nodes participating in the cluster run a
	// version of this library aware of the V2 API. See pb.ConfChangeV2 for
	// usage details and semantics.
	// ProposeConfChange提议配置更改。与任何提议一样，配置更改可能会被丢弃，也可能会返回错误。特别是，除非领导者确定其日志中没有未应用的配置更改，否则将丢弃配置更改。
	// 该方法接受pb.ConfChange（已弃用）或pb.ConfChangeV2消息。
	// 后者允许通过联合共识进行任意配置更改，特别是包括替换投票者。
	// 仅当参与集群的所有节点运行的版本都知道V2 API时，才允许传递ConfChangeV2消息。有关用法详细信息和语义，请参见pb.ConfChangeV2。
	ProposeConfChange(ctx context.Context, cc pb.ConfChangeI) error

	// Step advances the state machine using the given message. ctx.Err() will be returned, if any.
	// Step方法使用给定的消息推进状态机。如果有，则返回ctx.Err()。
	Step(ctx context.Context, msg pb.Message) error

	// Ready returns a channel that returns the current point-in-time state.
	// Users of the Node must call Advance after retrieving the state returned by Ready (unless
	// async storage writes is enabled, in which case it should never be called).
	//
	// NOTE: No committed entries from the next Ready may be applied until all committed entries
	// and snapshots from the previous one have finished.
	// Ready返回一个Channel，该Channel返回当前的即时状态。节点的用户必须在检索Ready返回的状态后调用Advance（除非启用了async storage writes，在启用异步存储写入的情况下永远不应该调用Advance函数）。
	// 注意：在上一个Ready的所有已提交条目和快照都完成之前，不得应用下一个Ready中的已提交条目。
	Ready() <-chan Ready

	// Advance notifies the Node that the application has saved progress up to the last Ready.
	// It prepares the node to return the next available Ready.
	//
	// The application should generally call Advance after it applies the entries in last Ready.
	//
	// However, as an optimization, the application may call Advance while it is applying the
	// commands. For example. when the last Ready contains a snapshot, the application might take
	// a long time to apply the snapshot data. To continue receiving Ready without blocking raft
	// progress, it can call Advance before finishing applying the last ready.
	//
	// NOTE: Advance must not be called when using AsyncStorageWrites. Response messages from the
	// local append and apply threads take its place.
	// Advance通知Node应用程序已将进度保存到最后一个Ready。因此，Node准备返回下一个可用的Ready。
	// 应用程序通常在应用最后一个Ready中的条目后调用Advance。
	// 但是，作为优化，应用程序可以在应用命令时调用Advance。例如，当最后一个Ready包含快照时，应用程序可能需要很长时间来应用快照数据。为了继续接收Ready而不阻塞raft的进度，它可以在完成应用最后一个ready之前调用Advance。
	// 注意：在使用AsyncStorageWrites时，不得调用Advance。local append和apply线程的响应Messages代替了Advance。
	Advance()
	// ApplyConfChange applies a config change (previously passed to
	// ProposeConfChange) to the node. This must be called whenever a config
	// change is observed in Ready.CommittedEntries, except when the app decides
	// to reject the configuration change (i.e. treats it as a noop instead), in
	// which case it must not be called.
	//
	// Returns an opaque non-nil ConfState protobuf which must be recorded in
	// snapshots.
	// ApplyConfChange将配置更改（先前传递给ProposeConfChange的命令）应用到节点。
	// 每当在Ready.CommittedEntries中观察到配置更改时，都必须调用此方法，除非应用程序决定拒绝配置更改（即将其视为no-op操作），在这种情况下，不得调用此方法。
	// 返回一个不透明的非nil的ConfState protobuf(grpc的一种序列化协议)，该protobuf必须记录在快照中。
	ApplyConfChange(cc pb.ConfChangeI) *pb.ConfState

	// TransferLeadership attempts to transfer leadership to the given transferee.
	// TransferLeadership尝试将领导权转移给给定的接收者。
	TransferLeadership(ctx context.Context, lead, transferee uint64)

	// ForgetLeader forgets a follower's current leader, changing it to None. It
	// remains a leaderless follower in the current term, without campaigning.
	//
	// This is useful with PreVote+CheckQuorum, where followers will normally not
	// grant pre-votes if they've heard from the leader in the past election
	// timeout interval. Leaderless followers can grant pre-votes immediately, so
	// if a quorum of followers have strong reason to believe the leader is dead
	// (for example via a side-channel or external failure detector) and forget it
	// then they can elect a new leader immediately, without waiting out the
	// election timeout. They will also revert to normal followers if they hear
	// from the leader again, or transition to candidates on an election timeout.
	//
	// For example, consider a three-node cluster where 1 is the leader and 2+3
	// have just received a heartbeat from it. If 2 and 3 believe the leader has
	// now died (maybe they know that an orchestration system shut down 1's VM),
	// we can instruct 2 to forget the leader and 3 to campaign. 2 will then be
	// able to grant 3's pre-vote and elect 3 as leader immediately (normally 2
	// would reject the vote until an election timeout passes because it has heard
	// from the leader recently). However, 3 can not campaign unilaterally, a
	// quorum have to agree that the leader is dead, which avoids disrupting the
	// leader if individual nodes are wrong about it being dead.
	//
	// This does nothing with ReadOnlyLeaseBased, since it would allow a new
	// leader to be elected without the old leader knowing.
	// ForgetLeader忘记跟随者的当前领导者，将其更改为None。它在当前任期中仍然是一个无领导的跟随者，不会进行竞选。
	// 这在PreVote+CheckQuorum中很有用，因为通常情况下，如果跟随者在过去的选举超时间隔内听到过领导者的消息，它们通常不会授予pre-votes给候选人。
	// 无领导的跟随者可以立即授予pre-votes，因此如果大多数跟随者有充分的理由相信领导者已经死亡（例如通过side-channel或外部故障检测器），并且忘记了它，那么它们可以立即选举新的领导者，而不必等待选举超时。
	// 如果它们再次听到领导者的消息，它们将恢复为正常的跟随者，或者在选举超时时转换为候选人。
	// 例如，考虑一个三节点集群，其中1是领导者，2+3刚刚收到了来自1的心跳。如果2和3相信领导者现在已经死亡（也许它们知道编排系统关闭了1的VM），我们可以指示2忘记领导者，3进行竞选。
	// 然后2将能够授予3的pre-vote，并立即选举3为领导者（通常情况下，2会拒绝投票，直到选举超时结束，因为它最近听到了领导者的消息）。
	// 但是，3不能单方面进行竞选，必须有大多数节点同意领导者已经死亡，这样可以避免在个别节点错误地认为领导者已经死亡时干扰领导者。
	// 这在ReadOnlyLeaseBased中无效，因为它会允许新的领导者在旧的领导者不知道的情况下被选举。
	ForgetLeader(ctx context.Context) error

	// ReadIndex request a read state. The read state will be set in the ready.
	// Read state has a read index. Once the application advances further than the read
	// index, any linearizable read requests issued before the read request can be
	// processed safely. The read state will have the same rctx attached.
	// Note that request can be lost without notice, therefore it is user's job
	// to ensure read index retries.
	// ReadIndex请求一个read state。read state将设置在ready中。read state具有一个read index。一旦应用程序进度超过read index，那么在发出read请求之前的任何线性读请求都可以安全地处理。read state将附加相同的rctx。
	// 请注意，请求可能会在没有通知的情况下丢失，因此用户需要确保read index重试。
	ReadIndex(ctx context.Context, rctx []byte) error

	// Status returns the current status of the raft state machine.
	// Status返回raft状态机的当前状态。
	Status() Status
	// ReportUnreachable reports the given node is not reachable for the last send.
	// ReportUnreachable报告最后一次发送的给定节点不可达。
	ReportUnreachable(id uint64)
	// ReportSnapshot reports the status of the sent snapshot. The id is the raft ID of the follower
	// who is meant to receive the snapshot, and the status is SnapshotFinish or SnapshotFailure.
	// Calling ReportSnapshot with SnapshotFinish is a no-op. But, any failure in applying a
	// snapshot (for e.g., while streaming it from leader to follower), should be reported to the
	// leader with SnapshotFailure. When leader sends a snapshot to a follower, it pauses any raft
	// log probes until the follower can apply the snapshot and advance its state. If the follower
	// can't do that, for e.g., due to a crash, it could end up in a limbo, never getting any
	// updates from the leader. Therefore, it is crucial that the application ensures that any
	// failure in snapshot sending is caught and reported back to the leader; so it can resume raft
	// log probing in the follower.
	// ReportSnapshot报告发送的快照的状态。id是应该接收快照的跟随者的raft ID，status是SnapshotFinish或SnapshotFailure。使用SnapshotFinish调用ReportSnapshot是一个空操作。
	// 但是，应该将任何应用快照失败（例如，从领导者流式传输到跟随者时）报告给领导者。当领导者向某个跟随者发送快照时，
	// 它会暂停对当前follower的raft日志推进，直到跟随者能够应用快照并推进其状态。这意味着在当前follower应用快照之前，领导者不会向其发送任何新的日志条目。
	// 如果跟随者无法做到这一点，例如由于崩溃，它可能会陷入僵局，永远不会从领导者那里获得任何更新。
	// 因此，应用程序必须确保捕获并报告快照发送失败，以便它可以恢复跟随者中的raft日志探测。
	ReportSnapshot(id uint64, status SnapshotStatus)
	// Stop performs any necessary termination of the Node.
	// Stop执行节点的任何必要终止操作。
	Stop()
}

type Peer struct {
	ID      uint64
	Context []byte
}

func setupNode(c *Config, peers []Peer) *node {
	if len(peers) == 0 {
		panic("no peers given; use RestartNode instead")
	}
	rn, err := NewRawNode(c)
	if err != nil {
		panic(err)
	}
	err = rn.Bootstrap(peers)
	if err != nil {
		c.Logger.Warningf("error occurred during starting a new node: %v", err)
	}

	n := newNode(rn)
	return &n
}

// StartNode returns a new Node given configuration and a list of raft peers.
// It appends a ConfChangeAddNode entry for each given peer to the initial log.
//
// Peers must not be zero length; call RestartNode in that case.
func StartNode(c *Config, peers []Peer) Node {
	n := setupNode(c, peers)
	go n.run()
	return n
}

// RestartNode is similar to StartNode but does not take a list of peers.
// The current membership of the cluster will be restored from the Storage.
// If the caller has an existing state machine, pass in the last log index that
// has been applied to it; otherwise use zero.
func RestartNode(c *Config) Node {
	rn, err := NewRawNode(c)
	if err != nil {
		panic(err)
	}
	n := newNode(rn)
	go n.run()
	return &n
}

type msgWithResult struct {
	m      pb.Message
	result chan error
}

// node is the canonical implementation of the Node interface
// node是Node接口的规范实现
type node struct {
	propc      chan msgWithResult
	recvc      chan pb.Message
	confc      chan pb.ConfChangeV2
	confstatec chan pb.ConfState
	readyc     chan Ready
	advancec   chan struct{}
	tickc      chan struct{}
	done       chan struct{}
	stop       chan struct{}
	status     chan chan Status

	rn *RawNode
}

func newNode(rn *RawNode) node {
	return node{
		propc:      make(chan msgWithResult),
		recvc:      make(chan pb.Message),
		confc:      make(chan pb.ConfChangeV2),
		confstatec: make(chan pb.ConfState),
		readyc:     make(chan Ready),
		advancec:   make(chan struct{}),
		// make tickc a buffered chan, so raft node can buffer some ticks when the node
		// is busy processing raft messages. Raft node will resume process buffered
		// ticks when it becomes idle.
		tickc:  make(chan struct{}, 128),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		status: make(chan chan Status),
		rn:     rn,
	}
}

func (n *node) Stop() {
	select {
	case n.stop <- struct{}{}:
		// Not already stopped, so trigger it
	case <-n.done:
		// Node has already been stopped - no need to do anything
		return
	}
	// Block until the stop has been acknowledged by run()
	<-n.done
}

func (n *node) run() {
	var propc chan msgWithResult
	var readyc chan Ready
	var advancec chan struct{}
	var rd Ready

	r := n.rn.raft

	lead := None

	for {
		if advancec == nil && n.rn.HasReady() {
			// Populate a Ready. Note that this Ready is not guaranteed to
			// actually be handled. We will arm readyc, but there's no guarantee
			// that we will actually send on it. It's possible that we will
			// service another channel instead, loop around, and then populate
			// the Ready again. We could instead force the previous Ready to be
			// handled first, but it's generally good to emit larger Readys plus
			// it simplifies testing (by emitting less frequently and more
			// predictably).
			rd = n.rn.readyWithoutAccept()
			readyc = n.readyc
		}

		if lead != r.lead {
			if r.hasLeader() {
				if lead == None {
					r.logger.Infof("raft.node: %x elected leader %x at term %d", r.id, r.lead, r.Term)
				} else {
					r.logger.Infof("raft.node: %x changed leader from %x to %x at term %d", r.id, lead, r.lead, r.Term)
				}
				propc = n.propc
			} else {
				r.logger.Infof("raft.node: %x lost leader %x at term %d", r.id, lead, r.Term)
				propc = nil
			}
			lead = r.lead
		}

		select {
		// TODO: maybe buffer the config propose if there exists one (the way
		// described in raft dissertation)
		// Currently it is dropped in Step silently.
		case pm := <-propc:
			m := pm.m
			m.From = r.id
			err := r.Step(m)
			if pm.result != nil {
				pm.result <- err
				close(pm.result)
			}
		case m := <-n.recvc:
			if IsResponseMsg(m.Type) && !IsLocalMsgTarget(m.From) && r.trk.Progress[m.From] == nil {
				// Filter out response message from unknown From.
				break
			}
			r.Step(m)
		case cc := <-n.confc:
			_, okBefore := r.trk.Progress[r.id]
			cs := r.applyConfChange(cc)
			// If the node was removed, block incoming proposals. Note that we
			// only do this if the node was in the config before. Nodes may be
			// a member of the group without knowing this (when they're catching
			// up on the log and don't have the latest config) and we don't want
			// to block the proposal channel in that case.
			//
			// NB: propc is reset when the leader changes, which, if we learn
			// about it, sort of implies that we got readded, maybe? This isn't
			// very sound and likely has bugs.
			if _, okAfter := r.trk.Progress[r.id]; okBefore && !okAfter {
				var found bool
				for _, sl := range [][]uint64{cs.Voters, cs.VotersOutgoing} {
					for _, id := range sl {
						if id == r.id {
							found = true
							break
						}
					}
					if found {
						break
					}
				}
				if !found {
					propc = nil
				}
			}
			select {
			case n.confstatec <- cs:
			case <-n.done:
			}
		case <-n.tickc:
			n.rn.Tick()
		case readyc <- rd:
			n.rn.acceptReady(rd)
			if !n.rn.asyncStorageWrites {
				advancec = n.advancec
			} else {
				rd = Ready{}
			}
			readyc = nil
		case <-advancec:
			n.rn.Advance(rd)
			rd = Ready{}
			advancec = nil
		case c := <-n.status:
			c <- getStatus(r)
		case <-n.stop:
			close(n.done)
			return
		}
	}
}

// Tick increments the internal logical clock for this Node. Election timeouts
// and heartbeat timeouts are in units of ticks.
func (n *node) Tick() {
	select {
	case n.tickc <- struct{}{}:
	case <-n.done:
	default:
		n.rn.raft.logger.Warningf("%x A tick missed to fire. Node blocks too long!", n.rn.raft.id)
	}
}

func (n *node) Campaign(ctx context.Context) error { return n.step(ctx, pb.Message{Type: pb.MsgHup}) }

func (n *node) Propose(ctx context.Context, data []byte) error {
	return n.stepWait(ctx, pb.Message{Type: pb.MsgProp, Entries: []pb.Entry{{Data: data}}})
}

func (n *node) Step(ctx context.Context, m pb.Message) error {
	// Ignore unexpected local messages receiving over network.
	if IsLocalMsg(m.Type) && !IsLocalMsgTarget(m.From) {
		// TODO: return an error?
		return nil
	}
	return n.step(ctx, m)
}

func confChangeToMsg(c pb.ConfChangeI) (pb.Message, error) {
	typ, data, err := pb.MarshalConfChange(c)
	if err != nil {
		return pb.Message{}, err
	}
	return pb.Message{Type: pb.MsgProp, Entries: []pb.Entry{{Type: typ, Data: data}}}, nil
}

func (n *node) ProposeConfChange(ctx context.Context, cc pb.ConfChangeI) error {
	msg, err := confChangeToMsg(cc)
	if err != nil {
		return err
	}
	return n.Step(ctx, msg)
}

func (n *node) step(ctx context.Context, m pb.Message) error {
	return n.stepWithWaitOption(ctx, m, false)
}

func (n *node) stepWait(ctx context.Context, m pb.Message) error {
	return n.stepWithWaitOption(ctx, m, true)
}

// Step advances the state machine using msgs. The ctx.Err() will be returned,
// if any.
func (n *node) stepWithWaitOption(ctx context.Context, m pb.Message, wait bool) error {
	if m.Type != pb.MsgProp {
		select {
		case n.recvc <- m:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-n.done:
			return ErrStopped
		}
	}
	ch := n.propc
	pm := msgWithResult{m: m}
	if wait {
		pm.result = make(chan error, 1)
	}
	select {
	case ch <- pm:
		if !wait {
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-n.done:
		return ErrStopped
	}
	select {
	case err := <-pm.result:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-n.done:
		return ErrStopped
	}
	return nil
}

func (n *node) Ready() <-chan Ready { return n.readyc }

func (n *node) Advance() {
	select {
	case n.advancec <- struct{}{}:
	case <-n.done:
	}
}

func (n *node) ApplyConfChange(cc pb.ConfChangeI) *pb.ConfState {
	var cs pb.ConfState
	select {
	case n.confc <- cc.AsV2():
	case <-n.done:
	}
	select {
	case cs = <-n.confstatec:
	case <-n.done:
	}
	return &cs
}

func (n *node) Status() Status {
	c := make(chan Status)
	select {
	case n.status <- c:
		return <-c
	case <-n.done:
		return Status{}
	}
}

func (n *node) ReportUnreachable(id uint64) {
	select {
	case n.recvc <- pb.Message{Type: pb.MsgUnreachable, From: id}:
	case <-n.done:
	}
}

func (n *node) ReportSnapshot(id uint64, status SnapshotStatus) {
	rej := status == SnapshotFailure

	select {
	case n.recvc <- pb.Message{Type: pb.MsgSnapStatus, From: id, Reject: rej}:
	case <-n.done:
	}
}

func (n *node) TransferLeadership(ctx context.Context, lead, transferee uint64) {
	select {
	// manually set 'from' and 'to', so that leader can voluntarily transfers its leadership
	case n.recvc <- pb.Message{Type: pb.MsgTransferLeader, From: transferee, To: lead}:
	case <-n.done:
	case <-ctx.Done():
	}
}

func (n *node) ForgetLeader(ctx context.Context) error {
	return n.step(ctx, pb.Message{Type: pb.MsgForgetLeader})
}

func (n *node) ReadIndex(ctx context.Context, rctx []byte) error {
	return n.step(ctx, pb.Message{Type: pb.MsgReadIndex, Entries: []pb.Entry{{Data: rctx}}})
}
