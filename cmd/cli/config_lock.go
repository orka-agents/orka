package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const configLockTimeout = time.Minute

func lockConfig(ctx context.Context) (func(), error) {
	path := configPath()
	if path == "" {
		return nil, errors.New("cannot determine home directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	file, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the config lock: %w", err)
	}
	for {
		locked, err := tryLockFile(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("lock the config: %w", err)
		}
		if locked {
			return func() {
				unlockFile(file)
				_ = file.Close()
			}, nil
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func updateConfig(change func(*orkaConfig) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), configLockTimeout)
	defer cancel()
	unlock, err := lockConfig(ctx)
	if err != nil {
		return fmt.Errorf("wait for the config: %w", err)
	}
	defer unlock()
	cfg := loadConfig()
	if err := change(&cfg); err != nil {
		return err
	}
	return saveConfig(cfg)
}
