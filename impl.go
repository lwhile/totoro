package totoro

import (
	"context"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"math/big"
)

func (c *Client) ChainID(ctx context.Context) (*big.Int, error) {
	cli, idx := c.getAvailableEthClient(len(c.rpcList))
	res, err := cli.ChainID(ctx)
	if err != nil {
		c.saveEthCliErr(idx, err)
	}
	return res, err
}

func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	cli, idx := c.getAvailableEthClient(len(c.rpcList))
	res, err := cli.BlockNumber(ctx)
	if err != nil {
		c.saveEthCliErr(idx, err)
	}
	return res, err
}

func (c *Client) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	cli, idx := c.getAvailableEthClient(len(c.rpcList))
	res, err := cli.FilterLogs(ctx, q)
	if err != nil {
		c.saveEthCliErr(idx, err)
	}
	return res, err
}
