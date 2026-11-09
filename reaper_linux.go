package main

import (
	"bytes"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// when minisv is PID 1 (Docker ENTRYPOINT) orphaned grandchildren are
// re-parented to it and stay zombies until somebody waits for them
func startReaper() {
	if os.Getpid() != 1 {
		return
	}
	log.Println("Running as PID 1, reaping orphaned processes")

	chld := make(chan os.Signal, 1)
	signal.Notify(chld, syscall.SIGCHLD)
	go func() {
		// SIGCHLDs coalesce and a zombie may predate the Notify,
		// so scan on a timer as well
		tick := time.NewTicker(10 * time.Second)
		for {
			select {
			case <-chld:
			case <-tick.C:
			}
			reapOrphans()
		}
	}()
}

// reapOrphans waits for zombie children that were not started by minisv
func reapOrphans() {
	entries, err := os.ReadDir("/proc")
	if nil != err {
		return
	}

	// the scan runs unlocked, so a task start is not held up by it;
	// childPids is checked under the lock right before the wait
	var zombies []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if nil != err {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if nil != err {
			continue
		}
		// "pid (comm) state ppid ...": comm may contain spaces and parens
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
		if len(fields) >= 2 && fields[0] == "Z" && fields[1] == "1" {
			zombies = append(zombies, pid)
		}
	}
	if len(zombies) == 0 {
		return
	}

	childMu.Lock()
	defer childMu.Unlock()
	for _, pid := range zombies {
		if !childPids[pid] {
			_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		}
	}
}
