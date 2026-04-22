# totoro

`totoro` is a lightweight Go wrapper around `go-ethereum/ethclient` for EVM RPC reads. It is designed for applications that rely on free or public RPC endpoints, where individual endpoints may fail, hang, lag behind, or be rate-limited.

## Availability Model

Create a client with multiple RPC URLs:

```go
client, err := totoro.NewEthereumClient(ctx, []string{
	"https://polygon.llamarpc.com",
	"https://polygon.meowrpc.com",
})
if err != nil {
	return err
}
defer client.Close()
```

The client keeps every configured endpoint in an internal endpoint table. Endpoints that fail to dial during startup are retained as unhealthy endpoints instead of failing the whole client immediately. Client creation succeeds when at least one endpoint can serve the initial block number request.

Each RPC attempt has an independent timeout. A slow endpoint cannot block the whole operation indefinitely:

```go
client, err := totoro.NewEthereumClient(
	ctx,
	rpcs,
	totoro.WithAttemptTimeout(3*time.Second),
	totoro.WithRetryBackoff(30*time.Second, 5*time.Minute),
)
```

Failed endpoints are marked unhealthy and skipped until their retry time. The retry delay grows with consecutive failures up to the configured maximum. A successful request resets the endpoint back to healthy.

Use `Health` to inspect endpoint state:

```go
for _, endpoint := range client.Health() {
	fmt.Println(endpoint.URL, endpoint.Healthy, endpoint.ObservedHead, endpoint.LastError)
}
```

## Log Subscriptions

Use `SubscribeLogs` for the newer subscription API. It accepts a full `ethereum.FilterQuery`, exposes both logs and polling errors, and stops with `Close`.

```go
sub := client.SubscribeLogs(ctx, ethereum.FilterQuery{
	Addresses: []common.Address{contract},
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

Subscriptions process only blocks older than the configured confirmation depth:

```go
client, err := totoro.NewEthereumClient(
	ctx,
	rpcs,
	totoro.WithConfirmations(6),
	totoro.WithMaxLogBlockRange(1_000),
)
```

`WithMaxLogBlockRange` controls the maximum `eth_getLogs` span per request. Large catch-up ranges are split into chunks. The subscription cursor advances only after each chunk succeeds, so a failed chunk does not silently skip logs.

Log delivery is best-effort at-least-once within the in-memory subscription lifecycle. Duplicate logs returned from overlapping or inconsistent RPC responses are deduplicated by `(blockHash, txHash, logIndex)` before delivery.

## Legacy Subscription API

The older `Subscribe(ch)` API remains available for callers that configure filters with `AddSubscribeContract` and `AddSubscribeTopic`. New code should prefer `SubscribeLogs` because it accepts full EVM log filter semantics and exposes polling errors.
