package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("LE0X_SUPERVISOR_HELPER") != "1" {
		return
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	mode := os.Args[separator]
	switch mode {
	case "output":
		fmt.Fprint(os.Stdout, "stdout-marker")
		fmt.Fprint(os.Stderr, "stderr-marker")
	case "large-output":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 256)+"tail-marker")
	case "crash":
		os.Exit(7)
	case "term":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		fmt.Fprint(os.Stdout, "ready\n")
		<-ch
		fmt.Fprint(os.Stdout, "term-received")
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprint(os.Stdout, "ready\n")
		time.Sleep(time.Minute)
	case "group":
		child := exec.Command("/bin/sleep", "60")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stdout, "child=%d\n", child.Process.Pid)
		_ = child.Wait()
	case "env":
		fmt.Fprintf(os.Stdout, "explicit=%s inherited=%s", os.Getenv("LE0X_EXPLICIT"), os.Getenv("LE0X_PARENT_SECRET"))
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

func helperPlan(t *testing.T, mode string, policy model.RestartPolicy) model.ExecutionPlan {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.NewExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	return model.ExecutionPlan{
		ExecutionID:   id,
		Executable:    executable,
		Args:          []string{"-test.run=TestHelperProcess", "--", mode},
		Environment:   map[string]string{"LE0X_SUPERVISOR_HELPER": "1"},
		RestartPolicy: policy,
	}
}

func sleepPlan(t *testing.T) model.ExecutionPlan {
	t.Helper()
	id, err := identity.NewExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	return model.ExecutionPlan{ExecutionID: id, Executable: "/bin/sleep", Args: []string{"60"}, RestartPolicy: model.RestartNever}
}

func waitFor(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not reached")
}

func TestStartSleepIdempotencyStopAndRestart(t *testing.T) {
	s := New(Config{StopGrace: 100 * time.Millisecond})
	defer s.Shutdown(context.Background())
	plan := sleepPlan(t)
	first, _, err := s.Start(plan)
	if err != nil || first.State != model.ExecutionRunning || first.PID <= 0 {
		t.Fatalf("start: %+v %v", first, err)
	}
	if err := syscall.Kill(first.PID, 0); err != nil {
		t.Fatalf("process is not running: %v", err)
	}
	duplicate, message, err := s.Start(plan)
	if err != nil || message != "already running" || duplicate.PID != first.PID {
		t.Fatalf("duplicate start: %+v %q %v", duplicate, message, err)
	}
	conflict := plan
	conflict.Args = []string{"61"}
	if _, _, err = s.Start(conflict); code(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("different plan error=%v", err)
	}
	restarted, _, err := s.Restart(plan.ExecutionID)
	if err != nil || restarted.PID <= 0 || restarted.PID == first.PID || restarted.RestartCount != 1 {
		t.Fatalf("restart: %+v %v", restarted, err)
	}
	stopped, _, err := s.Stop(plan.ExecutionID)
	if err != nil || stopped.State != model.ExecutionStopped || stopped.PID != 0 {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	time.Sleep(20 * time.Millisecond)
	if snap, _ := s.Get(plan.ExecutionID); snap.State != model.ExecutionStopped || snap.PID != 0 {
		t.Fatalf("explicit stop restarted process: %+v", snap)
	}
	unknown, _ := identity.NewExecutionID()
	if _, message, err = s.Stop(unknown); err != nil || message != "already stopped" {
		t.Fatalf("nonexistent stop: %q %v", message, err)
	}
	if _, _, err = s.Restart(unknown); code(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("unknown restart error=%v", err)
	}
}

func TestPlanValidationRejectsRelativePathsAndShells(t *testing.T) {
	id, _ := identity.NewExecutionID()
	s := New(Config{})
	for _, plan := range []model.ExecutionPlan{
		{ExecutionID: id, Executable: "sleep"},
		{ExecutionID: id, Executable: "/bin/sh", Args: []string{"-c", "sleep 1"}},
	} {
		if _, _, err := s.Start(plan); code(err) != farmerr.CONFIG_CONFLICT {
			t.Fatalf("plan accepted: %+v error=%v", plan, err)
		}
	}
	missing := model.ExecutionPlan{ExecutionID: id, Executable: "/definitely/not/a/le0xfarm-command"}
	if _, _, err := s.Start(missing); code(err) != farmerr.MISSING_COMMAND {
		t.Fatalf("missing executable error=%v", err)
	}
	notExecutable := t.TempDir() + "/program"
	if err := os.WriteFile(notExecutable, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Start(model.ExecutionPlan{ExecutionID: id, Executable: notExecutable}); code(err) != farmerr.PERMISSION_DENIED {
		t.Fatalf("non-executable error=%v", err)
	}
}

func TestChildEnvironmentIsBoundedAndDoesNotInheritParentSecrets(t *testing.T) {
	t.Setenv("LE0X_PARENT_SECRET", "must-not-leak")
	s := New(Config{})
	defer s.Shutdown(context.Background())
	plan := helperPlan(t, "env", model.RestartNever)
	plan.Environment["LE0X_EXPLICIT"] = "allowed"
	if _, _, err := s.Start(plan); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		snapshot, _ := s.Get(plan.ExecutionID)
		return snapshot.State == model.ExecutionStopped || snapshot.State == model.ExecutionCrashed
	})
	snapshot, _ := s.Get(plan.ExecutionID)
	if !strings.Contains(snapshot.Stdout, "explicit=allowed inherited=") || strings.Contains(snapshot.Stdout, "must-not-leak") {
		t.Fatalf("unexpected child environment: %q", snapshot.Stdout)
	}

	tooMany := sleepPlan(t)
	tooMany.Environment = make(map[string]string, 65)
	for i := 0; i < 65; i++ {
		tooMany.Environment[fmt.Sprintf("LE0X_%d", i)] = "x"
	}
	if _, _, err := s.Start(tooMany); code(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("oversized environment accepted: %v", err)
	}
}

func TestGracefulTermAndKillFallback(t *testing.T) {
	t.Run("term", func(t *testing.T) {
		s := New(Config{StopGrace: time.Second})
		plan := helperPlan(t, "term", model.RestartNever)
		if _, _, err := s.Start(plan); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 3*time.Second, func() bool { snap, _ := s.Get(plan.ExecutionID); return strings.Contains(snap.Stdout, "ready") })
		snap, _, err := s.Stop(plan.ExecutionID)
		if err != nil || !strings.Contains(snap.Stdout, "term-received") {
			t.Fatalf("graceful stop: %+v %v", snap, err)
		}
	})
	t.Run("kill", func(t *testing.T) {
		grace := 25 * time.Millisecond
		s := New(Config{StopGrace: grace})
		plan := helperPlan(t, "ignore-term", model.RestartNever)
		if _, _, err := s.Start(plan); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 3*time.Second, func() bool { snap, _ := s.Get(plan.ExecutionID); return strings.Contains(snap.Stdout, "ready") })
		started := time.Now()
		snap, _, err := s.Stop(plan.ExecutionID)
		if err != nil || time.Since(started) < grace || snap.ExitCode == nil || *snap.ExitCode == 0 {
			t.Fatalf("kill fallback: %+v elapsed=%s err=%v", snap, time.Since(started), err)
		}
	})
}

func TestOutputCaptureIsBounded(t *testing.T) {
	s := New(Config{TailBytes: 64})
	plan := helperPlan(t, "output", model.RestartNever)
	if _, _, err := s.Start(plan); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { snap, _ := s.Get(plan.ExecutionID); return snap.State == model.ExecutionCrashed })
	snap, _ := s.Get(plan.ExecutionID)
	if !strings.Contains(snap.Stdout, "stdout-marker") || !strings.Contains(snap.Stderr, "stderr-marker") {
		t.Fatalf("captured output: stdout=%q stderr=%q", snap.Stdout, snap.Stderr)
	}
	large := helperPlan(t, "large-output", model.RestartNever)
	if _, _, err := s.Start(large); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { snap, _ := s.Get(large.ExecutionID); return snap.State == model.ExecutionCrashed })
	snap, _ = s.Get(large.ExecutionID)
	if len(snap.Stdout) > 64 || !strings.HasSuffix(snap.Stdout, "tail-marker") {
		t.Fatalf("tail not bounded: len=%d value=%q", len(snap.Stdout), snap.Stdout)
	}
}

func TestWatchdogRestartStopAndCrashLoop(t *testing.T) {
	s := New(Config{RestartInitial: 2 * time.Millisecond, RestartMax: 4 * time.Millisecond, CrashLimit: 3, CrashWindow: time.Minute})
	plan := helperPlan(t, "crash", model.RestartOnFailure)
	if _, _, err := s.Start(plan); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { snap, _ := s.Get(plan.ExecutionID); return snap.RestartCount >= 1 })
	waitFor(t, 3*time.Second, func() bool { snap, _ := s.Get(plan.ExecutionID); return snap.State == model.ExecutionFailed })
	snap, _ := s.Get(plan.ExecutionID)
	if snap.RestartCount != 2 || !strings.Contains(snap.LastError, string(farmerr.PROCESS_CRASHED)) {
		t.Fatalf("crash loop: %+v", snap)
	}

	stopPlan := helperPlan(t, "crash", model.RestartOnFailure)
	stopper := New(Config{RestartInitial: time.Second, RestartMax: time.Second})
	if _, _, err := stopper.Start(stopPlan); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { snap, _ := stopper.Get(stopPlan.ExecutionID); return snap.State == model.ExecutionBackoff })
	if _, _, err := stopper.Stop(stopPlan.ExecutionID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if snap, _ := stopper.Get(stopPlan.ExecutionID); snap.State != model.ExecutionStopped || snap.PID != 0 {
		t.Fatalf("stop during backoff restarted: %+v", snap)
	}
}

func TestProcessGroupAndShutdownCleanup(t *testing.T) {
	s := New(Config{StopGrace: 100 * time.Millisecond})
	plan := helperPlan(t, "group", model.RestartNever)
	parent, _, err := s.Start(plan)
	if err != nil {
		t.Fatal(err)
	}
	var childPID int
	waitFor(t, 3*time.Second, func() bool {
		snap, _ := s.Get(plan.ExecutionID)
		line := strings.TrimPrefix(strings.TrimSpace(snap.Stdout), "child=")
		childPID, _ = strconv.Atoi(line)
		return childPID > 0
	})
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(parent.PID, 0); !errorsIsNoProcess(err) {
		t.Fatalf("parent remains: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return processGone(childPID) })
}

func TestMaintenanceHoldStopsOnlyManagedAndSuppressesStart(t *testing.T) {
	unmanaged := exec.Command("/bin/sleep", "60")
	if err := unmanaged.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unmanaged.Process.Kill()
		_, _ = unmanaged.Process.Wait()
	})

	s := New(Config{StopGrace: 100 * time.Millisecond})
	defer s.Shutdown(context.Background())
	plan := sleepPlan(t)
	running, _, err := s.Start(plan)
	if err != nil || running.State != model.ExecutionRunning {
		t.Fatalf("managed start=%+v err=%v", running, err)
	}
	active, revision, err := s.ApplyMaintenanceHold(true, 1)
	if err != nil || !active || revision != 1 {
		t.Fatalf("apply hold active=%t revision=%d err=%v", active, revision, err)
	}
	managed, ok := s.Get(plan.ExecutionID)
	if !ok || managed.State != model.ExecutionStopped || managed.PID != 0 {
		t.Fatalf("managed process not exactly stopped: %+v", managed)
	}
	if err := syscall.Kill(unmanaged.Process.Pid, 0); err != nil {
		t.Fatalf("same-name unmanaged process was affected: %v", err)
	}
	if active, revision, err := s.ApplyMaintenanceHold(true, 1); err != nil || !active || revision != 1 {
		t.Fatalf("idempotent enter active=%t revision=%d err=%v", active, revision, err)
	}
	if _, _, err := s.Start(plan); code(err) != farmerr.MAINTENANCE_HOLD {
		t.Fatalf("START during hold error=%v", err)
	}
	if _, _, err := s.Stop(plan.ExecutionID); err != nil {
		t.Fatalf("explicit STOP during hold failed: %v", err)
	}
	if active, revision, err := s.ApplyMaintenanceHold(false, 2); err != nil || active || revision != 2 {
		t.Fatalf("release hold active=%t revision=%d err=%v", active, revision, err)
	}
	if active, revision, err := s.ApplyMaintenanceHold(false, 2); err != nil || active || revision != 2 {
		t.Fatalf("idempotent exit active=%t revision=%d err=%v", active, revision, err)
	}
	if restarted, _, err := s.Start(plan); err != nil || restarted.State != model.ExecutionRunning {
		t.Fatalf("START after hold release=%+v err=%v", restarted, err)
	}
}

func TestMaintenanceHoldWinsPrepareToStartBarrier(t *testing.T) {
	s := New(Config{})
	defer s.Shutdown(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	s.SetStartValidator(func(model.ExecutionPlan) (func(), error) {
		close(entered)
		<-release
		return nil, nil
	})
	plan := sleepPlan(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Start(plan)
		done <- err
	}()
	<-entered
	if _, _, err := s.ApplyMaintenanceHold(true, 1); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; code(err) != farmerr.MAINTENANCE_HOLD {
		t.Fatalf("barrier START error=%v", err)
	}
	if snapshot, ok := s.Get(plan.ExecutionID); !ok || snapshot.PID != 0 || snapshot.State != model.ExecutionStopped {
		t.Fatalf("process crossed hold boundary: %+v", snapshot)
	}
}

func TestMaintenanceHoldCancelsWatchdogRestart(t *testing.T) {
	restart := make(chan time.Time)
	s := New(Config{RestartInitial: time.Second, RestartMax: time.Second, After: func(time.Duration) <-chan time.Time { return restart }})
	defer s.Shutdown(context.Background())
	plan := helperPlan(t, "crash", model.RestartOnFailure)
	if _, _, err := s.Start(plan); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		snapshot, _ := s.Get(plan.ExecutionID)
		return snapshot.State == model.ExecutionBackoff
	})
	if _, _, err := s.ApplyMaintenanceHold(true, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	snapshot, _ := s.Get(plan.ExecutionID)
	if snapshot.State != model.ExecutionStopped || snapshot.PID != 0 {
		t.Fatalf("watchdog defeated hold: %+v", snapshot)
	}
}

func code(err error) farmerr.Code {
	value, _ := farmerr.CodeOf(err)
	return value
}

func errorsIsNoProcess(err error) bool { return err == syscall.ESRCH }

func processGone(pid int) bool {
	if errorsIsNoProcess(syscall.Kill(pid, 0)) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	return os.IsNotExist(err) || (err == nil && strings.Contains(string(stat), ") Z "))
}
