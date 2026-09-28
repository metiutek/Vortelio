//go:build !windows

package commands

import "golang.org/x/sys/unix"

// enableOutputProcessing restores output post-processing (OPOST/ONLCR) that
// term.MakeRaw turns off, so "\n" still returns the cursor to column 0.
func enableOutputProcessing(fd int) {
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return
	}
	t.Oflag |= unix.OPOST | unix.ONLCR
	unix.IoctlSetTermios(fd, ioctlSetTermios, t)
}

func enableVTOutput() {}
