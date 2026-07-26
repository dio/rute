//go:build darwin || linux

package accountstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type lockedFile struct {
	file *os.File
}

func lockFile(ctx context.Context, path string) (*lockedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(
		path,
		unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("accountstore: open credential lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("accountstore: inspect credential lock: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		file.Close()
		return nil, fmt.Errorf(
			"accountstore: credential lock permissions are %04o; require 0600",
			info.Mode().Perm(),
		)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		<-timer.C
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			file.Close()
			return nil, fmt.Errorf("accountstore: lock credential store: %w", err)
		}
		timer.Reset(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
			timer.Reset(0)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		file.Close()
		return nil, err
	}
	return &lockedFile{file: file}, nil
}

func (lock *lockedFile) Close() error {
	fd := int(lock.file.Fd())
	unlockErr := unix.Flock(fd, unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("accountstore: unlock credential store: %w", unlockErr)
	}
	return closeErr
}
