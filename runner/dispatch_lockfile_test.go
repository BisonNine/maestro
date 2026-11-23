//go:build unix

/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gofrs/flock"

	"go.bison9.com/maestro/global"
)

// TestDispatch_LockFileSurvivesGoroutineExit reproduces a caller that, woken
// by the completion callback, opens the taskset's lock file before the
// dispatch goroutine exits. The lock file must stay in place: the caller's
// open file must still be the one every later caller locks, and later reads
// must succeed.
func TestDispatch_LockFileSurvivesGoroutineExit(t *testing.T) {
	llmsJSON := `{
		"id": "test-llm",
		"type": "command",
		"command": "/bin/echo",
		"args": ["{{PROMPT}}"],
		"description": "Enabled test LLM",
		"enabled": true
	}`
	runner, tmpDir := setupTestRunnerWithLLMConfig(t, llmsJSON, "test-llm")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	projectName := "alice"
	if _, err := runner.projects.Create(projectName, "Alice", "", "", "", "none"); err != nil {
		t.Fatalf("Failed to create project: %v", err)
	}
	path := "dispatch/lockfile"
	if _, err := runner.tasks.CreateTaskSet(projectName, path, "lockfile", "", nil, false, global.Limits{}, true, ""); err != nil {
		t.Fatalf("Failed to create taskset: %v", err)
	}
	task, err := runner.tasks.CreateTask(projectName, path, "lockfile", "",
		&global.WorkExecution{Prompt: "irrelevant", Status: global.ExecutionStatusWaiting}, nil)
	if err != nil {
		t.Fatalf("Failed to create task: %v", err)
	}
	lockPath := filepath.Join(runner.projects.GetTasksDir(projectName), "dispatch__lockfile.json.lock")

	// The sink runs on the dispatch goroutine, before it exits: it plays a
	// caller (Bob) that, woken by the callback, has just opened the lock file
	// and has not locked it yet.
	var bob *os.File
	var sinkErr error
	sink := func([]byte) {
		bob, sinkErr = os.Open(lockPath)
	}
	defer func() {
		if bob != nil {
			_ = bob.Close()
		}
	}()

	failingGetTask := func(_, _ string) (*global.Task, string, error) {
		return nil, "", errors.New("simulated GetTask failure")
	}
	req := &DispatchRequest{Project: projectName, Path: path, Title: "lockfile", Prompt: "irrelevant"}

	runner.activeRuns.Add(1)
	runner.runDispatchExecution(context.Background(), req, task, path, failingGetTask, sink)
	runner.Wait()

	if sinkErr != nil {
		t.Fatalf("Sink could not open %s: %v", lockPath, sinkErr)
	}
	if err := syscall.Flock(int(bob.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("Bob lock: %v", err)
	}

	// The lock file must still be the inode Bob holds.
	pathInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Lock file gone after dispatch goroutine exit: %v", err)
	}
	bobInfo, err := bob.Stat()
	if err != nil {
		t.Fatalf("Stat Bob's lock file: %v", err)
	}
	if !os.SameFile(pathInfo, bobInfo) {
		t.Fatalf("Lock file at %s is not the file Bob holds", lockPath)
	}

	// While Bob holds the lock, Alice must not get it.
	alice := flock.New(lockPath)
	locked, err := alice.TryLock()
	if err != nil {
		t.Fatalf("Alice TryLock: %v", err)
	}
	if locked {
		_ = alice.Unlock()
		t.Fatal("Alice and Bob both hold the taskset lock")
	}

	// Once Bob is done, readers succeed.
	if err := syscall.Flock(int(bob.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("Bob unlock: %v", err)
	}
	if _, err := runner.tasks.GetTaskSet(projectName, path); err != nil {
		t.Fatalf("GetTaskSet after dispatch: %v", err)
	}
	if _, _, err := runner.tasks.GetTask(projectName, task.UUID); err != nil {
		t.Fatalf("GetTask after dispatch: %v", err)
	}
}
