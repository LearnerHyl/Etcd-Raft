# ETCD-Raft源码阅读大纲
首先，Node接口是raft层提供给应用程序的交互接口，应用程序通过调用Node提供的相关API使用Raft库，如propose请求，接收raft状态机已经提交的日志等等交互都是首先通过Node接口。

RawNode其实本质上可以归到Raft结构体中，但是为了使代码可读性更高，就将RawNode单独拎出来了，RawNode内部有一个raft对象，还有关于hardState和softState相关定义。

RawNode负责检查raft当前是否有新的状态、待持久化的日志、待应用的日志，若有，则打包到一个Ready中，Node会定期检查rawNode是否有这些内容，若有Node会将新的Ready传到应用层，当应用层应用完毕后，会相关API告知Node，之后Node会告知RawNode，从而更新状态并开始准备下一个Ready。

给代码加了一些注释；另外，
读完后，相关笔记会上传到个人主页。
## 1. 日志相关部分
阅读了Storage、unstable、raftLog这些与日志存储相关的结构体代码；一个store节点包含了上层状态机和下层raftNode，
而raftNode内部又包含了RawNode以及下面的Raft算法库。
## 2.MessageType阅读
Etcd-Raft中包含了多达24中消息，每一种消息负责处理一种行为，基本上把这些消息类型读完之后，大体上应该就对Etcd-Raft库中的设计有一定的概念了。
将消息类型阅读完毕。
## 3.集群成员变更
Etcd-Raft的ConfChangeV2类型同时支持one at a time和jointConsensus类型的变更，其实JointConsensus本质上包含了若干个的Single类型的变更(Add、Remove、Update、AddLearner)。
- one at a time:这种方式一次只允许影响一个Voter。
- JointConsensus：本质上是由若干个one at a time变更组成的，因此一次可以影响多个Voter。

注意，ETCD-Raft库规定Learners和Voter是不能相交的，这在一定程度上可以简化设计，具体可以看集群成员变更之中的源码。

重点介绍JointConsensus：本质上是获取Cold的一个副本，在其基础上应用一系列变更，让Cold和Cnew同时存在，即中间状态，完成后需要退出JointConfig状态。
## 4.ReadIndex优化
层的状态机有关。

即使Raft算法本身保证了其日志的故障容错有序共识，但是在通过Raft算法实现系统时，仍会存在有关消息服务质量（Quality of Service，QoS；如至多一次、至少一次、恰好一次等语义问题）。如果状态机模块什么都不做，那么Raft算法会提供至少一次语义，即一个命令不会丢失，但是可能会多次提交或应用，因此上层状态机必须做点什么来确保Linearizability语义。

此外，这同样需要客户端的配合，客户端也需要做一些事情，从而确保状态机可以即时的丢弃一些无用的信息，可以更好的提供服务。

我们的重点是在raft算法中应该做什么来实现ReadIndex优化。Etcd-raft中提供了ReadIndex和LeadRead两种方法，下面简要说明。

## 4.1 状态机/客户端的工作（自raft论文）

为了在 Raft 中实现线性可读写，服务器必须过滤掉重复的请求。基本思想是服务器保存客户端操作的结果，并使用它们来跳过多次执行相同请求。为了实现这一点，给每个客户端分配一个唯一标识符，并让客户端为每个命令分配唯一的序列号。每个服务器的状态机为每个客户端维护一个会话。该会话跟踪对于特定客户端而言已处理的最新序列号，以及相关的响应。如果服务器收到一个序列号已经执行过的命令，它会立即响应，而不会重新执行请求。

在对重复请求进行过滤的情况下，Raft 提供了线性可读写。Raft 日志提供了一个命令在每个服务器上应用的串行顺序。根据命令首次出现在 Raft 日志中的时间，命令根据其首次出现在 Raft 日志中的时间立即且仅生效一次，因为如上所述，任何在后续出现的重复命令都会被状态机过滤掉。

> 这就意味着每个不同的proposal可以被commit多次，在log中出现多次，但永远只会被apply一次。

这种方法还推广到允许来自单个客户端的并发请求**。客户端的会话不仅跟踪客户端的最新序列号和响应，还包括一组序列号和响应对。**在每个请求中，客户端包含它尚未收到响应的最低序列号，然后状态机丢弃所有更低的序列号（相比于客户端维护的尚未收到响应的最低序列号更低）的响应。

## 4.2 ReadIndex

显然，只读请求并没有需要写入的数据，因此并不需要将其写入Raft日志，而只需要关注收到请求时leader的*commit index*。只要在该*commit index*被应用到状态机后执行读操作，就能保证其线性一致性。因此使用了**ReadIndex**的leader在收到只读请求时，会按如下方式处理：

1. 记录当前的*commit index*，作为*read index*。
2. 向集群中的所有节点广播一次心跳，如果收到了数量达到quorum的心跳响应，leader可以得知当收到该只读请求时，其一定是集群的合法leader。
3. 继续执行，直到leader本地的*apply index*大于等于之前记录的*read index*。此时可以保证只读操作的线性一致性。
4. 让状态机执行只读操作，并将结果返回给客户端。

可以看出，**ReadIndex**的方法只需要一轮心跳广播，既不需要落盘，且其网络开销也很小。**ReadIndex**方法对吞吐量的提升十分显著，但由于其仍需要一轮心跳广播，其对延迟的优化并不明显。

需要注意的是，实现**ReadIndex**时需要注意一个特殊情况。当新leader刚刚当选时，其*commit index*可能并不是此时集群的*commit index*。因此，需要等到新leader至少提交了一条日志时，才能保证其*commit index*能反映集群此时的*commit index*。幸运的是，新leader当选时为了提交非本term的日志，会提交一条空日志。因此，leader只需要等待该日志提交就能开始提供**ReadIndex**服务，而无需再提交额外的空日志。

通过**ReadIndex**机制，还能实现*follower read*。当follower收到只读请求后，可以给leader发送一条获取*read index*的消息，当leader通过心跳广播确认自己是合法的leader后，将其记录的*read index*返回给follower，follower等到自己的*apply index*大于等于其收到的*read index*后，即可以安全地提供满足线性一致性的只读服务。

## 4.3 Lease Read

**ReadIndex**虽然提升了只读请求的吞吐量，但是由于其还需要一轮心跳广播，因此只读请求延迟的优化并不明显。**而Lease Read在损失了一定的安全性的前提下，进一步地优化了延迟。**

**Lease Read**同样是为了确认当前的leader为合法的leader，但是这种方法中，leader是通过心跳与时钟来检查自身合法性的。leader并不会专门的为ReadIndex请求向集群中广播一次心跳。

当leader的*heartbeat timeout*超时时，其需要向所有节点广播心跳消息。设心跳广播前的时间戳为start，当leader收到了至少quorum数量的节点的响应时，该leader可以认为其lease的有效期为[start,start+election timeout/clock drift bound]。
$$
LeaseInvalidTime = [start, start+election timeout/clock drift bound]
$$
因为如果在start时发送的心跳获得了至少quorum数量节点的响应，那么至少要在*election timeout*后，集群才会选举出新的leader。但是，由于不同节点的cpu时钟可能有不同程度的漂移，这会导致在一个很小的时间窗口内，即使leader认为其持有lease，但集群已经选举出了新的leader。这与Raft选举优化*Leader Lease*存在同样的问题。因此，一些系统在实现**Lease Read**时缩小了leader持有lease的时间，选择了一个略小于*election timeout*的时间，以减小时钟漂移带来的影响。

当leader持有lease时，leader认为此时其为合法的leader，因此可以直接将其*commit index*作为*read index*。后续的处理流程与**ReadIndex**相同。

需要注意的是，与**Leader Lease**相同，**Lease Read**机制同样需要在选举时开启**Check Quorum**机制。其原因与**Leader Lease**相同，详见[深入浅出etcd/raft —— 0x03 Raft选举 - 叉鸽 MrCroxx 的博客](http://blog.mrcroxx.com/posts/code-reading/etcdraft-made-simple/3-election/)，这里不再赘述。

> 有些文章中常常将实现线性一致性只读请求优化**Lease Read**机制和选举优化**Leader Lease**混淆。
>
> **Leader Lease**是保证follower在能收到合法的leader的消息时拒绝其它candidate，以避免不必要的选举的机制。
>
> **Lease Read**是leader为确认自己是合法leader，以保证只通过leader为只读请求提供服务时，满足线性一致性的机制。
>
## 4.4 总结
本节介绍了etcd/raft中只读请求算法优化与实现。etcd/raft中只读请求优化几乎完全是按照论文实现的。在其它的一些基于Raft算法的系统中，其实现的方式可能稍有不同，如：不是通过**Check Quorum**实现leader的lease，而是通过日志复制消息为lease续约，且lease的时间也小于*election timeout*，以减小时钟漂移对一致性的影响。