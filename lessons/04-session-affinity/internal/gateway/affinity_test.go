package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func sessionRequest(t testing.TB, session string, stream bool) Request {
	t.Helper()
	r := request(t, stream)
	r.Session = session
	return r
}

func selectedTarget(t testing.TB, g *Gateway, r Request) Target {
	t.Helper()
	result, err := g.Do(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	return Target{Provider: result.Provider, Model: result.Model}
}

func TestSessionParsingAndValidation(t *testing.T) {
	for _, value := range []string{`null`, `0`, `true`, `{}`, `[]`, `""`, `"  "`, fmt.Sprintf("%q", strings.Repeat("x", 257)), fmt.Sprintf("%q", strings.Repeat("会", 86))} {
		_, err := ParseRequest([]byte(`{"messages":[{}],"gateway_session":` + value + `}`))
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted gateway_session=%s: %v", value, err)
		}
	}
	for _, session := range []string{"tenant-1/user-2/session-3", strings.Repeat("x", 256), strings.Repeat("会", 85)} {
		value, _ := json.Marshal(session)
		r, err := ParseRequest([]byte(`{"messages":[{}],"gateway_session":` + string(value) + `}`))
		if err != nil || r.Session != session || r.Fields["gateway_session"] == nil {
			t.Fatalf("session not preserved for persistence: %+v %v", r, err)
		}
	}
	if r := request(t, false); r.Session != "" {
		t.Fatal("missing session should keep ordinary load balancing")
	}
}

func TestSessionStableAcrossCallsAndGateways(t *testing.T) {
	ps := map[string]Provider{"a": constantProvider(), "b": constantProvider()}
	first := testGateway(t, ps, oneGroup("a", "b"), 2, DefaultTimeouts())
	// 新实例和相反的配置顺序均不能改变首选。
	second := testGateway(t, ps, oneGroup("b", "a"), 2, DefaultTimeouts())
	for _, tier := range []string{"economy", "balanced", "powerful"} {
		r := sessionRequest(t, "tenant/user/session", false)
		r.Tier = tier
		want := selectedTarget(t, first, r)
		for i := 0; i < 20; i++ {
			selectedTarget(t, first, request(t, false)) // 其他请求推进共享 cursor。
			selectedTarget(t, second, sessionRequest(t, fmt.Sprintf("other/%d", i), false))
			for _, g := range []*Gateway{first, second} {
				if got := selectedTarget(t, g, r); got != want {
					t.Fatalf("%s session moved: got %+v want %+v", tier, got, want)
				}
			}
		}
	}
	assertFree(t, first)
	assertFree(t, second)
}

func TestSessionDistributesDifferentKeys(t *testing.T) {
	g := testGateway(t, map[string]Provider{"a": constantProvider(), "b": constantProvider()}, oneGroup("a", "b"), 2, DefaultTimeouts())
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		target := selectedTarget(t, g, sessionRequest(t, fmt.Sprintf("tenant/user/session-%d", i), false))
		counts[target.Provider]++
	}
	if counts["a"] < 60 || counts["b"] < 60 {
		t.Fatalf("keys not reasonably distributed: %v", counts)
	}
	assertFree(t, g)
}

func TestSessionPinsActualModel(t *testing.T) {
	groups := [][]Target{{{Provider: "a", Model: "model-1"}, {Provider: "a", Model: "model-2"}}}
	g := testGateway(t, map[string]Provider{"a": constantProvider()}, groups, 2, DefaultTimeouts())
	r := sessionRequest(t, "session-model", false)
	want := selectedTarget(t, g, r)
	for i := 0; i < 20; i++ {
		selectedTarget(t, g, request(t, false))
		if got := selectedTarget(t, g, r); got != want {
			t.Fatalf("actual model changed: %+v -> %+v", want, got)
		}
	}
}

func TestSessionCapacitySoftFallbackAndReturn(t *testing.T) {
	g := testGateway(t, map[string]Provider{"a": constantProvider(), "b": constantProvider()}, oneGroup("a", "b"), 2, DefaultTimeouts())
	r := sessionRequest(t, "capacity-session", true)
	open := func() *Result {
		t.Helper()
		result, err := g.Do(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { result.Body.Close() })
		return result
	}
	first, second := open(), open()
	if first.Provider != second.Provider {
		t.Fatal("preferred still has capacity but request moved to the idle backend")
	}
	third, fourth := open(), open()
	if third.Provider == first.Provider || fourth.Provider != third.Provider {
		t.Fatal("full preferred backend should cause temporary fallback")
	}
	if result, err := g.Do(context.Background(), r); !errors.Is(err, ErrBusy) {
		if result != nil {
			result.Body.Close()
		}
		t.Fatalf("overbooked full backends: %v", err)
	}
	first.Body.Close()
	first.Body.Close()
	recovered := open()
	if recovered.Provider != first.Provider {
		t.Fatal("temporary fallback permanently rebound the session")
	}
	for _, result := range []*Result{second, third, fourth, recovered} {
		result.Body.Close()
	}
	assertFree(t, g)
}

func TestSessionFailureFallbackAndRecovery(t *testing.T) {
	for _, status := range []int{429, 500, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var fail atomic.Bool
			var bad string
			calls := map[string]*atomic.Int32{"a": {}, "b": {}, "c": {}}
			ps := map[string]Provider{}
			for _, name := range []string{"a", "b", "c"} {
				ps[name] = providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
					calls[name].Add(1)
					if name == bad && fail.Load() {
						return nil, &UpstreamError{Status: status}
					}
					return io.NopCloser(strings.NewReader(`{}`)), nil
				})
			}
			groups := append(oneGroup("a", "b"), []Target{{Provider: "c", Model: "real-c"}})
			g := testGateway(t, ps, groups, 2, DefaultTimeouts())
			r := sessionRequest(t, "failure-session", false)
			want := selectedTarget(t, g, r)
			bad = want.Provider
			for _, count := range calls {
				count.Store(0)
			}
			fail.Store(true)
			result, err := g.Do(context.Background(), r)
			if status == 400 {
				if err == nil {
					result.Body.Close()
					t.Fatal("4xx must not fallback")
				}
				var upstream *UpstreamError
				if !errors.As(err, &upstream) || upstream.Status != status {
					t.Fatal(err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				result.Body.Close()
				if result.Provider == bad || result.Provider == "c" {
					t.Fatal("did not exhaust the primary group first")
				}
			}
			if calls[bad].Load() != 1 || calls["c"].Load() != 0 {
				t.Fatalf("unexpected retries: %v", calls)
			}
			if status == 400 && calls["a"].Load()+calls["b"].Load() != 1 {
				t.Fatal("terminal error retried")
			}
			fail.Store(false)
			if got := selectedTarget(t, g, r); got != want {
				t.Fatal("did not return to recovered preferred target")
			}
			assertFree(t, g)
		})
	}
}

func TestSessionFallbackGroupsAreFinite(t *testing.T) {
	var calls atomic.Int32
	fail := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		calls.Add(1)
		return nil, &UpstreamError{Status: 500}
	})
	ps := map[string]Provider{"a": fail, "b": fail, "c": fail}
	groups := append(oneGroup("a", "b"), []Target{{Provider: "c", Model: "real-c"}})
	g := testGateway(t, ps, groups, 2, DefaultTimeouts())
	if _, err := g.Do(context.Background(), sessionRequest(t, "failed-session", false)); err == nil || calls.Load() != 3 {
		t.Fatalf("each target must be tried exactly once: calls=%d err=%v", calls.Load(), err)
	}
	assertFree(t, g)
}

func TestSessionConcurrentCapacityAndRelease(t *testing.T) {
	g := testGateway(t, map[string]Provider{"a": constantProvider(), "b": constantProvider()}, oneGroup("a", "b"), 3, DefaultTimeouts())
	r := sessionRequest(t, "concurrent-session", true)
	results := make(chan *Result, 128)
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Go(func() {
			result, err := g.Do(context.Background(), r)
			if err == nil {
				results <- result
			} else if !errors.Is(err, ErrBusy) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(results)
	counts := map[string]int{}
	for result := range results {
		counts[result.Provider]++
		result.Body.Close()
	}
	if counts["a"] != 3 || counts["b"] != 3 {
		t.Fatalf("capacity exceeded or unused: %v", counts)
	}
	assertFree(t, g)
}

func TestSessionHTTPAndProviderIsolation(t *testing.T) {
	times := DefaultTimeouts()
	upstream := func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		if _, ok := fields["gateway_session"]; ok {
			t.Error("gateway_session leaked to upstream")
		}
		if fields["tools"] == nil || string(fields["prompt_cache_key"]) != `"cache-prefix"` {
			t.Error("ordinary provider fields lost")
		}
		if string(fields["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: token\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
		}
	}
	g := testGateway(t, map[string]Provider{"a": clientProvider(t, upstream, times), "b": clientProvider(t, upstream, times)}, oneGroup("a", "b"), 2, times)
	server := httptest.NewServer(NewAPI(g, "", 2, 2).Handler())
	defer server.Close()
	var want Target
	for i := 0; i < 10; i++ {
		stream := i%2 == 1
		payload := fmt.Sprintf(`{"model":"economy","messages":[{}],"gateway_session":"http-session","tools":[],"prompt_cache_key":"cache-prefix","stream":%t}`, stream)
		resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%d %s %v", resp.StatusCode, body, err)
		}
		if stream && !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
			t.Fatal("incomplete SSE")
		}
		got := Target{Provider: resp.Header.Get("X-Gateway-Provider"), Model: resp.Header.Get("X-Gateway-Model")}
		if i == 0 {
			want = got
		} else if got != want {
			t.Fatal("JSON/SSE requests moved within one session")
		}
	}
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{}],"gateway_session":null}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal(resp.StatusCode)
	}
	assertFree(t, g)
}

func TestSessionTopologyChangesPreserveUnaffectedKeys(t *testing.T) {
	ps := map[string]Provider{"a": constantProvider(), "b": constantProvider(), "c": constantProvider()}
	before := testGateway(t, ps, oneGroup("a", "b"), 2, DefaultTimeouts())
	added := testGateway(t, ps, oneGroup("c", "b", "a"), 2, DefaultTimeouts())
	removed := testGateway(t, ps, oneGroup("a", "c"), 2, DefaultTimeouts())
	var moved int
	for i := 0; i < 200; i++ {
		r := sessionRequest(t, fmt.Sprintf("topology-session-%d", i), false)
		old := selectedTarget(t, before, r)
		newTarget := selectedTarget(t, added, r)
		if newTarget != old {
			moved++
			if newTarget.Provider != "c" {
				t.Fatal("adding c moved a session between existing targets")
			}
		}
		withoutB := selectedTarget(t, removed, r)
		if newTarget.Provider != "b" && withoutB != newTarget {
			t.Fatal("removing b moved an unaffected session")
		}
	}
	if moved == 0 || moved == 200 {
		t.Fatalf("unexpected redistribution: %d", moved)
	}
}
