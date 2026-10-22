package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func TestMaintenanceCommands(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	snapshot := filepath.Join(root, "snapshot")
	engine, err := storage.NewFilesystemEngine(data, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CreateBucket("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PutObject(context.Background(), "source", "key", strings.NewReader("cli"), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"backup", "--data-dir", data, "--output", snapshot}, {"verify-backup", "--source", snapshot}, {"restore", "--source", snapshot, "--output", filepath.Join(root, "restored")}} {
		if err := maintenance(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"backup"}, {"restore"}, {"verify-backup"}, {"unknown"}, {"backup", "--bad-flag"}, {"backup", "unexpected"}} {
		if err := maintenance(args); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
}
