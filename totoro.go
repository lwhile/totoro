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
)

type clientConfig struct {
	attemptTimeout      time.Duration
	blockUpdateInterval time.Duration
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

// EthereumClient represents a ethereum client.
type EthereumClient struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	rpcs           []string
	ethClis        []*ethclient.Client
	initialBlock   uint64
	attemptTimeout time.Duration
	updateInterval time.Duration

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
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	clientCtx, cancel := context.WithCancel(ctx)
	sub := &EthereumClient{
		ctx:            clientCtx,
		cancel:         cancel,
		rpcs:           append([]string(nil), rpcs...),
		ethClis:        make([]*ethclient.Client, 0, len(rpcs)),
		attemptTimeout: cfg.attemptTimeout,
		updateInterval: cfg.blockUpdateInterval,
		contracts:      map[common.Address]struct{}{},
		topics:         map[string]struct{}{},
	}
	for _, rpc := range rpcs {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, cfg.attemptTimeout)
		cli, err := ethclient.DialContext(attemptCtx, rpc)
		attemptCancel()
		if err != nil {
			sub.closeClients()
			cancel()
			return nil, err
		}
		sub.ethClis = append(sub.ethClis, cli)
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
	var lastErr error
	for i, cli := range ec.ethClis {
		attemptCtx, cancel, err := ec.newAttemptContext(ctx)
		if err != nil {
			return 0, err
		}
		num, err := cli.BlockNumber(attemptCtx)
		cancel()
		if err != nil {
			lastErr = err
			ec.logError(err, ec.rpcs[i])
			continue
		}
		if num == 0 {
			err = fmt.Errorf("rpc returned block number 0")
			lastErr = err
			ec.logError(err, ec.rpcs[i])
			continue
		}
		return num, nil
	}
	return 0, allClientsDownError(lastErr)
}

func (ec *EthereumClient) FilterLogs(ctx context.Context, filter ethereum.FilterQuery) ([]types.Log, error) {
	var lastErr error
	for i, cli := range ec.ethClis {
		attemptCtx, cancel, err := ec.newAttemptContext(ctx)
		if err != nil {
			return nil, err
		}
		logs, err := cli.FilterLogs(attemptCtx, filter)
		cancel()
		if err != nil {
			lastErr = err
			ec.logError(err, ec.rpcs[i])
			continue
		}
		return logs, nil
	}
	return nil, allClientsDownError(lastErr)
}

func (ec *EthereumClient) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	var lastErr error
	for i, cli := range ec.ethClis {
		attemptCtx, cancel, err := ec.newAttemptContext(ctx)
		if err != nil {
			return nil, err
		}
		receipt, err := cli.TransactionReceipt(attemptCtx, txHash)
		cancel()
		if err != nil {
			lastErr = err
			ec.logError(err, ec.rpcs[i])
			continue
		}
		return receipt, nil
	}
	return nil, allClientsDownError(lastErr)
}

func (ec *EthereumClient) BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error) {
	var lastErr error
	for i, cli := range ec.ethClis {
		attemptCtx, cancel, err := ec.newAttemptContext(ctx)
		if err != nil {
			return nil, err
		}
		block, err := cli.BlockByNumber(attemptCtx, number)
		cancel()
		if err != nil {
			lastErr = err
			ec.logError(err, ec.rpcs[i])
			continue
		}
		return block, nil
	}
	return nil, allClientsDownError(lastErr)
}

func (ec *EthereumClient) TransactionByHash(ctx context.Context, hash common.Hash) (tx *types.Transaction, isPending bool, err error) {
	var lastErr error
	for i, cli := range ec.ethClis {
		attemptCtx, cancel, err := ec.newAttemptContext(ctx)
		if err != nil {
			return nil, false, err
		}
		tx, isPending, err = cli.TransactionByHash(attemptCtx, hash)
		cancel()
		if err != nil {
			lastErr = err
			ec.logError(err, ec.rpcs[i])
			continue
		}
		return tx, isPending, nil
	}
	return nil, false, allClientsDownError(lastErr)
}

func (ec *EthereumClient) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	var lastErr error
	for i, cli := range ec.ethClis {
		attemptCtx, cancel, err := ec.newAttemptContext(ctx)
		if err != nil {
			return nil, err
		}
		price, err := cli.SuggestGasPrice(attemptCtx)
		cancel()
		if err != nil {
			lastErr = err
			ec.logError(err, ec.rpcs[i])
			continue
		}
		return price, nil
	}
	return nil, allClientsDownError(lastErr)
}

func (ec *EthereumClient) GetAvailableRPCCli() (*ethclient.Client, error) {
	var lastErr error
	for _, cli := range ec.ethClis {
		attemptCtx, cancel, err := ec.newAttemptContext(ec.ctx)
		if err != nil {
			return nil, err
		}
		_, err = cli.BlockNumber(attemptCtx)
		cancel()
		if err != nil {
			lastErr = err
			ec.logError(err)
			continue
		}
		return cli, nil
	}
	return nil, allClientsDownError(lastErr)
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
	for _, cli := range ec.ethClis {
		cli.Close()
	}
}

func allClientsDownError(lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("all eth clients are down")
	}
	return fmt.Errorf("all eth clients are down: %w", lastErr)
}
