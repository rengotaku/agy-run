package runner

import (
	"context"
	"os/exec"
)

// commandContext builds the agy command bound to ctx. The caller sets a
// group-kill cmd.Cancel (see attempt), so cancelling ctx — a watchdog limit, a
// received signal, or normal teardown — reaps agy's whole process group; the
// child never outlives the run and no descendant is orphaned (issue #7).
func commandContext(ctx context.Context, bin string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}
