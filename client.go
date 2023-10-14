package totoro

import (
	"context"
	"github.com/ethereum/go-ethereum/ethclient"
)

type EthClient struct {
	ctx        context.Context
	rpcList    []string
	ethClients []*ethclient.Client
	lastErr    error
	currIdx    int
}

func (e *EthClient) setLastErr(err error) {
	e.lastErr = err
}

func (e *EthClient) resetLastErr() {
	e.lastErr = nil
}

func (e *EthClient) dialAndSetEthClient(idx int) error {
	rpc := e.rpcList[idx]
	cli, err := ethclient.DialContext(e.ctx, rpc)
	if err != nil {
		return err
	}
	e.ethClients[idx] = cli
	return nil
}

func (e *EthClient) incrCurrIdx() {
	e.currIdx += 1
	e.currIdx = e.currIdx % len(e.rpcList)
}

func (e *EthClient) getNextAvailableClient() *ethclient.Client {
	if len(e.ethClients) == 0 {
		e.ethClients = make([]*ethclient.Client, len(e.rpcList))
	}
	for {
		if e.lastErr == nil && e.ethClients[e.currIdx] != nil {
			return e.ethClients[e.currIdx]
		}
		e.incrCurrIdx()
		if e.ethClients[e.currIdx] == nil {
			if err := e.dialAndSetEthClient(e.currIdx); err != nil {
				continue
			}
		}
		return e.ethClients[e.currIdx]
	}
}

func NewClient(ctx context.Context, rpcList []string) *EthClient {
	cli := &EthClient{
		currIdx: 0,
		ctx:     ctx,
		rpcList: rpcList,
	}
	return cli
}
