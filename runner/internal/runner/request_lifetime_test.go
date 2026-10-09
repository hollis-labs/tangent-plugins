package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSuccessfulLaunchSurvivesRequestCompletion(t *testing.T) {
	engine := NewEngine(nil)
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	caller := &fakeTools{}
	engine.SetToolCaller(caller)
	ctx, complete := context.WithCancel(context.Background())
	defer complete()
	result, err := engine.Launch(ctx, LaunchParams{SessionID: "request-lifetime", AgentID: "test-agent", Prompt: "Please confirm you want to proceed?", Command: "cat"})
	if err != nil {
		t.Fatal(err)
	}
	complete()
	deadline := time.Now().Add(2 * time.Second)
	for len(caller.getCalls(EnqueueTurnTool)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(caller.getCalls(EnqueueTurnTool)) == 0 {
		t.Fatal("successful launch lost its session when the request completed")
	}
	if _, err := engine.SendTurn(context.Background(), SendTurnParams{SessionID: result.SessionID, ResponseText: "proceed"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(result.SessionID); err != nil {
		t.Fatal(err)
	}
	assertReaped(t, engine.sessions[result.SessionID])
}

func TestEngineCloseStopsOwnedSessions(t *testing.T) {
	engine := NewEngine(nil)
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, id := range []string{"close-a", "close-b"} {
		if _, err := engine.Launch(context.Background(), LaunchParams{SessionID: id, AgentID: "test-agent", Prompt: "ready", Command: "cat"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	for _, session := range engine.sessions {
		assertReaped(t, session)
	}
	if _, err := engine.Launch(context.Background(), LaunchParams{SessionID: "after-close", AgentID: "test-agent", Prompt: "ready", Command: "cat"}); err == nil {
		t.Fatal("closed engine accepted a launch")
	}
}

func TestCancelledLaunchHasNoSession(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			engine := NewEngine(nil)
			defer func() { _ = engine.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			cancel()
			if _, err := engine.Launch(ctx, LaunchParams{SessionID: "refused", AgentID: "test-agent", Prompt: "ready", Command: "cat"}); !errors.Is(err, want) {
				t.Fatalf("launch error = %v, want %v", err, want)
			}
			if _, exists := engine.sessions["refused"]; exists {
				t.Fatal("cancelled launch installed a session")
			}
		})
	}
}

func TestLaunchCancellationDuringStartupReaps(t *testing.T) {
	for _, closeEngine := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "engine"}[closeEngine], func(t *testing.T) {
			engine := NewEngine(nil)
			t.Cleanup(func() {
				if err := engine.Close(); err != nil {
					t.Error(err)
				}
			})
			marker := filepath.Join(t.TempDir(), "started")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := engine.Launch(ctx, LaunchParams{SessionID: "startup", AgentID: "test-agent", Prompt: strings.Repeat("p", 256<<10), Command: os.Args[0], Args: []string{"-test.run=^TestRunnerStartupHelper$"}, Env: []string{"RUNNER_STARTUP_HELPER=" + marker}})
				done <- err
			}()
			limit := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(limit) {
					t.Fatal("startup child never entered")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if closeEngine {
				if err := engine.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("startup error = %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("cancelled startup did not stop")
			}
			if session := engine.sessions["startup"]; session != nil {
				assertReaped(t, session)
			}
		})
	}
}
func TestRunnerStartupHelper(t *testing.T) {
	marker := os.Getenv("RUNNER_STARTUP_HELPER")
	if marker == "" {
		return
	}
	// #nosec G703 -- only this test passes its private t.TempDir marker to the helper.
	if err := os.WriteFile(marker, []byte("started"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func assertReaped(t *testing.T, session *ActiveSession) {
	t.Helper()
	select {
	case <-session.reaped:
	default:
		t.Fatal("owned child not reaped")
	}
	if session.Cmd.ProcessState == nil || session.Cmd.ProcessState.Pid() != session.Cmd.Process.Pid {
		t.Fatal("owned child has no terminal process state")
	}
}
