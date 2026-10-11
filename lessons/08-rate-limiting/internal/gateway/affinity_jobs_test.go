package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/llm-gateway/internal/jobs"
)

func TestSessionJobsPreserveKeyAcrossRestart(t *testing.T) {
	times := DefaultTimeouts()
	ps := map[string]Provider{}
	for _, name := range []string{"a", "b"} {
		ps[name] = clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
			var fields map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
				t.Error(err)
			}
			if fields["gateway_session"] != nil {
				t.Error("session leaked to provider during worker execution")
			}
			var model string
			if err := json.Unmarshal(fields["model"], &model); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(Target{Provider: name, Model: model})
		}, times)
	}
	g := testGateway(t, ps, oneGroup("a", "b"), 2, times)
	payload := `{"model":"economy","messages":[{}],"gateway_session":"tenant/user/job-session"}`
	r, err := ParseRequest([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	want := selectedTarget(t, g, r)
	path := t.TempDir() + "/jobs.db"
	store, err := jobs.Open(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(NewAPI(g, store, "", 2, 2).Handler())
	defer server.Close()
	var ids []string
	// 先通过 HTTP 持久化，关闭并重开数据库后才启动 worker。
	for i := 0; i < 2; i++ {
		resp, err := http.Post(server.URL+"/v1/jobs", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		var job jobs.Job
		err = json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 202 {
			t.Fatalf("submit: %d %v", resp.StatusCode, err)
		}
		ids = append(ids, job.ID)
	}
	resp, err := http.Post(server.URL+"/v1/jobs", "application/json", strings.NewReader(`{"messages":[{}],"gateway_session":false}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal("invalid session accepted for persistence")
	}
	server.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := jobs.Open(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	fresh := testGateway(t, ps, oneGroup("b", "a"), 2, times)
	for i := 0; i < 3; i++ {
		selectedTarget(t, fresh, sessionRequest(t, fmt.Sprint("other-", i), false))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- resumed.Run(ctx, 2, fresh.ExecuteJob) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for _, id := range ids {
		deadline := time.Now().Add(3 * time.Second)
		for {
			job, err := resumed.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if job.State == "failed" {
				t.Fatalf("job failed: %s", job.Error)
			}
			if job.State == "done" {
				var got Target
				if err := json.Unmarshal(job.Result, &got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("persisted session lost: got %+v want %+v", got, want)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("job did not finish: %+v", job)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	assertFree(t, fresh)
}
