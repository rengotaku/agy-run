package runner

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// existingAgyPIDs returns the PIDs of running processes named exactly "agy"
// (the /proc equivalent of `pgrep -x agy`), excluding this process. Best-effort:
// an unreadable /proc yields an empty slice.
func existingAgyPIDs(self int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == "agy" {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}
