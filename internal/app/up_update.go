package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/mattn/go-isatty"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// up offers a release before upCore can register a Project or start a Lead.
// An existing Agent's turn is never allowed to install from inside its pane.
func (s *Service) up(ctx *axi.Context, args []string) error {
	parsed, err := parseUpArgs(args)
	if err != nil {
		return err
	}
	if len(parsed.Positionals) > 0 {
		return axi.Usage("up does not take positional arguments")
	}
	if s.herdrContext == nil || !s.herdrContext() {
		return s.upCore(ctx, args)
	}
	known, err := s.offerUpUpdate(ctx, args)
	if err != nil {
		return err
	}
	err = s.upCore(ctx, args)
	if !known {
		return err
	}
	if err == nil {
		_, _ = fmt.Fprintln(ctx.ErrOut, "A newer posse release is available. Run `posse update` to install it.")
		return nil
	}
	var structured *axi.Error
	if errors.As(err, &structured) {
		updated := *structured
		updated.Help = append(append([]string(nil), structured.Help...), "Run `posse update` to install the newer release")
		return &updated
	}
	return err
}

func (s *Service) offerUpUpdate(ctx *axi.Context, args []string) (bool, error) {
	if s.currentVersion() == "dev" {
		return false, nil
	}
	release, known := s.cachedRelease(ctx.Context)
	if !known || !newerVersion(s.currentVersion(), release.Tag) {
		return false, nil
	}
	home, err := s.homePath()
	if err != nil {
		return true, err
	}
	if ctx.JSON || s.insidePosseTurn(ctx.Context, home) {
		return true, nil
	}
	confirm := s.updateConfirm
	if confirm == nil {
		confirm = terminalUpdateConfirm
	}
	summary := clipRunes(strings.Join(strings.Fields(release.Body), " "), 240)
	question := fmt.Sprintf("Posse %s is available (current %s). %s\nUpdate before starting the Lead?", release.Tag, s.currentVersion(), summary)
	yes, interactive, err := confirm(ctx.ErrOut, question)
	if err != nil {
		return true, err
	}
	if !interactive || !yes {
		return true, nil
	}
	var installed updateInstallResult
	if err := s.installRelease(ctx.Context, release, false, &installed); err != nil {
		return true, err
	}
	reexec := s.reexecUpdate
	if reexec == nil {
		reexec = syscall.Exec
	}
	// Exec retains the shell's cwd, stdio, and environment. Keep the original
	// arguments verbatim, including --yes (which never consented to this update).
	argv := append([]string{installed.binary, "up"}, args...)
	if err := reexec(installed.binary, argv, os.Environ()); err != nil {
		return true, fmt.Errorf("re-exec updated posse: %w", err)
	}
	return true, axi.Failure("update_reexec_returned", "updated posse unexpectedly returned without starting up", false)
}

func terminalUpdateConfirm(out io.Writer, question string) (bool, bool, error) {
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return false, false, nil
	}
	if _, err := fmt.Fprint(out, question+" [y/N] "); err != nil {
		return false, true, err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, true, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", true, nil
}

func (s *Service) insidePosseTurn(ctx context.Context, home string) bool {
	if os.Getenv(workerHomeEnv) != "" || s.workerCaller(ctx, home) {
		return true
	}
	paneID := os.Getenv("HERDR_PANE_ID")
	if paneID == "" {
		return false
	}
	db, err := store.OpenReadOnly(home)
	if err != nil {
		return false
	}
	defer db.Close()
	if _, err := db.TaskByPane(ctx, paneID); err == nil {
		return true
	}
	_, err = db.ProjectByLeadPane(ctx, paneID)
	return err == nil
}
