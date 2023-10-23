package totoro

import (
	"context"
	"github.com/ethereum/go-ethereum/ethclient"
	"sync"
)

type Client struct {
	ctx              context.Context
	lock             sync.RWMutex
	rpcList          []string
	rawEthClients    []*ethclient.Client
	rawEthClientErrs []error
	currIdx          int
	lastErrIdx       int
}

func NewClient(ctx context.Context, rpcList []string) *Client {
	cli := &Client{
		ctx:              ctx,
		rpcList:          rpcList,
		lock:             sync.RWMutex{},
		rawEthClients:    make([]*ethclient.Client, len(rpcList)),
		rawEthClientErrs: make([]error, len(rpcList)),
		currIdx:          0,
		lastErrIdx:       -1,
	}
	return cli
}

func (c *Client) incrCurrIdx() {
	c.currIdx += 1
	c.currIdx = c.currIdx % len(c.rpcList)
}

func (c *Client) isCurrRawCliNil() bool {
	idx := c.currIdx
	if c.rawEthClients[idx] == nil {
		return true
	} else {
		return false
	}
}

func (c *Client) dialAndSetCurrRawEthCli() {
	idx := c.currIdx
	cli, err := ethclient.DialContext(c.ctx, c.rpcList[idx])
	if err == nil && cli != nil {
		c.rawEthClients[idx] = cli
	}
}

func (c *Client) isCurrRawCliHasErr() bool {
	idx := c.currIdx
	if c.rawEthClientErrs[idx] != nil {
		return true
	} else {
		return false
	}
}

func (c *Client) resetCurrIdxRawEthCliErr() {
	idx := c.currIdx
	c.rawEthClientErrs[idx] = nil
}

func (c *Client) isCurrRawCliAvailable() bool {
	idx := c.currIdx
	return c.rawEthClients[idx] != nil && c.rawEthClientErrs[idx] == nil
}

func (c *Client) saveEthCliErr(idx int, err error) {
	c.lock.Lock()
	c.rawEthClientErrs[idx] = err
	c.lock.Unlock()
}

func (c *Client) getAvailableEthClient(max int) (*ethclient.Client, int) {
	if c.isCurrRawCliAvailable() {
		return c.rawEthClients[c.currIdx], c.currIdx
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	iterIdx := 0
	var retCli *ethclient.Client
	for {
		if c.isCurrRawCliNil() {
			c.dialAndSetCurrRawEthCli()
		}
		if c.isCurrRawCliHasErr() && iterIdx > 0 {
			if c.currIdx == 0 || iterIdx%c.currIdx == 0 {
				c.resetCurrIdxRawEthCliErr()
			}
		}
		if !c.isCurrRawCliHasErr() && !c.isCurrRawCliNil() {
			retCli = c.rawEthClients[c.currIdx]
		} else {
			c.incrCurrIdx()
		}
		iterIdx += 1
		if iterIdx >= max || retCli != nil {
			return retCli, c.currIdx
		}
	}
}
