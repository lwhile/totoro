package totoro

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/sirupsen/logrus"
)

const (
	defaultBlockUpdateInternal = time.Second * 30
	defaultAttemptTimeout      = time.Second * 5
	defaultMinRetryBackoff     = time.Second * 30
	defaultMaxRetryBackoff     = time.Minute * 5
)

type clientConfig struct {
	attemptTimeout      time.Duration
	blockUpdateInterval time.Duration
	minRetryBackoff     time.Duration
	maxRetryBackoff     time.Duration
	confirmations       uint64
	maxLogBlockRange    uint64
}

// Option configures an EthereumClient.
type Option func(*clientConfig) error

// WithAttemptTimeout configures the timeout for each RPC endpoint attempt.
func WithAttemptTimeout(timeout time.Duration) Option {
	return func(cfg *clientConfig) error {
		if timeout <= 0 {
			return fmt.Errorf("attempt timeout must be positive")
		}
		cfg.attemptTimeout = timeout
		return nil
	}
}

// WithBlockUpdateInterval configures the background block number polling interval.
func WithBlockUpdateInterval(interval time.Duration) Option {
	return func(cfg *clientConfig) error {
		if interval <= 0 {
			return fmt.Errorf("block update interval must be positive")
		}
		cfg.blockUpdateInterval = interval
		return nil
	}
}

// WithRetryBackoff configures the minimum and maximum retry backoff for failed endpoints.
func WithRetryBackoff(minBackoff, maxBackoff time.Duration) Option {
	return func(cfg *clientConfig) error {
		if minBackoff <= 0 {
			return fmt.Errorf("minimum retry backoff must be positive")
		}
		if maxBackoff < minBackoff {
			return fmt.Errorf("maximum retry backoff must be greater than or equal to minimum retry backoff")
		}
		cfg.minRetryBackoff = minBackoff
		cfg.maxRetryBackoff = maxBackoff
		return nil
	}
}

// WithConfirmations configures how many latest blocks log subscriptions leave unprocessed.
func WithConfirmations(confirmations uint64) Option {
	return func(cfg *clientConfig) error {
		cfg.confirmations = confirmations
		return nil
	}
}

// WithMaxLogBlockRange configures the maximum block span for each eth_getLogs call.
func WithMaxLogBlockRange(blocks uint64) Option {
	return func(cfg *clientConfig) error {
		if blocks == 0 {
			return fmt.Errorf("maximum log block range must be positive")
		}
		cfg.maxLogBlockRange = blocks
		return nil
	}
}

type endpointHealth uint8

const (
	endpointHealthHealthy endpointHealth = iota + 1
	endpointHealthUnhealthy
)

type rpcEndpoint struct {
	mu sync.RWMutex

	url    string
	client *ethclient.Client
	health endpointHealth

	consecutiveFailures uint64
	lastErr             error
	lastSuccess         time.Time
	latency             time.Duration
	observedHead        uint64
	nextRetry           time.Time
}

// EndpointHealth describes the current state of one configured RPC endpoint.
type EndpointHealth struct {
	URL                 string
	Healthy             bool
	ConsecutiveFailures uint64
	LastError           string
	LastSuccess         time.Time
	Latency             time.Duration
	ObservedHead        uint64
	NextRetry           time.Time
}

// LogSubscription exposes logs and errors from a polling log subscription.
type LogSubscription struct {
	logs      chan types.Log
	errs      chan error
	done      chan struct{}
	cancel    context.CancelFunc
	closeOnce sync.Once

	mu        sync.RWMutex
	prevBlock uint64
}

func newRPCEndpoint(url string) *rpcEndpoint {
	return &rpcEndpoint{
		url:    url,
		health: endpointHealthHealthy,
	}
}

// EthereumClient represents a ethereum client.
type EthereumClient struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	initialBlock     uint64
	attemptTimeout   time.Duration
	updateInterval   time.Duration
	minRetryBackoff  time.Duration
	maxRetryBackoff  time.Duration
	confirmations    uint64
	maxLogBlockRange uint64
	endpoints        []*rpcEndpoint

	mu        sync.RWMutex
	prevBlock uint64
	currBlock uint64
	updateChs map[chan struct{}]struct{}
	contracts map[common.Address]struct{}
	topics    map[string]struct{}
	logger    *logrus.Entry

	closeOnce sync.Once
}

func NewEthereumClient(ctx context.Context, rpcs []string, opts ...Option) (*EthereumClient, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is nil")
	}
	cfg := clientConfig{
		attemptTimeout:      defaultAttemptTimeout,
		blockUpdateInterval: defaultBlockUpdateInternal,
		minRetryBackoff:     defaultMinRetryBackoff,
		maxRetryBackoff:     defaultMaxRetryBackoff,
		maxLogBlockRange:    1_000,
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	clientCtx, cancel := context.WithCancel(ctx)
	sub := &EthereumClient{
		ctx:              clientCtx,
		cancel:           cancel,
		attemptTimeout:   cfg.attemptTimeout,
		updateInterval:   cfg.blockUpdateInterval,
		minRetryBackoff:  cfg.minRetryBackoff,
		maxRetryBackoff:  cfg.maxRetryBackoff,
		confirmations:    cfg.confirmations,
		maxLogBlockRange: cfg.maxLogBlockRange,
		endpoints:        make([]*rpcEndpoint, 0, len(rpcs)),
		updateChs:        map[chan struct{}]struct{}{},
		contracts:        map[common.Address]struct{}{},
		topics:           map[string]struct{}{},
	}
	now := time.Now()
	for _, rpc := range rpcs {
		endpoint := newRPCEndpoint(rpc)
		attemptCtx, attemptCancel := context.WithTimeout(ctx, cfg.attemptTimeout)
		cli, err := ethclient.DialContext(attemptCtx, rpc)
		attemptCancel()
		if err != nil {
			endpoint.recordFailure(now, 0, err, cfg.minRetryBackoff, cfg.maxRetryBackoff)
			sub.endpoints = append(sub.endpoints, endpoint)
			continue
		}
		endpoint.setClient(cli)
		endpoint.recordSuccess(now, 0)
		sub.endpoints = append(sub.endpoints, endpoint)
	}
	blockNum, err := sub.blockNumber(ctx)
	if err != nil {
		sub.closeClients()
		cancel()
		return nil, err
	}
	sub.initialBlock = blockNum
	sub.prevBlock = blockNum
	sub.currBlock = blockNum
	sub.wg.Add(1)
	go sub.updateBlockNumLoop()
	return sub, nil
}

// Close stops background workers and closes all RPC clients.
func (ec *EthereumClient) Close() {
	ec.closeOnce.Do(func() {
		if ec.cancel != nil {
			ec.cancel()
		}
		ec.wg.Wait()
		ec.closeClients()
	})
}

func (ec *EthereumClient) updateBlockNumLoop() {
	defer ec.wg.Done()

	ticker := time.NewTicker(ec.updateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			blockNum, err := ec.BlockNumber()
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				ec.logError(err)
				continue
			}
			ec.setCurrBlock(blockNum)
			ec.notifyBlockUpdate()
		case <-ec.ctx.Done():
			return
		}
	}
}

func (ec *EthereumClient) SetLogger(entry *logrus.Entry) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	ec.logger = entry
}

// Subscribe sends logs matching the client's configured contract/topic filters to ch.
func (ec *EthereumClient) Subscribe(ch chan types.Log) {
	sub := ec.subscribeLogs(ec.ctx, ec.subscribeFilter)
	defer sub.Close()

	for {
		select {
		case log := <-sub.Logs():
			select {
			case ch <- log:
			case <-ec.ctx.Done():
				return
			}
		case err := <-sub.Errors():
			ec.logError(err)
		case <-ec.ctx.Done():
			return
		}
	}
}

func (ec *EthereumClient) AddSubscribeContract(contracts ...common.Address) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	for _, contract := range contracts {
		ec.contracts[contract] = struct{}{}
	}
}

func (ec *EthereumClient) RemoveSubscribeContract(contracts ...common.Address) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	for _, contract := range contracts {
		delete(ec.contracts, contract)
	}
}

func (ec *EthereumClient) AddSubscribeTopic(topics ...string) error {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	for _, topic := range topics {
		if !isHexHash(topic) {
			return fmt.Errorf("invalid topic hash %q", topic)
		}
		ec.topics[topic] = struct{}{}
	}
	return nil
}

// AddSubscribeTopics adds typed topic0 filters for the legacy Subscribe API.
func (ec *EthereumClient) AddSubscribeTopics(topics ...common.Hash) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	for _, topic := range topics {
		ec.topics[topic.Hex()] = struct{}{}
	}
}

func (ec *EthereumClient) RemoveSubscribeTopic(topics ...string) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	for _, topic := range topics {
		delete(ec.topics, topic)
	}
}

func (ec *EthereumClient) getEthereumQueryFilter() (ethereum.FilterQuery, uint64, bool) {
	ec.mu.RLock()
	defer ec.mu.RUnlock()

	if ec.currBlock <= ec.prevBlock {
		return ethereum.FilterQuery{}, 0, false
	}

	addresses := make([]common.Address, 0, len(ec.contracts))
	for addr := range ec.contracts {
		addresses = append(addresses, addr)
	}
	topics := make([]common.Hash, 0, len(ec.topics))
	for topic := range ec.topics {
		topics = append(topics, common.HexToHash(topic))
	}
	fromBlock := ec.prevBlock
	if fromBlock > 0 {
		fromBlock--
	}
	return ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(ec.currBlock),
		Addresses: addresses,
		Topics:    [][]common.Hash{topics},
	}, ec.currBlock, true
}

// SubscribeLogs starts a polling log subscription for the provided Ethereum filter.
func (ec *EthereumClient) SubscribeLogs(ctx context.Context, filter ethereum.FilterQuery) *LogSubscription {
	filter = cloneFilterQuery(filter)
	return ec.subscribeLogs(ctx, func() ethereum.FilterQuery {
		return cloneFilterQuery(filter)
	})
}

func (ec *EthereumClient) subscribeLogs(ctx context.Context, filterProvider func() ethereum.FilterQuery) *LogSubscription {
	if ctx == nil {
		ctx = ec.ctx
	}
	subCtx, cancel := context.WithCancel(ctx)
	sub := &LogSubscription{
		logs:      make(chan types.Log, 64),
		errs:      make(chan error, 16),
		done:      make(chan struct{}),
		cancel:    cancel,
		prevBlock: ec.subscriptionStartBlock(),
	}

	updateCh := make(chan struct{}, 1)
	ec.registerUpdateCh(updateCh)
	ec.wg.Add(1)
	go func() {
		defer ec.wg.Done()
		defer ec.clearUpdateCh(updateCh)
		defer close(sub.done)

		for {
			select {
			case <-updateCh:
				if err := ec.pollSubscription(subCtx, sub, filterProvider()); err != nil {
					sub.sendError(err)
				}
			case <-subCtx.Done():
				return
			case <-ec.ctx.Done():
				return
			}
		}
	}()

	return sub
}

// Logs returns subscription log deliveries.
func (sub *LogSubscription) Logs() <-chan types.Log {
	return sub.logs
}

// Errors returns subscription polling errors.
func (sub *LogSubscription) Errors() <-chan error {
	return sub.errs
}

// Close stops the subscription.
func (sub *LogSubscription) Close() {
	sub.closeOnce.Do(func() {
		sub.cancel()
		<-sub.done
	})
}

// Health returns a snapshot of all configured RPC endpoints.
func (ec *EthereumClient) Health() []EndpointHealth {
	snapshots := make([]EndpointHealth, 0, len(ec.endpoints))
	for _, endpoint := range ec.endpoints {
		snapshots = append(snapshots, endpoint.snapshot())
	}
	return snapshots
}

func (ec *EthereumClient) BlockNumber() (uint64, error) {
	return ec.blockNumber(ec.ctx)
}

func (ec *EthereumClient) blockNumber(ctx context.Context) (uint64, error) {
	return doRPC(ec, ctx, func(ctx context.Context, cli *ethclient.Client) (uint64, error) {
		num, err := cli.BlockNumber(ctx)
		if err != nil {
			return 0, err
		}
		if num == 0 {
			return 0, fmt.Errorf("rpc returned block number 0")
		}
		return num, nil
	}, func(endpoint *rpcEndpoint, num uint64) {
		endpoint.setObservedHead(num)
	})
}

func (ec *EthereumClient) FilterLogs(ctx context.Context, filter ethereum.FilterQuery) ([]types.Log, error) {
	return doRPC(ec, ctx, func(ctx context.Context, cli *ethclient.Client) ([]types.Log, error) {
		return cli.FilterLogs(ctx, filter)
	}, nil)
}

func (ec *EthereumClient) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	return doRPC(ec, ctx, func(ctx context.Context, cli *ethclient.Client) (*types.Receipt, error) {
		return cli.TransactionReceipt(ctx, txHash)
	}, nil)
}

func (ec *EthereumClient) BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error) {
	return doRPC(ec, ctx, func(ctx context.Context, cli *ethclient.Client) (*types.Block, error) {
		return cli.BlockByNumber(ctx, number)
	}, nil)
}

func (ec *EthereumClient) TransactionByHash(ctx context.Context, hash common.Hash) (tx *types.Transaction, isPending bool, err error) {
	type transactionByHashResult struct {
		tx        *types.Transaction
		isPending bool
	}
	result, err := doRPC(ec, ctx, func(ctx context.Context, cli *ethclient.Client) (transactionByHashResult, error) {
		tx, isPending, err := cli.TransactionByHash(ctx, hash)
		return transactionByHashResult{tx: tx, isPending: isPending}, err
	}, nil)
	if err != nil {
		return nil, false, err
	}
	return result.tx, result.isPending, nil
}

func (ec *EthereumClient) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return doRPC(ec, ctx, func(ctx context.Context, cli *ethclient.Client) (*big.Int, error) {
		return cli.SuggestGasPrice(ctx)
	}, nil)
}

func (ec *EthereumClient) GetAvailableRPCCli() (*ethclient.Client, error) {
	return doRPC(ec, ec.ctx, func(ctx context.Context, cli *ethclient.Client) (*ethclient.Client, error) {
		if _, err := cli.BlockNumber(ctx); err != nil {
			return nil, err
		}
		return cli, nil
	}, nil)
}

func (ec *EthereumClient) logError(err error, rpcs ...string) {
	if err == nil || errors.Is(err, ethereum.NotFound) {
		return
	}

	ec.mu.RLock()
	logger := ec.logger
	ec.mu.RUnlock()
	if logger != nil {
		if len(rpcs) > 0 {
			logger.WithField("rpc", rpcs[0]).Error(err)
		} else {
			logger.Error(err)
		}
	}
}

func (ec *EthereumClient) newAttemptContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("context is nil")
	}
	if ec.attemptTimeout <= 0 {
		return nil, nil, fmt.Errorf("attempt timeout must be positive")
	}
	attemptCtx, cancel := context.WithTimeout(ctx, ec.attemptTimeout)
	return attemptCtx, cancel, nil
}

func (ec *EthereumClient) pollSubscription(ctx context.Context, sub *LogSubscription, baseFilter ethereum.FilterQuery) error {
	fromBlock, toBlock, ok := ec.nextSubscriptionRange(sub)
	if !ok {
		return nil
	}

	seen := make(map[logKey]struct{})
	for _, blockRange := range logBlockRanges(fromBlock, toBlock, ec.maxLogBlockRange) {
		filter := cloneFilterQuery(baseFilter)
		filter.FromBlock = new(big.Int).SetUint64(blockRange.from)
		filter.ToBlock = new(big.Int).SetUint64(blockRange.to)

		logs, err := ec.FilterLogs(ctx, filter)
		if err != nil {
			return err
		}
		for _, log := range logs {
			key := newLogKey(log)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			select {
			case sub.logs <- log:
			case <-ctx.Done():
				return ctx.Err()
			case <-ec.ctx.Done():
				return ec.ctx.Err()
			}
		}
		sub.setPrevBlock(blockRange.to)
	}

	return nil
}

func (ec *EthereumClient) nextSubscriptionRange(sub *LogSubscription) (uint64, uint64, bool) {
	ec.mu.RLock()
	currBlock := ec.currBlock
	confirmations := ec.confirmations
	ec.mu.RUnlock()

	if currBlock <= confirmations {
		return 0, 0, false
	}
	toBlock := currBlock - confirmations
	prevBlock := sub.prevBlockValue()
	if toBlock <= prevBlock {
		return 0, 0, false
	}
	return prevBlock + 1, toBlock, true
}

func (ec *EthereumClient) subscribeFilter() ethereum.FilterQuery {
	ec.mu.RLock()
	defer ec.mu.RUnlock()

	addresses := make([]common.Address, 0, len(ec.contracts))
	for addr := range ec.contracts {
		addresses = append(addresses, addr)
	}
	topics := make([]common.Hash, 0, len(ec.topics))
	for topic := range ec.topics {
		topics = append(topics, common.HexToHash(topic))
	}
	return ethereum.FilterQuery{
		Addresses: addresses,
		Topics:    [][]common.Hash{topics},
	}
}

func (ec *EthereumClient) registerUpdateCh(updateCh chan struct{}) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	if ec.updateChs == nil {
		ec.updateChs = map[chan struct{}]struct{}{}
	}
	ec.updateChs[updateCh] = struct{}{}
}

func (ec *EthereumClient) subscriptionStartBlock() uint64 {
	ec.mu.RLock()
	defer ec.mu.RUnlock()

	return ec.prevBlock
}

type rpcOperation[T any] func(context.Context, *ethclient.Client) (T, error)

func doRPC[T any](
	ec *EthereumClient,
	ctx context.Context,
	op rpcOperation[T],
	onSuccess func(*rpcEndpoint, T),
) (T, error) {
	var zero T
	var lastErr error
	attempted := false
	now := time.Now()

	for _, endpoint := range ec.endpoints {
		if !endpoint.canAttempt(now) {
			if lastErr == nil {
				lastErr = fmt.Errorf("all endpoints are in retry backoff")
			}
			continue
		}
		attempted = true

		attemptCtx, cancel, err := ec.newAttemptContext(ctx)
		if err != nil {
			return zero, err
		}
		start := time.Now()
		cli, err := endpoint.clientForAttempt(attemptCtx)
		if err == nil {
			var result T
			result, err = op(attemptCtx, cli)
			latency := time.Since(start)
			cancel()
			if err != nil {
				endpoint.recordFailure(time.Now(), latency, err, ec.minRetryBackoff, ec.maxRetryBackoff)
				ec.logError(err, endpoint.url)
				lastErr = err
				continue
			}
			endpoint.recordSuccess(time.Now(), latency)
			if onSuccess != nil {
				onSuccess(endpoint, result)
			}
			return result, nil
		}

		latency := time.Since(start)
		cancel()
		endpoint.recordFailure(time.Now(), latency, err, ec.minRetryBackoff, ec.maxRetryBackoff)
		ec.logError(err, endpoint.url)
		lastErr = err
	}
	if !attempted && lastErr == nil {
		lastErr = fmt.Errorf("no rpc endpoints configured")
	}
	return zero, allClientsDownError(lastErr)
}

func (ec *EthereumClient) setCurrBlock(blockNum uint64) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	ec.currBlock = blockNum
}

func (ec *EthereumClient) setPrevBlock(blockNum uint64) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	ec.prevBlock = blockNum
}

func (ec *EthereumClient) notifyBlockUpdate() {
	ec.mu.RLock()
	updateChs := make([]chan struct{}, 0, len(ec.updateChs))
	for updateCh := range ec.updateChs {
		updateChs = append(updateChs, updateCh)
	}
	ec.mu.RUnlock()

	for _, updateCh := range updateChs {
		select {
		case updateCh <- struct{}{}:
		default:
		}
	}
}

func (ec *EthereumClient) clearUpdateCh(updateCh chan struct{}) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	if ec.updateChs != nil {
		delete(ec.updateChs, updateCh)
	}
}

func (ec *EthereumClient) closeClients() {
	for _, endpoint := range ec.endpoints {
		endpoint.close()
	}
}

func (ep *rpcEndpoint) setClient(cli *ethclient.Client) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	ep.client = cli
}

func (ep *rpcEndpoint) canAttempt(now time.Time) bool {
	ep.mu.RLock()
	defer ep.mu.RUnlock()

	if ep.health == endpointHealthHealthy {
		return true
	}
	return ep.nextRetry.IsZero() || !now.Before(ep.nextRetry)
}

func (ep *rpcEndpoint) clientForAttempt(ctx context.Context) (*ethclient.Client, error) {
	ep.mu.RLock()
	cli := ep.client
	ep.mu.RUnlock()
	if cli != nil {
		return cli, nil
	}

	cli, err := ethclient.DialContext(ctx, ep.url)
	if err != nil {
		return nil, err
	}

	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.client != nil {
		cli.Close()
		return ep.client, nil
	}
	ep.client = cli
	return ep.client, nil
}

func (ep *rpcEndpoint) recordSuccess(now time.Time, latency time.Duration) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	ep.health = endpointHealthHealthy
	ep.consecutiveFailures = 0
	ep.lastErr = nil
	ep.lastSuccess = now
	ep.latency = latency
	ep.nextRetry = time.Time{}
}

func (ep *rpcEndpoint) recordFailure(
	now time.Time,
	latency time.Duration,
	err error,
	minBackoff time.Duration,
	maxBackoff time.Duration,
) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	ep.health = endpointHealthUnhealthy
	ep.consecutiveFailures++
	ep.lastErr = err
	ep.latency = latency
	ep.nextRetry = now.Add(retryBackoff(ep.consecutiveFailures, minBackoff, maxBackoff))
}

func (ep *rpcEndpoint) setObservedHead(head uint64) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	ep.observedHead = head
}

func (ep *rpcEndpoint) close() {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.client != nil {
		ep.client.Close()
		ep.client = nil
	}
}

func (ep *rpcEndpoint) snapshot() EndpointHealth {
	ep.mu.RLock()
	defer ep.mu.RUnlock()

	snapshot := EndpointHealth{
		URL:                 ep.url,
		Healthy:             ep.health == endpointHealthHealthy,
		ConsecutiveFailures: ep.consecutiveFailures,
		LastSuccess:         ep.lastSuccess,
		Latency:             ep.latency,
		ObservedHead:        ep.observedHead,
		NextRetry:           ep.nextRetry,
	}
	if ep.lastErr != nil {
		snapshot.LastError = ep.lastErr.Error()
	}
	return snapshot
}

func (sub *LogSubscription) sendError(err error) {
	if err == nil {
		return
	}
	select {
	case sub.errs <- err:
	default:
	}
}

func (sub *LogSubscription) prevBlockValue() uint64 {
	sub.mu.RLock()
	defer sub.mu.RUnlock()

	return sub.prevBlock
}

func (sub *LogSubscription) setPrevBlock(blockNum uint64) {
	sub.mu.Lock()
	defer sub.mu.Unlock()

	sub.prevBlock = blockNum
}

type logBlockRange struct {
	from uint64
	to   uint64
}

func logBlockRanges(fromBlock, toBlock, maxRange uint64) []logBlockRange {
	if fromBlock > toBlock {
		return nil
	}
	if maxRange == 0 {
		maxRange = toBlock - fromBlock + 1
	}
	var ranges []logBlockRange
	for start := fromBlock; start <= toBlock; {
		end := start + maxRange - 1
		if end < start || end > toBlock {
			end = toBlock
		}
		ranges = append(ranges, logBlockRange{from: start, to: end})
		if end == toBlock {
			break
		}
		start = end + 1
	}
	return ranges
}

type logKey struct {
	blockHash common.Hash
	txHash    common.Hash
	index     uint
}

func newLogKey(log types.Log) logKey {
	return logKey{
		blockHash: log.BlockHash,
		txHash:    log.TxHash,
		index:     log.Index,
	}
}

func cloneFilterQuery(filter ethereum.FilterQuery) ethereum.FilterQuery {
	cloned := filter
	if filter.FromBlock != nil {
		cloned.FromBlock = new(big.Int).Set(filter.FromBlock)
	}
	if filter.ToBlock != nil {
		cloned.ToBlock = new(big.Int).Set(filter.ToBlock)
	}
	if filter.BlockHash != nil {
		blockHash := *filter.BlockHash
		cloned.BlockHash = &blockHash
	}
	cloned.Addresses = append([]common.Address(nil), filter.Addresses...)
	if filter.Topics != nil {
		cloned.Topics = make([][]common.Hash, len(filter.Topics))
		for i, topics := range filter.Topics {
			cloned.Topics[i] = append([]common.Hash(nil), topics...)
		}
	}
	return cloned
}

func isHexHash(s string) bool {
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, r := range s[2:] {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func retryBackoff(failures uint64, minBackoff, maxBackoff time.Duration) time.Duration {
	if minBackoff <= 0 {
		return 0
	}
	backoff := minBackoff
	for i := uint64(1); i < failures; i++ {
		if backoff >= maxBackoff/2 {
			return maxBackoff
		}
		backoff *= 2
	}
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}

func (sub *LogSubscription) prevBlockForTest() uint64 {
	return sub.prevBlockValue()
}

func allClientsDownError(lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("all eth clients are down")
	}
	return fmt.Errorf("all eth clients are down: %w", lastErr)
}
