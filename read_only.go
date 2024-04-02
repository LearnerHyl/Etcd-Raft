// Copyright 2016 The etcd Authors
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

import pb "go.etcd.io/raft/v3/raftpb"

// ReadState provides state for read only query.
// It's caller's responsibility to call ReadIndex first before getting
// this state from ready, it's also caller's duty to differentiate if this
// state is what it requests through RequestCtx, eg. given a unique id as
// RequestCtx
// ReadState为只读查询提供状态。
// 在从ready中获取此状态之前，调用者有责任首先调用ReadIndex，调用者还有责任区分此状态是否是通过RequestCtx请求的，
// 例如，通过RequestCtx给定唯一ID
type ReadState struct {
	Index      uint64 // 当前raft状态机的提交索引
	RequestCtx []byte // 请求上下文
}

type readIndexStatus struct {
	// req是readOnly结构体中的requestCtx对应的原始只读请求消息
	req pb.Message
	// index是待处理的只读请求的索引，需要等到appliedIndex大于等于index时才能处理
	index uint64
	// NB: this never records 'false', but it's more convenient to use this
	// instead of a map[uint64]struct{} due to the API of quorum.VoteResult. If
	// this becomes performance sensitive enough (doubtful), quorum.VoteResult
	// can change to an API that is closer to that of CommittedIndex.
	//
	// NB: 这里永远不会记录'false'，但是由于quorum.VoteResult的API，使用map[uint64]struct{}比较方便。
	// 如果这变得足够重要（不太可能），quorum.VoteResult可以更改为接近CommittedIndex的API。
	// 只有ReadOnlySafe模式下才会使用这个字段，因为每个ReadIndex请求都需要等待大多数节点的确认
	acks map[uint64]bool
}

// pendingReadIndex和readIndexQueue字段只有在ReadOnlySafe模式下才会使用
// 在ReadOnlySafe状态下，每个ReadIndex请求都需要Leader广播一轮心跳消息，等待大多数节点的确认
type readOnly struct {
	// 处理ReadOnly请求的选项：ReadOnlySafe、ReadOnlyLeaseBased
	option ReadOnlyOption
	// pendingReadIndex记录了所有的只读请求，key是请求的上下文，value是请求的状态
	pendingReadIndex map[string]*readIndexStatus
	// readIndexQueue记录了所有的只读请求的上下文(requestCtx)，按照请求的顺序排列
	readIndexQueue []string
}

func newReadOnly(option ReadOnlyOption) *readOnly {
	return &readOnly{
		option:           option,
		pendingReadIndex: make(map[string]*readIndexStatus),
	}
}

// addRequest adds a read only request into readonly struct.
// `index` is the commit index of the raft state machine when it received
// the read only request.
// `m` is the original read only request message from the local or remote node.
// addRequest将只读请求添加到readonly结构体中。
// `index`是raft状态机在接收到只读请求时,节点当前的commitIndex。
// `m`是来自本地或远程节点的原始只读请求消息。
func (ro *readOnly) addRequest(index uint64, m pb.Message) {
	s := string(m.Entries[0].Data)
	// 如果请求已经存在，则直接返回
	if _, ok := ro.pendingReadIndex[s]; ok {
		return
	}
	ro.pendingReadIndex[s] = &readIndexStatus{index: index, req: m, acks: make(map[uint64]bool)}
	ro.readIndexQueue = append(ro.readIndexQueue, s)
}

// recvAck notifies the readonly struct that the raft state machine received
// an acknowledgment of the heartbeat that attached with the read only request
// context.
// recvAck通知readonly结构体，raft状态机已经收到了附带只读请求上下文的心跳的确认。
// 只有ReadOnlySafe模式下才会使用这个方法，因为每个ReadIndex请求都需要等待大多数节点的确认
func (ro *readOnly) recvAck(id uint64, context []byte) map[uint64]bool {
	rs, ok := ro.pendingReadIndex[string(context)]
	if !ok {
		return nil
	}

	rs.acks[id] = true
	return rs.acks
}

// advance advances the read only request queue kept by the readonly struct.
// It dequeues the requests until it finds the read only request that has
// the same context as the given `m`.
// advance推进readonly结构体保留的只读请求队列。
// 它出队请求，直到找到具有与给定`m`相同的上下文的只读请求。
func (ro *readOnly) advance(m pb.Message) []*readIndexStatus {
	var (
		i     int
		found bool
	)

	ctx := string(m.Context)
	var rss []*readIndexStatus

	for _, okctx := range ro.readIndexQueue {
		i++
		rs, ok := ro.pendingReadIndex[okctx]
		if !ok {
			panic("cannot find corresponding read state from pending map")
		}
		rss = append(rss, rs)
		if okctx == ctx {
			found = true
			break
		}
	}

	if found {
		ro.readIndexQueue = ro.readIndexQueue[i:]
		for _, rs := range rss {
			delete(ro.pendingReadIndex, string(rs.req.Entries[0].Data))
		}
		return rss
	}

	return nil
}

// lastPendingRequestCtx returns the context of the last pending read only
// request in readonly struct.
// lastPendingRequestCtx返回readonly结构体中最后一个待处理的只读请求的上下文。
// Note: 该方法只有在ReadOnlySafe模式下才会使用
// 用来批量处理ReadIndex请求，因为如果队列中靠后的ReadIndex请求已经被处理，那么队列中前面的ReadIndex请求也会被处理
func (ro *readOnly) lastPendingRequestCtx() string {
	if len(ro.readIndexQueue) == 0 {
		return ""
	}
	return ro.readIndexQueue[len(ro.readIndexQueue)-1]
}
