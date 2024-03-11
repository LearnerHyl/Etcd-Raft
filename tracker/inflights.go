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

// inflight describes an in-flight MsgApp message.
// inflight描述了一个正在传输中的MsgApp消息。
type inflight struct {
	index uint64 // the index of the last entry inside the message
	bytes uint64 // the total byte size of the entries in the message
}

// Inflights limits the number of MsgApp (represented by the largest index
// contained within) sent to followers but not yet acknowledged by them. Callers
// use Full() to check whether more messages can be sent, call Add() whenever
// they are sending a new append, and release "quota" via FreeLE() whenever an
// ack is received.
// Inflights限制了发送给follower但尚未被follower确认的MsgApp消息的数量（由Msg中包含的日志条目的最大索引表示）。
// 调用者使用Full()来检查是否可以发送更多的消息，每当发送一个新的append请求时，调用Add()，并在收到ack时通过FreeLE()释放“配额”。
type Inflights struct {
	// the starting index in the buffer
	start int

	count int    // number of inflight messages in the buffer
	bytes uint64 // number of inflight bytes

	size     int    // the max number of inflight messages
	maxBytes uint64 // the max total byte size of inflight messages

	// buffer is a ring buffer containing info about all in-flight messages.
	// buffer是一个环形缓冲区，包含了所有正在传输中的消息的信息。
	buffer []inflight
}

// NewInflights sets up an Inflights that allows up to size inflight messages,
// with the total byte size up to maxBytes. If maxBytes is 0 then there is no
// byte size limit. The maxBytes limit is soft, i.e. we accept a single message
// that brings it from size < maxBytes to size >= maxBytes.
func NewInflights(size int, maxBytes uint64) *Inflights {
	return &Inflights{
		size:     size,
		maxBytes: maxBytes,
	}
}

// Clone returns an *Inflights that is identical to but shares no memory with
// the receiver.
func (in *Inflights) Clone() *Inflights {
	ins := *in
	ins.buffer = append([]inflight(nil), in.buffer...)
	return &ins
}

// Add notifies the Inflights that a new message with the given index and byte
// size is being dispatched. Full() must be called prior to Add() to verify that
// there is room for one more message, and consecutive calls to Add() must
// provide a monotonic sequence of indexes.
// Add通知Inflights正在发送一个新的消息，该消息的索引和字节大小分别为index和bytes。
// 在调用Add()之前必须调用Full()来验证是否有足够的空间来存储一个新的消息，并且连续调用Add()必须提供一个单调递增的索引序列。
func (in *Inflights) Add(index, bytes uint64) {
	if in.Full() {
		panic("cannot add into a Full inflights")
	}
	next := in.start + in.count
	size := in.size
	if next >= size {
		next -= size
	}
	if next >= len(in.buffer) {
		in.grow()
	}
	in.buffer[next] = inflight{index: index, bytes: bytes}
	in.count++
	in.bytes += bytes
}

// grow the inflight buffer by doubling up to inflights.size. We grow on demand
// instead of preallocating to inflights.size to handle systems which have
// thousands of Raft groups per process.
func (in *Inflights) grow() {
	newSize := len(in.buffer) * 2
	if newSize == 0 {
		newSize = 1
	} else if newSize > in.size {
		newSize = in.size
	}
	newBuffer := make([]inflight, newSize)
	copy(newBuffer, in.buffer)
	in.buffer = newBuffer
}

// FreeLE frees the inflights smaller or equal to the given `to` flight.
// FreeLE释放小于或等于给定`to`索引值的inflight消息。
func (in *Inflights) FreeLE(to uint64) {
	// 如果当前没有已发送但未收到ack的消息，或者该index小于inflights中的第一个消息的index，
	// 即该index小于inflights中的第一个消息的index，说明该消息已经被收到了，直接返回。
	if in.count == 0 || to < in.buffer[in.start].index {
		// out of the left side of the window
		return
	}

	idx := in.start  // 释放完毕后，idx指向inflights中第一个大于to的消息。
	var i int        // 存储最终要释放的inflight消息的数量
	var bytes uint64 // 待释放的inflight消息的总字节数
	for i = 0; i < in.count; i++ {
		if to < in.buffer[idx].index { // found the first large inflight
			break
		}
		bytes += in.buffer[idx].bytes

		// increase index and maybe rotate
		// 注意buffer是一个环形缓冲区，所以当idx+1大于等于size时，需要将idx减去size。
		size := in.size
		if idx++; idx >= size {
			idx -= size
		}
	}
	// 到此，idx指向MsgApp消息是当前inflights中第一个大于to的消息。
	// free i inflights and set new start index
	in.count -= i
	in.bytes -= bytes
	in.start = idx
	if in.count == 0 {
		// inflights is empty, reset the start index so that we don't grow the
		// buffer unnecessarily.
		// inflights为空，重置start索引，以便我们不必要地增加缓冲区的大小。
		in.start = 0
	}
}

// Full returns true if no more messages can be sent at the moment.
func (in *Inflights) Full() bool {
	return in.count == in.size || (in.maxBytes != 0 && in.bytes >= in.maxBytes)
}

// Count returns the number of inflight messages.
func (in *Inflights) Count() int { return in.count }

// reset frees all inflights.
func (in *Inflights) reset() {
	in.start = 0
	in.count = 0
	in.bytes = 0
}
