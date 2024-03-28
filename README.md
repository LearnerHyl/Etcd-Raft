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
TODO