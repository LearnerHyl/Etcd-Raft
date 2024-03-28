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

package confchange

import (
	"errors"
	"fmt"
	"strings"

	"go.etcd.io/raft/v3/quorum"
	pb "go.etcd.io/raft/v3/raftpb"
	"go.etcd.io/raft/v3/tracker"
)

// Changer facilitates configuration changes. It exposes methods to handle
// simple and joint consensus while performing the proper validation that allows
// refusing invalid configuration changes before they affect the active
// configuration.
// Changer简化了配置更改。它公开了处理简单和联合共识的方法，同时执行适当的验证，
// 在无效配置更改影响当前活动配置之前拒绝它们。
type Changer struct {
	// 当前raft节点的tracker.ProgressTracker
	Tracker tracker.ProgressTracker
	// 当前raft节点日志的lastIndex
	LastIndex uint64
}

// EnterJoint verifies that the outgoing (=right) majority config of the joint
// config is empty and initializes it with a copy of the incoming (=left)
// majority config. That is, it transitions from
// EnterJoint验证联合配置的右侧（=right）大多数配置是否为空，并使用传入（=left）大多数配置的副本进行初始化。
// 即，它从
//
//	(1 2 3)&&()
//
// to
//
//	(1 2 3)&&(1 2 3).
//
// The supplied changes are then applied to the incoming majority config,
// resulting in a joint configuration that in terms of the Raft thesis[1]
// (Section 4.3) corresponds to `C_{new,old}`.
// 然后，将提供的更改应用于传入的大多数配置，从而产生一个联合配置，
// 该配置在Raft论文[1]（第4.3节）中对应于`C_{new,old}`。
//
// [1]: https://github.com/ongardie/dissertation/blob/master/online-trim.pdf
func (c Changer) EnterJoint(autoLeave bool, ccs ...pb.ConfChangeSingle) (tracker.Config, tracker.ProgressMap, error) {
	// 检查并复制当前配置和进度
	// incoming配置是在outgoing配置的基础上进行变更的
	cfg, trk, err := c.checkAndCopy()
	if err != nil {
		return c.err(err)
	}
	// 如果当前配置是联合配置，返回错误
	// 我们不能反复进入联合配置
	if joint(cfg) {
		err := errors.New("config is already joint")
		return c.err(err)
	}
	// 若即将切换到的新配置为空，则返回错误
	// 我们不允许Joint Consensus中的新配置为空
	if len(incoming(cfg.Voters)) == 0 {
		// We allow adding nodes to an empty config for convenience (testing and
		// bootstrap), but you can't enter a joint state.
		// 为了方便（测试和引导），我们允许将节点添加到空配置中，但不能进入联合状态。
		err := errors.New("can't make a zero-voter config joint")
		return c.err(err)
	}
	// Clear the outgoing config.
	// 清除old配置
	*outgoingPtr(&cfg.Voters) = quorum.MajorityConfig{}
	// Copy incoming to outgoing.
	// 将incoming配置复制到outgoing配置
	for id := range incoming(cfg.Voters) {
		outgoing(cfg.Voters)[id] = struct{}{}
	}
	// 应用配置变更，将changes应用到incoming配置
	if err := c.apply(&cfg, trk, ccs...); err != nil {
		return c.err(err)
	}
	cfg.AutoLeave = autoLeave
	// 检查当前JointConfig中的配置信息是否合法，是否与tracker中的信息相符
	return checkAndReturn(cfg, trk)
}

// LeaveJoint transitions out of a joint configuration. It is an error to call
// this method if the configuration is not joint, i.e. if the outgoing majority
// config Voters[1] is empty.
//
// The outgoing majority config of the joint configuration will be removed,
// that is, the incoming config is promoted as the sole decision maker. In the
// notation of the Raft thesis[1] (Section 4.3), this method transitions from
// `C_{new,old}` into `C_new`.
//
// At the same time, any staged learners (LearnersNext) the addition of which
// was held back by an overlapping voter in the former outgoing config will be
// inserted into Learners.
//
// [1]: https://github.com/ongardie/dissertation/blob/master/online-trim.pdf
func (c Changer) LeaveJoint() (tracker.Config, tracker.ProgressMap, error) {
	cfg, trk, err := c.checkAndCopy()
	if err != nil {
		return c.err(err)
	}
	if !joint(cfg) {
		err := errors.New("can't leave a non-joint config")
		return c.err(err)
	}
	for id := range cfg.LearnersNext {
		nilAwareAdd(&cfg.Learners, id)
		trk[id].IsLearner = true
	}
	cfg.LearnersNext = nil

	for id := range outgoing(cfg.Voters) {
		_, isVoter := incoming(cfg.Voters)[id]
		_, isLearner := cfg.Learners[id]

		if !isVoter && !isLearner {
			delete(trk, id)
		}
	}
	*outgoingPtr(&cfg.Voters) = nil
	cfg.AutoLeave = false

	return checkAndReturn(cfg, trk)
}

// Simple carries out a series of configuration changes that (in aggregate)
// mutates the incoming majority config Voters[0] by at most one. This method
// will return an error if that is not the case, if the resulting quorum is
// zero, or if the configuration is in a joint state (i.e. if there is an
// outgoing configuration).
// Simple执行一系列配置更改，这些更改（总体上）最多会改变incoming大多数配置Voters[0]中的一个成员。
// 如果不是这种情况，如果结果的多数派为零，或者配置处于联合状态（即存在outgoing配置），该方法将返回错误。
func (c Changer) Simple(ccs ...pb.ConfChangeSingle) (tracker.Config, tracker.ProgressMap, error) {
	// 检查并复制当前配置和进度
	cfg, trk, err := c.checkAndCopy()
	if err != nil {
		return c.err(err)
	}
	// 若当前配置是联合配置，则返回错误
	// 这种方法只能用于非联合配置
	if joint(cfg) {
		err := errors.New("can't apply simple config change in joint config")
		return c.err(err)
	}
	// 应用配置变更，将changes应用到incoming配置
	// 一般changes的数量不会超过1
	if err := c.apply(&cfg, trk, ccs...); err != nil {
		return c.err(err)
	}
	// 这种配置变更方法最多允许一个节点的变更
	// 若变更的节点数量超过1，则返回错误，应该调用JointConsensus方法
	if n := symdiff(incoming(c.Tracker.Voters), incoming(cfg.Voters)); n > 1 {
		return tracker.Config{}, nil, errors.New("more than one voter changed without entering joint config")
	}

	return checkAndReturn(cfg, trk)
}

// apply a change to the configuration. By convention, changes to voters are
// always made to the incoming majority config Voters[0]. Voters[1] is either
// empty or preserves the outgoing majority configuration while in a joint state.
// 应用一个change到配置。按照惯例，对投票者的更改总是应用到incoming大多数配置Voters[0]。
// 在联合状态下，Voters[1]要么为空，要么保留了outgoing状态下的大多数配置。
func (c Changer) apply(cfg *tracker.Config, trk tracker.ProgressMap, ccs ...pb.ConfChangeSingle) error {
	for _, cc := range ccs { // 根据ConfChangeSingle的数量，循环执行
		if cc.NodeID == 0 {
			// etcd replaces the NodeID with zero if it decides (downstream of
			// raft) to not apply a change, so we have to have explicit code
			// here to ignore these.
			// 如果etcd决定（raft的下游）不应用更改，则将NodeID替换为零，
			// 因此我们必须在此处有明确的代码来忽略这些更改。
			// 注意这个决定只能在etcd内部做，raft库不会做这个决定
			continue
		}
		// 根据cc.Type的不同，执行不同的操作
		switch cc.Type {
		case pb.ConfChangeAddNode:
			c.makeVoter(cfg, trk, cc.NodeID)
		case pb.ConfChangeAddLearnerNode:
			c.makeLearner(cfg, trk, cc.NodeID)
		case pb.ConfChangeRemoveNode:
			c.remove(cfg, trk, cc.NodeID)
		case pb.ConfChangeUpdateNode:
		default:
			return fmt.Errorf("unexpected conf type %d", cc.Type)
		}
	}
	// 配置更改完毕后，若新配置中没有投票者，则返回错误
	if len(incoming(cfg.Voters)) == 0 {
		return errors.New("removed all voters")
	}
	return nil
}

// makeVoter adds or promotes the given ID to be a voter in the incoming
// majority config.
// makeVoter将给定的ID添加或提升为incoming大多数配置中的投票者。
func (c Changer) makeVoter(cfg *tracker.Config, trk tracker.ProgressMap, id uint64) {
	pr := trk[id]
	if pr == nil {
		c.initProgress(cfg, trk, id, false /* isLearner */)
		return
	}

	pr.IsLearner = false
	nilAwareDelete(&cfg.Learners, id)
	nilAwareDelete(&cfg.LearnersNext, id)
	incoming(cfg.Voters)[id] = struct{}{}
}

// makeLearner makes the given ID a learner or stages it to be a learner once
// an active joint configuration is exited.
// makeLearner使给定的ID成为学习者，或者一旦退出活动联合配置，就将其设置为学习者。
//
// The former happens when the peer is not a part of the outgoing config, in
// which case we either add a new learner or demote a voter in the incoming
// config.
// 前者发生在该peer不是outgoing配置的一部分时，此时我们要么添加一个新的学习者，要么降级incoming配置中的投票者。
//
// The latter case occurs when the configuration is joint and the peer is a
// voter in the outgoing config. In that case, we do not want to add the peer
// as a learner because then we'd have to track a peer as a voter and learner
// simultaneously. Instead, we add the learner to LearnersNext, so that it will
// be added to Learners the moment the outgoing config is removed by
// LeaveJoint().
// 后一种情况发生在配置是联合的且peer是outgoing配置中的投票者时。在这种情况下，我们不希望将peer添加为学习者，
// 因为那样我们将不得不同时跟踪peer作为投票者和学习者。相反，我们将学习者添加到LearnersNext，
// 这样一旦LeaveJoint()删除outgoing配置，它将被添加到Learners中。
func (c Changer) makeLearner(cfg *tracker.Config, trk tracker.ProgressMap, id uint64) {
	pr := trk[id]
	// 若该id不在outgoing配置中，则直接添加为learner
	if pr == nil {
		c.initProgress(cfg, trk, id, true /* isLearner */)
		return
	}
	if pr.IsLearner {
		return
	}
	// Remove any existing voter in the incoming config...
	c.remove(cfg, trk, id)
	// ... but save the Progress.
	trk[id] = pr
	// Use LearnersNext if we can't add the learner to Learners directly, i.e.
	// if the peer is still tracked as a voter in the outgoing config. It will
	// be turned into a learner in LeaveJoint().
	//
	// Otherwise, add a regular learner right away.
	if _, onRight := outgoing(cfg.Voters)[id]; onRight {
		nilAwareAdd(&cfg.LearnersNext, id)
	} else {
		pr.IsLearner = true
		nilAwareAdd(&cfg.Learners, id)
	}
}

// remove this peer as a voter or learner from the incoming config.
// 从incoming配置中删除此peer，该peer是投票者或学习者。
func (c Changer) remove(cfg *tracker.Config, trk tracker.ProgressMap, id uint64) {
	if _, ok := trk[id]; !ok {
		return
	}

	// 首先一定要把peer从voters中删除
	delete(incoming(cfg.Voters), id)
	nilAwareDelete(&cfg.Learners, id)
	nilAwareDelete(&cfg.LearnersNext, id)

	// If the peer is still a voter in the outgoing config, keep the Progress.
	// 如果peer仍然是outgoing配置中的投票者，则保留该peer的Progress。
	if _, onRight := outgoing(cfg.Voters)[id]; !onRight {
		delete(trk, id)
	}
}

// initProgress initializes a new progress for the given node or learner.
// initProgress为给定的节点或学习者初始化一个新的progress。
func (c Changer) initProgress(cfg *tracker.Config, trk tracker.ProgressMap, id uint64, isLearner bool) {
	if !isLearner {
		incoming(cfg.Voters)[id] = struct{}{}
	} else {
		nilAwareAdd(&cfg.Learners, id)
	}
	trk[id] = &tracker.Progress{
		// Initializing the Progress with the last index means that the follower
		// can be probed (with the last index).
		// 使用last index初始化Progress意味着可以对跟随者进行探测（使用last index）。
		//
		// TODO(tbg): seems awfully optimistic. Using the first index would be
		// better. The general expectation here is that the follower has no log
		// at all (and will thus likely need a snapshot), though the app may
		// have applied a snapshot out of band before adding the replica (thus
		// making the first index the better choice).
		// TODO(tbg): 似乎非常乐观。使用第一个index会更好。这里的一般期望是跟随者根本没有日志
		// （因此可能需要快照），尽管应用程序可能在添加副本之前通过带外方式应用了快照（因此使第一个index成为更好的选择）。
		Match:     0,
		Next:      max(c.LastIndex, 1), // invariant: Match < Next
		Inflights: tracker.NewInflights(c.Tracker.MaxInflight, c.Tracker.MaxInflightBytes),
		IsLearner: isLearner,
		// When a node is first added, we should mark it as recently active.
		// Otherwise, CheckQuorum may cause us to step down if it is invoked
		// before the added node has had a chance to communicate with us.
		// 当首次添加节点时，我们应该将其标记为最近活动的。
		// 否则，如果在新添加的节点有机会与我们通信之前调用CheckQuorum，那么CheckQuorum可能会导致我们下台。
		RecentActive: true,
	}
}

// checkInvariants makes sure that the config and progress are compatible with
// each other. This is used to check both what the Changer is initialized with,
// as well as what it returns.
// checkInvariants确保config和progress彼此兼容。这用于检查Changer初始化的内容以及返回的内容。
func checkInvariants(cfg tracker.Config, trk tracker.ProgressMap) error {
	// NB: intentionally allow the empty config. In production we'll never see a
	// non-empty config (we prevent it from being created) but we will need to
	// be able to *create* an initial config, for example during bootstrap (or
	// during tests). Instead of having to hand-code this, we allow
	// transitioning from an empty config into any other legal and non-empty
	// config.
	// 注意：故意允许空配置。在生产中，我们永远不会看到非空配置（我们会阻止其创建），
	// 但我们需要能够*创建*初始配置，例如在引导期间（或在测试期间）。我们允许从空配置过渡到任何其他合法且非空的配置，
	// 而不是手动编码。
	for _, ids := range []map[uint64]struct{}{
		cfg.Voters.IDs(),
		cfg.Learners,
		cfg.LearnersNext,
	} { // 检查当前配置中的所有类型的节点都否都有自己的Tracker
		for id := range ids {
			if _, ok := trk[id]; !ok {
				return fmt.Errorf("no progress for %d", id)
			}
		}
	}

	// Any staged learner was staged because it could not be directly added due
	// to a conflicting voter in the outgoing config.
	// 任何暂存的学习者都是因为由于outgoing配置中的冲突投票者而无法直接添加而被暂存的。
	for id := range cfg.LearnersNext {
		// LearnNext中出现的节点，必须在旧配置中是投票者
		// 因为我们不能直接把旧配置中的投票者变成学习者，所以我们把它们暂存到LearnNext中
		// 一旦旧配置被删除，这些节点就会被添加到Learners中
		if _, ok := outgoing(cfg.Voters)[id]; !ok {
			return fmt.Errorf("%d is in LearnersNext, but not Voters[1]", id)
		}
		// 若当前节点已经是Learner，那么它同时出现在LearnNext中明显是错误的
		if trk[id].IsLearner {
			return fmt.Errorf("%d is in LearnersNext, but is already marked as learner", id)
		}
	}
	// Conversely Learners and Voters doesn't intersect at all.
	// 相反，Learners和Voters根本不相交。这是etcd-raft的一个特性，Learners和Voters是两个独立的集合，这简化了很多逻辑
	for id := range cfg.Learners {
		if _, ok := outgoing(cfg.Voters)[id]; ok {
			return fmt.Errorf("%d is in Learners and Voters[1]", id)
		}
		if _, ok := incoming(cfg.Voters)[id]; ok {
			return fmt.Errorf("%d is in Learners and Voters[0]", id)
		}
		if !trk[id].IsLearner {
			return fmt.Errorf("%d is in Learners, but is not marked as learner", id)
		}
	}

	if !joint(cfg) { // 若当前配置不是联合配置
		// We enforce that empty maps are nil instead of zero.
		// 我们强制执行空映射为nil而不是零。
		if outgoing(cfg.Voters) != nil { // 非JointConfig下，旧配置(即cfg.Voters[1])必须为空
			return fmt.Errorf("cfg.Voters[1] must be nil when not joint")
		}
		if cfg.LearnersNext != nil { // 非JointConfig下，LearnersNext必须为空
			return fmt.Errorf("cfg.LearnersNext must be nil when not joint")
		}
		if cfg.AutoLeave { // 非JointConfig下，AutoLeave必须为false
			return fmt.Errorf("AutoLeave must be false when not joint")
		}
	}

	return nil
}

// checkAndCopy copies the tracker's config and progress map (deeply enough for
// the purposes of the Changer) and returns those copies. It returns an error
// if checkInvariants does.
// checkAndCopy复制tracker的config和progress map（对于Changer的目的足够深度）并返回这些副本。
// 如果checkInvariants返回错误，则返回错误。
func (c Changer) checkAndCopy() (tracker.Config, tracker.ProgressMap, error) {
	cfg := c.Tracker.Config.Clone()
	trk := tracker.ProgressMap{}

	for id, pr := range c.Tracker.Progress {
		// A shallow copy is enough because we only mutate the Learner field.
		ppr := *pr
		trk[id] = &ppr
	}
	return checkAndReturn(cfg, trk)
}

// checkAndReturn calls checkInvariants on the input and returns either the
// resulting error or the input.
// checkAndReturn在输入上调用checkInvariants，并返回结果错误或输入。
func checkAndReturn(cfg tracker.Config, trk tracker.ProgressMap) (tracker.Config, tracker.ProgressMap, error) {
	if err := checkInvariants(cfg, trk); err != nil {
		return tracker.Config{}, tracker.ProgressMap{}, err
	}
	return cfg, trk, nil
}

// err returns zero values and an error.
func (c Changer) err(err error) (tracker.Config, tracker.ProgressMap, error) {
	return tracker.Config{}, nil, err
}

// nilAwareAdd populates a map entry, creating the map if necessary.
func nilAwareAdd(m *map[uint64]struct{}, id uint64) {
	if *m == nil {
		*m = map[uint64]struct{}{}
	}
	(*m)[id] = struct{}{}
}

// nilAwareDelete deletes from a map, nil'ing the map itself if it is empty after.
func nilAwareDelete(m *map[uint64]struct{}, id uint64) {
	if *m == nil {
		return
	}
	delete(*m, id)
	if len(*m) == 0 {
		*m = nil
	}
}

// symdiff returns the count of the symmetric difference between the sets of
// uint64s, i.e. len( (l - r) \union (r - l)).
func symdiff(l, r map[uint64]struct{}) int {
	var n int
	pairs := [][2]quorum.MajorityConfig{
		{l, r}, // count elems in l but not in r
		{r, l}, // count elems in r but not in l
	}
	for _, p := range pairs {
		for id := range p[0] {
			if _, ok := p[1][id]; !ok {
				n++
			}
		}
	}
	return n
}

func joint(cfg tracker.Config) bool {
	return len(outgoing(cfg.Voters)) > 0
}

func incoming(voters quorum.JointConfig) quorum.MajorityConfig      { return voters[0] }
func outgoing(voters quorum.JointConfig) quorum.MajorityConfig      { return voters[1] }
func outgoingPtr(voters *quorum.JointConfig) *quorum.MajorityConfig { return &voters[1] }

// Describe prints the type and NodeID of the configuration changes as a
// space-delimited string.
func Describe(ccs ...pb.ConfChangeSingle) string {
	var buf strings.Builder
	for _, cc := range ccs {
		if buf.Len() > 0 {
			buf.WriteByte(' ')
		}
		fmt.Fprintf(&buf, "%s(%d)", cc.Type, cc.NodeID)
	}
	return buf.String()
}
