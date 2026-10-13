//go:build !unix

package main

import (
	"errors"
	"os"
)

func acquireOwnerLock(string) (*os.File, error) {
	return nil, errors.New("local intercom requires Unix domain sockets and file locks")
}

func releaseOwnerLock(*os.File) {}
