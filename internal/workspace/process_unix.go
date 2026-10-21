//go:build darwin || linux

package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func inside(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// refuseProcessHolders is deliberately read-only. It never signals, adopts, or
// stops a process to make cleanup succeed. This complements runtime job checks;
// arbitrary detached processes outside the checkout remain outside this MVP's
// non-sandboxed authority model.
func refuseProcessHolders(ctx context.Context, path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return errors.New("cannot inspect process cwd ownership before cleanup")
		}
		for _, entry := range entries {
			if _, err = strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			base := filepath.Join("/proc", entry.Name())
			info, err := os.Stat(base)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return errors.New("cannot establish process ownership before cleanup")
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return errors.New("cannot decode process ownership")
			}
			if stat.Uid != uint32(os.Getuid()) {
				continue
			}
			cwd, err := os.Readlink(filepath.Join(base, "cwd"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return errors.New("cannot establish an owned process cwd before cleanup")
			}
			if inside(resolved, cwd) {
				return errors.New("refusing cleanup: a process still holds a cwd inside the workspace")
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-Fpn", "-u", strconv.Itoa(os.Getuid()))
	command.Env = environment()
	output, err := command.Output()
	if err != nil {
		return errors.New("cannot prove workspace cwd quiescence through lsof; cleanup refused")
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "n") && inside(resolved, strings.TrimPrefix(line, "n")) {
			return errors.New("refusing cleanup: a process still holds a cwd inside the workspace")
		}
	}
	return nil
}
