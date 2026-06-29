package session

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func startRaceTestServer(t *testing.T) *sshserver.Server {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}

func raceTestShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "bash"
}

func raceTestShellArgs() []string {
	if runtime.GOOS == "windows" {
		return []string{"-NoLogo", "-NoProfile"}
	}
	return nil
}

func raceTestInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func raceTestInteractiveOutputCommand(s string) string {
	if runtime.GOOS == "windows" {
		return "Write-Output " + s
	}
	return "echo " + s
}

func raceTestSleepCommand(seconds string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "Start-Sleep -Seconds " + seconds}
	}
	return "sleep", []string{seconds}
}

func TestRace_ConcurrentSendInput(t *testing.T) {
	srv := startRaceTestServer(t)

	s, err := New(srv, Config{Command: raceTestShell(), Args: raceTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	time.Sleep(300 * time.Millisecond)

	var wg sync.WaitGroup
	errCount := int64(0)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			err := s.SendInput(raceTestInput(raceTestInteractiveOutputCommand("concurrent"+strings.Repeat(" ", id%3))), false)
			if err != nil {
				atomic.AddInt64(&errCount, 1)
			}
		}(i)
	}
	wg.Wait()

	t.Logf("concurrent sends completed, %d errors (expected ~0)", atomic.LoadInt64(&errCount))
}

func TestRace_SendInputDuringTerminate(t *testing.T) {
	srv := startRaceTestServer(t)

	s, err := New(srv, Config{Command: raceTestShell(), Args: raceTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	var wg sync.WaitGroup
	errCount := int64(0)

	wg.Add(2)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		s.Terminate(true, 0)
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			err := s.SendInput(raceTestInput(raceTestInteractiveOutputCommand("race_test")), false)
			if err != nil {
				atomic.AddInt64(&errCount, 1)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	wg.Wait()

	time.Sleep(200 * time.Millisecond)
	info := s.Info()
	if info.Status != api.SessionExited {
		t.Fatalf("expected 'exited', got %q", info.Status)
	}
	t.Logf("send-during-terminate completed, %d send errors (expected some)", atomic.LoadInt64(&errCount))
}

func TestRace_ConcurrentTerminate(t *testing.T) {
	srv := startRaceTestServer(t)

	command, args := raceTestSleepCommand("60")
	s, err := New(srv, Config{Command: command, Args: args, Mode: api.ModePipe, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Terminate(false, 1*time.Second)
		}()
	}
	wg.Wait()

	time.Sleep(300 * time.Millisecond)
	info := s.Info()
	if info.Status != api.SessionExited {
		t.Fatalf("expected 'exited' after concurrent terminate, got %q", info.Status)
	}
}

func TestRace_ConcurrentReadWrite(t *testing.T) {
	srv := startRaceTestServer(t)

	s, err := New(srv, Config{Command: raceTestShell(), Args: raceTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	time.Sleep(300 * time.Millisecond)

	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				s.SendInput(raceTestInput(raceTestInteractiveOutputCommand("rw"+strings.Repeat(" ", id))), false)
				time.Sleep(20 * time.Millisecond)
			}
		}(i)
	}

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				s.ReadOutput(context.Background(), 200*time.Millisecond, true, 0, 0)
			}
		}()
	}

	wg.Wait()
}

func TestRace_ConcurrentInfoAndTerminate(t *testing.T) {
	srv := startRaceTestServer(t)

	command, args := raceTestSleepCommand("60")
	s, err := New(srv, Config{Command: command, Args: args, Mode: api.ModePipe, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				info := s.Info()
				_ = info.Status
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		s.Terminate(true, 0)
	}()

	wg.Wait()
}
