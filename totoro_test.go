package totoro

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestBlockNumberFallsBackAfterRPCError(t *testing.T) {
	failingRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		return testRPCResult{err: &testRPCError{code: -32000, message: "forced failure"}}
	})
	workingRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		if method != "eth_blockNumber" {
			t.Errorf("test RPC method = %s, want eth_blockNumber", method)
		}
		return testRPCResult{result: "0x2a"}
	})

	client, err := NewEthereumClient(context.Background(), []string{failingRPC.URL, workingRPC.URL})
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{failingRPC.URL, workingRPC.URL}, err)
	}
	defer client.Close()

	got, err := client.BlockNumber()
	if err != nil {
		t.Fatalf("BlockNumber() error = %v, want nil", err)
	}
	if got != 42 {
		t.Errorf("BlockNumber() = %d, want %d", got, 42)
	}
}

func TestBlockNumberTimesOutSlowRPCAndFallsBack(t *testing.T) {
	slowRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		select {
		case <-time.After(200 * time.Millisecond):
			return testRPCResult{result: "0x29"}
		case <-r.Context().Done():
			return testRPCResult{skipResponse: true}
		}
	})
	workingRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		return testRPCResult{result: "0x2b"}
	})

	start := time.Now()
	client, err := NewEthereumClient(
		context.Background(),
		[]string{slowRPC.URL, workingRPC.URL},
		WithAttemptTimeout(30*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{slowRPC.URL, workingRPC.URL}, err)
	}
	defer client.Close()

	got, err := client.BlockNumber()
	if err != nil {
		t.Fatalf("BlockNumber() error = %v, want nil", err)
	}
	if got != 43 {
		t.Errorf("BlockNumber() = %d, want %d", got, 43)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("BlockNumber() fallback elapsed = %s, want <= %s", elapsed, 150*time.Millisecond)
	}
}

func TestNewEthereumClientAllowsPartialRPCInitialization(t *testing.T) {
	workingRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		return testRPCResult{result: "0x2c"}
	})

	client, err := NewEthereumClient(context.Background(), []string{"://bad-url", workingRPC.URL})
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{"://bad-url", workingRPC.URL}, err)
	}
	defer client.Close()

	got, err := client.BlockNumber()
	if err != nil {
		t.Fatalf("BlockNumber() error = %v, want nil", err)
	}
	if got != 44 {
		t.Errorf("BlockNumber() = %d, want %d", got, 44)
	}
}

func TestNewEthereumClientFailsWhenAllRPCsUnavailable(t *testing.T) {
	client, err := NewEthereumClient(context.Background(), []string{"://bad-url"})
	if err == nil {
		client.Close()
		t.Fatalf("NewEthereumClient(%v) error = nil, want non-nil", []string{"://bad-url"})
	}
}

func TestBlockNumberSkipsFailedEndpointDuringBackoff(t *testing.T) {
	var failingCalls atomic.Int64
	failingRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		failingCalls.Add(1)
		return testRPCResult{err: &testRPCError{code: -32000, message: "forced failure"}}
	})
	workingRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		return testRPCResult{result: "0x2d"}
	})

	client, err := NewEthereumClient(
		context.Background(),
		[]string{failingRPC.URL, workingRPC.URL},
		WithRetryBackoff(time.Second, time.Second),
	)
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{failingRPC.URL, workingRPC.URL}, err)
	}
	defer client.Close()

	if got := failingCalls.Load(); got != 1 {
		t.Fatalf("failing RPC calls after NewEthereumClient() = %d, want %d", got, 1)
	}
	got, err := client.BlockNumber()
	if err != nil {
		t.Fatalf("BlockNumber() error = %v, want nil", err)
	}
	if got != 45 {
		t.Errorf("BlockNumber() = %d, want %d", got, 45)
	}
	if got := failingCalls.Load(); got != 1 {
		t.Errorf("failing RPC calls after backoff skip = %d, want %d", got, 1)
	}
}

func TestBlockNumberRetriesEndpointAfterBackoff(t *testing.T) {
	var recoveringCalls atomic.Int64
	recoveringRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		call := recoveringCalls.Add(1)
		if call == 1 {
			return testRPCResult{err: &testRPCError{code: -32000, message: "temporary failure"}}
		}
		return testRPCResult{result: "0x64"}
	})
	workingRPC := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		return testRPCResult{result: "0x2e"}
	})

	client, err := NewEthereumClient(
		context.Background(),
		[]string{recoveringRPC.URL, workingRPC.URL},
		WithRetryBackoff(10*time.Millisecond, 10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{recoveringRPC.URL, workingRPC.URL}, err)
	}
	defer client.Close()

	time.Sleep(20 * time.Millisecond)
	got, err := client.BlockNumber()
	if err != nil {
		t.Fatalf("BlockNumber() error = %v, want nil", err)
	}
	if got != 100 {
		t.Errorf("BlockNumber() = %d, want %d", got, 100)
	}
	if got := recoveringCalls.Load(); got != 2 {
		t.Errorf("recovering RPC calls = %d, want %d", got, 2)
	}
}

func TestCloseStopsBackgroundBlockUpdater(t *testing.T) {
	var calls atomic.Int64
	rpc := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		if method != "eth_blockNumber" {
			t.Errorf("test RPC method = %s, want eth_blockNumber", method)
		}
		call := calls.Add(1)
		return testRPCResult{result: fmt.Sprintf("0x%x", call)}
	})

	client, err := NewEthereumClient(
		context.Background(),
		[]string{rpc.URL},
		WithBlockUpdateInterval(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{rpc.URL}, err)
	}

	waitForRPCRequests(t, &calls, 2)
	client.Close()
	afterClose := calls.Load()
	time.Sleep(40 * time.Millisecond)

	if got := calls.Load(); got != afterClose {
		t.Errorf("RPC calls after Close() = %d, want %d", got, afterClose)
	}
}

func TestSubscribeFilterMutationIsConcurrentSafe(t *testing.T) {
	client := &EthereumClient{
		contracts: map[common.Address]struct{}{},
		topics:    map[string]struct{}{},
		prevBlock: 10,
		currBlock: 11,
	}

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			addr := common.BigToAddress(big.NewInt(int64(i)))
			client.AddSubscribeContract(addr)
			client.RemoveSubscribeContract(addr)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			topic := common.BigToHash(big.NewInt(int64(i))).Hex()
			client.AddSubscribeTopic(topic)
			client.RemoveSubscribeTopic(topic)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _, _ = client.getEthereumQueryFilter()
		}
	}()

	wg.Wait()
}

type testRPCHandler func(r *http.Request, method string) testRPCResult

type testRPCResult struct {
	result       any
	err          *testRPCError
	skipResponse bool
}

type testRPCError struct {
	code    int
	message string
}

func newTestRPCServer(t *testing.T, handler testRPCHandler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Decode(JSON-RPC request) error = %v, want nil", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		rpcResult := handler(r, req.Method)
		if rpcResult.skipResponse {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
		}
		if rpcResult.err != nil {
			resp["error"] = map[string]any{
				"code":    rpcResult.err.code,
				"message": rpcResult.err.message,
			}
		} else {
			resp["result"] = rpcResult.result
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("Encode(JSON-RPC response) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func waitForRPCRequests(t *testing.T, calls *atomic.Int64, want int64) {
	t.Helper()

	deadline := time.After(500 * time.Millisecond)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if got := calls.Load(); got >= want {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("RPC request count = %d, want at least %d", calls.Load(), want)
		case <-ticker.C:
		}
	}
}
