//go:build !unix

package intercom

import (
	"errors"
	"os"
)

func acquireOwnerLock(string) (*os.File, error) {
	return nil, errors.New("local intercom requires Unix domain sockets and file locks")
}

func releaseOwnerLock(*os.File) {}

func directoryOwnedByUser(os.FileInfo) bool { return false }

func safeDirectoryAncestor(os.FileInfo) bool { return false }
