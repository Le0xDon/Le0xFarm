// Package supervisor manages direct child-process lifecycles for resolved plans.
package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

const defaultTail = 64 * 1024

type Config struct {
	StopGrace      time.Duration
	RestartInitial time.Duration
	RestartMax     time.Duration
	CrashLimit     int
	CrashWindow    time.Duration
	TailBytes      int
	After          func(time.Duration) <-chan time.Time
}
type Snapshot struct {
	ExecutionID  identity.ExecutionID
	State        model.ExecutionStatus
	PID          int
	StartedAt    time.Time
	ExitCode     *int
	RestartCount uint32
	LastError    string
	Stdout       string
	Stderr       string
}
type Supervisor struct {
	mu         sync.Mutex
	cfg        Config
	executions map[identity.ExecutionID]*execution
	closed     bool
}
type execution struct {
	plan           model.ExecutionPlan
	state          model.ExecutionStatus
	cmd            *exec.Cmd
	pid            int
	started        time.Time
	exitCode       *int
	restarts       uint32
	lastErr        string
	stdout, stderr *tail
	desired        bool
	done           chan struct{}
	generation     uint64
	crashes        []time.Time
	backoffCancel  chan struct{}
}
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *tail) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}
func (w *tail) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.buf...))
}

func New(cfg Config) *Supervisor {
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 5 * time.Second
	}
	if cfg.RestartInitial <= 0 {
		cfg.RestartInitial = time.Second
	}
	if cfg.RestartMax <= 0 {
		cfg.RestartMax = 30 * time.Second
	}
	if cfg.CrashLimit <= 0 {
		cfg.CrashLimit = 5
	}
	if cfg.CrashWindow <= 0 {
		cfg.CrashWindow = 10 * time.Minute
	}
	if cfg.TailBytes <= 0 {
		cfg.TailBytes = defaultTail
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	return &Supervisor{cfg: cfg, executions: map[identity.ExecutionID]*execution{}}
}
func validate(p model.ExecutionPlan) error {
	if err := p.ExecutionID.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid ExecutionID", err)
	}
	if p.Executable == "" || !filepath.IsAbs(p.Executable) || filepath.Clean(p.Executable) != p.Executable || bytes.IndexByte([]byte(p.Executable), 0) >= 0 {
		return typed(farmerr.CONFIG_CONFLICT, "executable must be a clean absolute path", nil)
	}
	switch filepath.Base(p.Executable) {
	case "sh", "bash", "dash", "zsh", "ksh":
		return typed(farmerr.CONFIG_CONFLICT, "shell executables are not allowed", nil)
	}
	info, err := os.Stat(p.Executable)
	if err != nil {
		return typed(farmerr.MISSING_COMMAND, "executable is unavailable", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return typed(farmerr.PERMISSION_DENIED, "executable is not an executable regular file", nil)
	}
	if p.WorkingDirectory != "" {
		if !filepath.IsAbs(p.WorkingDirectory) || filepath.Clean(p.WorkingDirectory) != p.WorkingDirectory {
			return typed(farmerr.CONFIG_CONFLICT, "working directory must be a clean absolute path", nil)
		}
		i, e := os.Stat(p.WorkingDirectory)
		if e != nil || !i.IsDir() {
			return typed(farmerr.CONFIG_CONFLICT, "working directory is invalid", e)
		}
	}
	for _, a := range p.Args {
		if bytes.IndexByte([]byte(a), 0) >= 0 {
			return typed(farmerr.CONFIG_CONFLICT, "argument contains NUL", nil)
		}
	}
	for k, v := range p.Environment {
		if k == "" || bytes.IndexByte([]byte(k), 0) >= 0 || bytes.IndexByte([]byte(k), '=') >= 0 || bytes.IndexByte([]byte(v), 0) >= 0 {
			return typed(farmerr.CONFIG_CONFLICT, "environment entry is invalid", nil)
		}
	}
	if p.RestartPolicy != "" && p.RestartPolicy != model.RestartNever && p.RestartPolicy != model.RestartOnFailure {
		return typed(farmerr.CONFIG_CONFLICT, "restart policy is invalid", nil)
	}
	return nil
}
func (s *Supervisor) Start(p model.ExecutionPlan) (Snapshot, string, error) {
	if err := validate(p); err != nil {
		return Snapshot{}, "", err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Snapshot{}, "", typed(farmerr.CONFIG_CONFLICT, "supervisor is shutting down", nil)
	}
	if e, ok := s.executions[p.ExecutionID]; ok {
		if !reflect.DeepEqual(e.plan, p) {
			s.mu.Unlock()
			return Snapshot{}, "", typed(farmerr.CONFIG_CONFLICT, "ExecutionID already has a different plan", nil)
		}
		if e.desired && (e.state == model.ExecutionRunning || e.state == model.ExecutionStarting || e.state == model.ExecutionBackoff) {
			snap := snapshot(e)
			s.mu.Unlock()
			return snap, "already running", nil
		}
		e.desired = true
		cancelBackoff(e)
		e.crashes = nil
		e.lastErr = ""
		e.exitCode = nil
		e.generation++
		gen := e.generation
		s.mu.Unlock()
		return s.startExisting(e, gen, "started")
	}
	e := &execution{plan: p, state: model.ExecutionStarting, desired: true, stdout: &tail{max: s.cfg.TailBytes}, stderr: &tail{max: s.cfg.TailBytes}, generation: 1}
	s.executions[p.ExecutionID] = e
	s.mu.Unlock()
	return s.startExisting(e, 1, "started")
}
func (s *Supervisor) startExisting(e *execution, gen uint64, msg string) (Snapshot, string, error) {
	cmd := exec.Command(e.plan.Executable, e.plan.Args...)
	cmd.Dir = e.plan.WorkingDirectory
	cmd.Env = environment(e.plan.Environment)
	cmd.Stdout = e.stdout
	cmd.Stderr = e.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	s.mu.Lock()
	if !e.desired || e.generation != gen {
		snap := snapshot(e)
		s.mu.Unlock()
		return snap, "stopped", nil
	}
	e.state = model.ExecutionStarting
	e.done = make(chan struct{})
	e.cmd = cmd
	if err := cmd.Start(); err != nil {
		e.state = model.ExecutionFailed
		e.lastErr = err.Error()
		e.desired = false
		close(e.done)
		snap := snapshot(e)
		s.mu.Unlock()
		code := farmerr.INTERNAL_ERROR
		if errors.Is(err, os.ErrPermission) {
			code = farmerr.PERMISSION_DENIED
		} else if errors.Is(err, os.ErrNotExist) {
			code = farmerr.MISSING_COMMAND
		}
		return snap, "", typed(code, "process start failed", err)
	}
	e.pid = cmd.Process.Pid
	e.started = time.Now().UTC()
	e.state = model.ExecutionRunning
	snap := snapshot(e)
	s.mu.Unlock()
	go s.wait(e, cmd, gen)
	return snap, msg, nil
}
func environment(extra map[string]string) []string {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}
func (s *Supervisor) wait(e *execution, cmd *exec.Cmd, gen uint64) {
	err := cmd.Wait()
	now := time.Now().UTC()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	s.mu.Lock()
	if e.cmd != cmd || e.generation != gen {
		s.mu.Unlock()
		return
	}
	e.exitCode = &exit
	e.pid = 0
	close(e.done)
	if !e.desired {
		e.state = model.ExecutionStopped
		s.mu.Unlock()
		return
	}
	e.state = model.ExecutionCrashed
	e.lastErr = fmt.Sprintf("process exited with code %d", exit)
	cut := now.Add(-s.cfg.CrashWindow)
	kept := e.crashes[:0]
	for _, t := range e.crashes {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	e.crashes = append(kept, now)
	if e.plan.RestartPolicy != model.RestartOnFailure {
		e.desired = false
		e.state = model.ExecutionCrashed
		s.mu.Unlock()
		return
	}
	if len(e.crashes) >= s.cfg.CrashLimit {
		e.desired = false
		e.state = model.ExecutionFailed
		e.lastErr = string(farmerr.PROCESS_CRASHED) + ": crash loop limit reached"
		s.mu.Unlock()
		return
	}
	delay := s.cfg.RestartInitial << (len(e.crashes) - 1)
	if delay > s.cfg.RestartMax {
		delay = s.cfg.RestartMax
	}
	e.state = model.ExecutionBackoff
	e.restarts++
	e.generation++
	next := e.generation
	cancel := make(chan struct{})
	e.backoffCancel = cancel
	s.mu.Unlock()
	go func() {
		select {
		case <-s.cfg.After(delay):
		case <-cancel:
			return
		}
		s.mu.Lock()
		if e.backoffCancel == cancel {
			e.backoffCancel = nil
		}
		ok := e.desired && e.generation == next && !s.closed
		s.mu.Unlock()
		if ok {
			_, _, _ = s.startExisting(e, next, "watchdog restart")
		}
	}()
}
func (s *Supervisor) Stop(id identity.ExecutionID) (Snapshot, string, error) {
	if err := id.Validate(); err != nil {
		return Snapshot{}, "", typed(farmerr.CONFIG_CONFLICT, "invalid ExecutionID", err)
	}
	s.mu.Lock()
	e, ok := s.executions[id]
	if !ok {
		s.mu.Unlock()
		return Snapshot{ExecutionID: id, State: model.ExecutionStopped}, "already stopped", nil
	}
	e.desired = false
	cancelBackoff(e)
	cmd := e.cmd
	done := e.done
	if e.state == model.ExecutionStopped || e.pid == 0 {
		e.generation++
		e.state = model.ExecutionStopped
		snap := snapshot(e)
		s.mu.Unlock()
		return snap, "already stopped", nil
	}
	e.state = model.ExecutionStopping
	pid := e.pid
	s.mu.Unlock()
	if cmd != nil && pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(s.cfg.StopGrace):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-done
		}
	}
	s.mu.Lock()
	e.state = model.ExecutionStopped
	e.pid = 0
	snap := snapshot(e)
	s.mu.Unlock()
	return snap, "stopped", nil
}
func (s *Supervisor) Restart(id identity.ExecutionID) (Snapshot, string, error) {
	if err := id.Validate(); err != nil {
		return Snapshot{}, "", typed(farmerr.CONFIG_CONFLICT, "invalid ExecutionID", err)
	}
	s.mu.Lock()
	e, ok := s.executions[id]
	if !ok {
		s.mu.Unlock()
		return Snapshot{}, "", typed(farmerr.CONFIG_CONFLICT, "unknown ExecutionID", nil)
	}
	plan := e.plan
	count := e.restarts + 1
	s.mu.Unlock()
	if _, _, err := s.Stop(id); err != nil {
		return Snapshot{}, "", err
	}
	snap, msg, err := s.Start(plan)
	s.mu.Lock()
	if cur := s.executions[id]; cur != nil && cur.restarts < count {
		cur.restarts = count
		snap = snapshot(cur)
	}
	s.mu.Unlock()
	return snap, msg, err
}
func (s *Supervisor) Get(id identity.ExecutionID) (Snapshot, bool) {
	if id.Validate() != nil {
		return Snapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.executions[id]
	if !ok {
		return Snapshot{}, false
	}
	return snapshot(e), true
}
func (s *Supervisor) List() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Snapshot, 0, len(s.executions))
	for _, e := range s.executions {
		out = append(out, snapshot(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExecutionID.String() < out[j].ExecutionID.String() })
	return out
}
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	ids := make([]identity.ExecutionID, 0, len(s.executions))
	for id := range s.executions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		done := make(chan struct{})
		go func(id identity.ExecutionID) { _, _, _ = s.Stop(id); close(done) }(id)
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func snapshot(e *execution) Snapshot {
	return Snapshot{ExecutionID: e.plan.ExecutionID, State: e.state, PID: e.pid, StartedAt: e.started, ExitCode: e.exitCode, RestartCount: e.restarts, LastError: e.lastErr, Stdout: e.stdout.String(), Stderr: e.stderr.String()}
}

func cancelBackoff(e *execution) {
	if e.backoffCancel != nil {
		close(e.backoffCancel)
		e.backoffCancel = nil
	}
}
func typed(code farmerr.Code, msg string, err error) error {
	d := map[string]string{}
	if err != nil {
		d["reason"] = err.Error()
	}
	return farmerr.Error{Code: code, HumanMessage: msg, Details: d}
}
