package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A Watch pass request runs one full Watch pass on demand, so a caller can
// wait for a pass to finish instead of waiting out the pass interval. The
// caller writes a fresh token to the request file. A Watch reads the token
// when a pass starts and writes it to the done file when that pass ends, so a
// matching done token proves a whole pass ran after the request.
var watchPassInterval = 2 * time.Second
var watchPassRequestCheck = 50 * time.Millisecond
var watchPassWaitLimit = 5 * time.Minute

func watchPassRequestPath(home string, project store.Project) string {
	return filepath.Join(home, "projects", project.Name, "watch-pass.request")
}

func watchPassDonePath(home string, project store.Project) string {
	return filepath.Join(home, "projects", project.Name, "watch-pass.done")
}

func readWatchPassToken(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(contents))
}

// waitForWatchPass waits out the pass interval and returns early when a pass
// request arrives that no pass has served yet.
func waitForWatchPass(ctx context.Context, home string, project store.Project, served string, stop <-chan os.Signal) (os.Signal, error) {
	interval := time.NewTimer(watchPassInterval)
	defer interval.Stop()
	check := time.NewTicker(watchPassRequestCheck)
	defer check.Stop()
	request := watchPassRequestPath(home, project)
	for {
		select {
		case sig := <-stop:
			return sig, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-interval.C:
			return nil, nil
		case <-check.C:
			if token := readWatchPassToken(request); token != "" && token != served {
				return nil, nil
			}
		}
	}
}

func (s *Service) watchPass(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("_watch-pass does not take arguments")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	running := func() bool {
		processes, err := systemWatchProcesses(home)
		if err != nil {
			return false
		}
		for _, process := range processes {
			if process.PollOnly && (process.WatchProjectID == project.ID || processBelongsToProject(process, project)) && watchPIDRunning(process.PID) {
				return true
			}
		}
		return false
	}
	if !running() {
		return axi.Failure("watch_not_running", "no Project Watch is running to serve the pass", false, "Start the Project Watch, then retry")
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return err
	}
	token := hex.EncodeToString(bytes)
	if err := writeFile(watchPassRequestPath(home, project), []byte(token+"\n")); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx.Context, watchPassWaitLimit)
	defer cancel()
	check := time.NewTicker(watchPassRequestCheck)
	defer check.Stop()
	liveness := time.NewTicker(time.Second)
	defer liveness.Stop()
	for readWatchPassToken(watchPassDonePath(home, project)) != token {
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return axi.Failure("watch_pass_timeout", "the Project Watch did not finish a pass in "+watchPassWaitLimit.String(), true, "Inspect the Watch log, then retry")
			}
			return waitCtx.Err()
		case <-liveness.C:
			if !running() {
				return axi.Failure("watch_not_running", "the Project Watch exited before it served the pass", true, "Start the Project Watch, then retry")
			}
		case <-check.C:
		}
	}
	return ctx.Print(axi.Object{{Key: "watch_pass", Value: "done"}})
}
