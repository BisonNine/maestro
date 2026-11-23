//go:build unix

/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"go.bison9.com/maestro/config"
	"go.bison9.com/maestro/global"
	"go.bison9.com/maestro/logging"
	"go.bison9.com/maestro/projects"
)

func newDeleteTestService(t *testing.T) (*Service, *projects.Service) {
	t.Helper()
	cfg := config.New(config.WithBaseDir(t.TempDir()))
	if err := cfg.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	lg, err := logging.New(filepath.Join(t.TempDir(), "test.log"), cfg.DirMode(), cfg.FileMode())
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	projectsSvc := projects.NewService(cfg, lg)
	if _, err := projectsSvc.Create("alice", "Alice", "", "", "", "none"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return NewService(cfg, projectsSvc, lg), projectsSvc
}

// TestDeleteTaskSet_KeepsLockFile: a caller (Bob) that opened the lock file
// before the delete still shares one lock with every later caller (Alice).
func TestDeleteTaskSet_KeepsLockFile(t *testing.T) {
	svc, projectsSvc := newDeleteTestService(t)
	if _, err := svc.CreateTaskSet("alice", "review", "Review", "", nil, false, global.Limits{}, true, ""); err != nil {
		t.Fatalf("CreateTaskSet: %v", err)
	}
	lockPath := filepath.Join(projectsSvc.GetTasksDir("alice"), "review.json.lock")

	bob, err := os.Open(lockPath)
	if err != nil {
		t.Fatalf("Bob open: %v", err)
	}
	defer func() { _ = bob.Close() }()

	if err := svc.DeleteTaskSet("alice", "review"); err != nil {
		t.Fatalf("DeleteTaskSet: %v", err)
	}

	if err := syscall.Flock(int(bob.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("Bob lock: %v", err)
	}

	alice, err := os.OpenFile(lockPath, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("Alice open: %v", err)
	}
	defer func() { _ = alice.Close() }()
	if err := syscall.Flock(int(alice.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("Alice and Bob both hold the task set lock")
	}

	if err := syscall.Flock(int(bob.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("Bob unlock: %v", err)
	}
	if _, err := svc.GetTaskSet("alice", "review"); err == nil || !strings.Contains(err.Error(), "task set not found") {
		t.Fatalf("GetTaskSet after delete: err = %v, want task set not found", err)
	}
}

// TestDeleteTaskSet_ConcurrentReaders: readers racing a delete see the task
// set or "not found", never a lock-file error.
func TestDeleteTaskSet_ConcurrentReaders(t *testing.T) {
	svc, _ := newDeleteTestService(t)
	const iterations, readers = 50, 8

	for i := 0; i < iterations; i++ {
		if _, err := svc.CreateTaskSet("alice", "review", "Review", "", nil, false, global.Limits{}, true, ""); err != nil {
			t.Fatalf("iteration %d: CreateTaskSet: %v", i, err)
		}
		task, err := svc.CreateTask("alice", "review", "Bob", "", &global.WorkExecution{Prompt: "Bob", Status: global.ExecutionStatusWaiting}, nil)
		if err != nil {
			t.Fatalf("iteration %d: CreateTask: %v", i, err)
		}

		start := make(chan struct{})
		errs := make(chan error, readers*3+1)
		var wg sync.WaitGroup
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := svc.GetTaskSet("alice", "review"); err != nil && !strings.Contains(err.Error(), "task set not found") {
					errs <- err
				}
				if _, err := svc.ListTaskSets("alice", ""); err != nil {
					errs <- err
				}
				if _, _, err := svc.GetTask("alice", task.UUID); err != nil && !strings.Contains(err.Error(), "task not found") {
					errs <- err
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := svc.DeleteTaskSet("alice", "review"); err != nil {
				errs <- err
			}
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("iteration %d: %v", i, err)
		}
	}
}
