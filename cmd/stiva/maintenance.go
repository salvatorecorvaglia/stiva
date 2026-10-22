package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/salvatorecorvaglia/stiva/internal/backup"
)

func maintenance(args []string) error {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	data := flags.String("data-dir", os.Getenv("STIVA_DATA_DIR"), "stopped Stiva data directory")
	source := flags.String("source", "", "backup directory")
	output := flags.String("output", "", "new destination directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch args[0] {
	case "backup":
		if *data == "" || *output == "" {
			return errors.New("backup requires --data-dir and --output")
		}
		return backup.Create(*data, *output)
	case "verify-backup":
		if *source == "" {
			return errors.New("verify-backup requires --source")
		}
		return backup.Verify(*source)
	case "restore":
		if *source == "" || *output == "" {
			return errors.New("restore requires --source and --output")
		}
		return backup.Restore(*source, *output)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
