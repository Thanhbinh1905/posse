package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
)

// Where a posse home comes from, in order of precedence.
const (
	HomeSourceEnv     = "env"
	HomeSourcePointer = "pointer"
	HomeSourceDefault = "default"
)

// maxPointerSize bounds the home pointer file; a longer file is not a path.
const maxPointerSize = 4096

// Home is a resolved posse home. Pointer is the home pointer file consulted
// for this environment, whether or not it exists.
type Home struct {
	Path    string
	Source  string
	Pointer string
}

// DefaultHome resolves the posse home of this process.
func DefaultHome() (string, error) {
	home, err := ResolveHome(ProcessLookup)
	return home.Path, err
}

// ProcessLookup reads the process environment. Without $HOME it falls back to
// the account's home directory, as posse always has.
func ProcessLookup(key string) string {
	value := os.Getenv(key)
	if key == "HOME" && value == "" {
		if current, err := user.Current(); err == nil {
			return current.HomeDir
		}
	}
	return value
}

// ResolveHome resolves the posse home for an environment: POSSE_HOME when set,
// otherwise the folder named by the home pointer file, otherwise ~/.posse. An
// unreadable or invalid pointer is an error, never a silent fallback.
func ResolveHome(lookup func(string) string) (Home, error) {
	pointer, err := HomePointerPath(lookup)
	if value := lookup("POSSE_HOME"); value != "" {
		return Home{Path: filepath.Clean(value), Source: HomeSourceEnv, Pointer: pointer}, nil
	}
	if err != nil {
		return Home{}, err
	}
	path, found, err := readHomePointer(pointer)
	if err != nil {
		return Home{}, err
	}
	if found {
		return Home{Path: path, Source: HomeSourcePointer, Pointer: pointer}, nil
	}
	return Home{Path: filepath.Join(lookup("HOME"), ".posse"), Source: HomeSourceDefault, Pointer: pointer}, nil
}

// HomePointerPath is $XDG_CONFIG_HOME/posse/home, or ~/.config/posse/home when
// XDG_CONFIG_HOME is unset or relative. Herdr uses the same config root on
// Linux and macOS.
func HomePointerPath(lookup func(string) string) (string, error) {
	if xdg := lookup("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "posse", "home"), nil
	}
	home := lookup("HOME")
	if !filepath.IsAbs(home) {
		return "", errors.New("cannot locate the posse home pointer: HOME is not an absolute path")
	}
	return filepath.Join(home, ".config", "posse", "home"), nil
}

func readHomePointer(pointer string) (string, bool, error) {
	file, err := os.Open(pointer)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read posse home pointer %s: %w", pointer, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPointerSize+1))
	if err != nil {
		return "", false, fmt.Errorf("read posse home pointer %s: %w", pointer, err)
	}
	invalid := func(reason string) error {
		return fmt.Errorf("posse home pointer %s is invalid: %s; fix or remove it, or set POSSE_HOME", pointer, reason)
	}
	if len(data) > maxPointerSize {
		return "", false, invalid("it is too long")
	}
	path := strings.TrimSuffix(string(data), "\n")
	if err := validHomePath(path); err != nil {
		return "", false, invalid(err.Error())
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", false, invalid(fmt.Sprintf("data folder %s is not available: %v", path, err))
	}
	if !info.IsDir() {
		return "", false, invalid(fmt.Sprintf("%s is not a folder", path))
	}
	return filepath.Clean(path), true, nil
}

func validHomePath(path string) error {
	if path == "" {
		return errors.New("the path is empty")
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return errors.New("the path contains a control character")
		}
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not an absolute path", path)
	}
	return nil
}

// HasHomeData reports whether dir holds posse state: a database, Projects,
// config or a setup record.
func HasHomeData(dir string) bool {
	for _, name := range []string{"posse.db", "projects", "config.toml", "setup.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// ExpandHomeChoice turns a typed data folder into a clean absolute path,
// expanding a leading ~.
func ExpandHomeChoice(lookup func(string) string, choice string) (string, error) {
	if choice == "~" || strings.HasPrefix(choice, "~/") {
		home := lookup("HOME")
		if !filepath.IsAbs(home) {
			return "", errors.New("cannot expand ~: HOME is not an absolute path")
		}
		choice = filepath.Join(home, strings.TrimPrefix(choice, "~"))
	}
	if err := validHomePath(choice); err != nil {
		return "", err
	}
	return filepath.Clean(choice), nil
}

// SetHomePointer makes choice the posse home for every later process that
// has no POSSE_HOME. It never moves, merges or deletes data: it refuses to
// leave a home that holds posse data, and to adopt a non-empty folder that is
// not a posse home. Choosing ~/.posse while no pointer exists writes nothing.
func SetHomePointer(lookup func(string) string, choice string) (Home, error) {
	if value := lookup("POSSE_HOME"); value != "" {
		return Home{}, fmt.Errorf("POSSE_HOME is set to %s and overrides the saved data folder; unset it to choose another folder", value)
	}
	target, err := ExpandHomeChoice(lookup, choice)
	if err != nil {
		return Home{}, fmt.Errorf("cannot use %q as the posse data folder: %w", choice, err)
	}
	current, err := ResolveHome(lookup)
	if err != nil {
		return Home{}, err
	}
	if samePathOnDisk(current.Path, target) {
		return current, nil
	}
	if HasHomeData(current.Path) {
		return Home{}, fmt.Errorf("posse already keeps its data in %s; posse does not move it. To use %s, stop posse, move the folder there yourself (mv %q %q), then choose %s again", current.Path, target, current.Path, target, target)
	}
	info, statErr := os.Stat(target)
	switch {
	case statErr == nil && !info.IsDir():
		return Home{}, fmt.Errorf("cannot use %s as the posse data folder: it is not a folder", target)
	case statErr == nil && !HasHomeData(target):
		entries, err := os.ReadDir(target)
		if err != nil {
			return Home{}, fmt.Errorf("cannot use %s as the posse data folder: %w", target, err)
		}
		if len(entries) > 0 {
			return Home{}, fmt.Errorf("cannot use %s as the posse data folder: it holds other files; choose an empty or new folder", target)
		}
	case statErr != nil && !errors.Is(statErr, os.ErrNotExist):
		return Home{}, fmt.Errorf("cannot use %s as the posse data folder: %w", target, statErr)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return Home{}, fmt.Errorf("create posse data folder %s: %w", target, err)
	}
	if err := os.MkdirAll(filepath.Dir(current.Pointer), 0o700); err != nil {
		return Home{}, fmt.Errorf("create %s: %w", filepath.Dir(current.Pointer), err)
	}
	if err := atomicfile.Write(current.Pointer, []byte(target+"\n"), 0o600); err != nil {
		return Home{}, fmt.Errorf("write posse home pointer %s: %w", current.Pointer, err)
	}
	return Home{Path: target, Source: HomeSourcePointer, Pointer: current.Pointer}, nil
}

func samePathOnDisk(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	left, leftErr := os.Stat(a)
	right, rightErr := os.Stat(b)
	return leftErr == nil && rightErr == nil && os.SameFile(left, right)
}
