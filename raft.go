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
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"

	"go.etcd.io/raft/v3/confchange"
	"go.etcd.io/raft/v3/quorum"
	pb "go.etcd.io/raft/v3/raftpb"
	"go.etcd.io/raft/v3/tracker"
)

const (
	// None is a placeholder node ID used when there is no leader.
	None uint64 = 0
	// LocalAppendThread is a reference to a local thread that saves unstable
	// log entries and snapshots to stable storage. The identifier is used as a
	// target for MsgStorageAppend messages when AsyncStorageWrites is enabled.
	LocalAppendThread uint64 = math.MaxUint64
	// LocalApplyThread is a reference to a local thread that applies committed
	// log entries to the local state machine. The identifier is used as a
	// target for MsgStorageApply messages when AsyncStorageWrites is enabled.
	LocalApplyThread uint64 = math.MaxUint64 - 1
)

// Possible values for StateType.
const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
	StatePreCandidate
	numStates
)

type ReadOnlyOption int

const (
	// ReadOnlySafe guarantees the linearizability of the read only request by
	// communicating with the quorum. It is the default and suggested option.
	// ReadOnlySafe通过与法定人数通信，保证了只读请求的线性化。这是默认和建议的选项。
	ReadOnlySafe ReadOnlyOption = iota
	// ReadOnlyLeaseBased ensures linearizability of the read only request by
	// relying on the leader lease. It can be affected by clock drift.
	// If the clock drift is unbounded, leader might keep the lease longer than it
	// should (clock can move backward/pause without any bound). ReadIndex is not safe
	// in that case.
	// ReadOnlyLeaseBased通过依赖领导者租约来保证只读请求的线性化。它可能会受到时钟漂移的影响。
	// 如果时钟漂移是无界的，领导者可能会保留比规定时间来说更长的租约（时钟可以在没有任何限制的情况下向后移动/暂停）。
	// 在这种情况下，ReadIndex是不安全的。
	ReadOnlyLeaseBased
)

// Possible values for CampaignType
const (
	// campaignPreElection represents the first phase of a normal election when
	// Config.PreVote is true.
	// campaignPreElection 表示Config.PreVote为true时正常选举的第一阶段
	campaignPreElection CampaignType = "CampaignPreElection"
	// campaignElection represents a normal (time-based) election (the second phase
	// of the election when Config.PreVote is true).
	// campaignElection 表示正常（基于时间的）选举（Config.PreVote为true时的第二阶段）
	campaignElection CampaignType = "CampaignElection"
	// campaignTransfer represents the type of leader transfer
	campaignTransfer CampaignType = "CampaignTransfer"
)

const noLimit = math.MaxUint64

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// lockedRand is a small wrapper around rand.Rand to provide
// synchronization among multiple raft groups. Only the methods needed
// by the code are exposed (e.g. Intn).
type lockedRand struct {
	mu sync.Mutex
}

func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	r.mu.Unlock()
	return int(v.Int64())
}

var globalRand = &lockedRand{}

// CampaignType represents the type of campaigning
// the reason we use the type of string instead of uint64
// is because it's simpler to compare and fill in raft entries
type CampaignType string

// StateType represents the role of a node in a cluster.
type StateType uint64

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
	"StatePreCandidate",
}

func (st StateType) String() string {
	return stmap[st]
}

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	// ElectionTick是必须在选举之间传递的Node.Tick调用次数。也就是说，如果在ElectionTick过去之前，
	// 跟随者没有收到当前任期的领导者的任何消息，它将成为候选者并开始选举。ElectionTick必须大于HeartbeatTick。
	// 我们建议ElectionTick = 10 * HeartbeatTick，以避免不必要的领导者切换。
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	// HeartbeatTick是必须在心跳之间传递的Node.Tick调用次数。也就是说，领导者每HeartbeatTick个tick
	// 发送一次心跳消息以维持其领导地位。
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	// Storage是raft的存储。raft生成要存储在存储中的条目和状态。当需要时，raft从存储中读取持久化的条目和状态。
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	// Applied是最后应用的索引。只有在重新启动raft时才应该设置。raft不会返回小于或等于Applied的条目给应用程序。
	// 如果在重新启动时未设置Applied，raft可能会返回先前应用的条目。这是一个非常依赖于应用程序的配置。
	Applied uint64

	// AsyncStorageWrites configures the raft node to write to its local storage
	// (raft log and state machine) using a request/response message passing
	// interface instead of the default Ready/Advance function call interface.
	// Local storage messages can be pipelined and processed asynchronously
	// (with respect to Ready iteration), facilitating reduced interference
	// between Raft proposals and increased batching of log appends and state
	// machine application. As a result, use of asynchronous storage writes can
	// reduce end-to-end commit latency and increase maximum throughput.
	// AsyncStorageWrites配置raft节点使用请求/响应消息传递接口而不是
	// 默认的Ready/Advance函数调用接口将数据写入本地存储（raft日志和状态机）。
	// 本地存储消息可以进行流水线处理并异步处理（与Ready迭代相对），有助于减少Raft提案之间的干扰，
	// 并增加日志附加和状态机应用的批处理。因此，使用异步存储写入可以减少端到端提交延迟并增加最大吞吐量。
	//
	// When true, the Ready.Message slice will include MsgStorageAppend and
	// MsgStorageApply messages. The messages will target a LocalAppendThread
	// and a LocalApplyThread, respectively. Messages to the same target must be
	// reliably processed in order. In other words, they can't be dropped (like
	// messages over the network) and those targeted at the same thread can't be
	// reordered. Messages to different targets can be processed in any order.
	// 当为true时，Ready.Message切片将包含MsgStorageAppend和MsgStorageApply消息。
	// 这些消息将分别针对LocalAppendThread和LocalApplyThread。必须按顺序可靠地处理相同目标的消息。
	// 换句话说，它们不能被丢弃（例如网络上的消息），并且针对同一线程的消息不能被重新排序。
	// 可以以任何顺序处理针对不同目标的消息。
	//
	// MsgStorageAppend carries Raft log entries to append, election votes /
	// term changes / updated commit indexes to persist, and snapshots to apply.
	// All writes performed in service of a MsgStorageAppend must be durable
	// before response messages are delivered. However, if the MsgStorageAppend
	// carries no response messages, durability is not required. The message
	// assumes the role of the Entries, HardState, and Snapshot fields in Ready.
	// MsgStorageAppend携带要附加的Raft日志条目、选举投票/任期更改/更新的提交索引以及要应用的快照。
	// 在传递响应消息之前，MsgStorageAppend中执行的所有写操作都必须是持久的。
	// 但是，如果MsgStorageAppend不携带响应消息，则不需要持久性。该消息扮演Ready中的Entries、HardState和Snapshot字段的角色。
	//
	// MsgStorageApply carries committed entries to apply. Writes performed in
	// service of a MsgStorageApply need not be durable before response messages
	// are delivered. The message assumes the role of the CommittedEntries field
	// in Ready.
	// MsgStorageApply携带要应用的已提交条目。在传递响应消息之前，MsgStorageApply中执行的写操作无需是持久的。
	// 该消息扮演Ready中的CommittedEntries字段的角色。
	//
	// Local messages each carry one or more response messages which should be
	// delivered after the corresponding storage write has been completed. These
	// responses may target the same node or may target other nodes. The storage
	// threads are not responsible for understanding the response messages, only
	// for delivering them to the correct target after performing the storage
	// write.
	// 本地消息每个携带一个或多个响应消息，这些响应消息应在相应的存储写入完成后传递。
	// 这些响应可能针对同一节点，也可能针对其他节点。存储线程不负责理解响应消息，只负责在执行存储写入后将其传递到正确的目标。
	AsyncStorageWrites bool

	// MaxSizePerMsg limits the max byte size of each append message. Smaller
	// value lowers the raft recovery cost(initial probing and message lost
	// during normal operation). On the other side, it might affect the
	// throughput during normal replication. Note: math.MaxUint64 for unlimited,
	// 0 for at most one entry per message.
	// MaxSizePerMsg限制每个附加消息的最大字节大小。较小的值降低了raft恢复成本（初始探测和正常操作期间丢失的消息）。
	MaxSizePerMsg uint64
	// MaxCommittedSizePerReady limits the size of the committed entries which
	// can be applying at the same time.
	// MaxCommittedSizePerReady限制了可以同时应用的已提交条目的大小。
	//
	// Despite its name (preserved for compatibility), this quota applies across
	// Ready structs to encompass all outstanding entries in unacknowledged
	// MsgStorageApply messages when AsyncStorageWrites is enabled.
	// 尽管它的名称（为了兼容性而保留）是这样的，但是当启用AsyncStorageWrites时，此配额适用于Ready结构，
	MaxCommittedSizePerReady uint64
	// MaxUncommittedEntriesSize limits the aggregate byte size of the
	// uncommitted entries that may be appended to a leader's log. Once this
	// limit is exceeded, proposals will begin to return ErrProposalDropped
	// errors. Note: 0 for no limit.
	// MaxUncommittedEntriesSize限制了可以附加到领导者日志中的未提交条目的聚合字节大小。
	// 一旦超过此限制，提案将开始返回ErrProposalDropped错误。注意：0表示没有限制。
	MaxUncommittedEntriesSize uint64
	// MaxInflightMsgs limits the max number of in-flight append messages during
	// optimistic replication phase. The application transportation layer usually
	// has its own sending buffer over TCP/UDP. Setting MaxInflightMsgs to avoid
	// overflowing that sending buffer. TODO (xiangli): feedback to application to
	// limit the proposal rate?
	// MaxInflightMsgs限制了乐观复制阶段中在途附加消息的最大数量。
	// 应用传输层通常具有自己的TCP/UDP发送缓冲区。设置MaxInflightMsgs以避免溢出发送缓冲区。
	// TODO（xiangli）：反馈给应用程序以限制提案速率？
	MaxInflightMsgs int
	// MaxInflightBytes limits the number of in-flight bytes in append messages.
	// Complements MaxInflightMsgs. Ignored if zero.
	// MaxInflightBytes限制了附加消息中在途字节的数量。补充MaxInflightMsgs。如果为零，则忽略。
	//
	// This effectively bounds the bandwidth-delay product. Note that especially
	// in high-latency deployments setting this too low can lead to a dramatic
	// reduction in throughput. For example, with a peer that has a round-trip
	// latency of 100ms to the leader and this setting is set to 1 MB, there is a
	// throughput limit of 10 MB/s for this group. With RTT of 400ms, this drops
	// to 2.5 MB/s. See Little's law to understand the maths behind.
	// 这有效地限制了带宽延迟乘积。请注意，特别是在高延迟部署中，将此设置得太低可能会导致吞吐量大幅减少。
	// 例如，对于一个与leader之间的往返延迟为100ms的对等节点，如果此设置为1MB，则该组的吞吐量限制为10MB/s。
	// 如果RTT为400ms，则降至2.5MB/s。请参见Little's law以了解背后的数学原理。
	MaxInflightBytes uint64

	// CheckQuorum specifies if the leader should check quorum activity. Leader
	// steps down when quorum is not active for an electionTimeout.
	// CheckQuorum指定领导者是否应检查法定活动。当选举超时时，领导者会下台。
	CheckQuorum bool

	// PreVote enables the Pre-Vote algorithm described in raft thesis section
	// 9.6. This prevents disruption when a node that has been partitioned away
	// rejoins the cluster.
	// PreVote启用了raft论文第9.6节中描述的Pre-Vote算法。当一个被分区的节点重新加入集群时，这可以防止中断。
	PreVote bool

	// ReadOnlyOption specifies how the read only request is processed.
	// ReadOnlyOption指定如何处理只读请求。
	//
	// ReadOnlySafe guarantees the linearizability of the read only request by
	// communicating with the quorum. It is the default and suggested option.
	// ReadOnlySafe通过与法定人数通信，保证了只读请求的线性化。这是默认和建议的选项。
	//
	// ReadOnlyLeaseBased ensures linearizability of the read only request by
	// relying on the leader lease. It can be affected by clock drift.
	// If the clock drift is unbounded, leader might keep the lease longer than it
	// should (clock can move backward/pause without any bound). ReadIndex is not safe
	// in that case.
	// CheckQuorum MUST be enabled if ReadOnlyOption is ReadOnlyLeaseBased.
	// ReadOnlyLeaseBased通过依赖领导者租约来保证只读请求的线性化。它可能会受到时钟漂移的影响。
	// 如果时钟漂移是无界的，领导者可能会保留比规定时间来说更长的租约（时钟可以在没有任何限制的情况下向后移动/暂停）。
	// 在这种情况下，ReadIndex是不安全的。
	// 如果ReadOnlyOption是ReadOnlyLeaseBased，则必须启用CheckQuorum。
	ReadOnlyOption ReadOnlyOption

	// Logger is the logger used for raft log. For multinode which can host
	// multiple raft group, each raft group can have its own logger
	// Logger是用于raft日志的记录器。对于可以托管多个raft组的多节点，每个raft组都可以有自己的记录器
	Logger Logger

	// DisableProposalForwarding set to true means that followers will drop
	// proposals, rather than forwarding them to the leader. One use case for
	// this feature would be in a situation where the Raft leader is used to
	// compute the data of a proposal, for example, adding a timestamp from a
	// hybrid logical clock to data in a monotonically increasing way. Forwarding
	// should be disabled to prevent a follower with an inaccurate hybrid
	// logical clock from assigning the timestamp and then forwarding the data
	// to the leader.
	// 将DisableProposalForwarding设置为true意味着跟随者将丢弃提案，而不是将其转发给领导者。
	// 此功能的一个用例是在Raft领导者用于计算提案的数据的情况下，例如，以单调递增的方式向数据添加混合逻辑时钟的时间戳。
	// 应禁用转发以防止具有不准确混合逻辑时钟的跟随者分配时间戳，然后将数据转发给领导者。
	DisableProposalForwarding bool

	// DisableConfChangeValidation turns off propose-time verification of
	// configuration changes against the currently active configuration of the
	// raft instance. These checks are generally sensible (cannot leave a joint
	// config unless in a joint config, et cetera) but they have false positives
	// because the active configuration may not be the most recent
	// configuration. This is because configurations are activated during log
	// application, and even the leader can trail log application by an
	// unbounded number of entries.
	// Symmetrically, the mechanism has false negatives - because the check may
	// not run against the "actual" config that will be the predecessor of the
	// newly proposed one, the check may pass but the new config may be invalid
	// when it is being applied. In other words, the checks are best-effort.
	// DisableConfChangeValidation禁用对配置更改在raft实例的当前活动配置中的验证。
	// 这些检查通常是明智的（除非在联合配置中，否则不能离开联合配置等），但它们有误报，因为活动配置可能不是最新的配置。
	// 这是因为配置是在日志应用期间激活的，即使领导者也可能落后于日志应用，而日志应用的条目数量是无界的。
	//
	// 对称地，该机制存在误报，因为检查可能不会针对将成为新提议的配置的“实际”配置运行，检查可能会通过，
	// 但在应用时新配置可能无效。换句话说，这些检查是尽力而为的。
	// Users should *not* use this option unless they have a reliable mechanism
	// (above raft) that serializes and verifies configuration changes. If an
	// invalid configuration change enters the log and gets applied, a panic
	// will result.
	// 用户不应使用此选项，除非他们有一个可靠的机制（在raft之上）来串行化和验证配置更改。
	// 如果无效的配置更改进入日志并被应用，将导致恐慌。
	//
	// This option may be removed once false positives are no longer possible.
	// See: https://github.com/etcd-io/raft/issues/80
	// 一旦不再可能出现误报，此选项可能会被删除。请参见：
	DisableConfChangeValidation bool

	// StepDownOnRemoval makes the leader step down when it is removed from the
	// group or demoted to a learner.
	// StepDownOnRemoval使领导者在从组中删除或降级为学习者时下台。
	//
	// This behavior will become unconditional in the future. See:
	// https://github.com/etcd-io/raft/issues/83
	StepDownOnRemoval bool
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}
	if IsLocalMsgTarget(c.ID) {
		return errors.New("cannot use local target as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	if c.MaxUncommittedEntriesSize == 0 {
		c.MaxUncommittedEntriesSize = noLimit
	}

	// default MaxCommittedSizePerReady to MaxSizePerMsg because they were
	// previously the same parameter.
	if c.MaxCommittedSizePerReady == 0 {
		c.MaxCommittedSizePerReady = c.MaxSizePerMsg
	}

	if c.MaxInflightMsgs <= 0 {
		return errors.New("max inflight messages must be greater than 0")
	}
	if c.MaxInflightBytes == 0 {
		c.MaxInflightBytes = noLimit
	} else if c.MaxInflightBytes < c.MaxSizePerMsg {
		return errors.New("max inflight bytes must be >= max message size")
	}

	if c.Logger == nil {
		c.Logger = getLogger()
	}

	if c.ReadOnlyOption == ReadOnlyLeaseBased && !c.CheckQuorum {
		return errors.New("CheckQuorum must be enabled when ReadOnlyOption is ReadOnlyLeaseBased")
	}

	return nil
}

type raft struct {
	id uint64

	Term uint64
	Vote uint64

	readStates []ReadState

	// the log

	raftLog            *raftLog
	maxMsgSize         entryEncodingSize
	maxUncommittedSize entryPayloadSize

	trk tracker.ProgressTracker

	state StateType

	// isLearner is true if the local raft node is a learner.
	isLearner bool

	// msgs contains the list of messages that should be sent out immediately to
	// other nodes.
	// msgs 包含应立即发送到其他节点的消息列表。
	//
	// Messages in this list must target other nodes.
	// 此列表中的消息必须针对其他节点。
	msgs []pb.Message
	// msgsAfterAppend contains the list of messages that should be sent after
	// the accumulated unstable state (e.g. term, vote, []entry, and snapshot)
	// has been persisted to durable storage. This includes waiting for any
	// unstable state that is already in the process of being persisted (i.e.
	// has already been handed out in a prior Ready struct) to complete.
	// msgsAfterAppend 包含了一系列消息，当累积的不稳定状态（例如term、vote、[]entry和snapshot）已
	// 经被持久化到持久存储后，这些消息应该被发送。
	// 这包括等待任何处于被持久化过程中的unstable state（即已在先前的Ready结构中分发）完成持久化操作，
	// 之后发送这些消息标记着已经完成持久化操作。
	//
	// Messages in this list may target other nodes or may target this node.
	// 此列表中的消息可能针对其他节点，也可能针对此节点。
	//
	// Messages in this list have the type MsgAppResp, MsgVoteResp, or
	// MsgPreVoteResp. See the comment in raft.send for details.
	// 此列表中的消息类型为MsgAppResp、MsgVoteResp或MsgPreVoteResp。有关详细信息，请参见raft.send中的注释。
	// 里面存放的是Response类型消息，而这些消息大多数依赖于某些unstable state,只有当这些unstable state被持久化后才能发送
	msgsAfterAppend []pb.Message

	// the leader id
	lead uint64
	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in raft thesis 3.10.
	// 当其值不为零时，leadTransferee是领导者转移目标的id。
	// 遵循raft论文3.10中定义的过程。
	leadTransferee uint64
	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via pendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// 只有一个配置变更可以是挂起的（在日志中，但尚未应用）。
	// 这是通过pendingConfIndex强制执行的，该值设置为>=最新挂起的配置更改的日志索引（如果有）。
	// 只有当leader的应用索引大于此值时，才允许提议配置更改。
	pendingConfIndex uint64
	// disableConfChangeValidation is Config.DisableConfChangeValidation,
	// see there for details.
	// disableConfChangeValidation是Config.DisableConfChangeValidation，有关详细信息，请参见那里。
	// 配置更改验证是否被禁用，一般情况下是不会被禁用的
	disableConfChangeValidation bool
	// an estimate of the size of the uncommitted tail of the Raft log. Used to
	// prevent unbounded log growth. Only maintained by the leader. Reset on
	// term changes.
	// Raft日志未提交尾部大小的估计。用于防止无限制的日志增长。仅由leader维护。在term更改时重置。
	uncommittedSize entryPayloadSize

	// readOnly用于处理只读请求。
	readOnly *readOnly

	// number of ticks since it reached last electionTimeout when it is leader
	// or candidate.
	// number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int

	// 可选项：Leader应定期检查集群中的大多数节点是否活跃。如果Leader在选举超时期间没有收到大多数节点的消息，则它将放弃领导权。
	checkQuorum bool
	// 可选项：Leader在选举超时期间是否应该进行预投票。预投票是一种防止节点在重新加入集群时发生中断的机制。
	preVote bool

	heartbeatTimeout int
	electionTimeout  int
	// randomizedElectionTimeout is a random number between
	// [electiontimeout, 2 * electiontimeout - 1]. It gets reset
	// when raft changes its state to follower or candidate.
	randomizedElectionTimeout int
	disableProposalForwarding bool
	stepDownOnRemoval         bool

	tick func()
	step stepFunc

	logger Logger

	// pendingReadIndexMessages is used to store messages of type MsgReadIndex
	// that can't be answered as new leader didn't committed any log in
	// current term. Those will be handled as fast as first log is committed in
	// current term.
	// pendingReadIndexMessages用于存储MsgReadIndex类型的消息，
	// 因为新领导者在当前任期中没有提交任何日志，所以无法回答这些消息。
	// 这些消息将在当前任期中提交第一条日志时尽快处理。
	pendingReadIndexMessages []pb.Message
}

func newRaft(c *Config) *raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	raftlog := newLogWithSize(c.Storage, c.Logger, entryEncodingSize(c.MaxCommittedSizePerReady))
	hs, cs, err := c.Storage.InitialState()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}

	r := &raft{
		id:                          c.ID,
		lead:                        None,
		isLearner:                   false,
		raftLog:                     raftlog,
		maxMsgSize:                  entryEncodingSize(c.MaxSizePerMsg),
		maxUncommittedSize:          entryPayloadSize(c.MaxUncommittedEntriesSize),
		trk:                         tracker.MakeProgressTracker(c.MaxInflightMsgs, c.MaxInflightBytes),
		electionTimeout:             c.ElectionTick,
		heartbeatTimeout:            c.HeartbeatTick,
		logger:                      c.Logger,
		checkQuorum:                 c.CheckQuorum,
		preVote:                     c.PreVote,
		readOnly:                    newReadOnly(c.ReadOnlyOption),
		disableProposalForwarding:   c.DisableProposalForwarding,
		disableConfChangeValidation: c.DisableConfChangeValidation,
		stepDownOnRemoval:           c.StepDownOnRemoval,
	}

	lastID := r.raftLog.lastEntryID()
	cfg, trk, err := confchange.Restore(confchange.Changer{
		Tracker:   r.trk,
		LastIndex: lastID.index,
	}, cs)
	if err != nil {
		panic(err)
	}
	assertConfStatesEquivalent(r.logger, cs, r.switchToConfig(cfg, trk))

	if !IsEmptyHardState(hs) {
		r.loadState(hs)
	}
	if c.Applied > 0 {
		raftlog.appliedTo(c.Applied, 0 /* size */)
	}
	r.becomeFollower(r.Term, None)

	var nodesStrs []string
	for _, n := range r.trk.VoterNodes() {
		nodesStrs = append(nodesStrs, fmt.Sprintf("%x", n))
	}

	// TODO(pav-kv): it should be ok to simply print %+v for lastID.
	r.logger.Infof("newRaft %x [peers: [%s], term: %d, commit: %d, applied: %d, lastindex: %d, lastterm: %d]",
		r.id, strings.Join(nodesStrs, ","), r.Term, r.raftLog.committed, r.raftLog.applied, lastID.index, lastID.term)
	return r
}

func (r *raft) hasLeader() bool { return r.lead != None }

func (r *raft) softState() SoftState { return SoftState{Lead: r.lead, RaftState: r.state} }

func (r *raft) hardState() pb.HardState {
	return pb.HardState{
		Term:   r.Term,
		Vote:   r.Vote,
		Commit: r.raftLog.committed,
	}
}

// send schedules persisting state to a stable storage and AFTER that
// sending the message (as part of next Ready message processing).
// send调度将状态持久化到稳定存储，并在此之后发送消息（作为下一个Ready消息处理的一部分）。
func (r *raft) send(m pb.Message) {
	if m.From == None {
		m.From = r.id
	}
	if m.Type == pb.MsgVote || m.Type == pb.MsgVoteResp || m.Type == pb.MsgPreVote || m.Type == pb.MsgPreVoteResp {
		if m.Term == 0 {
			// All {pre-,}campaign messages need to have the term set when
			// sending.
			// - MsgVote: m.Term is the term the node is campaigning for,
			//   non-zero as we increment the term when campaigning.
			// - MsgVoteResp: m.Term is the new r.Term if the MsgVote was
			//   granted, non-zero for the same reason MsgVote is
			// - MsgPreVote: m.Term is the term the node will campaign,
			//   non-zero as we use m.Term to indicate the next term we'll be
			//   campaigning for
			// - MsgPreVoteResp: m.Term is the term received in the original
			//   MsgPreVote if the pre-vote was granted, non-zero for the
			//   same reasons MsgPreVote is
			// 所有{pre-,}campaign消息在发送时都需要设置term。
			// - MsgVote：m.Term是节点正在竞选的任期，当竞选时我们会递增term，因此m.Term是非零的。
			// - MsgVoteResp：如果MsgVote被授予，则m.Term是新的r.Term，由于MsgVote的原因，m.Term是非零的。
			// - MsgPreVote：m.Term是节点将要竞选的任期，由于我们使用m.Term来指示我们将要竞选的下一个任期，因此m.Term是非零的。
			// - MsgPreVoteResp：如果预投票被授予，则m.Term是从原始MsgPreVote消息中收到的任期，由于MsgPreVote的原因，m.Term是非零的。
			r.logger.Panicf("term should be set when sending %s", m.Type)
		}
	} else {
		// 因为我们在别的地方创建消息时是不会set term的，所以m.Term在这里时应该为0
		// 除了MsgVote、MsgVoteResp、MsgPreVote、MsgPreVoteResp这四种消息，其他消息的term都应该为0
		if m.Term != 0 {
			r.logger.Panicf("term should not be set when sending %s (was %d)", m.Type, m.Term)
		}
		// do not attach term to MsgProp, MsgReadIndex
		// proposals are a way to forward to the leader and
		// should be treated as local message.
		// MsgReadIndex is also forwarded to leader.
		//
		// 不要将term附加到MsgProp、MsgReadIndex提案上是一种转发到leader的方式，MsgProp
		// 和MsgReadIndex应该被视为本地消息。MsgReadIndex也被转发到leader。
		//
		// 换言之，我们只为除了MsgProp和MsgReadIndex之外的消息设置term
		// 因为MsgVote、MsgVoteResp、MsgPreVote、MsgPreVoteResp这四种消息是在别的地方创建的，所以它们的term是已经设置好的
		if m.Type != pb.MsgProp && m.Type != pb.MsgReadIndex {
			m.Term = r.Term
		}
	}
	if m.Type == pb.MsgAppResp || m.Type == pb.MsgVoteResp || m.Type == pb.MsgPreVoteResp {
		// If async storage writes are enabled, messages added to the msgs slice
		// are allowed to be sent out before unstable state (e.g. log entry
		// writes and election votes) have been durably synced to the local
		// disk.
		// 如果启用了异步存储写入，则允许在不稳定状态（例如日志条目写入和选举投票）已经持久地同步到本地磁盘之前，
		// 将消息添加到msgs切片中并发送出去。
		//
		// For most message types, this is not an issue. However, response
		// messages that relate to "voting" on either leader election or log
		// appends require durability before they can be sent. It would be
		// incorrect to publish a vote in an election before that vote has been
		// synced to stable storage locally. Similarly, it would be incorrect to
		// acknowledge a log append to the leader before that entry has been
		// synced to stable storage locally.
		// 对于大多数消息类型，这不是问题。然而，与"voting"有关的响应消息，无论是领导者选举还是日志附加
		// 都需要在发送之前进行持久化。在一轮选举中，发布投票之前必须将该投票结果同步到本地的稳定存储中。
		// 类似地，在将日志附加确认发送给leader之前，必须将该条目同步到本地的稳定存储中。
		//
		// Per the Raft thesis, section 3.8 Persisted state and server restarts:
		// 根据Raft论文第3.8节持久化状态和服务器重启：
		//
		// > Raft servers must persist enough information to stable storage to
		// > survive server restarts safely. In particular, each server persists
		// > its current term and vote; this is necessary to prevent the server
		// > from voting twice in the same term or replacing log entries from a
		// > newer leader with those from a deposed leader. Each server also
		// > persists new log entries before they are counted towards the entries’
		// > commitment; this prevents committed entries from being lost or
		// > “uncommitted” when servers restart
		// > Raft服务器必须将足够的信息持久化到稳定存储中，以安全地在服务器重启时生存。
		// > 特别是，每个服务器都会持久化其当前任期和投票；这是为了防止服务器在同一任期中投票两次
		// > 或用被罢免的领导者的日志条目替换来自新领导者的日志条目。每个服务器还会在将新的日志条目
		// > 计入条目的提交之前将其持久化；这可以防止在服务器重启时丢失或“未提交”已提交的条目。
		//
		// To enforce this durability requirement, these response messages are
		// queued to be sent out as soon as the current collection of unstable
		// state (the state that the response message was predicated upon) has
		// been durably persisted. This unstable state may have already been
		// passed to a Ready struct whose persistence is in progress or may be
		// waiting for the next Ready struct to begin being written to Storage.
		// These messages must wait for all of this state to be durable before
		// being published.
		// 为了强制执行这种持久性要求，一旦当前的不稳定状态集合（响应消息所依赖的状态）已经持久化，
		// 这些响应消息就会被排队发送。这种不稳定状态可能已经被传递给了一个正在持久化的Ready结构或者
		// 可能正在等待下一个Ready结构开始被写入Storage。这些消息必须等待所有这些状态都持久化后才能发布。
		//
		// Rejected responses (m.Reject == true) present an interesting case
		// where the durability requirement is less unambiguous. A rejection may
		// be predicated upon unstable state. For instance, a node may reject a
		// vote for one peer because it has already begun syncing its vote for
		// another peer. Or it may reject a vote from one peer because it has
		// unstable log entries that indicate that the peer is behind on its
		// log. In these cases, it is likely safe to send out the rejection
		// response immediately without compromising safety in the presence of a
		// server restart. However, because these rejections are rare and
		// because the safety of such behavior has not been formally verified,
		// we err on the side of safety and omit a `&& !m.Reject` condition
		// above.
		// 被拒绝的响应（m.Reject == true）提出了一个有趣的情况，其中持久性要求不那么明确。
		// 拒绝可能是基于不稳定状态的。例如，一个节点可能会拒绝对一个对等节点的投票，因为它已经开始同步
		// 对另一个对等节点的投票。或者它可能会拒绝来自一个对等节点的投票请求，因为它有不稳定的日志条目，
		// 这些日志条目表明该对等节点在日志上落后了。在这些情况下，立即发送拒绝响应似乎是安全的，而不会
		// 在服务器重启时影响安全性。然而，由于这些拒绝情况很少见，并且因为这种行为的安全性尚未得到正式验证，
		// 我们在安全性方面犯了错误，并省略了上面的`&& !m.Reject`条件。
		r.msgsAfterAppend = append(r.msgsAfterAppend, m)
	} else {
		if m.To == r.id {
			r.logger.Panicf("message should not be self-addressed when sending %s", m.Type)
		}
		// 非Response类型的消息，直接添加到msgs切片中，等待发送即可
		r.msgs = append(r.msgs, m)
	}
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer.
// sendAppend向给定的peer发送带有新条目（如果有的话）和当前commitIndex的append RPC。
func (r *raft) sendAppend(to uint64) {
	r.maybeSendAppend(to, true)
}

// maybeSendAppend sends an append RPC with new entries to the given peer,
// if necessary. Returns true if a message was sent. The sendIfEmpty
// argument controls whether messages with no entries will be sent
// ("empty" messages are useful to convey updated Commit indexes, but
// are undesirable when we're sending multiple messages in a batch).
// 如果有必要的话，maybeSendAppend 发送带有新条目的append RPC到给定的peer。
// 如果发送了消息，则返回true。sendIfEmpty参数控制是否发送没有条目的消息
// （“空”消息对于传达更新的提交索引很有用，但在批量发送多条消息时是不希望的）。
func (r *raft) maybeSendAppend(to uint64, sendIfEmpty bool) bool {
	pr := r.trk.Progress[to]
	if pr.IsPaused() {
		return false
	}

	prevIndex := pr.Next - 1
	prevTerm, err := r.raftLog.term(prevIndex)
	if err != nil {
		// The log probably got truncated at >= pr.Next, so we can't catch up the
		// follower log anymore. Send a snapshot instead.
		// 日志可能在>=pr.Next处被截断，因此我们无法再追赶追随者日志。发送快照。
		return r.maybeSendSnapshot(to, pr)
	}

	var ents []pb.Entry
	// In a throttled StateReplicate only send empty MsgApp, to ensure progress.
	// Otherwise, if we had a full Inflights and all inflight messages were in
	// fact dropped, replication to that follower would stall. Instead, an empty
	// MsgApp will eventually reach the follower (heartbeats responses prompt the
	// leader to send an append), allowing it to be acked or rejected, both of
	// which will clear out Inflights.
	// 在受限的StateReplicate状态下，只发送空的MsgApp，以确保进度。
	// 否则，如果我们有一个满的Inflights并且所有的inflight消息实际上都被丢弃了，那么复制到该follower将会停滞。
	// 相反，一个空的MsgApp最终会到达follower（心跳响应促使leader发送一个append），允许它被确认或拒绝，这两者都会清除Inflights。
	if pr.State != tracker.StateReplicate || !pr.Inflights.Full() {
		ents, err = r.raftLog.entries(pr.Next, r.maxMsgSize)
	}
	// 若没有实际的entries，且不发送同步commitIndex的空消息，则不发送消息
	if len(ents) == 0 && !sendIfEmpty {
		return false
	}
	// TODO(pav-kv): move this check up to where err is returned.
	if err != nil { // send a snapshot if we failed to get the entries
		return r.maybeSendSnapshot(to, pr)
	}

	// Send the actual MsgApp otherwise, and update the progress accordingly.
	r.send(pb.Message{
		To:      to,
		Type:    pb.MsgApp,
		Index:   prevIndex,
		LogTerm: prevTerm,
		Entries: ents,
		Commit:  r.raftLog.committed,
	})
	// 更新目标follower对应progress对象的状态
	pr.UpdateOnEntriesSend(len(ents), uint64(payloadsSize(ents)))
	return true
}

// maybeSendSnapshot fetches a snapshot from Storage, and sends it to the given
// node. Returns true iff the snapshot message has been emitted successfully.
// maybeSendSnapshot从Storage中获取快照，并将其发送到给定的节点。如果成功发送了快照消息，则返回true。
func (r *raft) maybeSendSnapshot(to uint64, pr *tracker.Progress) bool {
	if !pr.RecentActive {
		// 目标follower不是最近活跃的，不发送快照
		r.logger.Debugf("ignore sending snapshot to %x since it is not recently active", to)
		return false
	}

	snapshot, err := r.raftLog.snapshot()
	if err != nil {
		// 快照暂时不可用，需要等待或者重试
		if err == ErrSnapshotTemporarilyUnavailable {
			r.logger.Debugf("%x failed to send snapshot to %x because snapshot is temporarily unavailable", r.id, to)
			return false
		}
		// 若获取快照失败，且err != ErrSnapshotTemporarilyUnavailable，则直接panic
		// TODO:什么情况下会出现这种情况？
		panic(err) // TODO(bdarnell)
	}
	if IsEmptySnap(snapshot) {
		// 若快照为空，则直接panic
		panic("need non-empty snapshot")
	}
	sindex, sterm := snapshot.Metadata.Index, snapshot.Metadata.Term
	r.logger.Debugf("%x [firstindex: %d, commit: %d] sent snapshot[index: %d, term: %d] to %x [%s]",
		r.id, r.raftLog.firstIndex(), r.raftLog.committed, sindex, sterm, to, pr)
	pr.BecomeSnapshot(sindex)
	r.logger.Debugf("%x paused sending replication messages to %x [%s]", r.id, to, pr)

	r.send(pb.Message{To: to, Type: pb.MsgSnap, Snapshot: &snapshot})
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *raft) sendHeartbeat(to uint64, ctx []byte) {
	// Attach the commit as min(to.matched, r.committed).
	// When the leader sends out heartbeat message,
	// the receiver(follower) might not be matched with the leader
	// or it might not have all the committed entries.
	// The leader MUST NOT forward the follower's commit to
	// an unmatched index.
	// 将commit作为min(to.matched, r.committed)附加。
	// 当leader发送心跳消息时，接收者（follower）可能与leader不匹配，或者可能没有所有已提交的条目。
	// leader禁止发送大于follower的matchIndex的commitIndex
	commit := min(r.trk.Progress[to].Match, r.raftLog.committed)
	m := pb.Message{
		To:      to,
		Type:    pb.MsgHeartbeat,
		Commit:  commit,
		Context: ctx,
	}

	r.send(m)
}

// bcastAppend sends RPC, with entries to all peers that are not up-to-date
// according to the progress recorded in r.trk.
func (r *raft) bcastAppend() {
	r.trk.Visit(func(id uint64, _ *tracker.Progress) {
		if id == r.id {
			return
		}
		r.sendAppend(id)
	})
}

// bcastHeartbeat sends RPC, without entries to all the peers.
// bcastHeartbeat向所有对等节点发送RPC，不包含任何entries，
func (r *raft) bcastHeartbeat() {
	lastCtx := r.readOnly.lastPendingRequestCtx()
	if len(lastCtx) == 0 {
		r.bcastHeartbeatWithCtx(nil)
	} else { // 若存在，携带最后一个readIndex请求的ctx
		r.bcastHeartbeatWithCtx([]byte(lastCtx))
	}
}

func (r *raft) bcastHeartbeatWithCtx(ctx []byte) {
	r.trk.Visit(func(id uint64, _ *tracker.Progress) {
		if id == r.id {
			return
		}
		r.sendHeartbeat(id, ctx)
	})
}

func (r *raft) appliedTo(index uint64, size entryEncodingSize) {
	oldApplied := r.raftLog.applied
	newApplied := max(index, oldApplied)
	r.raftLog.appliedTo(newApplied, size)

	// 若当前节点是Leader，且设置了AutoLeave选项，且新应用的索引大于等于r.pendingConfIndex
	// raft节点的pendingConfIndex是在配置变更时设置的，当新应用的索引大于等于r.pendingConfIndex时，可以自动离开联合配置
	if r.trk.Config.AutoLeave && newApplied >= r.pendingConfIndex && r.state == StateLeader {
		// If the current (and most recent, at least for this leader's term)
		// configuration should be auto-left, initiate that now. We use a
		// nil Data which unmarshals into an empty ConfChangeV2 and has the
		// benefit that appendEntry can never refuse it based on its size
		// (which registers as zero).
		//
		m, err := confChangeToMsg(nil)
		if err != nil {
			panic(err)
		}
		// NB: this proposal can't be dropped due to size, but can be
		// dropped if a leadership transfer is in progress. We'll keep
		// checking this condition on each applied entry, so either the
		// leadership transfer will succeed and the new leader will leave
		// the joint configuration, or the leadership transfer will fail,
		// and we will propose the config change on the next advance.
		if err := r.Step(m); err != nil {
			r.logger.Debugf("not initiating automatic transition out of joint configuration %s: %v", r.trk.Config, err)
		} else {
			r.logger.Infof("initiating automatic transition out of joint configuration %s", r.trk.Config)
		}
	}
}

func (r *raft) appliedSnap(snap *pb.Snapshot) {
	index := snap.Metadata.Index
	r.raftLog.stableSnapTo(index)
	r.appliedTo(index, 0 /* size */)
}

// maybeCommit attempts to advance the commit index. Returns true if the commit
// index changed (in which case the caller should call r.bcastAppend). This can
// only be called in StateLeader.
// maybeCommit尝试推进commitIndex。
// 如果commitIndex更改，则返回true（在这种情况下，调用者应调用r.bcastAppend）。这只能在StateLeader中调用。
// 很显然commitIndex后马上调用r.bcastAppend，是为了让其他节点也能及时更新自己的commitIndex
func (r *raft) maybeCommit() bool {
	return r.raftLog.maybeCommit(entryID{term: r.Term, index: r.trk.Committed()})
}

func (r *raft) reset(term uint64) {
	if r.Term != term {
		r.Term = term
		r.Vote = None
	}
	r.lead = None

	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.resetRandomizedElectionTimeout()

	r.abortLeaderTransfer()

	r.trk.ResetVotes()
	r.trk.Visit(func(id uint64, pr *tracker.Progress) {
		*pr = tracker.Progress{
			Match:     0,
			Next:      r.raftLog.lastIndex() + 1,
			Inflights: tracker.NewInflights(r.trk.MaxInflight, r.trk.MaxInflightBytes),
			IsLearner: pr.IsLearner,
		}
		if id == r.id {
			pr.Match = r.raftLog.lastIndex()
		}
	})

	r.pendingConfIndex = 0
	r.uncommittedSize = 0
	r.readOnly = newReadOnly(r.readOnly.option)
}

func (r *raft) appendEntry(es ...pb.Entry) (accepted bool) {
	li := r.raftLog.lastIndex()
	for i := range es {
		es[i].Term = r.Term
		es[i].Index = li + 1 + uint64(i)
	}
	// Track the size of this uncommitted proposal.
	// 跟踪此未提交的提案的大小。
	if !r.increaseUncommittedSize(es) {
		r.logger.Warningf(
			"%x appending new entries to log would exceed uncommitted entry size limit; dropping proposal",
			r.id,
		)
		// Drop the proposal.
		return false
	}
	// use latest "last" index after truncate/append
	// 在截断/附加后使用最新的“last”索引
	li = r.raftLog.append(es...)
	// The leader needs to self-ack the entries just appended once they have
	// been durably persisted (since it doesn't send an MsgApp to itself). This
	// response message will be added to msgsAfterAppend and delivered back to
	// this node after these entries have been written to stable storage. When
	// handled, this is roughly equivalent to:
	// 一旦这些刚刚追加的日志条目被持久化（因为leader不会给自己发送MsgApp），leader需要自我确认这些条目。
	// 这个响应消息将被添加到msgsAfterAppend中，并在这些条目被写入稳定存储后传递回这个节点。
	// 当处理时，这大致相当于：
	//
	//  r.trk.Progress[r.id].MaybeUpdate(e.Index)
	//  if r.maybeCommit() {
	//  	r.bcastAppend()
	//  }
	r.send(pb.Message{To: r.id, Type: pb.MsgAppResp, Index: li})
	return true
}

// Leader和followers/candidates的具体tick行为是不同的：
// Leader：leader调用的是tickHeartbeat
// followers/candidates：调用的是tickElection

// tickElection is run by followers and candidates after r.electionTimeout.
// tickElection在r.electionTimeout之后由follower和candidate运行。
func (r *raft) tickElection() {
	r.electionElapsed++

	if r.promotable() && r.pastElectionTimeout() {
		r.electionElapsed = 0
		if err := r.Step(pb.Message{From: r.id, Type: pb.MsgHup}); err != nil {
			r.logger.Debugf("error occurred during election: %v", err)
		}
	}
}

// tickHeartbeat is run by leaders to send a MsgBeat after r.heartbeatTimeout.
// tickHeartbeat由leader运行，以在r.heartbeatTimeout之后发送MsgBeat。
func (r *raft) tickHeartbeat() {
	r.heartbeatElapsed++
	r.electionElapsed++

	if r.electionElapsed >= r.electionTimeout {
		r.electionElapsed = 0
		if r.checkQuorum {
			if err := r.Step(pb.Message{From: r.id, Type: pb.MsgCheckQuorum}); err != nil {
				r.logger.Debugf("error occurred during checking sending heartbeat: %v", err)
			}
		}
		// If current leader cannot transfer leadership in electionTimeout, it becomes leader again.
		if r.state == StateLeader && r.leadTransferee != None {
			r.abortLeaderTransfer()
		}
	}

	if r.state != StateLeader {
		return
	}

	if r.heartbeatElapsed >= r.heartbeatTimeout {
		r.heartbeatElapsed = 0
		if err := r.Step(pb.Message{From: r.id, Type: pb.MsgBeat}); err != nil {
			r.logger.Debugf("error occurred during checking sending heartbeat: %v", err)
		}
	}
}

func (r *raft) becomeFollower(term uint64, lead uint64) {
	r.step = stepFollower
	r.reset(term)
	r.tick = r.tickElection
	r.lead = lead
	r.state = StateFollower
	r.logger.Infof("%x became follower at term %d", r.id, r.Term)
}

func (r *raft) becomeCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		panic("invalid transition [leader -> candidate]")
	}
	r.step = stepCandidate
	r.reset(r.Term + 1)
	r.tick = r.tickElection
	r.Vote = r.id
	r.state = StateCandidate
	r.logger.Infof("%x became candidate at term %d", r.id, r.Term)
}

func (r *raft) becomePreCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		panic("invalid transition [leader -> pre-candidate]")
	}
	// Becoming a pre-candidate changes our step functions and state,
	// but doesn't change anything else. In particular it does not increase
	// r.Term or change r.Vote.
	// 成为pre-candidate会改变我们的step函数和状态，但不会改变其他任何东西。特别是它不会增加r.Term或更改r.Vote。
	r.step = stepCandidate
	r.trk.ResetVotes()
	r.tick = r.tickElection
	r.lead = None
	r.state = StatePreCandidate
	r.logger.Infof("%x became pre-candidate at term %d", r.id, r.Term)
}

func (r *raft) becomeLeader() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateFollower {
		panic("invalid transition [follower -> leader]")
	}
	r.step = stepLeader
	r.reset(r.Term)
	r.tick = r.tickHeartbeat
	r.lead = r.id
	r.state = StateLeader
	// Followers enter replicate mode when they've been successfully probed
	// (perhaps after having received a snapshot as a result). The leader is
	// trivially in this state. Note that r.reset() has initialized this
	// progress with the last index already.
	// 当follower被成功探测(可能是在成功接收快照之后)时，它们进入复制模式。leader显然处于这种状态。
	// 请注意，r.reset()已经使用最后一个索引初始化了这个progress。
	pr := r.trk.Progress[r.id]
	pr.BecomeReplicate()
	// The leader always has RecentActive == true; MsgCheckQuorum makes sure to
	// preserve this.
	pr.RecentActive = true

	// Conservatively set the pendingConfIndex to the last index in the
	// log. There may or may not be a pending config change, but it's
	// safe to delay any future proposals until we commit all our
	// pending log entries, and scanning the entire tail of the log
	// could be expensive.
	// 将pendingConfIndex保守地设置为日志中的最后一个索引。可能有、也可能没有待处理的配置更改，
	// 但是延迟任何未来的proposals，直到我们提交了所有pending状态的日志条目，扫描日志的整个尾部可能是昂贵的。
	r.pendingConfIndex = r.raftLog.lastIndex()

	emptyEnt := pb.Entry{Data: nil}
	if !r.appendEntry(emptyEnt) {
		// This won't happen because we just called reset() above.
		// 这不会发生，因为我们刚刚在上面调用了reset()。
		r.logger.Panic("empty entry was dropped")
	}
	// The payloadSize of an empty entry is 0 (see TestPayloadSizeOfEmptyEntry),
	// so the preceding log append does not count against the uncommitted log
	// quota of the new leader. In other words, after the call to appendEntry,
	// r.uncommittedSize is still 0.
	// 空条目的payloadSize为0（参见TestPayloadSizeOfEmptyEntry），因此前面的日志附加不计入新leader的未提交日志配额。
	// 换句话说，在调用appendEntry之后，r.uncommittedSize仍然为0。
	r.logger.Infof("%x became leader at term %d", r.id, r.Term)
}

func (r *raft) hup(t CampaignType) {
	if r.state == StateLeader {
		r.logger.Debugf("%x ignoring MsgHup because already leader", r.id)
		return
	}

	if !r.promotable() {
		r.logger.Warningf("%x is unpromotable and can not campaign", r.id)
		return
	}
	// 若有未处理的配置更改，则不发起选举，这是为了保证每次集群成员变化时只会有一个节点在发生变化
	// 不能同时有多个集群成员在发生状态变化(如集群中不允许同时发生集群成员变更和选举)
	if r.hasUnappliedConfChanges() {
		r.logger.Warningf("%x cannot campaign at term %d since there are still pending configuration changes to apply", r.id, r.Term)
		return
	}

	r.logger.Infof("%x is starting a new election at term %d", r.id, r.Term)
	r.campaign(t)
}

// errBreak is a sentinel error used to break a callback-based loop.
var errBreak = errors.New("break")

func (r *raft) hasUnappliedConfChanges() bool {
	if r.raftLog.applied >= r.raftLog.committed { // in fact applied == committed
		return false
	}
	found := false
	// Scan all unapplied committed entries to find a config change. Paginate the
	// scan, to avoid a potentially unlimited memory spike.
	lo, hi := r.raftLog.applied+1, r.raftLog.committed+1
	// Reuse the maxApplyingEntsSize limit because it is used for similar purposes
	// (limiting the read of unapplied committed entries) when raft sends entries
	// via the Ready struct for application.
	// TODO(pavelkalinnikov): find a way to budget memory/bandwidth for this scan
	// outside the raft package.
	pageSize := r.raftLog.maxApplyingEntsSize
	if err := r.raftLog.scan(lo, hi, pageSize, func(ents []pb.Entry) error {
		for i := range ents {
			if ents[i].Type == pb.EntryConfChange || ents[i].Type == pb.EntryConfChangeV2 {
				found = true
				return errBreak
			}
		}
		return nil
	}); err != nil && err != errBreak {
		r.logger.Panicf("error scanning unapplied entries [%d, %d): %v", lo, hi, err)
	}
	return found
}

// campaign transitions the raft instance to candidate state. This must only be
// called after verifying that this is a legitimate transition.
// campaign将raft实例转换为候选人状态。只有在验证这是合法转换之后才能调用此函数。
func (r *raft) campaign(t CampaignType) {
	if !r.promotable() {
		// This path should not be hit (callers are supposed to check), but
		// better safe than sorry.
		// 不应该走到这个路径（调用者应该检查），但是小心总比后悔好。
		r.logger.Warningf("%x is unpromotable; campaign() should have been called", r.id)
	}
	var term uint64
	var voteMsg pb.MessageType
	if t == campaignPreElection {
		r.becomePreCandidate()
		voteMsg = pb.MsgPreVote
		// PreVote RPCs are sent for the next term before we've incremented r.Term.
		// PreVote类型的RPC在我们增加r.Term之前发送给下一个term。
		term = r.Term + 1
	} else {
		r.becomeCandidate()
		voteMsg = pb.MsgVote
		term = r.Term
	}
	var ids []uint64
	{
		idMap := r.trk.Voters.IDs()
		ids = make([]uint64, 0, len(idMap))
		for id := range idMap {
			ids = append(ids, id)
		}
		// 将当前具有投票权的节点的id按照从小到大的顺序排列
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	for _, id := range ids {
		if id == r.id { // 给自己投票
			// The candidate votes for itself and should account for this self
			// vote once the vote has been durably persisted (since it doesn't
			// send a MsgVote to itself). This response message will be added to
			// msgsAfterAppend and delivered back to this node after the vote
			// has been written to stable storage.
			// 候选人为自己投票，并且一旦投票被持久化，就应该考虑这个自我投票（因为它不会向自己发送MsgVote）。
			// 这个响应消息将被添加到msgsAfterAppend，并在投票被写入稳定存储后传递回这个节点。
			r.send(pb.Message{To: id, Term: term, Type: voteRespMsgType(voteMsg)})
			continue
		}
		// TODO(pav-kv): it should be ok to simply print %+v for the lastEntryID.
		last := r.raftLog.lastEntryID()
		r.logger.Infof("%x [logterm: %d, index: %d] sent %s request to %x at term %d",
			r.id, last.term, last.index, voteMsg, id, r.Term)

		var ctx []byte
		if t == campaignTransfer {
			ctx = []byte(t)
		}
		r.send(pb.Message{To: id, Term: term, Type: voteMsg, Index: last.index, LogTerm: last.term, Context: ctx})
	}
}

func (r *raft) poll(id uint64, t pb.MessageType, v bool) (granted int, rejected int, result quorum.VoteResult) {
	if v {
		r.logger.Infof("%x received %s from %x at term %d", r.id, t, id, r.Term)
	} else {
		r.logger.Infof("%x received %s rejection from %x at term %d", r.id, t, id, r.Term)
	}
	r.trk.RecordVote(id, v)
	// 计算当前投票汇总是否到达了大多数
	return r.trk.TallyVotes()
}

func (r *raft) Step(m pb.Message) error {
	// Handle the message term, which may result in our stepping down to a follower.
	// 处理消息的term，这可能导致我们降级为follower。
	switch {
	case m.Term == 0:
		// local message
		// 本地消息，由该raftNode自己发出的消息
	case m.Term > r.Term: // 若消息的term大于当前raftNode的term
		if m.Type == pb.MsgVote || m.Type == pb.MsgPreVote { // 且当前节点收到了来自其他节点的请求投票的消息
			// 当msg的context为compaignTransfer时，表示Leader强制要求某个Follower开始竞选，因为当前集群的Leader要转移领导权
			// 当前peer收到的正是来自Leader的强制转移领导权的目标follower的请求投票的消息
			force := bytes.Equal(m.Context, []byte(campaignTransfer))
			// 是否在租约期内
			// 租约：在选举超时时间内收到了来自当前Leader的消息,即他知道当前Leader还活着
			inLease := r.checkQuorum && r.lead != None && r.electionElapsed < r.electionTimeout
			// 若是非强制，且该follower在租约期内，那么不需要做任何处理
			if !force && inLease {
				// If a server receives a RequestVote request within the minimum election timeout
				// of hearing from a current leader, it does not update its term or grant its vote
				// 如果服务器在最小选举超时时间内收到了Leader的消息，并且在此期间还收到了RequestVote请求，
				// 那么它不会更新自己的term，也不会投票
				// 非强制、且在租约期内的peer可以忽略该请求投票的消息
				last := r.raftLog.lastEntryID()
				// TODO(pav-kv): it should be ok to simply print the %+v of the lastEntryID.
				r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] ignored %s from %x [logterm: %d, index: %d] at term %d: lease is not expired (remaining ticks: %d)",
					r.id, last.term, last.index, r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term, r.electionTimeout-r.electionElapsed)
				return nil
			}
		}
		switch {
		case m.Type == pb.MsgPreVote:
			// Never change our term in response to a PreVote
			// 在回复一个PreVote类型的投票请求时，不会改变自己的term
		case m.Type == pb.MsgPreVoteResp && !m.Reject:
			// We send pre-vote requests with a term in our future. If the
			// pre-vote is granted, we will increment our term when we get a
			// quorum. If it is not, the term comes from the node that
			// rejected our vote so we should become a follower at the new
			// term.
			// 我们发送带有future term的PreVote请求。如果PreVote被授予，我们将在获得大多数PreVote肯定回复时增加我们的term。
			// 如果没有，term来自拒绝我们投票请求的节点，因此我们应该在新term下成为follower。
		default:
			// 默认情况下，收到的是来自Leader的MsgApp、MsgHeartbeat、MsgSnap这些消息，可以正常更新自己的term
			r.logger.Infof("%x [term: %d] received a %s message with higher term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
			if m.Type == pb.MsgApp || m.Type == pb.MsgHeartbeat || m.Type == pb.MsgSnap {
				r.becomeFollower(m.Term, m.From)
			} else {
				r.becomeFollower(m.Term, None)
			}
		}

	case m.Term < r.Term: // 若消息的term小于当前raftNode的term，这里处理完就会返回
		// 若启动了checkQuorum或者preVote机制，且当前收到的消息类型是MsgApp或MsgHeartbeat
		if (r.checkQuorum || r.preVote) && (m.Type == pb.MsgHeartbeat || m.Type == pb.MsgApp) {
			// We have received messages from a leader at a lower term. It is possible
			// that these messages were simply delayed in the network, but this could
			// also mean that this node has advanced its term number during a network
			// partition, and it is now unable to either win an election or to rejoin
			// the majority on the old term. If checkQuorum is false, this will be
			// handled by incrementing term numbers in response to MsgVote with a
			// higher term, but if checkQuorum is true we may not advance the term on
			// MsgVote and must generate other messages to advance the term. The net
			// result of these two features is to minimize the disruption caused by
			// nodes that have been removed from the cluster's configuration: a
			// removed node will send MsgVotes (or MsgPreVotes) which will be ignored,
			// but it will not receive MsgApp or MsgHeartbeat, so it will not create
			// disruptive term increases, by notifying leader of this node's activeness.
			// The above comments also true for Pre-Vote
			// 我们从leader处收到了一个低于当前节点term的消息。这可能是因为这些消息在网络中被延迟了，但这也可能意味着
			// 在网络分区期间，这个节点已经提升了它的term编号，现在它既无法赢得选举，也无法在旧term上重新加入大多数节点。
			// 如果checkQuorum为false，则将通过增加自身的Term编号来响应具有更高Term的MsgVote消息；但如果 checkQuorum 为true，
			// 我们可能不会因为MsgVote信息而推进自身Term，并且必须生成其他消息来推进自身Term。
			// 这两个特性(即checkQuorum和preVote)的综合结果是最小化被从集群配置中移除的节点所造成的干扰：
			// 删除的节点将发送将被忽略的 MsgVotes（或 MsgPreVotes），但不会接收 MsgApp 或 MsgHeartbeat消息，
			// 因此它通过通知领导者该节点的活跃性，从而不会造成干扰性的任期增加。
			// 上述评论对于Pre-Vote也是正确的。
			//
			// When follower gets isolated, it soon starts an election ending
			// up with a higher term than leader, although it won't receive enough
			// votes to win the election. When it regains connectivity, this response
			// with "pb.MsgAppResp" of higher term would force leader to step down.
			// However, this disruption is inevitable to free this stuck node with
			// fresh election. This can be prevented with Pre-Vote phase.
			// 当follower被隔离时，它很快就会开始一场选举，最终以比leader更高的term结束，尽管它不会收到足够的选票来赢得选举。
			// 当它恢复连接时，这个带有更高term的“pb.MsgAppResp”响应将迫使leader下台。然而，为了通过新的选举来释放这个卡住的节点，
			// 这种干扰是不可避免的。这可以通过Pre-Vote阶段来防止。
			r.send(pb.Message{To: m.From, Type: pb.MsgAppResp})
		} else if m.Type == pb.MsgPreVote {
			// 当前集群存在分区，有大多数的分区可能已经开始了新的Term，而少数分区可能刚刚超时，发起
			// 选举之前，先开始PreVote阶段，而此时分区恢复，因此节点可能收到来自之前Term的PreVote消息
			// Before Pre-Vote enable, there may have candidate with higher term,
			// but less log. After update to Pre-Vote, the cluster may deadlock if
			// we drop messages with a lower term.
			// 在启用Pre-Vote之前，可能有term更高但日志更少的候选者。在更新为Pre-Vote之后，
			// 如果我们丢弃具有较低term的消息，集群可能会陷入死锁。
			last := r.raftLog.lastEntryID()
			// TODO(pav-kv): it should be ok to simply print %+v of the lastEntryID.
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, last.term, last.index, r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			r.send(pb.Message{To: m.From, Term: r.Term, Type: pb.MsgPreVoteResp, Reject: true})
		} else if m.Type == pb.MsgStorageAppendResp { // 本地append storage线程处理完Append消息后，会向当前节点发送的消息
			if m.Index != 0 {
				// Don't consider the appended log entries to be stable because
				// they may have been overwritten in the unstable log during a
				// later term. See the comment in newStorageAppendResp for more
				// about this race.
				// 不要认为追加的日志条目是稳定的，因为它们可能在后续term的不稳定日志中被覆盖。
				// 有关此竞争的更多信息，请参见newStorageAppendResp中的注释。
				r.logger.Infof("%x [term: %d] ignored entry appends from a %s message with lower term [term: %d]",
					r.id, r.Term, m.Type, m.Term)
			}
			if m.Snapshot != nil {
				// Even if the snapshot applied under a different term, its
				// application is still valid. Snapshots carry committed
				// (term-independent) state.
				// 即使快照在不同的term下应用，它的应用仍然有效。快照携带已提交的（与term无关的）状态。
				r.appliedSnap(m.Snapshot)
			}
		} else {
			// ignore other cases
			r.logger.Infof("%x [term: %d] ignored a %s message with lower term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
		}
		return nil
	}
	// 到此，说明该msg的Term等于当前raftNode的Term；
	// 或者msg.Term > r.Term,并且是LeaderTransfer类型选举，或者当前节点不在Lease内。
	switch m.Type {
	case pb.MsgHup:
		// 当收到MsgHup消息时，说明当前节点要发起选举了
		// 根据是否启动了PreVote机制，来决定是直接发起选举，还是先发起PreVote
		if r.preVote {
			r.hup(campaignPreElection)
		} else {
			r.hup(campaignElection)
		}

	case pb.MsgStorageAppendResp:
		// 当收到MsgStorageAppendResp消息时，说明本地append storage线程处理完Append消息后，会向当前节点发送的消息
		if m.Index != 0 {
			r.raftLog.stableTo(entryID{term: m.LogTerm, index: m.Index})
		}
		if m.Snapshot != nil {
			r.appliedSnap(m.Snapshot)
		}

	case pb.MsgStorageApplyResp:
		// 当收到MsgStorageApplyResp消息时，说明本地apply storage线程处理完Apply消息后，会向当前节点发送的消息
		if len(m.Entries) > 0 {
			index := m.Entries[len(m.Entries)-1].Index
			r.appliedTo(index, entsSize(m.Entries))
			r.reduceUncommittedSize(payloadsSize(m.Entries))
		}

	case pb.MsgVote, pb.MsgPreVote:
		// We can vote if this is a repeat of a vote we've already cast...
		// 如果这是我们已经投过的票的重复请求，可能对方由于网络原因没有收到我们的投票回复
		canVote := r.Vote == m.From ||
			// ...we haven't voted and we don't think there's a leader yet in this term...
			// ...我们还没有投票，而且我们认为在这个term中还没有领导者...
			(r.Vote == None && r.lead == None) ||
			// ...or this is a PreVote for a future term...
			// ...或者这是一个未来term的PreVote...
			(m.Type == pb.MsgPreVote && m.Term > r.Term)
		// ...and we believe the candidate is up to date.
		lastID := r.raftLog.lastEntryID()
		candLastID := entryID{term: m.LogTerm, index: m.Index}
		if canVote && r.raftLog.isUpToDate(candLastID) {
			// Note: it turns out that that learners must be allowed to cast votes.
			// This seems counter- intuitive but is necessary in the situation in which
			// a learner has been promoted (i.e. is now a voter) but has not learned
			// about this yet.
			// 注意：事实证明，学习者必须被允许投票。这似乎是违反直觉的，但在以下情况下是必要的：
			// 一个学习者已经被提升（即现在是一个投票者），但它还没有了解到这一点。
			//
			// For example, consider a group in which id=1 is a learner and id=2 and
			// id=3 are voters. A configuration change promoting 1 can be committed on
			// the quorum `{2,3}` without the config change being appended to the
			// learner's log. If the leader (say 2) fails, there are de facto two
			// voters remaining. Only 3 can win an election (due to its log containing
			// all committed entries), but to do so it will need 1 to vote. But 1
			// considers itself a learner and will continue to do so until 3 has
			// stepped up as leader, replicates the conf change to 1, and 1 applies it.
			// Ultimately, by receiving a request to vote, the learner realizes that
			// the candidate believes it to be a voter, and that it should act
			// accordingly. The candidate's config may be stale, too; but in that case
			// it won't win the election, at least in the absence of the bug discussed
			// in:
			// https://github.com/etcd-io/etcd/issues/7625#issuecomment-488798263.
			// 例如，考虑一个组，其中id=1是一个learner，id=2和id=3是voter。在quorum`{2,3}`上提交了一个提升1的配置更改，
			// 而该配置更改尚未附加到learner的日志中。如果leader（例如2）失败，实际上只剩下两个voter。只有3可以赢得选举
			// （因为它的日志包含所有已提交的条目），但为了这样做，它需要1投票。但1认为自己是一个learner，并且会继续这样做，
			// 直到3成为leader，将配置更改复制到1，并且1应用它。最终，通过接收投票请求，learner意识到候选者认为它是一个voter，
			// 并且它应该相应地行事。候选者的配置也可能过时；但在这种情况下，它不会赢得选举，至少在没有讨论的bug的情况下：；链接如上。
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] cast %s for %x [logterm: %d, index: %d] at term %d",
				r.id, lastID.term, lastID.index, r.Vote, m.Type, m.From, candLastID.term, candLastID.index, r.Term)
			// When responding to Msg{Pre,}Vote messages we include the term
			// from the message, not the local term. To see why, consider the
			// case where a single node was previously partitioned away and
			// it's local term is now out of date. If we include the local term
			// (recall that for pre-votes we don't update the local term), the
			// (pre-)campaigning node on the other end will proceed to ignore
			// the message (it ignores all out of date messages).
			// The term in the original message and current local term are the
			// same in the case of regular votes, but different for pre-votes.
			// 当响应Msg{Pre,}Vote消息时，我们包含消息中的term，而不是本地term。
			// 要了解原因，请考虑先前被分区的单个节点，它的本地term现在已经过时。如果我们包含本地term（回想一下，对于Pre-Votes，我们不会更新本地term），
			// 那么另一端的（pre-）campaigning节点将继续忽略该消息（它会忽略所有过时的消息）。
			// 这一点看一下上面处理m.Term < r.Term的代码就能理解了
			//
			// 在常规投票的情况下，原始消息中的term和当前本地term是相同的，但对于Pre-Votes，它们是不同的。
			r.send(pb.Message{To: m.From, Term: m.Term, Type: voteRespMsgType(m.Type)}) // 注意这里的Term是消息中的Term
			if m.Type == pb.MsgVote {
				// Only record real votes.
				// 只记录真正的投票，PreVote只是为了预选，不会真正投票
				r.electionElapsed = 0
				r.Vote = m.From
			}
		} else {
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, lastID.term, lastID.index, r.Vote, m.Type, m.From, candLastID.term, candLastID.index, r.Term)
			r.send(pb.Message{To: m.From, Term: r.Term, Type: voteRespMsgType(m.Type), Reject: true}) // 这里可以使用本地的Term
		}

	default:
		// 当传入的msg.Type不是pb.MsgVote, pb.MsgPreVote, pb.MsgStorageAppendResp, pb.MsgStorageApplyResp时，调用r.step(r, m)
		// 下面这些类型
		err := r.step(r, m)
		if err != nil {
			return err
		}
	}
	return nil
}

type stepFunc func(r *raft, m pb.Message) error

func stepLeader(r *raft, m pb.Message) error {
	// These message types do not require any progress for m.From.
	// 这些消息类型不需要m.From的任何进度。
	switch m.Type {
	case pb.MsgBeat:
		r.bcastHeartbeat()
		return nil
	case pb.MsgCheckQuorum:
		if !r.trk.QuorumActive() {
			r.logger.Warningf("%x stepped down to follower since quorum is not active", r.id)
			r.becomeFollower(r.Term, None)
		}
		// Mark everyone (but ourselves) as inactive in preparation for the next
		// CheckQuorum.
		// 为下一次CheckQuorum做准备，标记所有人（除了我们自己）为不活跃状态。
		r.trk.Visit(func(id uint64, pr *tracker.Progress) {
			if id != r.id {
				pr.RecentActive = false
			}
		})
		return nil
	case pb.MsgProp:
		if len(m.Entries) == 0 {
			r.logger.Panicf("%x stepped empty MsgProp", r.id)
		}
		if r.trk.Progress[r.id] == nil {
			// If we are not currently a member of the range (i.e. this node
			// was removed from the configuration while serving as leader),
			// drop any new proposals.
			// 如果我们当前不是范围的成员（即在担任领导者时从配置中删除了此节点），则丢弃任何新提案。
			return ErrProposalDropped
		}
		if r.leadTransferee != None {
			r.logger.Debugf("%x [term %d] transfer leadership to %x is in progress; dropping proposal", r.id, r.Term, r.leadTransferee)
			return ErrProposalDropped
		}

		for i := range m.Entries {
			e := &m.Entries[i]
			var cc pb.ConfChangeI
			if e.Type == pb.EntryConfChange {
				var ccc pb.ConfChange
				if err := ccc.Unmarshal(e.Data); err != nil {
					panic(err)
				}
				cc = ccc
			} else if e.Type == pb.EntryConfChangeV2 {
				var ccc pb.ConfChangeV2
				if err := ccc.Unmarshal(e.Data); err != nil {
					panic(err)
				}
				cc = ccc
			}
			if cc != nil {
				// alreadyPending表示当前raftNode存在未应用的ConfChange消息
				alreadyPending := r.pendingConfIndex > r.raftLog.applied
				// alreadyJoint表示当前集群是否正在处于JointConfigurations状态
				alreadyJoint := len(r.trk.Config.Voters[1]) > 0
				// wantsLeaveJoint；若该消息的changes字段为空，则表示该消息是一个空的ConfChange消息，
				// 空的ConfChange消息只能用于将集群从JointConfigurations状态退出，进而完全进入新的配置状态。
				// 注意旧的消息格式同样会被转化为V2格式，所以这里只需要判断V2格式的ConfChange消息
				wantsLeaveJoint := len(cc.AsV2().Changes) == 0

				var failedCheck string
				if alreadyPending {
					failedCheck = fmt.Sprintf("possible unapplied conf change at index %d (applied to %d)", r.pendingConfIndex, r.raftLog.applied)
				} else if alreadyJoint && !wantsLeaveJoint {
					failedCheck = "must transition out of joint config first"
				} else if !alreadyJoint && wantsLeaveJoint {
					failedCheck = "not in joint state; refusing empty conf change"
				}
				// 若failedCheck不为空，且当前raftNode没有禁用配置更改验证，则忽略该ConfChange消息
				if failedCheck != "" && !r.disableConfChangeValidation {
					r.logger.Infof("%x ignoring conf change %v at config %s: %s", r.id, cc, r.trk.Config, failedCheck)
					m.Entries[i] = pb.Entry{Type: pb.EntryNormal}
				} else {
					// 若failedCheck为空，或者当前raftNode禁用了配置更改验证
					// 则将该ConfChange消息在日志中应该所处的索引记录到pendingConfIndex中
					// 意味着当前raftNode存在未应用的ConfChange消息
					r.pendingConfIndex = r.raftLog.lastIndex() + uint64(i) + 1
				}
			}
		}

		if !r.appendEntry(m.Entries...) {
			return ErrProposalDropped
		}
		r.bcastAppend()
		return nil
	case pb.MsgReadIndex:
		// only one voting member (the leader) in the cluster
		// 集群中只有一个投票成员（领导者）
		// 那么直接返回ReadIndexResp消息，即直接处理该ReadIndex消息
		if r.trk.IsSingleton() {
			if resp := r.responseToReadIndexReq(m, r.raftLog.committed); resp.To != None {
				r.send(resp)
			}
			return nil
		}

		// Postpone read only request when this leader has not committed
		// any log entry at its term.
		// 当该leader在其term中没有提交任何日志条目时，推迟只读请求
		if !r.committedEntryInCurrentTerm() {
			r.pendingReadIndexMessages = append(r.pendingReadIndexMessages, m)
			return nil
		}
		sendMsgReadIndexResponse(r, m)

		return nil
	case pb.MsgForgetLeader:
		return nil // noop on leader
	}

	// All other message types require a progress for m.From (pr).
	pr := r.trk.Progress[m.From]
	if pr == nil {
		r.logger.Debugf("%x no progress available for %x", r.id, m.From)
		return nil
	}
	switch m.Type {
	case pb.MsgAppResp:
		// NB: this code path is also hit from (*raft).advance, where the leader steps
		// an MsgAppResp to acknowledge the appended entries in the last Ready.
		// NB：此代码路径也会从(*raft).advance命中，其中领导者主动steps一条MsgAppResp消息
		// 以确认最后一个Ready中的附加条目。

		pr.RecentActive = true

		if m.Reject {
			// RejectHint is the suggested next base entry for appending (i.e.
			// we try to append entry RejectHint+1 next), and LogTerm is the
			// term that the follower has at index RejectHint. Older versions
			// of this library did not populate LogTerm for rejections and it
			// is zero for followers with an empty log.
			// RejectHint是建议用于附加的下一个基本条目（即我们尝试附加条目RejectHint+1），
			// LogTerm是跟随者在索引RejectHint处的任期。 旧版本的此库不会为rejections填充LogTerm，并且对于具有空日志的跟随者，它为零。
			//
			// Under normal circumstances, the leader's log is longer than the
			// follower's and the follower's log is a prefix of the leader's
			// (i.e. there is no divergent uncommitted suffix of the log on the
			// follower). In that case, the first probe reveals where the
			// follower's log ends (RejectHint=follower's last index) and the
			// subsequent probe succeeds.
			// 在正常情况下，领导者的日志比跟随者的日志长，跟随者的日志是领导者的前缀（即跟随者的日志没有发散的未提交后缀）。
			// 在这种情况下，第一个探测揭示了跟随者的日志结束的地方（RejectHint=跟随者的最后索引），随后的探测成功。
			//
			// However, when networks are partitioned or systems overloaded,
			// large divergent log tails can occur. The naive attempt, probing
			// entry by entry in decreasing order, will be the product of the
			// length of the diverging tails and the network round-trip latency,
			// which can easily result in hours of time spent probing and can
			// even cause outright outages. The probes are thus optimized as
			// described below.
			// 但是，当网络被分区或系统超载时，可能会出现大量发散的日志尾部。
			// 朴素的尝试，按降序逐个探测条目，将是发散尾部的长度和网络往返延迟的乘积，
			// 这很容易导致数小时的探测时间，甚至可能导致完全的中断。 因此，探测被优化如下所述。
			r.logger.Debugf("%x received MsgAppResp(rejected, hint: (index %d, term %d)) from %x for index %d",
				r.id, m.RejectHint, m.LogTerm, m.From, m.Index)
			nextProbeIdx := m.RejectHint
			if m.LogTerm > 0 {
				// If the follower has an uncommitted log tail, we would end up
				// probing one by one until we hit the common prefix.
				// 如果跟随者有一个未提交的日志尾部，我们将逐个探测，直到我们找到公共前缀。
				//
				// For example, if the leader has:
				//
				//   idx        1 2 3 4 5 6 7 8 9
				//              -----------------
				//   term (L)   1 3 3 3 5 5 5 5 5
				//   term (F)   1 1 1 1 2 2
				//
				// Then, after sending an append anchored at (idx=9,term=5) we
				// would receive a RejectHint of 6 and LogTerm of 2. Without the
				// code below, we would try an append at index 6, which would
				// fail again.
				// 然后，在发送一个以（idx=9，term=5）为锚点的附加后，我们将收到RejectHint为6和LogTerm为2。
				// 如果没有下面的代码，我们将尝试在索引6处附加，这将再次失败。
				//
				// However, looking only at what the leader knows about its own
				// log and the rejection hint, it is clear that a probe at index
				// 6, 5, 4, 3, and 2 must fail as well:
				// 但是，仅查看领导者对自己的日志和拒绝提示的了解，很明显，索引6、5、4、3和2的探测也必须失败：
				//
				// For all of these indexes, the leader's log term is larger than
				// the rejection's log term. If a probe at one of these indexes
				// succeeded, its log term at that index would match the leader's,
				// i.e. 3 or 5 in this example. But the follower already told the
				// leader that it is still at term 2 at index 6, and since the
				// log term only ever goes up (within a log), this is a contradiction.
				// 对于所有这些索引，领导者的日志任期都大于拒绝的日志任期。
				// 如果这些索引中的一个探测成功，那么它在该索引处的日志任期将与领导者的日志任期匹配，即在此示例中为3或5。
				// 但是，跟随者已经告诉领导者，它在索引6处仍然处于任期2，并且由于日志任期只会增加（在日志中），这是一个矛盾。
				//
				// At index 1, however, the leader can draw no such conclusion,
				// as its term 1 is not larger than the term 2 from the
				// follower's rejection. We thus probe at 1, which will succeed
				// in this example. In general, with this approach we probe at
				// most once per term found in the leader's log.
				// 但是，在索引1处，领导者无法得出这样的结论，因为它的任期1不大于跟随者的拒绝的任期2。
				// 因此，我们在1处进行探测，在这个例子中将成功。 通常，使用这种方法，我们在领导者的日志中找到的每个任期中最多探测一次。
				//
				// There is a similar mechanism on the follower (implemented in
				// handleAppendEntries via a call to findConflictByTerm) that is
				// useful if the follower has a large divergent uncommitted log
				// tail[1], as in this example:
				// 在跟随者上有一个类似的机制（通过调用findConflictByTerm在handleAppendEntries中实现），
				// 如果跟随者有一个大的发散的未提交的日志尾部[1]，则这是有用的，如下例所示：
				//
				//   idx        1 2 3 4 5 6 7 8 9
				//              -----------------
				//   term (L)   1 3 3 3 3 3 3 3 7
				//   term (F)   1 3 3 4 4 5 5 5 6
				//
				// Naively, the leader would probe at idx=9, receive a rejection
				// revealing the log term of 6 at the follower. Since the leader's
				// term at the previous index is already smaller than 6, the leader-
				// side optimization discussed above is ineffective. The leader thus
				// probes at index 8 and, naively, receives a rejection for the same
				// index and log term 5. Again, the leader optimization does not improve
				// over linear probing as term 5 is above the leader's term 3 for that
				// and many preceding indexes; the leader would have to probe linearly
				// until it would finally hit index 3, where the probe would succeed.
				// 朴素地，领导者将在idx=9处进行探测，收到一个拒绝，揭示了跟随者的日志任期为6。
				// 由于领导者在前一个索引处的任期已经小于6，因此上面讨论的领导者端优化是无效的。
				// 因此，领导者在索引8处进行探测，并且朴素地，收到了相同索引和日志任期5的拒绝。
				// 同样，领导者优化不会改善线性探测，因为任期5高于领导者的任期3，对于该索引和许多前面的索引；
				// 领导者必须线性探测，直到最终到达索引3，探测才会成功。
				//
				// Instead, we apply a similar optimization on the follower. When the
				// follower receives the probe at index 8 (log term 3), it concludes
				// that all of the leader's log preceding that index has log terms of
				// 3 or below. The largest index in the follower's log with a log term
				// of 3 or below is index 3. The follower will thus return a rejection
				// for index=3, log term=3 instead. The leader's next probe will then
				// succeed at that index.
				// 相反，我们在跟随者上应用了类似的优化。 当跟随者在索引8（日志任期3）处收到探测时，
				// 它得出结论：领导者的所有日志在该索引之前的索引都具有3或更低的日志任期。
				// 具有3或更低日志任期的跟随者日志中的最大索引是索引3。 因此，跟随者将返回索引=3，日志任期=3的rejectHint。
				// 领导者的下一个探测将在该索引处成功。
				//
				// [1]: more precisely, if the log terms in the large uncommitted
				// tail on the follower are larger than the leader's. At first,
				// it may seem unintuitive that a follower could even have such
				// a large tail, but it can happen:
				// [1]：更准确地说，如果跟随者上大量未提交的尾部中的日志任期大于领导者的日志任期。
				// 乍一看，跟随者甚至可能有这样一个大的尾部似乎有些不合逻辑，但它确实可能发生：
				//
				// 1. Leader appends (but does not commit) entries 2 and 3, crashes.
				//   idx        1 2 3 4 5 6 7 8 9
				//              -----------------
				//   term (L)   1 2 2     [crashes]
				//   term (F)   1
				//   term (F)   1
				//
				// 2. a follower becomes leader and appends entries at term 3.
				//              -----------------
				//   term (x)   1 2 2     [down]
				//   term (F)   1 3 3 3 3
				//   term (F)   1
				//
				// 3. term 3 leader goes down, term 2 leader returns as term 4
				//    leader. It commits the log & entries at term 4.
				//
				//              -----------------
				//   term (L)   1 2 2 2
				//   term (x)   1 3 3 3 3 [down]
				//   term (F)   1
				//              -----------------
				//   term (L)   1 2 2 2 4 4 4
				//   term (F)   1 3 3 3 3 [gets probed]
				//   term (F)   1 2 2 2 4 4 4
				//
				// 4. the leader will now probe the returning follower at index
				//    7, the rejection points it at the end of the follower's log
				//    which is at a higher log term than the actually committed
				//    log.
				// 4. 现在，领导者将在索引7处探测重回集群的跟随者，rejectHint将其指向跟随者日志的末尾，
				// 而跟随者日志的末尾的日志任期高于实际提交的日志。
				nextProbeIdx, _ = r.raftLog.findConflictByTerm(m.RejectHint, m.LogTerm)
			}
			if pr.MaybeDecrTo(m.Index, nextProbeIdx) {
				r.logger.Debugf("%x decreased progress of %x to [%s]", r.id, m.From, pr)
				if pr.State == tracker.StateReplicate {
					pr.BecomeProbe()
				}
				r.sendAppend(m.From)
			}
		} else {
			oldPaused := pr.IsPaused()
			// We want to update our tracking if the response updates our
			// matched index or if the response can move a probing peer back
			// into StateReplicate (see heartbeat_rep_recovers_from_probing.txt
			// for an example of the latter case).
			// NB: the same does not make sense for StateSnapshot - if `m.Index`
			// equals pr.Match we know we don't m.Index+1 in our log, so moving
			// back to replicating state is not useful; besides pr.PendingSnapshot
			// would prevent it.
			// 如果response更新了我们的matchIndex，或者response可以将探测的目标follower
			// 重新转换为StateReplicate(参见heartbeat_rep_recovers_from_probing.txt)，
			// 这两种情况下我们想要更新我们的tracking。
			// 注意：对于StateSnapshot来说，这种情况是没有意义的，如果`m.Index`等于pr.Match，
			// 我们知道我们的日志中没有m.Index+1，因此回到stateReplicate状态是没有用的；
			// 此外，pr.PendingSnapshot将阻止这种情况。
			if pr.MaybeUpdate(m.Index) || (pr.Match == m.Index && pr.State == tracker.StateProbe) {
				switch {
				case pr.State == tracker.StateProbe:
					pr.BecomeReplicate()
				case pr.State == tracker.StateSnapshot && pr.Match+1 >= r.raftLog.firstIndex():
					// Note that we don't take into account PendingSnapshot to
					// enter this branch. No matter at which index a snapshot
					// was actually applied, as long as this allows catching up
					// the follower from the log, we will accept it. This gives
					// systems more flexibility in how they implement snapshots;
					// see the comments on PendingSnapshot.
					// 注意，在这个分支中，我们不考虑PendingSnapshot。无论快照实际应用在哪个索引，
					// 只要这个快照允许follower从日志中赶上，我们就会接受它。这使得系统在实现快照时更加灵活；
					// 请参阅PendingSnapshot的注释。
					r.logger.Debugf("%x recovered from needing snapshot, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
					// Transition back to replicating state via probing state
					// (which takes the snapshot into account). If we didn't
					// move to replicating state, that would only happen with
					// the next round of appends (but there may not be a next
					// round for a while, exposing an inconsistent RaftStatus).
					// 通过StateProbe（考虑到快照）转换回StateReplicate状态。
					// 如果我们没有转换到StateReplicate状态，那只会在下一轮Append操作中发生
					// （但可能有一段时间没有下一轮Append操作，从而暴露了不一致的RaftStatus）。
					// Probe->Replicate,这是为了从快照状态平稳过渡到复制状态
					pr.BecomeProbe()
					pr.BecomeReplicate()
				case pr.State == tracker.StateReplicate:
					pr.Inflights.FreeLE(m.Index)
				}

				if r.maybeCommit() {
					// committed index has progressed for the term, so it is safe
					// to respond to pending read index requests
					// 提交的索引已经进展到了这个任期，所以可以安全地响应挂起的读索引请求
					releasePendingReadIndexMessages(r)
					r.bcastAppend()
				} else if oldPaused {
					// If we were paused before, this node may be missing the
					// latest commit index, so send it.
					// 如果之前被暂停，这个节点可能缺少最新的提交索引，所以发送它。
					r.sendAppend(m.From)
				}
				// We've updated flow control information above, which may
				// allow us to send multiple (size-limited) in-flight messages
				// at once (such as when transitioning from probe to
				// replicate, or when freeTo() covers multiple messages). If
				// we have more entries to send, send as many messages as we
				// can (without sending empty messages for the commit index)
				// 我们已经在上面更新了Inflights的信息，这可能允许我们一次发送多个（大小受限）的in-flight消息。
				// （例如，从探测状态转换到复制状态时，或者freeTo()释放了多个消息时）。
				// 如果我们有更多的条目要发送，尽可能多地发送消息（而不发送空消息以提交索引）
				if r.id != m.From {
					for r.maybeSendAppend(m.From, false /* sendIfEmpty */) {
					}
				}
				// Transfer leadership is in progress.
				// 领导权转移正在进行中。
				if m.From == r.leadTransferee && pr.Match == r.raftLog.lastIndex() {
					r.logger.Infof("%x sent MsgTimeoutNow to %x after received MsgAppResp", r.id, m.From)
					r.sendTimeoutNow(m.From)
				}
			}
		}
	case pb.MsgHeartbeatResp:
		pr.RecentActive = true
		pr.MsgAppFlowPaused = false

		// NB: if the follower is paused (full Inflights), this will still send an
		// empty append, allowing it to recover from situations in which all the
		// messages that filled up Inflights in the first place were dropped. Note
		// also that the outgoing heartbeat already communicated the commit index.
		// 注意：如果跟随者被暂停（Inflights已满），这仍然会发送一个空的append消息，
		// 使其能够从最初填满Inflights的所有消息都被丢弃的情况中恢复。注意，出站心跳已经传达了提交索引。
		//
		// If the follower is fully caught up but also in StateProbe (as can happen
		// if ReportUnreachable was called), we also want to send an append (it will
		// be empty) to allow the follower to transition back to StateReplicate once
		// it responds.
		// 如果跟随者已经完全赶上，但也处于StateProbe状态（如果调用了ReportUnreachable，可能会发生这种情况），
		// 我们还想发送一个append（它将是空的），以便让跟随者在响应后转换回StateReplicate状态。
		//
		// Note that StateSnapshot typically satisfies pr.Match < lastIndex, but
		// `pr.Paused()` is always true for StateSnapshot, so sendAppend is a
		// no-op.
		// 请注意，StateSnapshot通常满足pr.Match < lastIndex，但是`pr.Paused()`对于StateSnapshot始终为true，
		// 因此sendAppend是一个空操作。
		if pr.Match < r.raftLog.lastIndex() || pr.State == tracker.StateProbe {
			r.sendAppend(m.From)
		}

		// 若readOnly的机制不为ReadOnlySafe，说明ReadIndex请求不需要每次都CheckQuorum，直接返回
		// 消息的Context为空，意味着未携带ReadIndex请求，则直接返回
		if r.readOnly.option != ReadOnlySafe || len(m.Context) == 0 {
			return nil
		}

		// 若当前leader收到心跳响应后未达到Quorum，则直接返回
		if r.trk.Voters.VoteResult(r.readOnly.recvAck(m.From, m.Context)) != quorum.VoteWon {
			return nil
		}

		// 首先获取所有符合条件的ReadIndex消息
		// 一个RequestCtx唯一对应一个ReadIndex消息，当某条ReadIndex消息的Quorum达到时，说明之前的ReadIndex也可以被处理
		rss := r.readOnly.advance(m)
		for _, rs := range rss {
			if resp := r.responseToReadIndexReq(rs.req, rs.index); resp.To != None {
				r.send(resp)
			}
		}
	case pb.MsgSnapStatus:
		// 若当前peer的state不为StateSnapshot，说明是过期的消息，直接返回
		if pr.State != tracker.StateSnapshot {
			return nil
		}
		if !m.Reject { // 快照应用成功，切换该peer的状态为StateProbe
			pr.BecomeProbe()
			r.logger.Debugf("%x snapshot succeeded, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		} else {
			// NB: the order here matters or we'll be probing erroneously from
			// the snapshot index, but the snapshot never applied.
			// 注意：这里的顺序很重要，否则我们将从快照索引处开始错误地进行探测，但是快照从未应用。
			// 所以要先把pr.PendingSnapshot置为0，然后再调用BecomeProbe，启动探测状态
			pr.PendingSnapshot = 0
			pr.BecomeProbe()
			r.logger.Debugf("%x snapshot failed, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		}
		// If snapshot finish, wait for the MsgAppResp from the remote node before sending
		// out the next MsgApp.
		// 如果快照完成，等待远程节点的MsgAppResp，然后再发送下一个MsgApp。
		// If snapshot failure, wait for a heartbeat interval before next try
		// 如果快照失败，等待一个心跳间隔，然后再尝试
		pr.MsgAppFlowPaused = true
	case pb.MsgUnreachable:
		// During optimistic replication, if the remote becomes unreachable,
		// there is huge probability that a MsgApp is lost.
		// 在乐观复制期间，如果远程节点变得不可达，那么MsgApp很可能丢失。
		// 由上层RawNode发送MsgUnreachable消息，说明远程节点不可达
		if pr.State == tracker.StateReplicate {
			pr.BecomeProbe()
		}
		r.logger.Debugf("%x failed to send message to %x because it is unreachable [%s]", r.id, m.From, pr)
	case pb.MsgTransferLeader:
		if pr.IsLearner {
			// Learner状态的节点不能成为Leader，直接返回
			r.logger.Debugf("%x is learner. Ignored transferring leadership", r.id)
			return nil
		}
		leadTransferee := m.From
		lastLeadTransferee := r.leadTransferee
		// 若当前有正在进行的Leader Transfer操作
		if lastLeadTransferee != None {
			// 若当前正在进行的Leader Transfer操作的目标节点与当前请求的目标节点相同，则直接返回
			if lastLeadTransferee == leadTransferee {
				r.logger.Infof("%x [term %d] transfer leadership to %x is in progress, ignores request to same node %x",
					r.id, r.Term, leadTransferee, leadTransferee)
				return nil
			}
			// 若当前正在进行的Leader Transfer操作的目标节点与当前请求的目标节点不同，则取消当前的Leader Transfer操作
			// 之后开始处理新的Leader Transfer操作
			r.abortLeaderTransfer()
			r.logger.Infof("%x [term %d] abort previous transferring leadership to %x", r.id, r.Term, lastLeadTransferee)
		}
		if leadTransferee == r.id { // 若当前节点就是Leader，则直接返回
			r.logger.Debugf("%x is already leader. Ignored transferring leadership to self", r.id)
			return nil
		}
		// Transfer leadership to third party.
		r.logger.Infof("%x [term %d] starts to transfer leadership to %x", r.id, r.Term, leadTransferee)
		// Transfer leadership should be finished in one electionTimeout, so reset r.electionElapsed.
		// Leader Transfer操作应该在一个electionTimeout内完成，所以重置r.electionElapsed
		r.electionElapsed = 0
		r.leadTransferee = leadTransferee
		if pr.Match == r.raftLog.lastIndex() {
			// 若当前目标节点的MatchIndex等于当前节点的lastIndex，说明日志已经同步到最新，直接发送MsgTimeoutNow消息
			// 通知目标节点立即发起选举
			r.sendTimeoutNow(leadTransferee)
			r.logger.Infof("%x sends MsgTimeoutNow to %x immediately as %x already has up-to-date log", r.id, leadTransferee, leadTransferee)
		} else {
			// 否则，发送MsgApp消息，让目标节点尽快同步日志
			r.sendAppend(leadTransferee)
		}
	}
	return nil
}

// stepCandidate is shared by StateCandidate and StatePreCandidate; the difference is
// whether they respond to MsgVoteResp or MsgPreVoteResp.
// stepCandidate由StateCandidate和StatePreCandidate共享；它们的区别在于它们是响应MsgVoteResp还是MsgPreVoteResp。
func stepCandidate(r *raft, m pb.Message) error {
	// Only handle vote responses corresponding to our candidacy (while in
	// StateCandidate, we may get stale MsgPreVoteResp messages in this term from
	// our pre-candidate state).
	// 只处理与我们的候选资格相对应的投票响应（在StateCandidate状态下，在这个任期中，我们可能会从我们的PreCandidate状态中
	// 得到过时的MsgPreVoteResp消息）。
	var myVoteRespType pb.MessageType
	if r.state == StatePreCandidate {
		myVoteRespType = pb.MsgPreVoteResp
	} else {
		myVoteRespType = pb.MsgVoteResp
	}
	switch m.Type {
	case pb.MsgProp:
		r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
		return ErrProposalDropped
	case pb.MsgApp:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleAppendEntries(m)
	case pb.MsgHeartbeat:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleHeartbeat(m)
	case pb.MsgSnap:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleSnapshot(m)
	case myVoteRespType:
		// 计算当前集群中具有投票权的节点有多少个，以及有多少个拒绝了投票
		gr, rj, res := r.poll(m.From, m.Type, !m.Reject)
		r.logger.Infof("%x has received %d %s votes and %d vote rejections", r.id, gr, m.Type, rj)
		switch res {
		case quorum.VoteWon:
			if r.state == StatePreCandidate {
				r.campaign(campaignElection)
			} else {
				r.becomeLeader()
				r.bcastAppend()
			}
		case quorum.VoteLost:
			// pb.MsgPreVoteResp contains future term of pre-candidate
			// m.Term > r.Term; reuse r.Term
			// pb.MsgPreVoteResp包含了PreCandidate的未来任期m.Term > r.Term；重用r.Term
			r.becomeFollower(r.Term, None)
		}
	case pb.MsgTimeoutNow:
		// candidate不能成为Leader Transfer操作的对象，因为他已经在选举过程中了，TimeoutNow消息会让其马上再次发起选举，
		// 这明显是不合理的
		r.logger.Debugf("%x [term %d state %v] ignored MsgTimeoutNow from %x", r.id, r.Term, r.state, m.From)
	}
	return nil
}

func stepFollower(r *raft, m pb.Message) error {
	switch m.Type {
	case pb.MsgProp:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
			return ErrProposalDropped
		} else if r.disableProposalForwarding {
			r.logger.Infof("%x not forwarding to leader %x at term %d; dropping proposal", r.id, r.lead, r.Term)
			return ErrProposalDropped
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgApp:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleAppendEntries(m)
	case pb.MsgHeartbeat:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleHeartbeat(m)
	case pb.MsgSnap:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleSnapshot(m)
	case pb.MsgTransferLeader:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping leader transfer msg", r.id, r.Term)
			return nil
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgForgetLeader:
		if r.readOnly.option == ReadOnlyLeaseBased {
			r.logger.Error("ignoring MsgForgetLeader due to ReadOnlyLeaseBased")
			return nil
		}
		if r.lead != None {
			r.logger.Infof("%x forgetting leader %x at term %d", r.id, r.lead, r.Term)
			r.lead = None
		}
	case pb.MsgTimeoutNow:
		r.logger.Infof("%x [term %d] received MsgTimeoutNow from %x and starts an election to get leadership.", r.id, r.Term, m.From)
		// Leadership transfers never use pre-vote even if r.preVote is true; we
		// know we are not recovering from a partition so there is no need for the
		// extra round trip.
		r.hup(campaignTransfer)
	case pb.MsgReadIndex:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping index reading msg", r.id, r.Term)
			return nil
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgReadIndexResp:
		if len(m.Entries) != 1 {
			r.logger.Errorf("%x invalid format of MsgReadIndexResp from %x, entries count: %d", r.id, m.From, len(m.Entries))
			return nil
		}
		r.readStates = append(r.readStates, ReadState{Index: m.Index, RequestCtx: m.Entries[0].Data})
	}
	return nil
}

// logSliceFromMsgApp extracts the appended logSlice from a MsgApp message.
func logSliceFromMsgApp(m *pb.Message) logSlice {
	// TODO(pav-kv): consider also validating the logSlice here.
	return logSlice{
		term:    m.Term,
		prev:    entryID{term: m.LogTerm, index: m.Index},
		entries: m.Entries,
	}
}

func (r *raft) handleAppendEntries(m pb.Message) {
	// TODO(pav-kv): construct logSlice up the stack next to receiving the
	// message, and validate it before taking any action (e.g. bumping term).
	// 首先，从MsgApp消息中提取出追加的日志条目。
	a := logSliceFromMsgApp(&m)

	/* 如果Leader的append操作的prevLogIndex小于当前节点的commitIndex，
	则说明Leader的日志已经过期，当前节点不会接受Leader的append操作。*/
	if a.prev.index < r.raftLog.committed {
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
		return
	}
	// 尝试将日志追加到当前节点的日志中。若追加失败，意味着prev信息不匹配，继续向下走流程。
	if mlastIndex, ok := r.raftLog.maybeAppend(a, m.Commit); ok {
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: mlastIndex})
		return
	}
	r.logger.Debugf("%x [logterm: %d, index: %d] rejected MsgApp [logterm: %d, index: %d] from %x",
		r.id, r.raftLog.zeroTermOnOutOfBounds(r.raftLog.term(m.Index)), m.Index, m.LogTerm, m.Index, m.From)

	// Our log does not match the leader's at index m.Index. Return a hint to the
	// leader - a guess on the maximal (index, term) at which the logs match. Do
	// this by searching through the follower's log for the maximum (index, term)
	// pair with a term <= the MsgApp's LogTerm and an index <= the MsgApp's
	// Index. This can help skip all indexes in the follower's uncommitted tail
	// with terms greater than the MsgApp's LogTerm.
	//
	// See the other caller for findConflictByTerm (in stepLeader) for a much more
	// detailed explanation of this mechanism.
	// 我们的日志在索引m.Index处与Leader的日志不匹配。向Leader返回一个提示——
	// 一个关于最大匹配的(index, term)的猜测。通过搜索跟随者的日志，找到最大的
	// (index, term)对，其中term <= MsgApp的LogTerm，index <= MsgApp的Index。
	// 这可以帮助跳过跟随者未提交的尾部中所有term大于MsgApp的LogTerm的索引。
	//
	// 有关findConflictByTerm的其他调用者（在stepLeader中）的更详细的解释，请参见
	// 此机制。

	// NB: m.Index >= raftLog.committed by now (see the early return above), and
	// raftLog.lastIndex() >= raftLog.committed by invariant, so min of the two is
	// also >= raftLog.committed. Hence, the findConflictByTerm argument is within
	// the valid interval, which then will return a valid (index, term) pair with
	// a non-zero term (unless the log is empty). However, it is safe to send a zero
	// LogTerm in this response in any case, so we don't verify it here.
	// 注意：m.Index >= raftLog.committed，raftLog.lastIndex() >= raftLog.committed，
	// 所以两者的最小值也是 >= raftLog.committed。
	// 因此，findConflictByTerm的参数在有效区间内，然后将返回一个有效的(index, term)对，
	// 该对具有非零term（除非日志为空）。但是，在任何情况下，向此响应发送零LogTerm是安全的，
	// 因此我们不在此处验证它。
	// 取prevLogIndex和当前节点的commitIndex的最小值作为hintIndex
	hintIndex := min(m.Index, r.raftLog.lastIndex())
	// 通过hintIndex和m.LogTerm查找冲突的日志条目,获取最终的hintIndex和hintTerm
	// 返回的是当前节点日志中小于等于m.LogTerm的最大的index
	hintIndex, hintTerm := r.raftLog.findConflictByTerm(hintIndex, m.LogTerm)
	r.send(pb.Message{
		To:         m.From,
		Type:       pb.MsgAppResp,
		Index:      m.Index,
		Reject:     true,
		RejectHint: hintIndex,
		LogTerm:    hintTerm,
	})
}

func (r *raft) handleHeartbeat(m pb.Message) {
	r.raftLog.commitTo(m.Commit)
	r.send(pb.Message{To: m.From, Type: pb.MsgHeartbeatResp, Context: m.Context})
}

func (r *raft) handleSnapshot(m pb.Message) {
	// MsgSnap messages should always carry a non-nil Snapshot, but err on the
	// side of safety and treat a nil Snapshot as a zero-valued Snapshot.
	// MsgSnap 消息应该总是携带一个非nil的Snapshot，但是为了安全起见，将nil的Snapshot视为零值的Snapshot。
	var s pb.Snapshot
	if m.Snapshot != nil {
		s = *m.Snapshot
	}
	sindex, sterm := s.Metadata.Index, s.Metadata.Term
	if r.restore(s) {
		r.logger.Infof("%x [commit: %d] restored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, sindex, sterm)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.lastIndex()})
	} else {
		r.logger.Infof("%x [commit: %d] ignored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, sindex, sterm)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
	}
}

// restore recovers the state machine from a snapshot. It restores the log and the
// configuration of state machine. If this method returns false, the snapshot was
// ignored, either because it was obsolete or because of an error.
// restore从快照中恢复状态机。它恢复日志和状态机的配置。如果此方法返回false，则快照被忽略，
func (r *raft) restore(s pb.Snapshot) bool {
	// 若快照的索引小于当前节点的commitIndex，则说明快照已经过期，不会接受快照。
	if s.Metadata.Index <= r.raftLog.committed {
		return false
	}
	if r.state != StateFollower {
		// This is defense-in-depth: if the leader somehow ended up applying a
		// snapshot, it could move into a new term without moving into a
		// follower state. This should never fire, but if it did, we'd have
		// prevented damage by returning early, so log only a loud warning.
		// 这是深度防御：如果领导者以某种方式应用了快照，它可能会在不进入follower状态的情况下进入新任期。
		// 这应该永远不会触发，但如果确实发生了，我们通过提前返回来防止损害，所以只记录一个大声的警告。

		// At the time of writing, the instance is guaranteed to be in follower
		// state when this method is called.
		// 在写入快照时，保证在调用此方法时实例处于follower状态。
		r.logger.Warningf("%x attempted to restore snapshot as leader; should never happen", r.id)
		r.becomeFollower(r.Term+1, None)
		return false
	}

	// More defense-in-depth: throw away snapshot if recipient is not in the
	// config. This shouldn't ever happen (at the time of writing) but lots of
	// code here and there assumes that r.id is in the progress tracker.
	// 更深层次的防御：如果接收者不在配置中，则丢弃快照。这不应该发生（在写入时），
	found := false
	cs := s.Metadata.ConfState

	// 这个for循环的目的是检查当前节点是否在快照的ConfState中
	// 依次检查了Voters、Learners、VotersOutgoing三个集合
	// 这里的写法是将Voters、Learners、VotersOutgoing三个集合合并到一个数组中，然后遍历这个数组
	for _, set := range [][]uint64{
		cs.Voters,
		cs.Learners,
		cs.VotersOutgoing,
		// `LearnersNext` doesn't need to be checked. According to the rules, if a peer in
		// `LearnersNext`, it has to be in `VotersOutgoing`.
		// 不需要检查`LearnersNext`。根据规则，如果一个raft peer在`LearnersNext`中，它必须在`VotersOutgoing`中。
	} {
		for _, id := range set {
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
		r.logger.Warningf(
			"%x attempted to restore snapshot but it is not in the ConfState %v; should never happen",
			r.id, cs,
		)
		return false
	}

	// Now go ahead and actually restore.

	id := entryID{term: s.Metadata.Term, index: s.Metadata.Index}
	if r.raftLog.matchTerm(id) { // 若当前节点的日志中已经存在了这个快照的索引和任期
		// TODO(pav-kv): can print %+v of the id, but it will change the format.
		last := r.raftLog.lastEntryID()
		r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] fast-forwarded commit to snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, last.index, last.term, id.index, id.term)
		// 这种情况说明该raft peer中已经包含了快照携带的状态信息，所以不需要再次应用快照
		// 但是需要将当前节点的commitIndex更新为快照的索引
		r.raftLog.commitTo(s.Metadata.Index)
		return false
	}

	// 说明当前节点的日志中没有快照的索引和任期，需要应用快照从而与Leader保持一致
	// 这里相当于重置了当前节点的unstable状态
	r.raftLog.restore(s)

	// Reset the configuration and add the (potentially updated) peers in anew.
	// 根据快照中携带的ConfState信息，重置当前节点的配置
	r.trk = tracker.MakeProgressTracker(r.trk.MaxInflight, r.trk.MaxInflightBytes)
	cfg, trk, err := confchange.Restore(confchange.Changer{
		Tracker:   r.trk,
		LastIndex: r.raftLog.lastIndex(),
	}, cs)

	if err != nil {
		// This should never happen. Either there's a bug in our config change
		// handling or the client corrupted the conf change.
		// 这不应该发生。要么是我们的配置更改处理中存在错误，要么是客户端损坏了配置更改。
		panic(fmt.Sprintf("unable to restore config %+v: %s", cs, err))
	}

	// 确保当前节点更新后的配置与快照中携带的配置一致
	assertConfStatesEquivalent(r.logger, cs, r.switchToConfig(cfg, trk))

	last := r.raftLog.lastEntryID()
	r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] restored snapshot [index: %d, term: %d]",
		r.id, r.raftLog.committed, last.index, last.term, id.index, id.term)
	return true
}

// promotable indicates whether state machine can be promoted to leader,
// which is true when its own id is in progress list.
// promotable表示状态机是否可以晋升为领导者，当自己的id在进度列表中时为true。
func (r *raft) promotable() bool {
	pr := r.trk.Progress[r.id]
	return pr != nil && !pr.IsLearner && !r.raftLog.hasNextOrInProgressSnapshot()
}

func (r *raft) applyConfChange(cc pb.ConfChangeV2) pb.ConfState {
	cfg, trk, err := func() (tracker.Config, tracker.ProgressMap, error) {
		// 首先，根据ConfChangeV2消息中的配置变更信息，生成一个Changer对象
		changer := confchange.Changer{
			Tracker:   r.trk,
			LastIndex: r.raftLog.lastIndex(),
		}
		if cc.LeaveJoint() {
			// 若cc本身是一条空的配置变更消息，意味着当前节点可以离开JointConfig状态
			return changer.LeaveJoint()
		} else if autoLeave, ok := cc.EnterJoint(); ok {
			// 若本次变更使用的是Joint Consensus算法
			return changer.EnterJoint(autoLeave, cc.Changes...)
		}
		// 若变更操作只有一条，则采用one at a time的方式
		return changer.Simple(cc.Changes...)
	}()

	// 变更后的配置状态有问题：未通过checkAndReturn()检查
	if err != nil {
		// TODO(tbg): return the error to the caller.
		panic(err)
	}
	// 用新的配置和进度信息更新当前节点的配置和进度信息
	return r.switchToConfig(cfg, trk)
}

// switchToConfig reconfigures this node to use the provided configuration. It
// updates the in-memory state and, when necessary, carries out additional
// actions such as reacting to the removal of nodes or changed quorum
// requirements.
// switchToConfig用提供的配置重新配置此节点。它更新内存状态，并在必要时执行其他操作，
// 例如响应节点的移除或更改法定人数要求。
//
// The inputs usually result from restoring a ConfState or applying a ConfChange.
// 输入通常是从恢复ConfState或应用ConfChange中获得的。
func (r *raft) switchToConfig(cfg tracker.Config, trk tracker.ProgressMap) pb.ConfState {
	r.trk.Config = cfg
	r.trk.Progress = trk

	r.logger.Infof("%x switched to configuration %s", r.id, r.trk.Config)
	cs := r.trk.ConfState()
	pr, ok := r.trk.Progress[r.id]

	// Update whether the node itself is a learner, resetting to false when the
	// node is removed.
	// 更新节点本身是否为学习者，当节点被移除时重置为false。
	r.isLearner = ok && pr.IsLearner

	// 若当前节点在应用配置变更前是Leader，且在新配置中被移除或者被降级为学习者，则需要立即降级为Follower
	if (!ok || r.isLearner) && r.state == StateLeader {
		// This node is leader and was removed or demoted, step down if requested.
		// 该节点是Leader并且被移除或降级为学习者，如果需要则立即降级为Follower。
		//
		// We prevent demotions at the time writing but hypothetically we handle
		// them the same way as removing the leader.
		// 在写入时，我们避免节点降级；但在假设情况下，我们以与移除Leader相同的方式处理降级。
		//
		// TODO(tbg): ask follower with largest Match to TimeoutNow (to avoid
		// interruption). This might still drop some proposals but it's better than
		// nothing.
		// TODO(tbg):请求具有最大Match的follower接收TimeoutNow消息（以避免中断）。
		// 这可能仍然会丢弃一些提案，但总比没有好。
		if r.stepDownOnRemoval {
			r.becomeFollower(r.Term, None)
		}
		return cs
	}

	// The remaining steps only make sense if this node is the leader and there
	// are other nodes.
	// 如果当前节点是Leader并且有其他节点，则剩下的步骤才有意义。
	if r.state != StateLeader || len(cs.Voters) == 0 {
		return cs
	}

	if r.maybeCommit() {
		// If the configuration change means that more entries are committed now,
		// broadcast/append to everyone in the updated config.
		// 如果配置更改意味着现在提交了更多的条目，则广播/追加到更新后的配置中的每个人。
		r.bcastAppend()
	} else {
		// Otherwise, still probe the newly added replicas; there's no reason to
		// let them wait out a heartbeat interval (or the next incoming
		// proposal).
		// 否则，仍然探测新添加的副本；没有理由让它们等待一个心跳间隔（或下一个传入的提案）。
		r.trk.Visit(func(id uint64, pr *tracker.Progress) {
			if id == r.id {
				return
			}
			r.maybeSendAppend(id, false /* sendIfEmpty */)
		})
	}
	// If the leadTransferee was removed or demoted, abort the leadership transfer.
	// 如果leadTransferee被移除或降级，则中止领导权转移。
	if _, tOK := r.trk.Config.Voters.IDs()[r.leadTransferee]; !tOK && r.leadTransferee != 0 {
		r.abortLeaderTransfer()
	}

	return cs
}

func (r *raft) loadState(state pb.HardState) {
	if state.Commit < r.raftLog.committed || state.Commit > r.raftLog.lastIndex() {
		r.logger.Panicf("%x state.commit %d is out of range [%d, %d]", r.id, state.Commit, r.raftLog.committed, r.raftLog.lastIndex())
	}
	r.raftLog.committed = state.Commit
	r.Term = state.Term
	r.Vote = state.Vote
}

// pastElectionTimeout returns true if r.electionElapsed is greater
// than or equal to the randomized election timeout in
// [electiontimeout, 2 * electiontimeout - 1].
func (r *raft) pastElectionTimeout() bool {
	return r.electionElapsed >= r.randomizedElectionTimeout
}

func (r *raft) resetRandomizedElectionTimeout() {
	r.randomizedElectionTimeout = r.electionTimeout + globalRand.Intn(r.electionTimeout)
}

func (r *raft) sendTimeoutNow(to uint64) {
	r.send(pb.Message{To: to, Type: pb.MsgTimeoutNow})
}

func (r *raft) abortLeaderTransfer() {
	r.leadTransferee = None
}

// committedEntryInCurrentTerm return true if the peer has committed an entry in its term.
// 在当前任期内，如果节点已经提交了一个entry，则返回true。
func (r *raft) committedEntryInCurrentTerm() bool {
	// NB: r.Term is never 0 on a leader, so if zeroTermOnOutOfBounds returns 0,
	// we won't see it as a match with r.Term.
	// 注意：在领导者上，r.Term永远不会为0，因此如果zeroTermOnOutOfBounds返回0，
	// 我们将不会将其视为与r.Term匹配。
	return r.raftLog.zeroTermOnOutOfBounds(r.raftLog.term(r.raftLog.committed)) == r.Term
}

// responseToReadIndexReq constructs a response for `req`. If `req` comes from the peer
// itself, a blank value will be returned.
// responseToReadIndexReq为`req`构造一个响应。如果`req`来自peer自己，则返回一个空值。
func (r *raft) responseToReadIndexReq(req pb.Message, readIndex uint64) pb.Message {
	if req.From == None || req.From == r.id {
		r.readStates = append(r.readStates, ReadState{
			Index:      readIndex,
			RequestCtx: req.Entries[0].Data,
		})
		return pb.Message{}
	}
	return pb.Message{
		Type:    pb.MsgReadIndexResp,
		To:      req.From,
		Index:   readIndex,
		Entries: req.Entries,
	}
}

// increaseUncommittedSize computes the size of the proposed entries and
// determines whether they would push leader over its maxUncommittedSize limit.
// If the new entries would exceed the limit, the method returns false. If not,
// the increase in uncommitted entry size is recorded and the method returns
// true.
// increaseUncommittedSize计算提议的entries的大小，并确定它们是否会将leader推到其maxUncommittedSize限制之上。
// 如果新的entries将超过限制，则该方法返回false。如果没有，则记录本次未提交的entry大小的增加结果，并返回true。
//
// Empty payloads are never refused. This is used both for appending an empty
// entry at a new leader's term, as well as leaving a joint configuration.
func (r *raft) increaseUncommittedSize(ents []pb.Entry) bool {
	s := payloadsSize(ents)
	if r.uncommittedSize > 0 && s > 0 && r.uncommittedSize+s > r.maxUncommittedSize {
		// If the uncommitted tail of the Raft log is empty, allow any size
		// proposal. Otherwise, limit the size of the uncommitted tail of the
		// log and drop any proposal that would push the size over the limit.
		// Note the added requirement s>0 which is used to make sure that
		// appending single empty entries to the log always succeeds, used both
		// for replicating a new leader's initial empty entry, and for
		// auto-leaving joint configurations.
		// 如果Raft日志的未提交尾部为空，则允许任何大小的提议。否则，限制日志的未提交尾部的大小，
		// 并丢弃任何会将大小推到限制之上的提议。注意增加的要求是s>0，这用于确保将单个空条目附加到日志总是成功的，
		// 该机制用于复制新领导者的初始空条目，以及自动离开联合配置。
		return false
	}
	r.uncommittedSize += s
	return true
}

// reduceUncommittedSize accounts for the newly committed entries by decreasing
// the uncommitted entry size limit.
// reduceUncommittedSize通过减少未提交的entry大小限制来记录新提交的entries。
func (r *raft) reduceUncommittedSize(s entryPayloadSize) {
	if s > r.uncommittedSize {
		// uncommittedSize may underestimate the size of the uncommitted Raft
		// log tail but will never overestimate it. Saturate at 0 instead of
		// allowing overflow.
		// uncommittedSize可能低估未提交的Raft日志尾部的大小，但永远不会高估它。饱和到0，而不是允许溢出。
		r.uncommittedSize = 0
	} else {
		r.uncommittedSize -= s
	}
}

func releasePendingReadIndexMessages(r *raft) {
	if len(r.pendingReadIndexMessages) == 0 {
		// Fast path for the common case to avoid a call to storage.LastIndex()
		// via committedEntryInCurrentTerm.
		// 避免通过committedEntryInCurrentTerm调用storage.LastIndex()的快速路径。
		return
	}
	if !r.committedEntryInCurrentTerm() {
		// 这意味该节点在当前任期内还没有提交任何entry，所以不会有任何读索引请求被处理。
		r.logger.Error("pending MsgReadIndex should be released only after first commit in current term")
		return
	}

	msgs := r.pendingReadIndexMessages
	r.pendingReadIndexMessages = nil

	for _, m := range msgs {
		sendMsgReadIndexResponse(r, m)
	}

}
func sendMsgReadIndexResponse(r *raft, m pb.Message) {
	// thinking: use an internally defined context instead of the user given context.
	// We can express this in terms of the term and index instead of a user-supplied value.
	// This would allow multiple reads to piggyback on the same message.
	// 思考：使用内部定义的上下文，而不是用户给定的上下文。
	// 我们可以根据任期和索引来表达这一点，而不是用户提供的值。
	// 这将允许多个读取附加在同一消息上。
	switch r.readOnly.option {
	// If more than the local vote is needed, go through a full broadcast.
	// 如果需要的不仅仅是本地的投票，那么就需要进行全广播。
	case ReadOnlySafe:
		r.readOnly.addRequest(r.raftLog.committed, m)
		// The local node automatically acks the request.
		// 本地节点自动确认请求。
		r.readOnly.recvAck(r.id, m.Entries[0].Data)
		r.bcastHeartbeatWithCtx(m.Entries[0].Data)
	case ReadOnlyLeaseBased:
		if resp := r.responseToReadIndexReq(m, r.raftLog.committed); resp.To != None {
			r.send(resp)
		}
	}
}
