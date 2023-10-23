package main

import (
	"context"
	"github.com/lwhile/totoro"
)

func main() {
	rpcList := []string{
		"https://polygon.llamarpc.com",
		"https://polygon.meowrpc.com",
		"https://api.zan.top/node/v1/polygon/mainnet/public",
	}
	ctx := context.Background()
	totoroCli := totoro.NewClient(ctx, rpcList)
	_, _ = totoroCli.ChainID(context.Background())
}
