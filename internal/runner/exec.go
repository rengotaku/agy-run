package runner

import (
	"context"
	"os/exec"
)

// commandContext builds the agy command bound to ctx. The watchdog cancels ctx
// (via killGroup) on a limit; exec.CommandContext also kills the process if ctx
// is cancelled for any other reason, so the child never outlives the run.
func commandContext(ctx context.Context, bin string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}
