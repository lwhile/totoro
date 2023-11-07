package main

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/lwhile/totoro"
)

func main() {
	rpcList := []string{
		"https://polygon.llamarpc.com",
		"https://polygon.meowrpc.com",
		"https://api.zan.top/node/v1/polygon/mainnet/public",
	}
	subscriber, err := totoro.NewEventSubscriber(context.Background(), rpcList)
	if err != nil {
		panic(err)
	}
	ch := make(chan types.Log)
	subscriber.AddSubscribeTopic("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	go subscriber.Subscribe(ch)
	for {
		select {
		case log := <-ch:
			fmt.Println(log.BlockNumber, log.TxHash)
		}
	}
}
