package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/host/api"
)

// publishedVersion is the version go can fetch of what this binary is, or main if it's a local build.
func publishedVersion() string {
	if v := cli.Version(version); strings.HasPrefix(v, "v") && !strings.Contains(v, "+") {
		return v
	}
	return "main"
}

func runBackup(ctx context.Context, args []string) error {
	fs := newFlags("backup")
	t := addTarget(fs)
	_ = fs.Parse(args)
	resp, err := api.Call(ctx, t.transport(), api.Request{Type: api.TypeBackup})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	fmt.Println(resp.Backup)
	return nil
}
