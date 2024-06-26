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

/*
Package raft sends and receives messages in the Protocol Buffer format
defined in the raftpb package.
raft包使用raftpb包中定义的Protocol Buffer格式发送和接收消息。

Raft is a protocol with which a cluster of nodes can maintain a replicated state machine.
The state machine is kept in sync through the use of a replicated log.
For more details on Raft, see "In Search of an Understandable Consensus Algorithm"
(https://raft.github.io/raft.pdf) by Diego Ongaro and John Ousterhout.
Raft是一种协议，通过该协议，节点集群可以维护一个复制的状态机。状态机通过复制日志保持同步。
有关Raft的更多详细信息，请参见Diego Ongaro和John Ousterhout的“In Search of an Understandable Consensus Algorithm”（https://raft.github.io/raft.pdf）。

A simple example application, _raftexample_, is also available to help illustrate
how to use this package in practice:
还提供了一个简单的示例应用程序_raftexample_，以帮助说明如何在实践中使用此包：
https://github.com/etcd-io/etcd/tree/main/contrib/raftexample

# Usage

The primary object in raft is a Node. You either start a Node from scratch
using raft.StartNode or start a Node from some initial state using raft.RestartNode.
raft的主要对象是Node。您可以使用raft.StartNode从头开始启动Node，也可以使用raft.RestartNode从某个初始状态启动Node。

To start a node from scratch:

	storage := raft.NewMemoryStorage()
	c := &Config{
	  ID:              0x01,
	  ElectionTick:    10,
	  HeartbeatTick:   1,
	  Storage:         storage,
	  MaxSizePerMsg:   4096,
	  MaxInflightMsgs: 256,
	}
	n := raft.StartNode(c, []raft.Peer{{ID: 0x02}, {ID: 0x03}})

To restart a node from previous state:

	storage := raft.NewMemoryStorage()

	// recover the in-memory storage from persistent
	// snapshot, state and entries.
	storage.ApplySnapshot(snapshot)
	storage.SetHardState(state)
	storage.Append(entries)

	c := &Config{
	  ID:              0x01,
	  ElectionTick:    10,
	  HeartbeatTick:   1,
	  Storage:         storage,
	  MaxSizePerMsg:   4096,
	  MaxInflightMsgs: 256,
	}

	// restart raft without peer information.
	// peer information is already included in the storage.
	n := raft.RestartNode(c)

Now that you are holding onto a Node you have a few responsibilities:
现在您持有一个Node，您有一些责任：

First, you must read from the Node.Ready() channel and process the updates
it contains. These steps may be performed in parallel, except as noted in step
2.
首先，您必须从Node.Ready()通道中读取并处理它包含的更新。这些步骤可以并行执行，除非在步骤2中另有说明。

1. Write HardState, Entries, and Snapshot to persistent storage if they are
not empty. Note that when writing an Entry with Index i, any
previously-persisted entries with Index >= i must be discarded.
如果HardState、Entries和Snapshot不为空，则将它们写入持久存储。请注意，当写入索引为i的条目时，
必须丢弃任何索引大于或等于i的先前持久化的条目。

2. Send all Messages to the nodes named in the To field. It is important that
no messages be sent until the latest HardState has been persisted to disk,
and all Entries written by any previous Ready batch (Messages may be sent while
entries from the same batch are being persisted). To reduce the I/O latency, an
optimization can be applied to make leader write to disk in parallel with its
followers (as explained at section 10.2.1 in Raft thesis). If any Message has type
MsgSnap, call Node.ReportSnapshot() after it has been sent (these messages may be
large).
将所有消息发送到在To字段中指定的节点。重要的是，在最新的 **HardState** 被持久化到磁盘之前，
不要发送任何消息，并且所有由之前的 **Ready** 批次写入的条目都已经被持久化（尽管来自同一批次的条目正在被持久化，
但仍然可以发送消息）。为了减少 I/O 延迟，可以应用一种优化，使领导者与其跟随者并行写入磁盘(详见 Raft 论文的
第 10.2.1 节)。如果任何消息的类型是MsgSnap，在发送后调用 `Node.ReportSnapshot()`（这些消息可能很大）。

Note: Marshalling messages is not thread-safe; it is important that you
make sure that no new entries are persisted while marshalling.
The easiest way to achieve this is to serialize the messages directly inside
your main raft loop.
注意：消息的序列化不是线程安全的；重要的是确保在序列化时没有新条目被持久化。
实现这一点的最简单方法是直接在主raft循环中序列化消息。

3. Apply Snapshot (if any) and CommittedEntries to the state machine.
If any committed Entry has Type EntryConfChange, call Node.ApplyConfChange()
to apply it to the node. The configuration change may be cancelled at this point
by setting the NodeID field to zero before calling ApplyConfChange
(but ApplyConfChange must be called one way or the other, and the decision to cancel
must be based solely on the state machine and not external information such as
the observed health of the node).
将快照（如果有）和CommittedEntries应用于状态机。如果任何已提交的条目的类型为EntryConfChange，
则调用Node.ApplyConfChange()将其应用于节点。在调用ApplyConfChange之前，可以通过将NodeID字段设置为零来取消配置更改
（但必须以某种方式调用ApplyConfChange，并且取消决定必须仅基于状态机，而不是外部信息，例如观察到的节点健康状况）。

4. Call Node.Advance() to signal readiness for the next batch of updates.
This may be done at any time after step 1, although all updates must be processed
in the order they were returned by Ready.
调用Node.Advance()以表示准备好接收下一批更新。这可以在步骤1之后的任何时间完成，
尽管所有更新必须按照Ready返回的顺序进行处理。

Second, all persisted log entries must be made available via an
implementation of the Storage interface. The provided MemoryStorage
type can be used for this (if you repopulate its state upon a
restart), or you can supply your own disk-backed implementation.
其次，所有持久化的日志条目必须通过Storage接口的实现可用。
可以使用提供的MemoryStorage类型（如果在重新启动时重新填充其状态），
也可以提供自己的基于磁盘的实现。

Third, when you receive a message from another node, pass it to Node.Step:
当从另一个节点接收到消息时，请将其传递给Node.Step：

	func recvRaftRPC(ctx context.Context, m raftpb.Message) {
		n.Step(ctx, m)
	}

Finally, you need to call Node.Tick() at regular intervals (probably
via a time.Ticker). Raft has two important timeouts: heartbeat and the
election timeout. However, internally to the raft package time is
represented by an abstract "tick".
最后，您需要定期（可能通过time.Ticker）调用Node.Tick()。Raft有两个重要的超时：心跳和选举超时。
但是，在raft包内部，时间由抽象的“tick”表示。

The total state machine handling loop will look something like this:
整个状态机处理的循环将如下所示：

	for {
	  select {
	  case <-s.Ticker:
	    n.Tick()
	  case rd := <-s.Node.Ready():
	    saveToStorage(rd.State, rd.Entries, rd.Snapshot)
	    send(rd.Messages)
	    if !raft.IsEmptySnap(rd.Snapshot) {
	      processSnapshot(rd.Snapshot)
	    }
	    for _, entry := range rd.CommittedEntries {
	      process(entry)
	      if entry.Type == raftpb.EntryConfChange {
	        var cc raftpb.ConfChange
	        cc.Unmarshal(entry.Data)
	        s.Node.ApplyConfChange(cc)
	      }
	    }
	    s.Node.Advance()
	  case <-s.done:
	    return
	  }
	}

To propose changes to the state machine from your node take your application
data, serialize it into a byte slice and call:
要从节点提出对状态机的更改，请获取应用程序数据，将其序列化为字节片，并调用：

	n.Propose(ctx, data)

If the proposal is committed, data will appear in committed entries with type
raftpb.EntryNormal. There is no guarantee that a proposed command will be
committed; you may have to re-propose after a timeout.
如果提案被提交，数据将出现在具有类型raftpb.EntryNormal的已提交条目中。
不能保证提议的命令将被提交；您可能需要在超时后重新提出。

To add or remove a node in a cluster, build ConfChange struct 'cc' and call:
要在集群中添加或删除节点，请构建ConfChange结构'cc'并调用：

	n.ProposeConfChange(ctx, cc)

After config change is committed, some committed entry with type
raftpb.EntryConfChange will be returned. You must apply it to node through:
配置更改提交后，将返回一些具有类型raftpb.EntryConfChange的已提交条目。您必须通过以下方式将其应用于节点：

	var cc raftpb.ConfChange
	cc.Unmarshal(data)
	n.ApplyConfChange(cc)

Note: An ID represents a unique node in a cluster for all time. A
given ID MUST be used only once even if the old node has been removed.
This means that for example IP addresses make poor node IDs since they
may be reused. Node IDs must be non-zero.
注意：ID代表集群中的唯一节点，一直有效。给定的ID只能使用一次，即使旧节点已被删除。这意味着例如IP地址不适合用作节点ID，
因为它们可能被重用。节点ID必须是非零的。

# Usage with Asynchronous Storage Writes

The library can be configured with an alternate interface for local storage
writes that can provide better performance in the presence of high proposal
concurrency by minimizing interference between proposals. This feature is called
AsynchronousStorageWrites, and can be enabled using the flag on the Config
struct with the same name.
该库可以配置用于本地存储写入的替代接口。该接口通过最小化提案之间的干扰，可以在高提案并发性的情况下提供更好的性能。
此功能称为AsynchronousStorageWrites，并可以使用Config结构上的同名标志启用。

When Asynchronous Storage Writes is enabled, the responsibility of code using
the library is different from what was presented above. Users still read from
the Node.Ready() channel. However, they process the updates it contains in a
different manner. Users no longer consult the HardState, Entries, and Snapshot
fields (steps 1 and 3 above). They also no longer call Node.Advance() to
indicate that they have processed all entries in the Ready (step 4 above).
Instead, all local storage operations are also communicated through messages
present in the Ready.Message slice.
启用Asynchronous Storage Writes时，使用库的代码的责任与上面介绍的不同。用户仍然从Node.Ready()通道中读取。
但是，他们以不同的方式处理其中包含的更新。用户不再查看HardState、Entries和Snapshot字段（上面的步骤1和3）。
他们也不再调用Node.Advance()来指示他们已处理Ready中的所有条目（上面的步骤4）。
相反，所有本地存储操作也通过Ready.Message切片中的消息进行通信。

The local storage messages come in two flavors. The first flavor is log append
messages, which target a LocalAppendThread and carry Entries, HardState, and a
Snapshot. The second flavor is entry application messages, which target a
LocalApplyThread and carry CommittedEntries. Messages to the same target must be
reliably processed in order. Messages to different targets can be processed in
any order.
local storage消息有两种类型。第一种类型是日志附加消息，它们针对LocalAppendThread，并携带Entries、HardState和Snapshot。
第二种类型是entry application消息，它们针对LocalApplyThread，并携带CommittedEntries。
发送到相同目标的消息必须按顺序可靠处理。发送到不同目标的消息可以以任何顺序处理。

Each local storage message carries a slice of response messages that must
delivered after the corresponding storage write has been completed. These
responses may target the same node or may target other nodes.
每个local storage消息都携带一组响应消息的切片，这些响应消息必须在相应的storage write完成后传递。
这些响应可能针对同一节点，也可能针对其他节点。

With Asynchronous Storage Writes enabled, the total state machine handling loop
will look something like this:
启用Asynchronous Storage Writes后，整个状态机处理循环将如下所示：

	for {
	  select {
	  case <-s.Ticker:
	    n.Tick()
	  case rd := <-s.Node.Ready():
	    for _, m := range rd.Messages {
	      switch m.To {
	      case raft.LocalAppendThread:
	        toAppend <- m
	      case raft.LocalApplyThread:
	        toApply <-m
	      default:
	        sendOverNetwork(m)
	      }
	    }
	  case <-s.done:
	    return
	  }
	}

Usage of Asynchronous Storage Writes will typically also contain a pair of
storage handler threads, one for log writes (append) and one for entry
application to the local state machine (apply). Those will look something like:
使用Asynchronous Storage Writes通常还包含一对storage handler线程，一个用于日志写入（附加），一个用于将条目应用于本地状态机（应用）。
它们将如下所示：

	// append thread
	go func() {
	  for {
	    select {
	    case m := <-toAppend:
	      saveToStorage(m.State, m.Entries, m.Snapshot)
	      send(m.Responses)
	    case <-s.done:
	      return
	    }
	  }
	}

	// apply thread
	go func() {
	  for {
	    select {
	    case m := <-toApply:
	      for _, entry := range m.CommittedEntries {
	        process(entry)
	        if entry.Type == raftpb.EntryConfChange {
	          var cc raftpb.ConfChange
	          cc.Unmarshal(entry.Data)
	          s.Node.ApplyConfChange(cc)
	        }
	      }
	      send(m.Responses)
	    case <-s.done:
	      return
	    }
	  }
	}

# Implementation notes

This implementation is up to date with the final Raft thesis
(https://github.com/ongardie/dissertation/blob/master/stanford.pdf), although our
implementation of the membership change protocol differs somewhat from
that described in chapter 4. The key invariant that membership changes
happen one node at a time is preserved, but in our implementation the
membership change takes effect when its entry is applied, not when it
is added to the log (so the entry is committed under the old
membership instead of the new). This is equivalent in terms of safety,
since the old and new configurations are guaranteed to overlap.
这个实现是最新的Raft论文(https://github.com/ongardie/dissertation/blob/master/stanford.pdf)，
尽管我们的成员更改协议的实现与第4章中描述的有所不同。成员更改一次只发生一个节点的关键不变性被保留，
但在我们的实现中，成员更改在应用其条目时生效，而不是在将其添加到日志中时（因此该条目在旧成员身份下被提交，而不是新成员身份下被提交）。
就安全性而言，这是等效的，因为保证了旧配置和新配置重叠。

To ensure that we do not attempt to commit two membership changes at
once by matching log positions (which would be unsafe since they
should have different quorum requirements), we simply disallow any
proposed membership change while any uncommitted change appears in
the leader's log.
为了确保我们不会通过匹配日志位置（这是不安全的，因为它们应该具有不同的法定人数要求）同时尝试提交两个成员更改，
当领导者的日志中出现任何未提交的更改时，我们只是禁止任何提议的成员更改。即如果有未提交的更改，则不允许propose新的更改。

This approach introduces a problem when you try to remove a member
from a two-member cluster: If one of the members dies before the
other one receives the commit of the confchange entry, then the member
cannot be removed any more since the cluster cannot make progress.
For this reason it is highly recommended to use three or more nodes in
every cluster.
当您尝试从两个成员的集群中删除一个成员时，这种方法会引入一个问题：如果其中一个成员在另一个成员接收到confchange条目的提交之前死亡，
则该成员将无法再被删除，因为集群无法取得进展。因此，强烈建议在每个集群中使用三个或更多节点。
注意，两个节点的集群的quorum是2，所以如果一个节点挂掉，另一个节点就无法达到quorum，导致集群无法继续工作。

# MessageType

Package raft sends and receives message in Protocol Buffer format (defined
in raftpb package). Each state (follower, candidate, leader) implements its
own 'step' method ('stepFollower', 'stepCandidate', 'stepLeader') when
advancing with the given raftpb.Message. Each step is determined by its
raftpb.MessageType. Note that every step is checked by one common method
'Step' that safety-checks the terms of node and incoming message to prevent
stale log entries:
raft库使用Protocol Buffer格式发送和接收消息（在raftpb包中定义）。
每个状态（follower、candidate、leader）在使用给定的raftpb.Message推进时实现自己的“step”方法（“stepFollower”、“stepCandidate”、“stepLeader”）。每个步骤由其raftpb.MessageType确定。
请注意，每个步骤都由一个公共方法“Step”检查，该方法安全检查节点和传入消息的术语，以防止陈旧的日志条目：

	'MsgHup' is used for election. If a node is a follower or candidate, the
	'tick' function in 'raft' struct is set as 'tickElection'. If a follower or
	candidate has not received any heartbeat before the election timeout, it
	passes 'MsgHup' to its Step method and becomes (or remains) a candidate to
	start a new election.
	`MsgHup`用于选举。如果节点是follower或candidate，则在“raft”结构中将“tick”函数设置为“tickElection”。
	如果follower或candidate在选举超时之前没有收到任何心跳，则将“MsgHup”传递给其Step方法，
	并成为（或保持）候选人以开始新的选举。

	'MsgBeat' is an internal type that signals the leader to send a heartbeat of
	the 'MsgHeartbeat' type. If a node is a leader, the 'tick' function in
	the 'raft' struct is set as 'tickHeartbeat', and triggers the leader to
	send periodic 'MsgHeartbeat' messages to its followers.
	`MsgBeat`是一个内部类型，用于通知领导者发送“MsgHeartbeat”类型的心跳。
	如果节点是领导者，则在“raft”结构中将“tick”函数设置为“tickHeartbeat”，并触发领导者定期向其关注者发送“MsgHeartbeat”消息。

	'MsgProp' proposes to append data to its log entries. This is a special
	type to redirect proposals to leader. Therefore, send method overwrites
	raftpb.Message's term with its HardState's term to avoid attaching its
	local term to 'MsgProp'. When 'MsgProp' is passed to the leader's 'Step'
	method, the leader first calls the 'appendEntry' method to append entries
	to its log, and then calls 'bcastAppend' method to send those entries to
	its peers. When passed to candidate, 'MsgProp' is dropped. When passed to
	follower, 'MsgProp' is stored in follower's mailbox(msgs) by the send
	method. It is stored with sender's ID and later forwarded to leader by
	rafthttp package.
	`MsgProp`建议将数据附加到其日志条目。这是一种特殊类型，用于将提案重定向到领导者。
	因此，send方法使用其HardState的术语覆盖raftpb.Message的术语，以避免将其本地术语附加到“MsgProp”。
	当“MsgProp”传递给领导者的“Step”方法时，领导者首先调用“appendEntry”方法将条目附加到其日志中，
	然后调用“bcastAppend”方法将这些条目发送到其对等方。当传递给候选人时，“MsgProp”被丢弃。
	当传递给follower时，“MsgProp”由send方法存储在follower的邮箱（msgs）中。它与发送者的ID一起存储，
	稍后由rafthttp包转发给leader。

	'MsgApp' contains log entries to replicate. A leader calls bcastAppend,
	which calls sendAppend, which sends soon-to-be-replicated logs in 'MsgApp'
	type. When 'MsgApp' is passed to candidate's Step method, candidate reverts
	back to follower, because it indicates that there is a valid leader sending
	'MsgApp' messages. Candidate and follower respond to this message in
	'MsgAppResp' type.
	`MsgApp`包含要复制的日志条目。领导者调用bcastAppend，该方法调用sendAppend，该方法以“MsgApp”类型发送即将复制的日志。
	当“MsgApp”传递给候选人的Step方法时，候选人会恢复为follower，因为它表示有一个有效的领导者发送“MsgApp”消息。
	候选人和follower以“MsgAppResp”类型响应此消息。

	'MsgAppResp' is response to log replication request('MsgApp'). When
	'MsgApp' is passed to candidate or follower's Step method, it responds by
	calling 'handleAppendEntries' method, which sends 'MsgAppResp' to raft
	mailbox.
	`MsgAppResp`是对日志复制请求（'MsgApp'）的响应。当“MsgApp”传递给候选人或follower的Step方法时，
	它通过调用“handleAppendEntries”方法响应，该方法将“MsgAppResp”发送到raft邮箱(msgs)。

	'MsgVote' requests votes for election. When a node is a follower or
	candidate and 'MsgHup' is passed to its Step method, then the node calls
	'campaign' method to campaign itself to become a leader. Once 'campaign'
	method is called, the node becomes candidate and sends 'MsgVote' to peers
	in cluster to request votes. When passed to leader or candidate's Step
	method and the message's Term is lower than leader's or candidate's,
	'MsgVote' will be rejected ('MsgVoteResp' is returned with Reject true).
	If leader or candidate receives 'MsgVote' with higher term, it will revert
	back to follower. When 'MsgVote' is passed to follower, it votes for the
	sender only when sender's last term is greater than MsgVote's term or
	sender's last term is equal to MsgVote's term but sender's last committed
	index is greater than or equal to follower's.
	`MsgVote`请求选举投票。当节点是follower或candidate，并且将“MsgHup”传递给其Step方法时，
	然后节点调用“campaign”方法来竞选成为领导者。一旦调用了“campaign”方法，节点就会成为候选人，
	并向集群中的对等方发送“MsgVote”以请求投票。当传递给领导者或候选人的Step方法并且消息的Term低于领导者或候选人的Term时，
	“MsgVote”将被拒绝（返回带有Reject true的“MsgVoteResp”）。如果领导者或候选人收到了更高term的“MsgVote”，
	它将恢复为follower。当“MsgVote”传递给follower时，只有当发送者的最后一个term大于MsgVote的term或发送者的最后一个term等于MsgVote的term，
	但发送者的最后一个提交的索引大于或等于follower's时，它才为发送者投票。

	'MsgVoteResp' contains responses from voting request. When 'MsgVoteResp' is
	passed to candidate, the candidate calculates how many votes it has won. If
	it's more than majority (quorum), it becomes leader and calls 'bcastAppend'.
	If candidate receives majority of votes of denials, it reverts back to
	follower.
	`MsgVoteResp`包含投票请求的响应。当`MsgVoteResp`传递给候选人时，候选人计算它赢得了多少票。如果它超过了多数（法定人数），
	它将成为领导者并调用`bcastAppend`。如果候选人收到了否决的多数票，它将恢复为follower。

	'MsgPreVote' and 'MsgPreVoteResp' are used in an optional two-phase election
	protocol. When Config.PreVote is true, a pre-election is carried out first
	(using the same rules as a regular election), and no node increases its term
	number unless the pre-election indicates that the campaigning node would win.
	This minimizes disruption when a partitioned node rejoins the cluster.
	`MsgPreVote`和`MsgPreVoteResp`在可选的两阶段选举协议中使用。当Config.PreVote为true时，
	首先进行预选举（使用与常规选举相同的规则），除非预选举表明竞选节点将获胜，否则没有节点会增加其Term号。
	当分区节点重新加入集群时，这将最小化中断。

	'MsgSnap' requests to install a snapshot message. When a node has just
	become a leader or the leader receives 'MsgProp' message, it calls
	'bcastAppend' method, which then calls 'sendAppend' method to each
	follower. In 'sendAppend', if a leader fails to get term or entries,
	the leader requests snapshot by sending 'MsgSnap' type message.
	`MsgSnap`请求安装快照消息。当节点刚刚成为领导者或领导者收到`MsgProp`消息时，它调用`bcastAppend`方法，
	然后调用`sendAppend`方法到每个follower。在`sendAppend`中，如果领导者未能获取term或条目，
	领导者将通过发送`MsgSnap`类型消息来请求快照。

	'MsgSnapStatus' tells the result of snapshot install message. When a
	follower rejected 'MsgSnap', it indicates the snapshot request with
	'MsgSnap' had failed from network issues which causes the network layer
	to fail to send out snapshots to its followers. Then leader considers
	follower's progress as probe. When 'MsgSnap' were not rejected, it
	indicates that the snapshot succeeded and the leader sets follower's
	progress to probe and resumes its log replication.
	`MsgSnapStatus`告诉快照安装消息的结果。当follower拒绝`MsgSnap`时，它表示
	`MsgSnap`的快照请求由于网络问题而失败，这导致网络层无法将快照发送给其followers。
	然后leader将该follower的状态视作为StateProbe(此时应该没有实际set该follower的state为StateProbe)。
	当`MsgSnap`没有被拒绝时，它表示快照成功，leader将follower的状态设置为StateProbe，并恢复其日志复制。

	'MsgHeartbeat' sends heartbeat from leader. When 'MsgHeartbeat' is passed
	to candidate and message's term is higher than candidate's, the candidate
	reverts back to follower and updates its committed index from the one in
	this heartbeat. And it sends the message to its mailbox. When
	'MsgHeartbeat' is passed to follower's Step method and message's term is
	higher than follower's, the follower updates its leaderID with the ID
	from the message.
	`MsgHeartbeat`从leader发送心跳。当`MsgHeartbeat`传递给候选人并且消息的term高于候选人的term时，
	候选人将恢复为follower，并从此心跳中更新其commitIndex。然后将消息发送到其邮箱(raft.msgs)。
	当`MsgHeartbeat`传递给follower的Step方法并且消息的term高于follower的term时，follower将其leaderID更新为消息中的ID。


	'MsgHeartbeatResp' is a response to 'MsgHeartbeat'. When 'MsgHeartbeatResp'
	is passed to leader's Step method, the leader knows which follower
	responded. And only when the leader's last committed index is greater than
	follower's Match index, the leader runs 'sendAppend` method.
	`MsgHeartbeatResp`是对`MsgHeartbeat`的响应。当`MsgHeartbeatResp`传递给leader的Step方法时，
	leader知道哪个follower做出了响应。只有当leader的最后提交的索引大于follower的Match索引时，
	leader才运行`sendAppend`方法。

	'MsgUnreachable' tells that request(message) wasn't delivered. When
	'MsgUnreachable' is passed to leader's Step method, the leader discovers
	that the follower that sent this 'MsgUnreachable' is not reachable, often
	indicating 'MsgApp' is lost. When follower's progress state is replicate,
	the leader sets it back to probe.
	`MsgUnreachable`表示请求（消息）未被传递。当`MsgUnreachable`传递给leader的Step方法时，
	leader发现发送此`MsgUnreachable`的follower不可达，通常表示`MsgApp`丢失。
	当follower的progress状态为replicate时，leader将其设置回probe。

	'MsgStorageAppend' is a message from a node to its local append storage
	thread to write entries, hard state, and/or a snapshot to stable storage.
	The message will carry one or more responses, one of which will be a
	'MsgStorageAppendResp' back to itself. The responses can also contain
	'MsgAppResp', 'MsgVoteResp', and 'MsgPreVoteResp' messages. Used with
	AsynchronousStorageWrites.
	`MsgStorageAppend`是节点发送给它自己本地的append storage线程的消息，用于将条目、硬状态和/或快照写入稳定存储。
	该消息将携带一个或多个响应，其中一个将是返回给自己的`MsgStorageAppendResp`。
	响应还可以包含`MsgAppResp`、`MsgVoteResp`和`MsgPreVoteResp`消息。用于AsynchronousStorageWrites。


	'MsgStorageApply' is a message from a node to its local apply storage
	thread to apply committed entries. The message will carry one response,
	which will be a 'MsgStorageApplyResp' back to itself. Used with
	AsynchronousStorageWrites.
	`MsgStorageApply`是节点发送给它自己本地的apply storage线程的消息，用于应用已提交的条目。
	该消息将携带一个响应，这将是返回给自己的`MsgStorageApplyResp`。用于AsynchronousStorageWrites。
*/
package raft
