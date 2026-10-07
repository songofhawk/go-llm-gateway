// Package jobs 提供单进程使用的 SQLite 持久化异步任务队列。
// 数据库是任务状态的事实来源；worker 只在短数据库操作期间持有数据库资源，
// 调用 Execute 时不会持有事务或锁。
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var (
	// ErrFull 表示未完成任务数已达到容量上限。
	ErrFull = errors.New("job queue is full")
	// ErrNotFound 表示指定任务不存在。
	ErrNotFound = errors.New("job not found")
)

const (
	stateQueued     = "queued"
	stateRunning    = "running"
	stateProcessing = "processing"
	stateCompleted  = "completed"
	stateDone       = "done"
	stateFailed     = "failed"
	dbBudget        = 3 * time.Second
	pollInterval    = 100 * time.Millisecond
)

// Execute 调用上游完成任务。实现应响应 context 取消。
type Execute func(context.Context, json.RawMessage) (json.RawMessage, error)

// Job 是提交结果和查询结果的稳定视图。
type Job struct {
	ID            string          `json:"id"`
	State         string          `json:"state"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
	Processed     bool            `json:"processed"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	ResultSummary string          `json:"result_summary,omitempty"`
	ResultLength  int             `json:"result_length,omitempty"`
}

// Store 管理单进程任务队列。
type Store struct {
	db       *sql.DB
	capacity int
	submitMu sync.Mutex // 把容量检查和插入串行化，避免同进程并发提交超限。
	runMu    sync.Mutex // 同一个 Store 同时只允许一个 Run。
}

// Open 打开或创建 SQLite 数据库，并初始化最小任务表。
func Open(path string, capacity int) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	if capacity < 1 {
		return nil, errors.New("capacity must be positive")
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// 本教学网关按单进程运行。单连接既避免 SQLite 写锁竞争，也让
	// 状态更新保持短小；网络调用在数据库连接释放后才开始。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), dbBudget)
	defer cancel()
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 3000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set sqlite busy timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable sqlite WAL: %w", err)
	}
	const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  payload BLOB NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('queued','running','processing','completed','done','failed')),
  result BLOB,
  error TEXT NOT NULL DEFAULT '',
  processed INTEGER NOT NULL DEFAULT 0 CHECK (processed IN (0,1)),
  result_summary TEXT NOT NULL DEFAULT '',
  result_length INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_state_created ON jobs(state, created_at, id);`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize jobs schema: %w", err)
	}
	return &Store{db: db, capacity: capacity}, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// Submit 先把任务写入数据库，再返回任务信息。请求 context 只约束提交，
// worker 后续执行使用 Run 提供的生命周期 context。
func (s *Store) Submit(ctx context.Context, payload json.RawMessage) (job Job, retErr error) {
	if !json.Valid(payload) {
		return Job{}, errors.New("payload must be valid JSON")
	}
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()

	s.submitMu.Lock()
	defer s.submitMu.Unlock()

	conn, err := s.db.Conn(workCtx)
	if err != nil {
		return Job{}, fmt.Errorf("acquire submit connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(workCtx, `BEGIN IMMEDIATE`); err != nil {
		return Job{}, fmt.Errorf("begin submit transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// 清理不能继续使用已取消的提交 context，但也不能无限等待。
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), dbBudget)
			defer cleanupCancel()
			if _, rollbackErr := conn.ExecContext(cleanupCtx, `ROLLBACK`); rollbackErr != nil {
				if retErr == nil {
					retErr = fmt.Errorf("rollback submit transaction: %w", rollbackErr)
				} else {
					// 保留原始业务/数据库错误作为主要错误，同时记录清理失败。
					retErr = fmt.Errorf("%w (rollback also failed: %v)", retErr, rollbackErr)
				}
			}
		}
	}()

	var pending int
	if err := conn.QueryRowContext(workCtx, `SELECT COUNT(*) FROM jobs WHERE state IN ('queued','running','processing','completed')`).Scan(&pending); err != nil {
		return Job{}, fmt.Errorf("count pending jobs: %w", err)
	}
	if pending >= s.capacity {
		return Job{}, ErrFull
	}

	id, err := newID()
	if err != nil {
		return Job{}, fmt.Errorf("create job id: %w", err)
	}
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(workCtx, `INSERT INTO jobs(id,payload,state,created_at,updated_at) VALUES(?,?,?,?,?)`, id, []byte(payload), stateQueued, stamp, stamp); err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	if _, err := conn.ExecContext(workCtx, `COMMIT`); err != nil {
		return Job{}, fmt.Errorf("commit submitted job: %w", err)
	}
	committed = true
	return Job{ID: id, State: stateQueued, CreatedAt: now, UpdatedAt: now}, nil
}

// Get 读取任务当前状态。
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()
	job, err := scanJob(s.db.QueryRowContext(workCtx, `SELECT id,state,result,error,processed,created_at,updated_at,result_summary,result_length FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// Run 恢复可恢复状态后启动固定数量的 worker。它阻塞到 ctx 取消，
// 并等待所有 worker 退出。数据库错误会取消其他 worker 并返回。
func (s *Store) Run(ctx context.Context, workers int, execute Execute) error {
	if workers < 1 {
		return errors.New("workers must be positive")
	}
	if execute == nil {
		return errors.New("execute function is required")
	}
	if !s.runMu.TryLock() {
		return errors.New("store is already running")
	}
	defer s.runMu.Unlock()

	if err := s.recover(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.worker(runCtx, execute); err != nil && runCtx.Err() == nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
			}
		}()
	}
	<-runCtx.Done()
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (s *Store) recover(ctx context.Context) error {
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(workCtx, nil)
	if err != nil {
		return fmt.Errorf("begin recovery transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(workCtx, `UPDATE jobs SET state='queued',updated_at=? WHERE state='running'`, now); err != nil {
		return fmt.Errorf("recover interrupted jobs: %w", err)
	}
	// processing 表示 worker 已认领后处理；结果早已在 completed 阶段保存。
	// 恢复时退回 completed，继续后处理而不再次调用供应商。
	if _, err := tx.ExecContext(workCtx, `UPDATE jobs SET state='completed',updated_at=? WHERE state='processing' AND result IS NOT NULL`, now); err != nil {
		return fmt.Errorf("recover saved results: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit recovery: %w", err)
	}
	return nil
}

func (s *Store) worker(ctx context.Context, execute Execute) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		job, found, err := s.claimCompleted(ctx)
		if err != nil {
			return err
		}
		if found {
			if err := s.finishPostProcessing(ctx, job); err != nil {
				return err
			}
			continue
		}

		queued, found, err := s.claimQueued(ctx)
		if err != nil {
			return err
		}
		if found {
			result, err := execute(ctx, queued.Payload)
			if ctx.Err() != nil {
				// 留下 running 供下一次 Run 恢复，避免把关机误记为业务失败。
				return nil
			}
			if err != nil {
				if dbErr := s.fail(ctx, queued.ID, err); dbErr != nil {
					return dbErr
				}
				continue
			}
			if !json.Valid(result) {
				if dbErr := s.fail(ctx, queued.ID, errors.New("execute returned invalid JSON")); dbErr != nil {
					return dbErr
				}
				continue
			}
			if err := s.saveResult(ctx, queued.ID, result); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type claimedJob struct {
	ID      string
	Payload json.RawMessage
}

// claimQueued 用一个条件 UPDATE 原子认领工作。供应商调用发生在该语句
// 完成之后，因此 worker 之间不会对同一 queued 行同时执行。
func (s *Store) claimQueued(ctx context.Context) (claimedJob, bool, error) {
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()
	var job claimedJob
	var payload []byte
	err := s.db.QueryRowContext(workCtx, `UPDATE jobs SET state='running',updated_at=? WHERE id=(SELECT id FROM jobs WHERE state='queued' ORDER BY created_at,id LIMIT 1) AND state='queued' RETURNING id,payload`, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&job.ID, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return claimedJob{}, false, nil
	}
	if err != nil {
		return claimedJob{}, false, fmt.Errorf("claim queued job: %w", err)
	}
	job.Payload = append(json.RawMessage(nil), payload...)
	return job, true, nil
}

// claimCompleted 原子认领一条已保存结果的任务，交给确定性后处理。
// 状态更新和读取在同一条 UPDATE ... RETURNING 中完成，因此多个 worker
// 不会同时处理同一条结果。
func (s *Store) claimCompleted(ctx context.Context) (Job, bool, error) {
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()
	job, err := scanJob(s.db.QueryRowContext(workCtx, `UPDATE jobs SET state='processing',updated_at=? WHERE id=(SELECT id FROM jobs WHERE state='completed' ORDER BY created_at,id LIMIT 1) AND state='completed' RETURNING id,state,result,error,processed,created_at,updated_at,result_summary,result_length`, time.Now().UTC().Format(time.RFC3339Nano)))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("load completed job: %w", err)
	}
	return job, true, nil
}

func (s *Store) saveResult(ctx context.Context, id string, result json.RawMessage) error {
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()
	res, err := s.db.ExecContext(workCtx, `UPDATE jobs SET state='completed',result=?,error='',updated_at=? WHERE id=? AND state='running'`, []byte(result), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("save job result: %w", err)
	}
	if err := requireOneRow(res, "save job result"); err != nil {
		return err
	}
	return nil
}

func (s *Store) finishPostProcessing(ctx context.Context, job Job) error {
	result := string(job.Result)
	summary := result
	if utf8.RuneCountInString(summary) > 120 {
		runes := []rune(summary)
		summary = string(runes[:120])
	}
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()
	res, err := s.db.ExecContext(workCtx, `UPDATE jobs SET state='done',processed=1,result_summary=?,result_length=?,updated_at=? WHERE id=? AND state='processing' AND processed=0`, summary, len(job.Result), time.Now().UTC().Format(time.RFC3339Nano), job.ID)
	if err != nil {
		return fmt.Errorf("post-process job: %w", err)
	}
	return requireOneRow(res, "post-process job")
}

func (s *Store) fail(ctx context.Context, id string, cause error) error {
	workCtx, cancel := withDBBudget(ctx)
	defer cancel()
	message := cause.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	res, err := s.db.ExecContext(workCtx, `UPDATE jobs SET state='failed',error=?,updated_at=? WHERE id=? AND state='running'`, message, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("persist failed job: %w", err)
	}
	return requireOneRow(res, "persist failed job")
}

func scanJob(row *sql.Row) (Job, error) {
	var job Job
	var result []byte
	var processed int
	var created, updated string
	err := row.Scan(&job.ID, &job.State, &result, &job.Error, &processed, &created, &updated, &job.ResultSummary, &job.ResultLength)
	if err != nil {
		return Job{}, err
	}
	if result != nil {
		job.Result = append(json.RawMessage(nil), result...)
	}
	job.Processed = processed != 0
	job.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Job{}, fmt.Errorf("parse job creation time: %w", err)
	}
	job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return Job{}, fmt.Errorf("parse job update time: %w", err)
	}
	return job, nil
}

func requireOneRow(result sql.Result, operation string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s affected rows: %w", operation, err)
	}
	if count != 1 {
		return fmt.Errorf("%s: expected one affected row, got %d", operation, count)
	}
	return nil
}

func withDBBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, dbBudget)
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
