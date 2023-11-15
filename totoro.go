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

// EventSubscriber subscribe event by http but no websocket
type EventSubscriber struct {
	ctx          context.Context
	currIdx      int
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

func NewEventSubscriber(ctx context.Context, rpcs []string) (*EventSubscriber, error) {
	sub := &EventSubscriber{
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

func (es *EventSubscriber) updateBlockNumLoop() {
	ticker := time.NewTicker(defaultBlockUpdateInternal)
	for {
		select {
		case <-ticker.C:
			var (
				blockNum uint64
				err      error
			)
			if blockNum, err = es.BlockNumber(); err != nil {
				continue
			}
			es.currBlock = blockNum
			if es.updateCh != nil {
				es.updateCh <- struct{}{}
			}
		case <-es.ctx.Done():
			return
		}
	}
}

func (es *EventSubscriber) SetLogger(entry *logrus.Entry) {
	es.logger = entry
}

func (es *EventSubscriber) Subscribe(ch chan types.Log) {
	es.updateCh = make(chan struct{})
	for {
		select {
		case <-es.updateCh:
			if es.currBlock > es.prevBlock {
				var (
					logs []types.Log
					err  error
				)
				if logs, err = es.FilterLogs(es.ctx, es.getEthereumQueryFilter()); err != nil {
					if es.logger != nil {
						es.logger.Errorf("filter logs failed, err: %v", err)
					}
					continue
				}
				for _, log := range logs {
					ch <- log
				}
				es.prevBlock = es.currBlock
			}
		case <-es.ctx.Done():
			return
		}
	}
}

func (es *EventSubscriber) AddSubscribeContract(contracts ...common.Address) {
	for _, contract := range contracts {
		es.contracts[contract] = struct{}{}
	}
}

func (es *EventSubscriber) AddSubscribeTopic(topics ...string) {
	for _, topic := range topics {
		es.topics[topic] = struct{}{}
	}
}

func (es *EventSubscriber) getEthereumQueryFilter() ethereum.FilterQuery {
	addresses := make([]common.Address, 0, len(es.contracts))
	for addr := range es.contracts {
		addresses = append(addresses, addr)
	}
	topics := make([]common.Hash, 0, len(es.topics))
	for topic := range es.topics {
		topics = append(topics, common.HexToHash(topic))
	}
	return ethereum.FilterQuery{
		FromBlock: big.NewInt(int64(es.prevBlock - 1)),
		ToBlock:   big.NewInt(int64(es.currBlock)),
		Addresses: addresses,
		Topics:    [][]common.Hash{topics},
	}
}

func (es *EventSubscriber) BlockNumber() (uint64, error) {
	for _, cli := range es.ethClis {
		if num, err := cli.BlockNumber(es.ctx); err != nil || num == 0 {
			continue
		} else {
			return num, nil
		}
	}
	return 0, fmt.Errorf("all eth clients are down")
}

func (es *EventSubscriber) FilterLogs(ctx context.Context, filter ethereum.FilterQuery) ([]types.Log, error) {
	for _, cli := range es.ethClis {
		if logs, err := cli.FilterLogs(ctx, filter); err != nil {
			continue
		} else {
			return logs, nil
		}
	}
	return nil, fmt.Errorf("all eth clients are down")
}
