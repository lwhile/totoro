package main

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/lwhile/totoro"
)

func main() {
	rpcList := []string{
		"https://polygon.llamarpc.com",
		"https://polygon.meowrpc.com",
		"https://api.zan.top/node/v1/polygon/mainnet/public",
	}
	client, err := totoro.NewEthereumClient(context.Background(), rpcList)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ch := make(chan types.Log)
	client.AddSubscribeContract(common.HexToAddress("0xc2132D05D31c914a87C6611C10748AEb04B58e8F"))
	if err := client.AddSubscribeTopic("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"); err != nil {
		panic(err)
	}
	go client.Subscribe(ch)
	fmt.Println("start subscribe")
	for log := range ch {
		fmt.Println(log.BlockNumber, log.TxHash)
	}
}
