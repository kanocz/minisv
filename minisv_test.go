package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestCSRFGuard(t *testing.T) {
	cases := []struct {
		site, origin string
		want         int
	}{
		{"", "", 200},
		{"same-origin", "", 200},
		{"none", "", 200},
		{"cross-site", "", 403},
		{"same-site", "", 403},
		{"", "http://minisv:3443", 200},
		{"", "http://evil.example", 403},
		{"", "null", 403},
		// behind a reverse proxy Host differs from Origin
		{"same-origin", "https://minisv.example", 200},
	}
	h := csrfGuard(func(w http.ResponseWriter, r *http.Request) {})
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://minisv:3443/api/x/kill", nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h(rec, r)
		if rec.Code != c.want {
			t.Errorf("site=%q origin=%q: got %d, want %d", c.site, c.origin, rec.Code, c.want)
		}
	}
}

func TestTaskNameRe(t *testing.T) {
	for _, ok := range []string{"nginx", "my-task.1_a"} {
		if !taskNameRe.MatchString(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "../etc", "a b", "a/b"} {
		if taskNameRe.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAuthLimiter(t *testing.T) {
	l := &authLimiter{limit: 2, fails: map[string][]time.Time{}}
	l.fail("1.1.1.1")
	if l.blocked("1.1.1.1") {
		t.Fatal("blocked after one failure")
	}
	l.fail("1.1.1.1")
	if !l.blocked("1.1.1.1") || l.blocked("2.2.2.2") {
		t.Fatal("wrong address blocked")
	}
	l.reset("1.1.1.1")
	if l.blocked("1.1.1.1") {
		t.Fatal("blocked after reset")
	}
	l.limit = -1
	l.fail("1.1.1.1")
	l.fail("1.1.1.1")
	l.fail("1.1.1.1")
	if l.blocked("1.1.1.1") {
		t.Fatal("blocked with the limit off")
	}
}

func TestAuthCacheBypassesLimiter(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	h := basicAuth("admin", string(hash), 2)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {}))
	do := func(pass string) int {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.SetBasicAuth("admin", pass)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if c := do("secret"); c != 200 {
		t.Fatalf("valid login: %d", c)
	}
	do("wrong")
	do("wrong")
	if c := do("wrong"); c != 429 {
		t.Fatalf("after failures: %d, want 429", c)
	}
	// the already logged in client keeps working
	if c := do("secret"); c != 200 {
		t.Fatalf("cached login while blocked: %d", c)
	}
}

func TestWaitDelayIsNormalExit(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := exec.Command("sh", "-c", "sleep 2 & exit 0")
	cmd.Stdout = out
	cmd.WaitDelay = 100 * time.Millisecond
	done, err := startCmd(cmd)
	if nil != err {
		t.Fatal(err)
	}
	if err := <-done; nil != err {
		t.Fatalf("got %v, want nil", err)
	}
	if !strings.Contains(out.String(), "outlived") {
		t.Fatalf("no note in the log: %q", out.String())
	}
}

func TestStaticNoListing(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticHandler())
	for path, want := range map[string]int{"/static/": 404, "/static/favicon.svg": 200} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s: got %d, want %d", path, rec.Code, want)
		}
	}
}

func TestSignalGroupRefusesWaitedProcess(t *testing.T) {
	// two TERMs back to back coalesce in the kernel, so a test can't tell
	// one delivery from two; this checks delivery and the post-Wait guard
	out := &bytes.Buffer{}
	cmd := exec.Command("sh", "-c", "trap 'echo got; exit 0' TERM; while :; do sleep 0.1; done")
	cmd.Stdout = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	done, err := startCmd(cmd)
	if nil != err {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := signalGroup(cmd, syscall.SIGTERM); nil != err {
		t.Fatal(err)
	}
	<-done
	if !strings.Contains(out.String(), "got") {
		t.Fatalf("output %q, TERM not delivered", out.String())
	}
	// after Wait nothing is signalled, the pid may belong to someone else
	if err := signalGroup(cmd, syscall.SIGTERM); nil == err {
		t.Fatal("signal sent to a waited process")
	}
}

func TestTermChildSkipsExitedProcess(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := exec.Command("true")
	cmd.Stdout = out
	done, err := startCmd(cmd)
	if nil != err {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // exit status is waiting in done
	start := time.Now()
	termChild(true, cmd, done, 5, out, nil)
	if time.Since(start) > time.Second || strings.Contains(out.String(), "Error sending") {
		t.Fatalf("termChild signalled an exited process: %q", out.String())
	}
}

func TestStatusRunning(t *testing.T) {
	task := &Task{}
	if s := task.GetStatus(); s.Running || s.Status != "not started" {
		t.Fatalf("fresh task: %+v", s)
	}
	task.status.Store("new instance exited too fast")
	if !task.GetStatus().Running {
		t.Fatal("old instance keeps running after a failed restart")
	}
}

// runLoop starts Loop of a task writing to a temporary log dir
func runLoop(t *testing.T, task *Task) (exit chan bool, wg *sync.WaitGroup, logfile string) {
	t.Helper()
	dir := t.TempDir()
	aConfig.Store(&Config{LogDir: dir})
	task.initChannels()
	exit = make(chan bool)
	wg = &sync.WaitGroup{}
	wg.Add(1)
	go task.Loop(exit, wg)
	return exit, wg, filepath.Join(dir, task.name+".log")
}

func TestLoopNoBusyRestart(t *testing.T) {
	task := &Task{name: "crash", Command: "sh", Args: []string{"-c", "exit 1"}}
	exit, wg, logfile := runLoop(t, task)
	time.Sleep(2500 * time.Millisecond)
	close(exit)
	wg.Wait()
	// the log writer closes the file asynchronously
	time.Sleep(100 * time.Millisecond)

	data, err := os.ReadFile(logfile)
	if nil != err {
		t.Fatal(err)
	}
	// starts at 0, 1 and 2 seconds; without the minimum pause it is thousands
	if n := strings.Count(string(data), "[minisv] Starting"); n < 2 || n > 4 {
		t.Fatalf("%d starts in 2.5s, want 2..4", n)
	}
}

func TestLoopExitAfterFailedRestart(t *testing.T) {
	// the new instance dies during validation, the old one keeps running
	task := &Task{name: "flaky", Command: "sh", Wait: 5, StartTime: 1,
		Args: []string{"-c", "test -e $0 && exit 1; touch $0; exec sleep 30", filepath.Join(t.TempDir(), "x")}}
	exit, wg, _ := runLoop(t, task)
	time.Sleep(200 * time.Millisecond)

	task.rSignal <- true
	time.Sleep(1500 * time.Millisecond)
	if s := task.GetStatus(); s.Status != "new instance exited too fast" {
		t.Fatalf("after restart: %+v", s)
	}

	start := time.Now()
	close(exit)
	wg.Wait()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("exit took %s", took)
	}
}

func TestOneTimeSignalGate(t *testing.T) {
	task := &Task{name: "once", OneTime: true}
	task.initChannels()
	sent := task.signalOneTime(func() { task.sSignal <- true })
	if sent {
		t.Fatal("signal accepted for an idle task")
	}
	// a stop queued while idle is dropped at start
	task.sSignal <- true
	if code, _ := task.startOneTime(); code != 0 || len(task.sSignal) != 0 {
		t.Fatal("start failed or stale stop kept")
	}
	defer oneTimeWg.Done() // no Run here to do it
	if code, _ := task.startOneTime(); code != 409 {
		t.Fatalf("second start: %d, want 409", code)
	}
	exiting.Store(true)
	other := &Task{name: "late", OneTime: true}
	other.initChannels()
	code, _ := other.startOneTime()
	exiting.Store(false)
	if code != 503 {
		t.Fatalf("start while exiting: %d, want 503", code)
	}
	if !task.signalOneTime(func() { task.sSignal <- true }) || len(task.sSignal) != 1 {
		t.Fatal("signal not delivered to a running task")
	}
}

func TestKillDuringRestartSparesNewInstance(t *testing.T) {
	task := &Task{name: "sleep", Command: "sleep", Args: []string{"30"}, Wait: 1, StartTime: 1}
	exit, wg, logfile := runLoop(t, task)
	time.Sleep(200 * time.Millisecond)

	task.rSignal <- true
	task.cSignal <- syscall.SIGKILL // queued while Loop validates the new instance
	time.Sleep(2500 * time.Millisecond)
	if s := task.GetStatus(); s.Status != "restart ok" {
		t.Fatalf("after restart: %+v", s)
	}
	close(exit)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	data, _ := os.ReadFile(logfile)
	// initial start and the restart; a third one means the kill hit the new instance
	if n := strings.Count(string(data), "[minisv] Starting"); n != 2 {
		t.Fatalf("%d starts, want 2:\n%s", n, data)
	}
}

func TestKillDuringOldTerminationSparesNewInstance(t *testing.T) {
	// the old instance ignores TERM, so termChild is busy for Wait seconds
	// after the new one passed validation
	task := &Task{name: "stubborn", Command: "sh", Wait: 2, StartTime: 1,
		Args: []string{"-c", "trap '' TERM; while :; do sleep 1; done"}}
	exit, wg, logfile := runLoop(t, task)
	time.Sleep(200 * time.Millisecond)

	task.rSignal <- true
	time.Sleep(1700 * time.Millisecond) // validation over, old one being terminated
	if s := task.GetStatus(); s.Status != "restart ok" {
		t.Fatalf("during termination: %+v", s)
	}
	task.cSignal <- syscall.SIGKILL
	time.Sleep(2500 * time.Millisecond)
	close(exit)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	data, _ := os.ReadFile(logfile)
	if n := strings.Count(string(data), "[minisv] Starting"); n != 2 {
		t.Fatalf("%d starts, want 2:\n%s", n, data)
	}
}

func TestExitWaitsForOneTimeTask(t *testing.T) {
	task := &Task{name: "once", OneTime: true, Command: "sleep", Args: []string{"30"}, Wait: 1}
	aConfig.Store(&Config{LogDir: t.TempDir()})
	task.initChannels()
	if code, msg := task.startOneTime(); code != 0 {
		t.Fatal("start refused:", msg)
	}
	// Run reads the global at start; swap it before the goroutine exists
	exit := make(chan bool)
	needExit = exit
	go task.Run(nil)
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	close(exit)
	oneTimeWg.Wait()
	needExit = make(chan bool) // Run is done, nobody reads it now
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("exit took %s", took)
	}
	if s := task.GetStatus(); s.Running {
		t.Fatalf("still running after exit: %+v", s)
	}
}

func TestKillAfterStopSparesRestart(t *testing.T) {
	// the process ignores TERM, so stop is busy for Wait seconds
	task := &Task{name: "stubborn", Command: "sh", Wait: 1,
		Args: []string{"-c", "trap '' TERM; while :; do sleep 1; done"}}
	exit, wg, logfile := runLoop(t, task)
	time.Sleep(200 * time.Millisecond)

	task.sSignal <- true
	time.Sleep(200 * time.Millisecond)
	// both queued while stop waits out TERM: the kill in the buffer, the
	// restart in a blocked sender, so the loop sees them ready together
	task.cSignal <- syscall.SIGKILL
	go func() { task.rSignal <- true }()
	time.Sleep(2500 * time.Millisecond)
	if s := task.GetStatus(); !s.Running {
		t.Fatalf("after restart: %+v", s)
	}
	close(exit)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	data, _ := os.ReadFile(logfile)
	if strings.Contains(string(data), "signal: killed") {
		t.Fatalf("stale kill hit the restarted instance:\n%s", data)
	}
}

func TestDropStaleKillKeepsHup(t *testing.T) {
	task := &Task{}
	task.initChannels()
	task.cSignal <- syscall.SIGKILL
	task.dropStaleKill(true)
	if len(task.cSignal) != 0 {
		t.Fatal("kill kept")
	}
	task.cSignal <- syscall.SIGHUP
	task.dropStaleKill(true)
	if len(task.cSignal) != 1 {
		t.Fatal("hup dropped")
	}
	task.dropStaleKill(false)
	if len(task.cSignal) != 0 {
		t.Fatal("hup kept after stop")
	}
	// a handler filling the slot between take and put back must not block Loop
	hup := &Task{}
	hup.initChannels()
	hup.cSignal <- syscall.SIGHUP
	done := make(chan bool)
	go func() {
		// race the put back: the slot is taken by the time hup is returned
		hup.cSignal <- syscall.SIGKILL
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // let the sender queue up on the full buffer
	hup.dropStaleKill(true)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dropStaleKill blocked with a full buffer")
	}
}

func TestLoopExitAfterGracefulRestart(t *testing.T) {
	task := &Task{name: "sleep", Command: "sleep", Args: []string{"30"}, Wait: 5}
	exit, wg, _ := runLoop(t, task)
	time.Sleep(200 * time.Millisecond)

	task.rSignal <- true
	time.Sleep(500 * time.Millisecond)
	if s := task.GetStatus(); s.Status != "restart ok" || !s.Running {
		t.Fatalf("after restart: %+v", s)
	}

	// the old instance already exited; exit must not wait out Wait for it
	start := time.Now()
	close(exit)
	wg.Wait()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("exit took %s", took)
	}
}
