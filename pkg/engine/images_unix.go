// SPDX-License-Identifier: Apache-2.0
//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"
)

func imageFileID(info fs.FileInfo) (imageFileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink < 2 {
		return imageFileIdentity{}, false
	}
	return imageFileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, true
}

func lockImageProcesses(ctx context.Context, path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
