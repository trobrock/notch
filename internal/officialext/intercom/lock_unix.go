//go:build unix

package intercom

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func acquireOwnerLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("endpoint owner lock is held")
		}
		return nil, err
	}
	return file, nil
}

func releaseOwnerLock(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}

func directoryOwnedByUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func safeDirectoryAncestor(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	if !info.IsDir() {
		return false
	}
	// A root/user-owned sticky directory, such as /tmp, protects entries owned
	// by this user from replacement by other users despite its shared mode.
	return info.Mode().Perm()&0o022 == 0 || info.Mode()&os.ModeSticky != 0
}
