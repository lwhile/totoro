package totoro

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

func newTestTotoroClient() *Client {
	rpcList := []string{
		"https://polygon.llamarpc.com",
		"https://polygon.meowrpc.com",
		"https://api.zan.top/node/v1/polygon/mainnet/public",
	}
	ctx := context.Background()
	totoroCli := NewClient(ctx, rpcList)
	return totoroCli
}

func TestClient_incrCurrIdx(t *testing.T) {
	cli := newTestTotoroClient()
	require.Equal(t, 0, cli.currIdx)
	cli.incrCurrIdx()
	require.Equal(t, 1, cli.currIdx)
	cli.incrCurrIdx()
	require.Equal(t, 2, cli.currIdx)
	cli.incrCurrIdx()
	require.Equal(t, 0, cli.currIdx)
	cli.incrCurrIdx()
	require.Equal(t, 1, cli.currIdx)
}

func TestClient_1(t *testing.T) {
	cli := newTestTotoroClient()
	require.False(t, cli.isCurrRawCliAvailable())
	require.True(t, cli.isCurrRawCliNil())
	require.False(t, cli.isCurrRawCliHasErr())
}

func TestClient_2(t *testing.T) {
	cli := newTestTotoroClient()
	for i := 0; i < 10; i++ {
		ethCli, idx := cli.getAvailableEthClient(1)
		require.NotNil(t, ethCli)
		require.Equal(t, 0, idx)
	}
}

func TestClient_3(t *testing.T) {
	cli := newTestTotoroClient()
	ethCli0, idx := cli.getAvailableEthClient(1)
	require.NotNil(t, ethCli0)
	require.Equal(t, 0, idx)
	cli.saveEthCliErr(idx, errors.New("test error for client idx 0"))

	ethCli1, idx := cli.getAvailableEthClient(2)
	require.Equal(t, 1, idx)
	require.NotNil(t, ethCli1)
	cli.saveEthCliErr(idx, errors.New("test error for client idx 1"))

	ethCli2, idx := cli.getAvailableEthClient(3)
	require.Equal(t, 2, idx)
	require.NotNil(t, ethCli2)
	cli.saveEthCliErr(idx, errors.New("test error for client idx 2"))

	ethCli1a, idx := cli.getAvailableEthClient(4)
	require.Equal(t, 0, idx)
	require.NotNil(t, ethCli1a)
}
