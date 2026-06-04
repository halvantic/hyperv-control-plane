//go:build !windows

package main

import (
	"context"
	"errors"
)

// errWindowsOnly is returned by the service-management helpers off Windows. The
// agent runs as a Windows service in production; these stubs exist only so the
// codebase builds and tests on developer machines that are not Windows.
var errWindowsOnly = errors.New("service install/remove is only supported on Windows")

// runAgent runs the agent directly; there is no SCM off Windows.
func runAgent(ctx context.Context, r *runner, _ bool) error {
	r.log.Info("running in console mode (non-Windows build)")
	return r.run(ctx)
}

func installService(_, _, _, _ string, _ []string, _, _ string) error { return errWindowsOnly }

func removeService(_ string) error { return errWindowsOnly }
