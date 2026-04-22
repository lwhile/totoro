# totoro

> [English](README.md) | [中文](README.zh-CN.md)

`totoro` is a lightweight Go wrapper around `go-ethereum/ethclient` for EVM RPC reads and polling log subscriptions. It is designed for applications that depend on free or public RPC endpoints, where individual endpoints may fail, hang, lag behind, or be rate-limited.

**Use `totoro` when RPC availability matters more than binding your application to a single endpoint.**

## Features

- Multiple RPC endpoints with automatic failover.
- Per-attempt timeouts, so a slow endpoint cannot block a read indefinitely.
- Endpoint health tracking with retry backoff after failures.
- Startup tolerance for partially unavailable RPC lists.
- `eth_getLogs` polling subscriptions with confirmation depth, chunking, error reporting, and in-memory duplicate suppression.
- A small API surface that mirrors common `ethclient` reads.

## Install

```sh
go get github.com/lwhile/totoro
```

`totoro` currently targets Go 1.19 or newer.

## Quick Start

Create a client with several RPC URLs. Client creation succeeds when at least one configured endpoint can serve the initial block number request. Endpoints that fail during startup stay in the endpoint table as unhealthy endpoints and may be retried later.

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

Always call `Close` when the client is no longer needed. It stops the background block updater and closes all underlying RPC clients.

## Configuration

Pass options to `NewEthereumClient` to tune endpoint behavior.

| Option | Default | Purpose |
| --- | --- | --- |
| `WithAttemptTimeout` | `5s` | Maximum duration for one RPC endpoint attempt. |
| `WithBlockUpdateInterval` | `30s` | How often the background updater refreshes the latest block number. |
| `WithRetryBackoff` | `30s` minimum, `5m` maximum | Exponential retry delay for unhealthy endpoints. |
| `WithConfirmations` | `0` | Number of latest blocks that log subscriptions leave unprocessed. |
| `WithMaxLogBlockRange` | `1_000` | Maximum block span for each `eth_getLogs` request. |

Invalid option values return an error from `NewEthereumClient`, for example a non-positive timeout or a maximum retry backoff smaller than the minimum.

## Read Chain Data

The client exposes common read methods and runs each operation against the first currently available endpoint:

- `BlockNumber() (uint64, error)`
- `FilterLogs(ctx, filter) ([]types.Log, error)`
- `TransactionReceipt(ctx, txHash) (*types.Receipt, error)`
- `BlockByNumber(ctx, number) (*types.Block, error)`
- `TransactionByHash(ctx, hash) (*types.Transaction, bool, error)`
- `SuggestGasPrice(ctx) (*big.Int, error)`
- `GetAvailableRPCCli() (*ethclient.Client, error)`

Example `FilterLogs` call:

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

Use `FilterLogs` for explicit historical ranges. Use `SubscribeLogs` for continuous polling from the client's subscription cursor.

## Subscribe To Logs

New code should use `SubscribeLogs`. It accepts an `ethereum.FilterQuery`, exposes both logs and polling errors, and stops when either the provided context is canceled or the subscription is closed.

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

Subscription behavior:

- The subscription starts from the block observed during client creation.
- The subscription owns `FromBlock` and `ToBlock`; set addresses and topics in the filter, and use `FilterLogs` when you need a fixed historical range.
- Blocks newer than the configured confirmation depth are left unprocessed.
- Large catch-up ranges are split by `WithMaxLogBlockRange`.
- The cursor advances only after each chunk succeeds, so a failed chunk is retried instead of being silently skipped.
- Duplicate logs returned from overlapping or inconsistent RPC responses are deduplicated by `(blockHash, txHash, logIndex)` within the in-memory subscription lifecycle.

For production consumers, persist your own checkpoint after processing logs. `totoro` does not store subscription state across process restarts.

## Endpoint Health

Use `Health` to inspect every configured endpoint.

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

`Healthy` shows whether the endpoint is currently eligible for immediate use. `NextRetry` is set when an endpoint is in retry backoff.

## Logging

Background and per-endpoint RPC errors are logged only when you provide a `logrus.Entry`.

```go
client.SetLogger(logrus.New().WithField("component", "totoro"))
```

Passing `nil` disables logging:

```go
client.SetLogger(nil)
```

## Legacy Subscription API

The older `Subscribe(ch)` API remains available for existing callers that configure filters with `AddSubscribeContract`, `AddSubscribeTopic`, `AddSubscribeTopics`, `RemoveSubscribeContract`, and `RemoveSubscribeTopic`.

New code should prefer `SubscribeLogs` because it accepts an `ethereum.FilterQuery` directly and exposes polling errors to the caller.

## Development

Run the test suite:

```sh
go test ./...
```

Run the example:

```sh
go run ./example
```

The example uses public Polygon RPC endpoints and the legacy subscription API.
