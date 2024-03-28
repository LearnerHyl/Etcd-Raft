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

package raftpb

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gogo/protobuf/proto"
)

// ConfChangeI abstracts over ConfChangeV2 and (legacy) ConfChange to allow
// treating them in a unified manner.
// ConfChangeI抽象了ConfChangeV2和（传统的）ConfChange，以允许以统一的方式对待它们。
type ConfChangeI interface {
	AsV2() ConfChangeV2
	AsV1() (ConfChange, bool)
}

// MarshalConfChange calls Marshal on the underlying ConfChange or ConfChangeV2
// and returns the result along with the corresponding EntryType.
// MarshalConfChange:在底层ConfChange或ConfChangeV2上的Marshal并返回结果以及相应的EntryType。
func MarshalConfChange(c ConfChangeI) (EntryType, []byte, error) {
	var typ EntryType
	var ccdata []byte
	var err error
	if c == nil {
		// A nil data unmarshals into an empty ConfChangeV2 and has the benefit
		// that appendEntry can never refuse it based on its size (which
		// registers as zero).
		// 一个Nil数据被反序列化为一个空的ConfChangeV2，并且具有一个好处，
		// 即appendEntry永远不会拒绝它，因为它的大小为零。
		typ = EntryConfChangeV2
		ccdata = nil
	} else if ccv1, ok := c.AsV1(); ok {
		typ = EntryConfChange
		ccdata, err = ccv1.Marshal()
	} else {
		ccv2 := c.AsV2()
		typ = EntryConfChangeV2
		ccdata, err = ccv2.Marshal()
	}
	return typ, ccdata, err
}

// AsV2 returns a V2 configuration change carrying out the same operation.
// AsV2返回一个执行相同操作的V2配置更改。
func (c ConfChange) AsV2() ConfChangeV2 {
	return ConfChangeV2{
		Changes: []ConfChangeSingle{{
			Type:   c.Type,
			NodeID: c.NodeID,
		}},
		Context: c.Context,
	}
}

// AsV1 returns the ConfChange and true.
// AsV1返回ConfChange和true。
func (c ConfChange) AsV1() (ConfChange, bool) {
	return c, true
}

// AsV2 is the identity.
// AsV2是身份。
func (c ConfChangeV2) AsV2() ConfChangeV2 { return c }

// AsV1 returns ConfChange{} and false.
// AsV1返回ConfChange{}和false。
func (c ConfChangeV2) AsV1() (ConfChange, bool) { return ConfChange{}, false }

// EnterJoint returns two bools. The second bool is true if and only if this
// config change will use Joint Consensus, which is the case if it contains more
// than one change or if the use of Joint Consensus was requested explicitly.
// The first bool can only be true if second one is, and indicates whether the
// Joint State will be left automatically.
// EnterJoint返回两个布尔值。第二个布尔值仅在此配置更改将使用Joint Consensus时为true，
// 即如果它包含多个更改或者如果显式请求使用联合共识时，它将为true。
// 第一个布尔值只能在第二个布尔值为true时为true，并指示是否将自动离开联合状态。
func (c ConfChangeV2) EnterJoint() (autoLeave bool, ok bool) {
	// NB: in theory, more config changes could qualify for the "simple"
	// protocol but it depends on the config on top of which the changes apply.
	// For example, adding two learners is not OK if both nodes are part of the
	// base config (i.e. two voters are turned into learners in the process of
	// applying the conf change). In practice, these distinctions should not
	// matter, so we keep it simple and use Joint Consensus liberally.
	// 注意：理论上，更多的配置更改可能符合“简单”协议，但这取决于更改应用的配置。
	// 例如，如果两个节点都是基本配置的一部分（即在应用配置更改的过程中，两个投票者被转换为学习者），
	// 则添加两个学习者是不允许的。
	// 在实践中，这些区别不应该有影响，所以我们保持简单，并大量使用联合共识。
	// 因为JointConsensus本质上是包含了多个ConfChangeSingle的ConfChangeV2，每个ConfChangeSingle只能包含一个节点的变更。
	// 我们可以把将两个节点变成learner的操作拆分成两个ConfChangeSingle，这样就可以使用JointConsensus进行配置变更。
	//
	// 当c.Changes的长度大于1时，或者c.Transition不为ConfChangeTransitionAuto时，使用Joint Consensus。
	if c.Transition != ConfChangeTransitionAuto || len(c.Changes) > 1 {
		// Use Joint Consensus.
		var autoLeave bool
		switch c.Transition {
		case ConfChangeTransitionAuto:
			autoLeave = true
		case ConfChangeTransitionJointImplicit:
			autoLeave = true
		case ConfChangeTransitionJointExplicit:
		default:
			panic(fmt.Sprintf("unknown transition: %+v", c))
		}
		return autoLeave, true
	}
	return false, false
}

// LeaveJoint is true if the configuration change leaves a joint configuration.
// This is the case if the ConfChangeV2 is zero, with the possible exception of
// the Context field.
// 如果配置更改离开联合配置(即已经完成了配置更改)，则返回true。
// 除了Context字段外，如果ConfChangeV2为零，则返回true。
func (c ConfChangeV2) LeaveJoint() bool {
	// NB: c is already a copy.
	c.Context = nil
	return proto.Equal(&c, &ConfChangeV2{})
}

// ConfChangesFromString parses a Space-delimited sequence of operations into a
// slice of ConfChangeSingle. The supported operations are:
// - vn: make n a voter,
// - ln: make n a learner,
// - rn: remove n, and
// - un: update n.
// ConfChangesFromString将以空格分隔的操作序列解析为ConfChangeSingle的切片。
// 支持的操作有：
// - vn：使n成为投票者，
// - ln：使n成为学习者，
// - rn：删除n，以及
// - un：更新n。
func ConfChangesFromString(s string) ([]ConfChangeSingle, error) {
	var ccs []ConfChangeSingle
	toks := strings.Split(strings.TrimSpace(s), " ")
	if toks[0] == "" {
		toks = nil
	}
	for _, tok := range toks {
		if len(tok) < 2 {
			return nil, fmt.Errorf("unknown token %s", tok)
		}
		var cc ConfChangeSingle
		switch tok[0] {
		case 'v':
			cc.Type = ConfChangeAddNode
		case 'l':
			cc.Type = ConfChangeAddLearnerNode
		case 'r':
			cc.Type = ConfChangeRemoveNode
		case 'u':
			cc.Type = ConfChangeUpdateNode
		default:
			return nil, fmt.Errorf("unknown input: %s", tok)
		}
		id, err := strconv.ParseUint(tok[1:], 10, 64)
		if err != nil {
			return nil, err
		}
		cc.NodeID = id
		ccs = append(ccs, cc)
	}
	return ccs, nil
}

// ConfChangesToString is the inverse to ConfChangesFromString.
// ConfChangesToString是ConfChangesFromString的逆操作。
func ConfChangesToString(ccs []ConfChangeSingle) string {
	var buf strings.Builder
	for i, cc := range ccs {
		if i > 0 {
			buf.WriteByte(' ')
		}
		switch cc.Type {
		case ConfChangeAddNode:
			buf.WriteByte('v')
		case ConfChangeAddLearnerNode:
			buf.WriteByte('l')
		case ConfChangeRemoveNode:
			buf.WriteByte('r')
		case ConfChangeUpdateNode:
			buf.WriteByte('u')
		default:
			buf.WriteString("unknown")
		}
		fmt.Fprintf(&buf, "%d", cc.NodeID)
	}
	return buf.String()
}
