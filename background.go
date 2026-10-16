package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"im-agent/internal/config"
	"im-agent/internal/daemon"
)

func startBackground() error {
	root, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := daemon.NewClient(filepath.Join(root, "IM Agent"))
	return client.Start(ctx, config.Config{}, 0)
}
