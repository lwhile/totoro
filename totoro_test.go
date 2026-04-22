package totoro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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

func TestAddSubscribeTopicValidatesHash(t *testing.T) {
	client := &EthereumClient{
		contracts: map[common.Address]struct{}{},
		topics:    map[string]struct{}{},
	}

	if err := client.AddSubscribeTopic("bad-topic"); err == nil {
		t.Fatalf("AddSubscribeTopic(%q) error = nil, want non-nil", "bad-topic")
	}
	topic := common.BigToHash(big.NewInt(1)).Hex()
	if err := client.AddSubscribeTopic(topic); err != nil {
		t.Fatalf("AddSubscribeTopic(%q) error = %v, want nil", topic, err)
	}
}

func TestSubscribeLogsChunksDeduplicatesAndAdvancesCursor(t *testing.T) {
	var filtersMu sync.Mutex
	var filters []map[string]any
	logA := testLog(1, 0)
	logB := testLog(3, 0)
	rpc := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		switch method {
		case "eth_blockNumber":
			return testRPCResult{result: "0x5"}
		case "eth_getLogs":
			filter := decodeRPCFilter(t, r)
			filtersMu.Lock()
			filters = append(filters, filter)
			filtersMu.Unlock()
			switch filter["fromBlock"] {
			case "0x1":
				return testRPCResult{result: []types.Log{logA}}
			case "0x3":
				return testRPCResult{result: []types.Log{logA, logB}}
			case "0x5":
				return testRPCResult{result: []types.Log{}}
			default:
				t.Errorf("eth_getLogs fromBlock = %v, want one of 0x1, 0x3, 0x5", filter["fromBlock"])
				return testRPCResult{result: []types.Log{}}
			}
		default:
			t.Errorf("test RPC method = %s, want eth_blockNumber or eth_getLogs", method)
			return testRPCResult{err: &testRPCError{code: -32601, message: "method not found"}}
		}
	})

	client, err := NewEthereumClient(
		context.Background(),
		[]string{rpc.URL},
		WithConfirmations(0),
		WithMaxLogBlockRange(2),
	)
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{rpc.URL}, err)
	}
	defer client.Close()
	client.setPrevBlock(0)
	client.setCurrBlock(5)

	sub := client.SubscribeLogs(context.Background(), ethereumFilterForTest())
	defer sub.Close()
	client.notifyBlockUpdate()

	gotA := receiveLog(t, sub.Logs())
	gotB := receiveLog(t, sub.Logs())
	if gotA.TxHash != logA.TxHash {
		t.Errorf("first subscribed log TxHash = %s, want %s", gotA.TxHash, logA.TxHash)
	}
	if gotB.TxHash != logB.TxHash {
		t.Errorf("second subscribed log TxHash = %s, want %s", gotB.TxHash, logB.TxHash)
	}
	assertNoLog(t, sub.Logs())
	if got := sub.prevBlockForTest(); got != 5 {
		t.Errorf("subscription cursor = %d, want %d", got, 5)
	}

	filtersMu.Lock()
	defer filtersMu.Unlock()
	if got, want := len(filters), 3; got != want {
		t.Fatalf("eth_getLogs call count = %d, want %d", got, want)
	}
	if filters[0]["fromBlock"] != "0x1" || filters[0]["toBlock"] != "0x2" {
		t.Errorf("first eth_getLogs range = %v-%v, want 0x1-0x2", filters[0]["fromBlock"], filters[0]["toBlock"])
	}
	if filters[1]["fromBlock"] != "0x3" || filters[1]["toBlock"] != "0x4" {
		t.Errorf("second eth_getLogs range = %v-%v, want 0x3-0x4", filters[1]["fromBlock"], filters[1]["toBlock"])
	}
	if filters[2]["fromBlock"] != "0x5" || filters[2]["toBlock"] != "0x5" {
		t.Errorf("third eth_getLogs range = %v-%v, want 0x5-0x5", filters[2]["fromBlock"], filters[2]["toBlock"])
	}
}

func TestSubscribeLogsDoesNotAdvanceCursorAfterChunkError(t *testing.T) {
	logA := testLog(1, 0)
	rpc := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		switch method {
		case "eth_blockNumber":
			return testRPCResult{result: "0x5"}
		case "eth_getLogs":
			filter := decodeRPCFilter(t, r)
			if filter["fromBlock"] == "0x3" {
				return testRPCResult{err: &testRPCError{code: -32000, message: "range rejected"}}
			}
			return testRPCResult{result: []types.Log{logA}}
		default:
			return testRPCResult{err: &testRPCError{code: -32601, message: "method not found"}}
		}
	})

	client, err := NewEthereumClient(
		context.Background(),
		[]string{rpc.URL},
		WithConfirmations(0),
		WithMaxLogBlockRange(2),
	)
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{rpc.URL}, err)
	}
	defer client.Close()
	client.setPrevBlock(0)
	client.setCurrBlock(5)

	sub := client.SubscribeLogs(context.Background(), ethereumFilterForTest())
	defer sub.Close()
	client.notifyBlockUpdate()

	_ = receiveLog(t, sub.Logs())
	err = receiveErr(t, sub.Errors())
	if err == nil {
		t.Fatalf("subscription error = nil, want non-nil")
	}
	if got := sub.prevBlockForTest(); got != 2 {
		t.Errorf("subscription cursor after chunk error = %d, want %d", got, 2)
	}
}

func TestSubscribeLogsKeepsIndependentCursorPerSubscription(t *testing.T) {
	logA := testLog(1, 0)
	logB := testLog(1, 1)
	var calls atomic.Int64
	rpc := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		switch method {
		case "eth_blockNumber":
			return testRPCResult{result: "0x1"}
		case "eth_getLogs":
			call := calls.Add(1)
			if call%2 == 1 {
				return testRPCResult{result: []types.Log{logA}}
			}
			return testRPCResult{result: []types.Log{logB}}
		default:
			return testRPCResult{err: &testRPCError{code: -32601, message: "method not found"}}
		}
	})

	client, err := NewEthereumClient(context.Background(), []string{rpc.URL}, WithConfirmations(0))
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{rpc.URL}, err)
	}
	defer client.Close()
	client.setPrevBlock(0)
	client.setCurrBlock(1)

	subA := client.SubscribeLogs(context.Background(), ethereumFilterForTest())
	defer subA.Close()
	subB := client.SubscribeLogs(context.Background(), ethereumFilterForTest())
	defer subB.Close()
	client.notifyBlockUpdate()

	gotA := receiveLog(t, subA.Logs())
	gotB := receiveLog(t, subB.Logs())
	gotHashes := map[common.Hash]struct{}{
		gotA.TxHash: {},
		gotB.TxHash: {},
	}
	if _, ok := gotHashes[logA.TxHash]; !ok {
		t.Errorf("subscription logs missing TxHash %s; got %v and %v", logA.TxHash, gotA.TxHash, gotB.TxHash)
	}
	if _, ok := gotHashes[logB.TxHash]; !ok {
		t.Errorf("subscription logs missing TxHash %s; got %v and %v", logB.TxHash, gotA.TxHash, gotB.TxHash)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("eth_getLogs calls = %d, want %d", got, 2)
	}
	if got := subA.prevBlockForTest(); got != 1 {
		t.Errorf("first subscription cursor = %d, want %d", got, 1)
	}
	if got := subB.prevBlockForTest(); got != 1 {
		t.Errorf("second subscription cursor = %d, want %d", got, 1)
	}
}

func TestSubscribeReturnsAfterCloseWhenOutputChannelBlocks(t *testing.T) {
	rpc := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		switch method {
		case "eth_blockNumber":
			return testRPCResult{result: "0x1"}
		case "eth_getLogs":
			return testRPCResult{result: []types.Log{testLog(1, 0)}}
		default:
			return testRPCResult{err: &testRPCError{code: -32601, message: "method not found"}}
		}
	})

	client, err := NewEthereumClient(context.Background(), []string{rpc.URL}, WithConfirmations(0))
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{rpc.URL}, err)
	}
	client.setPrevBlock(0)
	client.setCurrBlock(1)

	logs := make(chan types.Log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Subscribe(logs)
	}()
	waitForUpdateSubscribers(t, client, 1)
	client.notifyBlockUpdate()
	time.Sleep(50 * time.Millisecond)
	client.Close()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Subscribe() did not return after Close() while output channel was blocked")
	}
}

func TestSubscribeRebuildsLegacyFilterEachPoll(t *testing.T) {
	var filtersMu sync.Mutex
	var filters []map[string]any
	rpc := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		switch method {
		case "eth_blockNumber":
			return testRPCResult{result: "0x2"}
		case "eth_getLogs":
			filter := decodeRPCFilter(t, r)
			filtersMu.Lock()
			filters = append(filters, filter)
			filtersMu.Unlock()
			return testRPCResult{result: []types.Log{}}
		default:
			return testRPCResult{err: &testRPCError{code: -32601, message: "method not found"}}
		}
	})

	client, err := NewEthereumClient(context.Background(), []string{rpc.URL}, WithConfirmations(0))
	if err != nil {
		t.Fatalf("NewEthereumClient(%v) error = %v, want nil", []string{rpc.URL}, err)
	}
	defer client.Close()
	client.setPrevBlock(0)
	logs := make(chan types.Log, 1)
	go client.Subscribe(logs)
	waitForUpdateSubscribers(t, client, 1)

	firstContract := common.BigToAddress(big.NewInt(10))
	client.AddSubscribeContract(firstContract)
	client.setCurrBlock(1)
	client.notifyBlockUpdate()
	waitForFilterCount(t, &filtersMu, &filters, 1)

	secondContract := common.BigToAddress(big.NewInt(20))
	client.AddSubscribeContract(secondContract)
	client.setCurrBlock(2)
	client.notifyBlockUpdate()
	waitForFilterCount(t, &filtersMu, &filters, 2)

	filtersMu.Lock()
	defer filtersMu.Unlock()
	firstAddresses := filterAddresses(t, filters[0])
	secondAddresses := filterAddresses(t, filters[1])
	if len(firstAddresses) != 1 {
		t.Fatalf("first filter addresses = %v, want length 1", firstAddresses)
	}
	if len(secondAddresses) != 2 {
		t.Fatalf("second filter addresses = %v, want length 2", secondAddresses)
	}
	if !containsString(secondAddresses, secondContract.Hex()) {
		t.Errorf("second filter addresses = %v, want to contain %s", secondAddresses, secondContract.Hex())
	}
}

func TestHealthSnapshots(t *testing.T) {
	rpc := newTestRPCServer(t, func(r *http.Request, method string) testRPCResult {
		return testRPCResult{result: "0x7"}
	})
	client, err := NewEthereumClient(context.Background(), []string{"://bad-url", rpc.URL})
	if err != nil {
		t.Fatalf("NewEthereumClient(...) error = %v, want nil", err)
	}
	defer client.Close()

	snapshots := client.Health()
	if got, want := len(snapshots), 2; got != want {
		t.Fatalf("Health() length = %d, want %d", got, want)
	}
	if snapshots[0].Healthy {
		t.Errorf("Health()[0].Healthy = true, want false")
	}
	if !snapshots[1].Healthy {
		t.Errorf("Health()[1].Healthy = false, want true")
	}
	if snapshots[1].ObservedHead != 7 {
		t.Errorf("Health()[1].ObservedHead = %d, want %d", snapshots[1].ObservedHead, 7)
	}
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
		body := readRequestBody(t, r)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("Decode(JSON-RPC request) error = %v, want nil", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = newRequestBody(body)

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

func decodeRPCFilter(t *testing.T, r *http.Request) map[string]any {
	t.Helper()

	body := readRequestBody(t, r)
	r.Body = newRequestBody(body)
	var req struct {
		Params []map[string]any `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("Decode(JSON-RPC params) error = %v, want nil", err)
	}
	if len(req.Params) != 1 {
		t.Fatalf("JSON-RPC params length = %d, want %d", len(req.Params), 1)
	}
	return req.Params[0]
}

func readRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()

	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll(request body) error = %v, want nil", err)
		}
	}
	return body
}

func newRequestBody(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}

func testLog(blockNumber uint64, index uint) types.Log {
	txHash := common.BigToHash(big.NewInt(int64(blockNumber*100 + uint64(index))))
	return types.Log{
		Address:     common.BigToAddress(big.NewInt(1)),
		Topics:      []common.Hash{common.BigToHash(big.NewInt(2))},
		Data:        []byte{0x01, 0x02},
		BlockNumber: blockNumber,
		TxHash:      txHash,
		TxIndex:     0,
		BlockHash:   common.BigToHash(big.NewInt(int64(blockNumber))),
		Index:       index,
		Removed:     false,
	}
}

func ethereumFilterForTest() ethereum.FilterQuery {
	return ethereum.FilterQuery{
		Addresses: []common.Address{common.BigToAddress(big.NewInt(1))},
		Topics:    [][]common.Hash{{common.BigToHash(big.NewInt(2))}},
	}
}

func receiveLog(t *testing.T, logs <-chan types.Log) types.Log {
	t.Helper()

	select {
	case log := <-logs:
		return log
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("receive subscription log timed out")
	}
	return types.Log{}
}

func assertNoLog(t *testing.T, logs <-chan types.Log) {
	t.Helper()

	select {
	case log := <-logs:
		t.Fatalf("received unexpected subscription log: %+v", log)
	case <-time.After(50 * time.Millisecond):
	}
}

func receiveErr(t *testing.T, errs <-chan error) error {
	t.Helper()

	select {
	case err := <-errs:
		return err
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("receive subscription error timed out")
	}
	return nil
}

func waitForFilterCount(t *testing.T, mu *sync.Mutex, filters *[]map[string]any, want int) {
	t.Helper()

	deadline := time.After(500 * time.Millisecond)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		mu.Lock()
		got := len(*filters)
		mu.Unlock()
		if got >= want {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("eth_getLogs filter count = %d, want at least %d", got, want)
		case <-ticker.C:
		}
	}
}

func waitForUpdateSubscribers(t *testing.T, client *EthereumClient, want int) {
	t.Helper()

	deadline := time.After(500 * time.Millisecond)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		client.mu.RLock()
		got := len(client.updateChs)
		client.mu.RUnlock()
		if got >= want {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("update subscriber count = %d, want at least %d", got, want)
		case <-ticker.C:
		}
	}
}

func filterAddresses(t *testing.T, filter map[string]any) []string {
	t.Helper()

	raw, ok := filter["address"]
	if !ok {
		return nil
	}
	switch value := raw.(type) {
	case string:
		return []string{value}
	case []any:
		addresses := make([]string, 0, len(value))
		for _, item := range value {
			address, ok := item.(string)
			if !ok {
				t.Fatalf("filter address item type = %T, want string", item)
			}
			addresses = append(addresses, address)
		}
		return addresses
	default:
		t.Fatalf("filter address type = %T, want string or []any", raw)
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
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
