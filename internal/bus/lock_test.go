package bus

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Helper-process pattern: when this env var is set, the test binary acts as one
// racing agent instead of running tests.
const raceHelperEnv = "ORCHESTRA_LOCK_RACE_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(raceHelperEnv) == "1" {
		os.Exit(raceWorker())
	}
	os.Exit(m.Run())
}

// raceWorker spins until a shared start time, then tries to acquire.
// It prints "won", "lost" or "error: ...".
func raceWorker() int {
	dir, agent := os.Getenv("RACE_DIR"), os.Getenv("RACE_AGENT")
	startNanos, _ := strconv.ParseInt(os.Getenv("RACE_START_UNIX_NANO"), 10, 64)
	locks, err := NewLockManager(dir)
	if err != nil {
		fmt.Println("error:", err)
		return 1
	}
	for time.Now().UnixNano() < startNanos {
	}
	ok, err := locks.Acquire("RACE", agent, "", 1800, false)
	switch {
	case err != nil:
		fmt.Println("error:", err)
	case ok:
		fmt.Println("won")
	default:
		fmt.Println("lost")
	}
	return 0
}

func runRace(t *testing.T, workers int) {
	t.Helper()
	dir := t.TempDir()
	// Leave time for every process to start before they all go at once.
	start := time.Now().Add(1500 * time.Millisecond).UnixNano()
	results := make([]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(),
				raceHelperEnv+"=1",
				"RACE_DIR="+dir,
				fmt.Sprintf("RACE_AGENT=agent%d", i),
				fmt.Sprintf("RACE_START_UNIX_NANO=%d", start),
			)
			out, err := cmd.Output()
			if err != nil {
				results[i] = "error: " + err.Error()
				return
			}
			results[i] = strings.TrimSpace(string(out))
		}(i)
	}
	wg.Wait()

	winners, errs := []int{}, 0
	for i, r := range results {
		switch {
		case r == "won":
			winners = append(winners, i)
		case strings.HasPrefix(r, "error"):
			errs++
		}
	}
	if len(winners) != 1 || errs != 0 {
		t.Fatalf("want exactly 1 winner and 0 errors, got winners=%v errors=%d results=%v", winners, errs, results)
	}
	locks, _ := NewLockManager(dir)
	if got, want := locks.CurrentLock()["agent_id"], fmt.Sprintf("agent%d", winners[0]); got != want {
		t.Fatalf("writer.lock names %v, winner was %s", got, want)
	}
}

func TestLockHasOneOwnerWhenProcessesRace(t *testing.T) { runRace(t, 8) }

func TestLockRaceStress(t *testing.T) { runRace(t, 20) }

func TestSingleWriterRules(t *testing.T) {
	locks, err := NewLockManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must := func(ok bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !must(locks.Acquire("TASK-1", "claude", "auth", 100, false)) {
		t.Fatal("first acquire refused")
	}
	if !locks.IsLocked() {
		t.Fatal("not locked after acquire")
	}
	if must(locks.Acquire("TASK-2", "codex", "", 100, false)) {
		t.Fatal("another agent acquired a held lock")
	}
	if must(locks.Release("codex", false)) {
		t.Fatal("a non-owner released the lock")
	}
	if !must(locks.Release("claude", false)) || locks.IsLocked() {
		t.Fatal("owner could not release")
	}
	if !must(locks.Acquire("TASK-2", "codex", "", 100, false)) || locks.CurrentLock()["agent_id"] != "codex" {
		t.Fatal("re-acquire after release failed")
	}
}

func TestSameAgentRefreshesLeaseAndForceOverrides(t *testing.T) {
	locks, _ := NewLockManager(t.TempDir())
	locks.Acquire("T", "claude", "", 10, false)
	first := locks.CurrentLock()["expires_at"].(float64)
	time.Sleep(20 * time.Millisecond)
	if ok, _ := locks.Acquire("T", "claude", "", 1000, false); !ok {
		t.Fatal("same agent refused")
	}
	if locks.CurrentLock()["expires_at"].(float64) <= first {
		t.Fatal("lease not refreshed")
	}
	if ok, _ := locks.Acquire("T2", "codex", "", 10, true); !ok || locks.CurrentLock()["agent_id"] != "codex" {
		t.Fatal("force did not take the lock")
	}
}

func TestExpiredLockCanBeTakenAndHeartbeatExtends(t *testing.T) {
	locks, _ := NewLockManager(t.TempDir())
	locks.Acquire("T", "claude", "", -1, false)
	if locks.IsLocked() {
		t.Fatal("expired lock reported as locked")
	}
	if ok, _ := locks.Acquire("T2", "codex", "", 100, false); !ok {
		t.Fatal("expired lock blocked a new writer")
	}
	if ok, _ := locks.Heartbeat("claude", 100); ok {
		t.Fatal("non-holder heartbeat succeeded")
	}
	if ok, _ := locks.Heartbeat("codex", 5000); !ok {
		t.Fatal("holder heartbeat failed")
	}
	if exp := locks.CurrentLock()["expires_at"].(float64); exp < float64(time.Now().Unix())+4000 {
		t.Fatalf("heartbeat did not extend: %v", exp)
	}
}

func TestCorruptOrMissingLockMeansUnlocked(t *testing.T) {
	locks, _ := NewLockManager(t.TempDir())
	if locks.CurrentLock() != nil || locks.IsLocked() {
		t.Fatal("missing lock reported")
	}
	os.WriteFile(locks.LockFile, []byte("{not json"), 0o644)
	if locks.CurrentLock() != nil || locks.IsLocked() {
		t.Fatal("corrupt lock reported as held")
	}
	if ok, _ := locks.Release("", false); !ok {
		t.Fatal("release of a corrupt lock failed")
	}
}
