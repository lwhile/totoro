package totoro

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/sirupsen/logrus"
	"math/big"
	"time"
)

const (
	defaultBlockUpdateInternal = time.Second * 30
)

// EthereumClient represents a ethereum client
type EthereumClient struct {
	ctx          context.Context
	rpcs         []string
	ethClis      []*ethclient.Client
	initialBlock uint64
	prevBlock    uint64
	currBlock    uint64
	updateCh     chan struct{}
	contracts    map[common.Address]struct{}
	topics       map[string]struct{}
	logger       *logrus.Entry
}

func NewEthereumClient(ctx context.Context, rpcs []string) (*EthereumClient, error) {
	sub := &EthereumClient{
		ctx:       ctx,
		rpcs:      rpcs,
		updateCh:  nil,
		ethClis:   make([]*ethclient.Client, 0),
		contracts: map[common.Address]struct{}{},
		topics:    map[string]struct{}{},
	}
	for _, rpc := range rpcs {
		var (
			cli *ethclient.Client
			err error
		)
		if cli, err = ethclient.DialContext(ctx, rpc); err != nil {
			return nil, err
		}
		sub.ethClis = append(sub.ethClis, cli)
	}
	blockNum, err := sub.BlockNumber()
	if err != nil {
		return nil, err
	}
	sub.initialBlock = blockNum
	sub.prevBlock = blockNum
	sub.currBlock = blockNum
	go sub.updateBlockNumLoop()
	return sub, nil
}

func (ec *EthereumClient) updateBlockNumLoop() {
	ticker := time.NewTicker(defaultBlockUpdateInternal)
	for {
		select {
		case <-ticker.C:
			var (
				blockNum uint64
				err      error
			)
			if blockNum, err = ec.BlockNumber(); err != nil {
				ec.logError(err)
				continue
			}
			ec.currBlock = blockNum
			if ec.updateCh != nil {
				ec.updateCh <- struct{}{}
			}
		case <-ec.ctx.Done():
			return
		}
	}
}

func (ec *EthereumClient) SetLogger(entry *logrus.Entry) {
	ec.logger = entry
}

func (ec *EthereumClient) Subscribe(ch chan types.Log) {
	ec.updateCh = make(chan struct{})
	for {
		select {
		case <-ec.updateCh:
			if ec.currBlock > ec.prevBlock {
				var (
					logs []types.Log
					err  error
				)
				if logs, err = ec.FilterLogs(ec.ctx, ec.getEthereumQueryFilter()); err != nil {
					ec.logError(err)
					continue
				}
				for _, log := range logs {
					ch <- log
				}
				ec.prevBlock = ec.currBlock
			}
		case <-ec.ctx.Done():
			return
		}
	}
}

func (ec *EthereumClient) AddSubscribeContract(contracts ...common.Address) {
	for _, contract := range contracts {
		ec.contracts[contract] = struct{}{}
	}
}

func (ec *EthereumClient) AddSubscribeTopic(topics ...string) {
	for _, topic := range topics {
		ec.topics[topic] = struct{}{}
	}
}

func (ec *EthereumClient) getEthereumQueryFilter() ethereum.FilterQuery {
	addresses := make([]common.Address, 0, len(ec.contracts))
	for addr := range ec.contracts {
		addresses = append(addresses, addr)
	}
	topics := make([]common.Hash, 0, len(ec.topics))
	for topic := range ec.topics {
		topics = append(topics, common.HexToHash(topic))
	}
	return ethereum.FilterQuery{
		FromBlock: big.NewInt(int64(ec.prevBlock - 1)),
		ToBlock:   big.NewInt(int64(ec.currBlock)),
		Addresses: addresses,
		Topics:    [][]common.Hash{topics},
	}
}

func (ec *EthereumClient) BlockNumber() (uint64, error) {
	for _, cli := range ec.ethClis {
		if num, err := cli.BlockNumber(ec.ctx); err != nil || num == 0 {
			ec.logError(err)
			continue
		} else {
			return num, nil
		}
	}
	return 0, fmt.Errorf("all eth clients are down")
}

func (ec *EthereumClient) FilterLogs(ctx context.Context, filter ethereum.FilterQuery) ([]types.Log, error) {
	var lastErr error
	for _, cli := range ec.ethClis {
		if logs, err := cli.FilterLogs(ctx, filter); err != nil {
			lastErr = err
			ec.logError(err)
			continue
		} else {
			return logs, nil
		}
	}
	return nil, fmt.Errorf("all eth clients are down:%w", lastErr)
}

func (ec *EthereumClient) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	var lastErr error
	for _, cli := range ec.ethClis {
		if receipt, err := cli.TransactionReceipt(ctx, txHash); err != nil {
			lastErr = err
			ec.logError(err)
			continue
		} else {
			return receipt, nil
		}
	}
	return nil, fmt.Errorf("all eth clients are down:%w", lastErr)
}

func (ec *EthereumClient) BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error) {
	var lastErr error
	for _, cli := range ec.ethClis {
		if block, err := cli.BlockByNumber(ctx, number); err != nil {
			lastErr = err
			ec.logError(err)
			continue
		} else {
			return block, nil
		}
	}
	return nil, fmt.Errorf("all eth clients are down:%w", lastErr)
}

func (ec *EthereumClient) TransactionByHash(ctx context.Context, hash common.Hash) (tx *types.Transaction, isPending bool, err error) {
	for _, cli := range ec.ethClis {
		if tx, isPending, err = cli.TransactionByHash(ctx, hash); err != nil {
			ec.logError(err)
			continue
		} else {
			return tx, isPending, nil
		}
	}
	return nil, false, fmt.Errorf("all eth clients are down:%w", err)
}

func (ec *EthereumClient) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	var lastErr error
	for _, cli := range ec.ethClis {
		if price, err := cli.SuggestGasPrice(ctx); err != nil {
			lastErr = err
			ec.logError(err)
			continue
		} else {
			return price, nil
		}
	}
	return nil, fmt.Errorf("all eth clients are down:%w", lastErr)
}

func (ec *EthereumClient) GetAvailableRPCCli() (*ethclient.Client, error) {
	var lastErr error
	for _, cli := range ec.ethClis {
		if _, err := cli.BlockNumber(ec.ctx); err != nil {
			lastErr = err
			ec.logError(err)
			continue
		} else {
			return cli, nil
		}
	}
	return nil, fmt.Errorf("all eth clients are down:%w", lastErr)
}

func (ec *EthereumClient) logError(err error) {
	if ec.logger != nil {
		ec.logger.Error(err)
	}
}
