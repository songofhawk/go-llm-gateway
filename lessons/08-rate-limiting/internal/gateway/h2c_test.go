package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 让一个正常流与反复取消的流共用连接，验证取消确实到达上游，
// 不破坏其他回答，不重建 TCP，也不遗留供应商名额。
func TestH2CCancelKeepsOtherStreamsAndConnection(t *testing.T) {
	startServer := func(handler http.Handler) (*httptest.Server, *atomic.Int32) {
		t.Helper()
		connections := new(atomic.Int32)
		s := httptest.NewUnstartedServer(handler)
		s.Config.Protocols = new(http.Protocols)
		s.Config.Protocols.SetHTTP1(true)
		s.Config.Protocols.SetUnencryptedHTTP2(true)
		s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		}
		s.Start()
		t.Cleanup(s.Close)
		return s, connections
	}
	finish := make(chan struct{})
	ended := make(chan struct{}, 1)
	upstream, upConnections := startServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var req struct{ Keep bool }
		_ = json.Unmarshal(data, &req)
		if r.ProtoMajor != 2 {
			t.Errorf("upstream used %s", r.Proto)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		http.NewResponseController(w).Flush()
		if req.Keep {
			select {
			case <-finish:
				fmt.Fprint(w, "data: [DONE]\n\n")
			case <-r.Context().Done():
			}
		} else {
			<-r.Context().Done()
			ended <- struct{}{}
		}
	}))
	client := NewH2CClient(time.Second, 4)
	t.Cleanup(client.CloseIdleConnections)
	p := &OpenAIProvider{URL: upstream.URL + "/v1", Client: client}
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 4, DefaultTimeouts())
	downstream, downConnections := startServer(NewAPI(g, nil, "", 4, 4).Handler())
	call := func(keep bool) (*http.Response, context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		payload := fmt.Sprintf(`{"model":"economy","messages":[{}],"stream":true,"keep":%t}`, keep)
		r, _ := http.NewRequestWithContext(ctx, "POST", downstream.URL+"/v1/chat/completions", strings.NewReader(payload))
		res, err := client.Do(r)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if res.StatusCode != 200 || res.ProtoMajor != 2 {
			cancel()
			res.Body.Close()
			t.Fatalf("response: %s %s", res.Proto, res.Status)
		}
		var b [1]byte
		if _, err := io.ReadFull(res.Body, b[:]); err != nil {
			cancel()
			res.Body.Close()
			t.Fatal(err)
		}
		return res, cancel
	}
	held, stopHeld := call(true)
	defer stopHeld()
	defer held.Body.Close()
	for i := 0; i < 128; i++ {
		res, cancel := call(false)
		cancel()
		res.Body.Close()
		select {
		case <-ended:
		case <-time.After(time.Second):
			t.Fatal("cancel did not reach upstream")
		}
	}
	close(finish)
	data, err := io.ReadAll(held.Body)
	if err != nil || !strings.Contains(string(data), "data: [DONE]\n\n") {
		t.Fatalf("other stream interrupted: %q %v", data, err)
	}
	held.Body.Close()
	if upConnections.Load() != 1 || downConnections.Load() != 1 {
		t.Fatalf("TCP churn: upstream=%d downstream=%d", upConnections.Load(), downConnections.Load())
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if g.Snapshot()["p"]["active"] == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("provider slots were not released")
}

func TestH2CRequiresHTTPProvider(t *testing.T) {
	if err := ValidateEndpoint(Endpoint{BaseURL: "https://example.com/v1", H2C: true}); err == nil {
		t.Fatal("h2c with HTTPS must fail instead of silently using the wrong protocol")
	}
}
