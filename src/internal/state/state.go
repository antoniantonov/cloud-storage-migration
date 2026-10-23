package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create private directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s must be a real private directory (mode 0700)", path)
	}
	return nil
}

func ReadFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s must be a regular private file (mode 0600)", path)
	}
	return os.ReadFile(path)
}

func WriteFile(path string, data []byte) (retErr error) {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("create state temporary file: %w", err)
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("clean state temporary file: %w", err))
		}
	}()
	if _, err := file.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write state: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync state: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func Lock(dir string) (func() error, error) {
	if err := EnsureDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("another migration is using %s", dir), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("lock state: %w", err), closeErr)
	}
	return func() error {
		return errors.Join(unix.Flock(fd, unix.LOCK_UN), unix.Close(fd))
	}, nil
}
