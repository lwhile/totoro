package totoro

import (
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"math/big"
	"time"
)

const defaultRefreshBlockTime = time.Second * 5
const defaultStepBackBlockNum = 10

func (c *Client) SubscribeEvent(addresses []common.Address, topics [][]common.Hash, ch chan types.Log) {
	go func() {
		for {
			lastBlock, err := c.BlockNumber(c.ctx)
			if err != nil {
				continue
			}
			for {
				time.Sleep(defaultRefreshBlockTime)
				currBlock, err := c.BlockNumber(c.ctx)
				if err != nil {
					continue
				}
				if currBlock > lastBlock {
					f := ethereum.FilterQuery{
						FromBlock: big.NewInt(int64(lastBlock - defaultStepBackBlockNum)),
						ToBlock:   big.NewInt(int64(currBlock)),
						Addresses: addresses,
						Topics:    topics,
					}
					logs, err := c.FilterLogs(c.ctx, f)
					if err != nil {
						continue
					}
					for _, log := range logs {
						ch <- log
					}
					lastBlock = currBlock
				}
			}
		}
	}()
}
