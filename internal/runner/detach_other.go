//go:build !linux

package runner

// detachFromLauncher is a no-op on non-Linux platforms. agy-run is distributed
// and run only on Linux (dotfiles' build-go-linux.sh); this stub exists solely so
// `go build ./...` still cross-compiles on darwin/others, where the launcher-
// teardown detach (issue #9) is irrelevant. The real implementation lives in
// detach_linux.go, which needs the Linux-only TCGETS ioctl.
func detachFromLauncher() bool { return false }
