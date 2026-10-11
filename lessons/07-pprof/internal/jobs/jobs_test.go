package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTestStore(t *testing.T, capacity int) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.sqlite")
	store, err := Open(path, capacity)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func submitTestJob(t *testing.T, store *Store, payload string) Job {
	t.Helper()
	job, err := store.Submit(context.Background(), json.RawMessage(payload))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	return job
}

func waitForState(t *testing.T, store *Store, id, state string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.Get(context.Background(), id)
		if err == nil && job.State == state {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := store.Get(context.Background(), id)
	t.Fatalf("job %s did not reach %s: job=%+v err=%v", id, state, job, err)
	return Job{}
}

func startRun(t *testing.T, store *Store, workers int, execute Execute) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- store.Run(ctx, workers, execute) }()
	return cancel, result
}

func stopRun(t *testing.T, cancel context.CancelFunc, result <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error on cancellation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not join workers after cancellation")
	}
}

func TestSubmitCapacityAndNotFound(t *testing.T) {
	store, _ := openTestStore(t, 1)
	job := submitTestJob(t, store, `{"prompt":"one"}`)
	if _, err := store.Submit(context.Background(), json.RawMessage(`{"prompt":"two"}`)); !errors.Is(err, ErrFull) {
		t.Fatalf("Submit() at capacity error = %v, want ErrFull", err)
	}
	if _, err := store.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() missing error = %v, want ErrNotFound", err)
	}
	if job.State != stateQueued {
		t.Fatalf("initial state = %q, want queued", job.State)
	}
	if _, err := store.Submit(context.Background(), json.RawMessage(`{broken`)); err == nil {
		t.Fatal("Submit() accepted invalid JSON")
	}
}

func TestConcurrentSubmitHonorsCapacityOne(t *testing.T) {
	store, _ := openTestStore(t, 1)
	const submitters = 8
	start := make(chan struct{})
	var successes atomic.Int32
	var full atomic.Int32
	var unexpected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < submitters; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			payload := json.RawMessage(fmt.Sprintf(`{"submitter":%d}`, n))
			_, err := store.Submit(context.Background(), payload)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrFull):
				full.Add(1)
			default:
				unexpected.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful concurrent submissions = %d, want exactly one", got)
	}
	if got := full.Load(); got != submitters-1 {
		t.Fatalf("ErrFull submissions = %d, want %d", got, submitters-1)
	}
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("unexpected submit errors = %d", got)
	}
}

func TestConcurrentWorkersClaimEachJobOnceAndPersistBeforeProcessing(t *testing.T) {
	store, _ := openTestStore(t, 16)
	const count = 12
	jobs := make([]Job, count)
	for i := range jobs {
		jobs[i] = submitTestJob(t, store, fmt.Sprintf(`{"n":%d}`, i))
	}

	var mu sync.Mutex
	calls := make(map[string]int)
	execute := func(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		calls[string(payload)]++
		mu.Unlock()
		var in struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(payload, &in); err != nil {
			return nil, err
		}
		return json.RawMessage(fmt.Sprintf(`{"answer":%d}`, in.N*2)), nil
	}
	cancel, result := startRun(t, store, 5, execute)
	for _, job := range jobs {
		got := waitForState(t, store, job.ID, stateDone)
		if !got.Processed || got.ResultLength == 0 || got.ResultSummary == "" {
			t.Fatalf("post-processing fields not persisted: %+v", got)
		}
		var body map[string]int
		if err := json.Unmarshal(got.Result, &body); err != nil || body["answer"] == 0 && string(got.Result) != `{"answer":0}` {
			t.Fatalf("result not persisted as JSON: result=%s err=%v", got.Result, err)
		}
	}
	stopRun(t, cancel, result)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != count {
		t.Fatalf("execute observed %d distinct payloads, want %d", len(calls), count)
	}
	for payload, n := range calls {
		if n != 1 {
			t.Errorf("payload %s executed %d times, want once", payload, n)
		}
	}
	if _, err := store.Submit(context.Background(), json.RawMessage(`{"n":99}`)); err != nil {
		t.Fatalf("completed jobs should free capacity: %v", err)
	}
}

func TestExecuteFailureIsPersisted(t *testing.T) {
	store, _ := openTestStore(t, 2)
	job := submitTestJob(t, store, `{"fail":true}`)
	cancel, result := startRun(t, store, 2, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("provider unavailable")
	})
	failed := waitForState(t, store, job.ID, stateFailed)
	stopRun(t, cancel, result)
	if failed.Error != "provider unavailable" {
		t.Fatalf("persisted error = %q", failed.Error)
	}
}

func TestRecoveryRequeuesRunningAndPromotesSavedResult(t *testing.T) {
	store, _ := openTestStore(t, 4)
	running := submitTestJob(t, store, `{"kind":"retry"}`)
	processing := submitTestJob(t, store, `{"kind":"saved"}`)
	if _, err := store.db.Exec(`UPDATE jobs SET state='running' WHERE id=?`, running.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='processing',result=? WHERE id=?`, []byte(`{"cached":true}`), processing.ID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	cancel, result := startRun(t, store, 2, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"retry":true}`), nil
	})
	waitForState(t, store, running.ID, stateDone)
	saved := waitForState(t, store, processing.ID, stateDone)
	stopRun(t, cancel, result)
	if calls.Load() != 1 {
		t.Fatalf("Execute called %d times; saved processing result must not call supplier", calls.Load())
	}
	if string(saved.Result) != `{"cached":true}` {
		t.Fatalf("recovered result = %s", saved.Result)
	}
}

func TestRecoveryAfterCloseAndReopen(t *testing.T) {
	store, path := openTestStore(t, 4)
	running := submitTestJob(t, store, `{"kind":"interrupted"}`)
	saved := submitTestJob(t, store, `{"kind":"result-already-saved"}`)
	if _, err := store.db.Exec(`UPDATE jobs SET state='running' WHERE id=?`, running.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='processing',result=? WHERE id=?`, []byte(`{"recovered":true}`), saved.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() before reopening: %v", err)
	}

	reopened, err := Open(path, 4)
	if err != nil {
		t.Fatalf("Open() same SQLite file: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var executeCalls atomic.Int32
	cancel, result := startRun(t, reopened, 2, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		executeCalls.Add(1)
		return json.RawMessage(`{"retried":true}`), nil
	})
	waitForState(t, reopened, running.ID, stateDone)
	recovered := waitForState(t, reopened, saved.ID, stateDone)
	stopRun(t, cancel, result)
	if got := executeCalls.Load(); got != 1 {
		t.Fatalf("Execute calls after close/reopen = %d, want one for running row only", got)
	}
	if string(recovered.Result) != `{"recovered":true}` {
		t.Fatalf("saved result changed after close/reopen: %s", recovered.Result)
	}
}

func TestSaveResultDatabaseFailureStopsRunAndCanRecover(t *testing.T) {
	store, _ := openTestStore(t, 2)
	job := submitTestJob(t, store, `{"answer":42}`)
	const trigger = `CREATE TRIGGER fail_save_result BEFORE UPDATE OF state ON jobs
WHEN OLD.state='running' AND NEW.state='completed'
BEGIN SELECT RAISE(FAIL, 'injected save result failure'); END;`
	if _, err := store.db.Exec(trigger); err != nil {
		t.Fatalf("create save-result trigger: %v", err)
	}
	var executeCalls atomic.Int32
	failedRun := make(chan error, 1)
	go func() {
		failedRun <- store.Run(context.Background(), 2, func(context.Context, json.RawMessage) (json.RawMessage, error) {
			executeCalls.Add(1)
			return json.RawMessage(`{"answer":42}`), nil
		})
	}()
	select {
	case err := <-failedRun:
		if err == nil {
			t.Fatal("Run() returned nil after save-result SQL failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after save-result SQL failure")
	}
	got, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get() after failed save: %v", err)
	}
	if got.State == stateDone || got.Result != nil {
		t.Fatalf("failed result write must not produce a completed task: %+v", got)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_save_result`); err != nil {
		t.Fatalf("drop save-result trigger: %v", err)
	}
	cancel, result := startRun(t, store, 1, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		executeCalls.Add(1)
		return json.RawMessage(`{"answer":42}`), nil
	})
	waitForState(t, store, job.ID, stateDone)
	stopRun(t, cancel, result)
	if got := executeCalls.Load(); got != 2 {
		t.Fatalf("Execute attempts after result-write failure = %d, want 2", got)
	}
}

func TestPostProcessingFailureRecoversWithoutReexecution(t *testing.T) {
	store, _ := openTestStore(t, 2)
	job := submitTestJob(t, store, `{"answer":42}`)
	const trigger = `CREATE TRIGGER fail_post_processing BEFORE UPDATE OF state ON jobs
WHEN OLD.state='processing' AND NEW.state='done'
BEGIN SELECT RAISE(FAIL, 'injected post-processing failure'); END;`
	if _, err := store.db.Exec(trigger); err != nil {
		t.Fatalf("create post-processing trigger: %v", err)
	}
	var executeCalls atomic.Int32
	failedRun := make(chan error, 1)
	go func() {
		failedRun <- store.Run(context.Background(), 2, func(context.Context, json.RawMessage) (json.RawMessage, error) {
			executeCalls.Add(1)
			return json.RawMessage(`{"answer":42}`), nil
		})
	}()
	select {
	case err := <-failedRun:
		if err == nil {
			t.Fatal("Run() returned nil after post-processing SQL failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after post-processing SQL failure")
	}
	interrupted, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get() after failed post-processing: %v", err)
	}
	if interrupted.State != stateProcessing || string(interrupted.Result) != `{"answer":42}` || interrupted.Processed {
		t.Fatalf("result must remain saved for post-processing recovery: %+v", interrupted)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_post_processing`); err != nil {
		t.Fatalf("drop post-processing trigger: %v", err)
	}
	cancel, result := startRun(t, store, 1, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		executeCalls.Add(1)
		return json.RawMessage(`{"unexpected":"supplier called again"}`), nil
	})
	done := waitForState(t, store, job.ID, stateDone)
	stopRun(t, cancel, result)
	if got := executeCalls.Load(); got != 1 {
		t.Fatalf("Execute calls after post-processing recovery = %d, want one", got)
	}
	if string(done.Result) != `{"answer":42}` || !done.Processed {
		t.Fatalf("post-processing recovery lost the saved result: %+v", done)
	}
}

func TestCancellationLeavesRunningJobForRecovery(t *testing.T) {
	store, _ := openTestStore(t, 2)
	job := submitTestJob(t, store, `{"long":true}`)
	started := make(chan struct{})
	var calls atomic.Int32
	cancel, result := startRun(t, store, 1, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not start")
	}
	stopRun(t, cancel, result)
	got, err := store.Get(context.Background(), job.ID)
	if err != nil || got.State != stateRunning {
		t.Fatalf("cancelled job state = %q, err=%v; want running for restart recovery", got.State, err)
	}

	cancel, result = startRun(t, store, 1, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"resumed":true}`), nil
	})
	waitForState(t, store, job.ID, stateDone)
	stopRun(t, cancel, result)
	if calls.Load() != 2 {
		t.Fatalf("execute attempts = %d, want 2 (at-least-once recovery)", calls.Load())
	}
}

func TestDatabaseFailureStopsRun(t *testing.T) {
	store, _ := openTestStore(t, 2)
	_ = submitTestJob(t, store, `{"x":1}`)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- store.Run(ctx, 2, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			cancelOnce.Do(func() { close(cancelled) })
			return nil, ctx.Err()
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start before database failure")
	}
	if _, err := store.db.Exec(`DROP TABLE jobs`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Run() with a database failure returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after database failure")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("database failure did not cancel the executing worker")
	}
}

func TestRunRejectsDuplicateInvocation(t *testing.T) {
	store, _ := openTestStore(t, 1)
	job := submitTestJob(t, store, `{}`)
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.Run(ctx, 1, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first Run did not start")
	}
	if err := store.Run(context.Background(), 1, func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }); err == nil {
		t.Fatal("second concurrent Run should be rejected")
	}
	cancel()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Run did not stop")
	}
	if got, err := store.Get(context.Background(), job.ID); err != nil || got.State != stateRunning {
		t.Fatalf("job state after stop = %q err=%v", got.State, err)
	}
}
