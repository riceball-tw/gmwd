package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockIsExclusiveAndReportsHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	unlock, err := lock(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lock(path)
	if err == nil || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("second lock: got %v, want 'already running (pid %d)'", err, os.Getpid())
	}
	unlock()
	unlock2, err := lock(path)
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	unlock2()
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("our own pid reported dead")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("exited pid %d reported alive", cmd.Process.Pid)
	}
}
