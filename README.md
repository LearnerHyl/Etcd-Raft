# ETCD-Raft源码阅读大纲
给代码加了一些注释；另外，
读完后，相关笔记会上传到个人主页。
## 1. 日志相关部分
阅读了Storage、unstable、raftLog这些与日志存储相关的结构体代码；一个store节点包含了上层状态机和下层raftNode，
而raftNode内部又包含了RawNode以及下面的Raft算法库。
## 2.MessageType阅读
Etcd-Raft中包含了多达24中消息，每一种消息负责处理一种行为，基本上把这些消息类型读完之后，大体上应该就对Etcd-Raft库中的设计有一定的概念了。

这部分正在做，还没完全读完。。。