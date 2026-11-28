package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/greenhouse"
)

const greenhouseRuntimeFlags = "--greenhouse-binary <absolute-binary> --greenhouse-home <absolute-home>"

func parseGreenhouseRuntime(posseHome string, flags map[string]string) (greenhouse.ReviewRuntime, error) {
	binary, hasBinary := flags["greenhouse-binary"]
	home, hasHome := flags["greenhouse-home"]
	var runtime greenhouse.ReviewRuntime
	if !hasBinary && !hasHome {
		return runtime, nil
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(home) {
		return runtime, axi.Usage("--greenhouse-binary and --greenhouse-home require absolute paths and must be supplied together")
	}
	executable, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return runtime, fmt.Errorf("resolve isolated Greenhouse binary: %w", err)
	}
	if _, err := exec.LookPath(executable); err != nil {
		return runtime, fmt.Errorf("check isolated Greenhouse binary: %w", err)
	}
	stateHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return runtime, fmt.Errorf("resolve isolated Greenhouse home: %w", err)
	}
	info, err := os.Stat(stateHome)
	if err != nil {
		return runtime, fmt.Errorf("check isolated Greenhouse home: %w", err)
	}
	if !info.IsDir() {
		return runtime, axi.Usage("--greenhouse-home must be an existing directory")
	}
	sharedHome := resolvedNewPath(greenhouse.HomeFor(posseHome))
	if lexicallyInside(stateHome, sharedHome) || lexicallyInside(sharedHome, stateHome) {
		return runtime, axi.Usage("--greenhouse-home must be separate from the shared Greenhouse home")
	}
	return greenhouse.ReviewRuntime{Binary: executable, Home: stateHome}, nil
}

func checkPendingGreenhouseRuntime(path string, requested greenhouse.ReviewRuntime) error {
	pending, exists, err := greenhouse.PendingRuntime(path)
	if err != nil {
		return err
	}
	if exists && pending != requested {
		help := "Continue without runtime flags"
		if pending != (greenhouse.ReviewRuntime{}) {
			help = "Continue from the Lead pane with the original --greenhouse-binary and --greenhouse-home"
		}
		return axi.Failure("greenhouse_runtime_conflict", "a pending Greenhouse review must continue with its original runtime", false, help)
	}
	return nil
}
