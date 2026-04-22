# totoro

> [English](README.md) | [中文](README.zh-CN.md)

`totoro` 是一个围绕 `go-ethereum/ethclient` 的轻量级 Go 封装，用于 EVM RPC 读取和轮询式日志订阅。它适合依赖免费或公共 RPC 的应用，因为这些端点可能会失败、卡住、落后或被限流。

**当 RPC 可用性比绑定单个端点更重要时，使用 `totoro`。**

## 功能

- 多 RPC 端点自动故障切换。
- 每次端点尝试都有独立超时，慢端点不会无限阻塞读取。
- 端点健康状态跟踪，失败后按退避时间重试。
- 初始化时允许部分 RPC 不可用。
- 基于 `eth_getLogs` 的轮询订阅，支持确认深度、分块请求、错误上报和内存内去重。
- 提供一组接近 `ethclient` 常用读取能力的小 API。

## 安装

```sh
go get github.com/lwhile/totoro
```

`totoro` 当前面向 Go 1.19 或更新版本。

## 快速开始

使用多个 RPC URL 创建客户端。只要至少一个配置的端点能返回初始区块号，客户端创建就会成功。初始化阶段失败的端点会保留在端点表中，并标记为不健康，之后仍可重试。

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/lwhile/totoro"
)

func main() {
	ctx := context.Background()
	rpcs := []string{
		"https://polygon.llamarpc.com",
		"https://polygon.meowrpc.com",
		"https://api.zan.top/node/v1/polygon/mainnet/public",
	}

	client, err := totoro.NewEthereumClient(
		ctx,
		rpcs,
		totoro.WithAttemptTimeout(3*time.Second),
		totoro.WithRetryBackoff(30*time.Second, 5*time.Minute),
	)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	blockNumber, err := client.BlockNumber()
	if err != nil {
		panic(err)
	}
	fmt.Println(blockNumber)
}
```

客户端不再使用时必须调用 `Close`。它会停止后台区块更新器，并关闭所有底层 RPC 客户端。

## 配置

向 `NewEthereumClient` 传入选项来调整端点行为。

| 选项 | 默认值 | 作用 |
| --- | --- | --- |
| `WithAttemptTimeout` | `5s` | 单个 RPC 端点尝试的最长耗时。 |
| `WithBlockUpdateInterval` | `30s` | 后台更新最新区块号的间隔。 |
| `WithRetryBackoff` | 最小 `30s`，最大 `5m` | 不健康端点的指数退避重试时间。 |
| `WithConfirmations` | `0` | 日志订阅保留不处理的最新区块数量。 |
| `WithMaxLogBlockRange` | `1_000` | 每次 `eth_getLogs` 请求的最大区块跨度。 |

无效选项会让 `NewEthereumClient` 返回错误，例如非正数超时，或最大退避时间小于最小退避时间。

## 读取链上数据

客户端暴露常用读取方法，每次操作会在当前第一个可用端点上执行：

- `BlockNumber() (uint64, error)`
- `FilterLogs(ctx, filter) ([]types.Log, error)`
- `TransactionReceipt(ctx, txHash) (*types.Receipt, error)`
- `BlockByNumber(ctx, number) (*types.Block, error)`
- `TransactionByHash(ctx, hash) (*types.Transaction, bool, error)`
- `SuggestGasPrice(ctx) (*big.Int, error)`
- `GetAvailableRPCCli() (*ethclient.Client, error)`

`FilterLogs` 示例：

```go
logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{
	Addresses: []common.Address{contractAddress},
	Topics:    [][]common.Hash{{transferTopic}},
})
if err != nil {
	return err
}
for _, log := range logs {
	handleLog(log)
}
```

如果需要明确的历史区间，使用 `FilterLogs`。如果需要从客户端订阅游标开始持续轮询，使用 `SubscribeLogs`。

## 订阅日志

新代码建议使用 `SubscribeLogs`。它接收 `ethereum.FilterQuery`，同时暴露日志和轮询错误，并会在传入的 context 取消或订阅被关闭时停止。

```go
sub := client.SubscribeLogs(ctx, ethereum.FilterQuery{
	Addresses: []common.Address{contractAddress},
	Topics:    [][]common.Hash{{transferTopic}},
})
defer sub.Close()

for {
	select {
	case log := <-sub.Logs():
		handleLog(log)
	case err := <-sub.Errors():
		handleSubscriptionError(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

订阅行为：

- 订阅从客户端创建时观察到的区块开始。
- 订阅会接管 `FromBlock` 和 `ToBlock`；过滤器中应设置地址和 topic。如果需要固定历史区间，请使用 `FilterLogs`。
- 比配置确认深度更新的区块会暂不处理。
- 大范围追赶会按 `WithMaxLogBlockRange` 分块。
- 游标只会在每个分块成功后推进，因此失败分块会重试，不会被静默跳过。
- 对于重叠或不一致 RPC 响应返回的重复日志，会在内存订阅生命周期内按 `(blockHash, txHash, logIndex)` 去重。

生产消费者应该在处理日志后持久化自己的 checkpoint。`totoro` 不会在进程重启后保存订阅状态。

## 端点健康状态

使用 `Health` 查看每个配置端点的状态快照。

```go
for _, endpoint := range client.Health() {
	fmt.Printf(
		"rpc=%s healthy=%t head=%d failures=%d latency=%s next_retry=%s last_error=%s\n",
		endpoint.URL,
		endpoint.Healthy,
		endpoint.ObservedHead,
		endpoint.ConsecutiveFailures,
		endpoint.Latency,
		endpoint.NextRetry,
		endpoint.LastError,
	)
}
```

`Healthy` 表示端点当前是否可立即使用。端点处于重试退避时，`NextRetry` 会被设置。

## 日志

只有在提供 `logrus.Entry` 后，后台和单个端点的 RPC 错误才会被记录。

```go
client.SetLogger(logrus.New().WithField("component", "totoro"))
```

传入 `nil` 可关闭日志：

```go
client.SetLogger(nil)
```

## 旧版订阅 API

旧的 `Subscribe(ch)` API 仍然保留，用于兼容通过 `AddSubscribeContract`、`AddSubscribeTopic`、`AddSubscribeTopics`、`RemoveSubscribeContract` 和 `RemoveSubscribeTopic` 配置过滤条件的现有调用方。

新代码建议使用 `SubscribeLogs`，因为它直接接受 `ethereum.FilterQuery`，并把轮询错误暴露给调用方。

## 开发

运行测试：

```sh
go test ./...
```

运行示例：

```sh
go run ./example
```

示例使用公共 Polygon RPC 端点和旧版订阅 API。
