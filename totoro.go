package totoro

import (
	"context"
	"errors"
	"fmt"
	"math/big"
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

	initialBlock    uint64
	attemptTimeout  time.Duration
	updateInterval  time.Duration
	minRetryBackoff time.Duration
	maxRetryBackoff time.Duration
	endpoints       []*rpcEndpoint

	mu        sync.RWMutex
	prevBlock uint64
	currBlock uint64
	updateCh  chan struct{}
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
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	clientCtx, cancel := context.WithCancel(ctx)
	sub := &EthereumClient{
		ctx:             clientCtx,
		cancel:          cancel,
		attemptTimeout:  cfg.attemptTimeout,
		updateInterval:  cfg.blockUpdateInterval,
		minRetryBackoff: cfg.minRetryBackoff,
		maxRetryBackoff: cfg.maxRetryBackoff,
		endpoints:       make([]*rpcEndpoint, 0, len(rpcs)),
		contracts:       map[common.Address]struct{}{},
		topics:          map[string]struct{}{},
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

func (ec *EthereumClient) Subscribe(ch chan types.Log) {
	updateCh := make(chan struct{}, 1)
	ec.mu.Lock()
	ec.updateCh = updateCh
	ec.mu.Unlock()
	defer ec.clearUpdateCh(updateCh)

	for {
		select {
		case <-updateCh:
			filter, toBlock, ok := ec.getEthereumQueryFilter()
			if !ok {
				continue
			}
			logs, err := ec.FilterLogs(ec.ctx, filter)
			if err != nil {
				ec.logError(err)
				continue
			}
			for _, log := range logs {
				select {
				case ch <- log:
				case <-ec.ctx.Done():
					return
				}
			}
			ec.setPrevBlock(toBlock)
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

func (ec *EthereumClient) AddSubscribeTopic(topics ...string) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	for _, topic := range topics {
		ec.topics[topic] = struct{}{}
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
	updateCh := ec.updateCh
	ec.mu.RUnlock()
	if updateCh == nil {
		return
	}

	select {
	case updateCh <- struct{}{}:
	default:
	}
}

func (ec *EthereumClient) clearUpdateCh(updateCh chan struct{}) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	if ec.updateCh == updateCh {
		ec.updateCh = nil
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

func allClientsDownError(lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("all eth clients are down")
	}
	return fmt.Errorf("all eth clients are down: %w", lastErr)
}
