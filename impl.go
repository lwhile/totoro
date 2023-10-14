package totoro

import (
	"context"
	"errors"
	"math/big"
)

var ErrNotAvailableClientFound = errors.New("not available client found")

func (e *EthClient) ChainID(ctx context.Context) (*big.Int, error) {
	for i := 0; i < len(e.rpcList); i++ {
		cli := e.getNextAvailableClient()
		if res, err := cli.ChainID(ctx); err != nil {
			continue
		} else {
			return res, nil
		}
	}
	return nil, ErrNotAvailableClientFound
}
