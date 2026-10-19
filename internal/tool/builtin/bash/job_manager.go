package bash

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusKilled    JobStatus = "killed"
)

type JobMeta struct {
	JobID      string    `json:"job_id"`
	Command    string    `json:"command"`
	SessionID  string    `json:"session_id,omitempty"`
	WorkDir    string    `json:"work_dir"`
	Status     JobStatus `json:"status"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	PID        int       `json:"pid"`
}

type jobProcess struct {
	meta JobMeta
	cmd  *exec.Cmd
	file *os.File
	mu   sync.RWMutex
}

func (proc *jobProcess) handleWaitResult(waitErr error) {
	proc.mu.Lock()
	defer proc.mu.Unlock()

	if proc.meta.Status == JobStatusKilled {
		proc.meta.FinishedAt = time.Now()
		return
	}

	if waitErr == nil {
		proc.meta.Status = JobStatusCompleted
		proc.meta.ExitCode = 0
	} else {
		proc.meta.Status = JobStatusFailed
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			proc.meta.ExitCode = exitErr.ExitCode()
		} else {
			proc.meta.ExitCode = -1
		}
	}
	proc.meta.FinishedAt = time.Now()
}

type JobManager struct {
	mu          sync.RWMutex
	jobsDir     string
	jobs        map[string]*jobProcess
	maxLogBytes int64
}

// SetMaxLogBytes 设置单个后台任务日志上限（R2-16）。
func (jm *JobManager) SetMaxLogBytes(max int64) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	jm.maxLogBytes = max
}

var (
	defaultJobManagerOnce sync.Once                  //nolint:gochecknoglobals
	defaultJobManager     atomic.Pointer[JobManager] //nolint:gochecknoglobals
)

// GetDefaultJobManager 返回进程级共享的 JobManager 单例。
func GetDefaultJobManager() *JobManager {
	if jm := defaultJobManager.Load(); jm != nil {
		return jm
	}
	defaultJobManagerOnce.Do(func() {
		defaultJobManager.Store(NewJobManager(""))
	})
	return defaultJobManager.Load()
}

// NewJobManager 创建 JobManager 实例。dir 为空时使用 os.TempDir()/polaris_jobs。
func NewJobManager(dir string) *JobManager {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "polaris_jobs")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("bash job_manager: mkdir failed", "dir", dir, "err", err)
	}
	return &JobManager{
		jobsDir:     dir,
		jobs:        make(map[string]*jobProcess),
		maxLogBytes: defaultMaxLogBytes,
	}
}

// StartJob 启动一个后台进程任务，记录元数据并实时重定向输出到日志文件。
func (jm *JobManager) StartJob(
	ctx context.Context,
	cmdStr string,
	workDir string,
	sessionID string,
	env []string,
) (*JobMeta, error) {
	if cmdStr == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "job_manager: command is empty")
	}

	rawID := uuid.NewString()
	jobID := "job_" + rawID
	jobPath := filepath.Join(jm.jobsDir, jobID)
	if err := os.MkdirAll(jobPath, 0o700); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "job_manager: create job dir", err)
	}

	logFilePath := filepath.Join(jobPath, "output.log")
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "job_manager: create log file", err)
	}

	jm.mu.RLock()
	maxLog := jm.maxLogBytes
	jm.mu.RUnlock()
	writer := newLimitedLogWriter(logFile, maxLog)

	cmd := exec.Command("bash", "-c", cmdStr)
	cmd.Dir = workDir
	cmd.Env = env
	cmd.Stdout = writer
	cmd.Stderr = writer
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, apperr.Wrap(apperr.CodeInternal, "job_manager: start process", err)
	}

	meta := JobMeta{
		JobID:     jobID,
		Command:   cmdStr,
		SessionID: sessionID,
		WorkDir:   workDir,
		Status:    JobStatusRunning,
		StartedAt: time.Now(),
		PID:       cmd.Process.Pid,
	}

	proc := &jobProcess{
		meta: meta,
		cmd:  cmd,
		file: logFile,
	}

	jm.mu.Lock()
	jm.jobs[jobID] = proc
	jm.mu.Unlock()

	if err := jm.writeMetaLocked(jobPath, meta); err != nil {
		slog.Warn("bash job_manager: write initial meta failed", "job_id", jobID, "err", err)
	}

	concurrent.SafeGo(context.Background(), "job_manager.waiter", func(context.Context) {
		waitErr := cmd.Wait()
		_ = logFile.Close()
		proc.handleWaitResult(waitErr)
		proc.mu.RLock()
		m := proc.meta
		proc.mu.RUnlock()
		if err := jm.writeMetaLocked(jobPath, m); err != nil {
			slog.Warn("bash job_manager: write final meta failed", "job_id", proc.meta.JobID, "err", err)
		}
	})

	metaCopy := meta
	return &metaCopy, nil
}

func (jm *JobManager) writeMetaLocked(jobPath string, meta JobMeta) error {
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "job_manager: marshal meta", err)
	}
	if err := os.WriteFile(filepath.Join(jobPath, "meta.json"), metaBytes, 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "job_manager: write meta", err)
	}
	return nil
}

func loadDiskJob(jobID, jobsDir string) (*JobMeta, error) {
	metaPath := filepath.Join(jobsDir, jobID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, "job_manager: job not found: "+jobID, err)
	}

	var meta JobMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "job_manager: parse meta.json", err)
	}
	return &meta, nil
}

// GetJob 获取单个任务元数据。
func (jm *JobManager) GetJob(jobID string) (*JobMeta, error) {
	if err := validateJobID(jobID); err != nil {
		return nil, err
	}

	jm.mu.RLock()
	proc, ok := jm.jobs[jobID]
	jm.mu.RUnlock()

	if ok {
		proc.mu.RLock()
		metaCopy := proc.meta
		proc.mu.RUnlock()
		return &metaCopy, nil
	}

	return loadDiskJob(jobID, jm.jobsDir)
}

// ListJobs 列出任务列表，可选按 sessionID 过滤。
func (jm *JobManager) ListJobs(sessionID string) ([]JobMeta, error) {
	resultsMap := make(map[string]JobMeta)

	jm.mu.RLock()
	for id, proc := range jm.jobs {
		proc.mu.RLock()
		meta := proc.meta
		proc.mu.RUnlock()
		if sessionID == "" || meta.SessionID == sessionID {
			resultsMap[id] = meta
		}
	}
	jm.mu.RUnlock()

	entries, err := os.ReadDir(jm.jobsDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			jobID := e.Name()
			if _, exists := resultsMap[jobID]; exists {
				continue
			}
			meta, err := loadDiskJob(jobID, jm.jobsDir)
			if err == nil && (sessionID == "" || meta.SessionID == sessionID) {
				resultsMap[jobID] = *meta
			}
		}
	}

	out := make([]JobMeta, 0, len(resultsMap))
	for _, meta := range resultsMap {
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

// GetJobOutput 获取指定任务的尾部日志内容（tail，R2-16 从尾部按字节倒读）。
func (jm *JobManager) GetJobOutput(jobID string, tailLines int) (string, error) {
	if err := validateJobID(jobID); err != nil {
		return "", err
	}
	if tailLines <= 0 {
		tailLines = 200
	}

	logPath := filepath.Join(jm.jobsDir, jobID, "output.log")
	file, err := os.Open(logPath)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeNotFound, "job_manager: log file not found", err)
	}
	defer file.Close()

	return readTailLines(file, tailLines)
}

// KillJob 终止一个正在运行的后台任务。
func (jm *JobManager) KillJob(jobID string) (*JobMeta, error) {
	if err := validateJobID(jobID); err != nil {
		return nil, err
	}

	jm.mu.RLock()
	proc, ok := jm.jobs[jobID]
	jm.mu.RUnlock()

	if !ok {
		meta, err := jm.GetJob(jobID)
		if err != nil {
			return nil, err
		}
		return meta, nil
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()

	if proc.meta.Status == JobStatusRunning {
		if proc.cmd != nil {
			if kErr := killProcessGroup(proc.cmd); kErr != nil && !errors.Is(kErr, os.ErrProcessDone) {
				slog.Debug("bash job_manager: kill process failed", "job_id", jobID, "err", kErr)
			}
		}
		proc.meta.Status = JobStatusKilled
		proc.meta.FinishedAt = time.Now()
		if wErr := jm.writeMetaLocked(filepath.Join(jm.jobsDir, jobID), proc.meta); wErr != nil {
			slog.Warn("bash job_manager: write killed meta failed", "job_id", jobID, "err", wErr)
		}
	}

	metaCopy := proc.meta
	return &metaCopy, nil
}

// CleanupSessionJobs 终止归属于指定 sessionID 的所有运行中任务。
func (jm *JobManager) CleanupSessionJobs(sessionID string) {
	if sessionID == "" {
		return
	}

	jm.mu.RLock()
	var toKill []*jobProcess
	for _, proc := range jm.jobs {
		proc.mu.RLock()
		if proc.meta.SessionID == sessionID && proc.meta.Status == JobStatusRunning {
			toKill = append(toKill, proc)
		}
		proc.mu.RUnlock()
	}
	jm.mu.RUnlock()

	for _, proc := range toKill {
		proc.mu.Lock()
		if proc.meta.Status == JobStatusRunning {
			if proc.cmd != nil {
				if kErr := killProcessGroup(proc.cmd); kErr != nil && !errors.Is(kErr, os.ErrProcessDone) {
					slog.Debug("bash job_manager: kill session process failed", "job_id", proc.meta.JobID, "err", kErr)
				}
			}
			proc.meta.Status = JobStatusKilled
			proc.meta.FinishedAt = time.Now()
			if wErr := jm.writeMetaLocked(filepath.Join(jm.jobsDir, proc.meta.JobID), proc.meta); wErr != nil {
				slog.Warn("bash job_manager: write session killed meta failed", "job_id", proc.meta.JobID, "err", wErr)
			}
		}
		proc.mu.Unlock()
	}
}
