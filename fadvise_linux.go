//go:build linux

package main

import (
	"os"
	"syscall"
)

const (
	fadvSequential = 2 // POSIX_FADV_SEQUENTIAL
	fadvDontNeed   = 4 // POSIX_FADV_DONTNEED
)

// adviseSequential tells the kernel the whole file is about to be read once,
// front to back, so it can read ahead aggressively. Advice is best effort.
func adviseSequential(file *os.File) {
	syscall.Syscall6(syscall.SYS_FADVISE64, file.Fd(), 0, 0, fadvSequential, 0, 0)
}

// adviseDone drops the file's pages from the page cache once hashed. A scan
// reads each file exactly once, so caching it only evicts what other programs
// on the host are actually using.
func adviseDone(file *os.File) {
	syscall.Syscall6(syscall.SYS_FADVISE64, file.Fd(), 0, 0, fadvDontNeed, 0, 0)
}
