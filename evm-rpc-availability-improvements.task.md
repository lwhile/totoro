# EVM RPC 可用性改进任务

## 背景

本项目是一个基于 `go-ethereum/ethclient` 的轻量级 Go 封装。它想解决的核心问题是：免费或公共 EVM RPC 经常失效、不稳定、响应慢、区块高度落后，或者被限流，因此需要通过多个 RPC endpoint 提升整体可用性。

当前实现会在 `NewEthereumClient` 中接收多个 RPC URL，创建一组 `ethclient.Client`，并在以下常见读操作中按顺序尝试这些 RPC：

- `BlockNumber`
- `FilterLogs`
- `TransactionReceipt`
- `BlockByNumber`
- `TransactionByHash`
- `SuggestGasPrice`
- `GetAvailableRPCCli`

它还实现了一个基于轮询的日志订阅流程：

- 后台循环每 30 秒更新一次最新区块高度。
- `Subscribe` 监听区块高度变化。
- 当链上区块推进时，从 `prevBlock - 1` 到 `currBlock` 调用 `FilterLogs`。
- 匹配已配置合约地址和 topic0 的日志会被推送到调用方传入的 channel。

这个方向是合理的：用多个 RPC endpoint 降低对单个不稳定免费 RPC 的依赖。但当前实现只做到了最基础的顺序 fallback。要真正服务于“免费 RPC 不稳定场景下提高可用性”这个目标，还需要补强故障隔离、超时控制、健康状态管理、订阅正确性和并发安全。

## 当前设计缺口

### 必须修复

- [ ] 允许部分 RPC 初始化失败。

  当前 `NewEthereumClient` 在任意一个 RPC 的 `ethclient.DialContext` 失败时就直接返回错误。这和项目的核心可用性目标冲突：一个免费 RPC 坏掉，不应该导致整个客户端不可用。

  期望行为：尽量初始化所有 endpoint，记录失败项；只要至少有一个 endpoint 可用，就返回 client。失败的 endpoint 后续通过后台探活或重试机制恢复。

- [x] 为每个 RPC attempt 增加独立超时。

  `BlockNumber`、`FilterLogs`、`TransactionReceipt` 等方法虽然会顺序尝试多个 endpoint，但每次 attempt 没有短超时保护。如果第一个 RPC 不是立即报错，而是长时间卡住，fallback 就无法及时切换到下一个 endpoint。

  期望行为：每次 endpoint 调用都用可配置的 timeout 包一层，例如默认 2-5 秒。fallback 需要同时处理明确错误和慢响应、挂起的 endpoint。

- [ ] 跟踪 endpoint 健康状态并实现退避。

  当前每个请求都会从第一个 endpoint 开始尝试。已知故障的 endpoint 会被反复尝试，永久故障或长期变慢的第一个 RPC 会拖慢所有调用。

  期望行为：维护每个 endpoint 的状态，例如连续失败次数、最近错误、最近成功时间、延迟、观测到的最新区块高度、下次可重试时间。对不健康 endpoint 做临时跳过，并使用指数退避重试。

- [x] 修复共享可变状态的并发访问问题。

  当前实现中存在多 goroutine 共享访问的字段：

  - `currBlock`
  - `prevBlock`
  - `updateCh`
  - `contracts`
  - `topics`
  - `logger`

  `updateBlockNumLoop`、`Subscribe`、`AddSubscribeContract`、`RemoveSubscribeContract`、`AddSubscribeTopic`、`RemoveSubscribeTopic` 和 `getEthereumQueryFilter` 都可能并发访问这些字段。这会导致 data race；对 map 来说，还可能触发 `concurrent map iteration and map write` 这类运行时 panic。

  期望行为：用 `sync.RWMutex` 保护共享状态，或者把订阅状态收敛到单个 owner goroutine 里，通过 command/channel 更新。

- [ ] 明确并修复订阅区块游标语义。

  `getEthereumQueryFilter` 当前查询 `prevBlock - 1` 到 `currBlock`。这可能是为了有意重叠一个区块，但代码没有按 `(blockHash, txHash, logIndex)` 去重，也没有处理链重组或 finality 延迟。

  期望行为：明确日志投递语义。为了适配公共 RPC 的可靠性问题，建议只处理达到一定确认数的区块，持久或显式维护 cursor，按确定区间查询，并对重叠范围内的日志做去重。

### 应该修复

- [ ] 避免用一个 endpoint 的区块高度去查询另一个落后的 endpoint。

  当前 `currBlock` 可能来自 endpoint A，而 `FilterLogs` 可能在 endpoint B 上执行。如果 B 的区块高度落后于 A，请求可能失败，或者返回不一致结果。

  期望行为：一次轮询周期尽量选定同一个健康 endpoint 完整执行；或者跟踪每个 endpoint 的观测高度，只查询到安全高度，例如 `minHealthyHead - confirmations`。

- [ ] 将大范围 `eth_getLogs` 查询拆分成小块。

  公共 RPC 经常限制 `eth_getLogs` 的区块跨度、返回日志数量或计算成本。如果客户端离线一段时间，`prevBlock` 到 `currBlock` 的范围可能变得很大。

  期望行为：增加可配置的 `MaxLogBlockRange`，按区块范围分片查询日志。每个分片成功后再推进 cursor。

- [ ] 向调用方暴露订阅错误。

  当前 `Subscribe` 只在内部记录错误并继续运行。调用方只能收到日志，无法感知 endpoint 故障、长时间不可用、连续 `getLogs` 失败或 cursor 停滞。

  期望行为：暴露 error channel，返回 subscription 对象，或者让 `Subscribe` 在 context 取消或订阅无法继续推进时返回 error。

- [x] 增加明确的生命周期管理。

  `NewEthereumClient` 会启动后台 goroutine，但 client 没有 `Close`、`Stop` 或 `Wait` 方法，也没有关闭底层 `ethclient.Client` 连接。

  期望行为：client 内部持有 lifecycle context，在 `Close` 中 cancel；停止 ticker；等待 goroutine 退出；关闭所有底层 RPC client。

- [ ] 区分 client 生命周期 context 和单次请求 context。

  当前 client 把 `context.Context` 存在 struct 里，并在请求中复用它。Go 代码里更常见的做法是每个请求显式传入 context。把 context 存进结构体容易让请求生命周期变得不清晰。

  期望行为：内部 context 只用于后台 worker 生命周期管理。公开方法应接收 `ctx context.Context`，并在内部派生每次 RPC attempt 的 timeout context。

### API 改进

- [ ] 支持更完整的日志过滤条件。

  当前订阅过滤器只支持合约地址和 topic0 的 OR 匹配，不能表达 topic1/topic2、wildcard 或更复杂的 `ethereum.FilterQuery` topic 组合。

  期望行为：直接接受 `ethereum.FilterQuery`，或者引入一个能表达完整 EVM 日志过滤语义的 `LogFilter` 类型。

- [ ] 在 API 边界校验 topic 和地址。

  `AddSubscribeTopic` 当前接收原始 string，并在后续用 `common.HexToHash` 转换。格式错误的 topic 不会在添加时立即暴露。

  期望行为：添加 topic 时就校验格式，无效值返回 error；也可以考虑直接接收 `common.Hash`。

- [ ] 增加 endpoint 选择策略。

  当前固定顺序 fallback 会把流量长期打到第一个健康 endpoint，直到它失败。这容易触发公共 RPC 的限流。

  期望行为：支持可配置的 endpoint 选择策略，例如 round-robin、随机、按延迟选择，或者对部分读请求采用 race 前 N 个 endpoint 并取最快结果。

- [ ] 改善日志和可观测性。

  当前如果设置了 `logrus.Entry`，client 会记录错误，但它没有暴露结构化的 endpoint 健康状态或指标。

  期望行为：暴露健康快照和关键计数，例如每个 endpoint 的成功次数、失败次数、超时次数、当前健康状态、最近错误和观测到的区块高度。

## 建议实施计划

### 阶段 1：安全性和生命周期

- [x] 为当前 fallback 行为补充测试。
- [x] 引入 `Close`，确保后台区块更新循环可以干净停止。
- [x] 保护共享状态，或者重构订阅状态所有权。
- [x] 增加每次 RPC attempt 的 timeout 配置。

### 阶段 2：endpoint 健康状态和 fallback

- [ ] 引入内部 endpoint 类型，保存 URL、client、健康状态、延迟、失败次数和重试时间。
- [ ] 用一个共享的 operation runner 替代重复的 fallback 循环。
- [ ] 在 endpoint 不健康且未到重试时间时跳过它。
- [ ] 允许部分初始化成功，只要至少一个 endpoint 可用即可。

### 阶段 3：订阅正确性

- [ ] 增加可配置的 confirmations。
- [ ] 增加可配置的 `MaxLogBlockRange`。
- [ ] 按分片查询日志，并且只在分片成功后推进 cursor。
- [ ] 对重叠范围内的日志做去重。
- [ ] 向调用方暴露订阅错误。

### 阶段 4：API 清理

- [ ] 替换或扩展 `AddSubscribeTopic`，支持类型化 topic 处理。
- [ ] 支持完整的 `ethereum.FilterQuery` 订阅语义。
- [ ] 增加 endpoint 健康状态查询 API。
- [ ] 文档化可用性模型、重试行为和日志投递保证。

## 建议测试

- [ ] 一个 RPC 初始化失败、另一个 RPC 可用时，client 仍能初始化成功。
- [ ] 所有 RPC 都不可用时，client 返回明确错误。
- [ ] 第一个 RPC 卡住时，会超时并 fallback 到第二个 RPC。
- [ ] 连续失败的 RPC 会在退避期内被跳过。
- [ ] 失效后恢复的 RPC 会在探活成功后重新变为健康。
- [ ] `FilterLogs` 会按区块范围分片查询，并且只在成功后推进 cursor。
- [ ] `FilterLogs` 失败时，订阅不会推进 `prevBlock`。
- [ ] 订阅能对重叠区块范围内的日志去重。
- [ ] 并发 add/remove 订阅过滤条件时不会发生 data race。
- [ ] `Close` 会停止后台 goroutine 并关闭底层 client。

## 验收标准

- 当部分配置的 RPC endpoint 启动时不可用，client 仍然可用。
- 慢 endpoint 不会无限期阻塞一次请求。
- 已知故障的 endpoint 会被临时避开，而不是每次调用都重复尝试。
- 订阅相关共享状态在 `go test -race` 下无 data race。
- 日志订阅能从临时 RPC 故障中恢复，并且不会静默丢失 cursor 进度。
- 大范围日志回补会被拆成对公共 RPC 更友好的小范围查询。
- 调用方可以观察订阅错误和 endpoint 健康状态。
- 库文档说明了可用性模型、重试行为和日志投递保证。
