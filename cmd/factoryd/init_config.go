package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"buildgate/internal/sessionconfig"
)

// initConfigMain implements `factoryd init-config`: writes the commented
// example session config (sessionconfig.Example) to the default path
// worker reads, refusing to overwrite an existing file.
// newInitConfigFlags builds `factoryd init-config`'s FlagSet in isolation
// from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newInitConfigFlags() (flags *flag.FlagSet, path *string) {
	flags = flag.NewFlagSet("init-config", flag.ContinueOnError)
	path = flags.String("path", sessionconfig.DefaultPaths()[0], "where to write the example config")
	plainFlagUsage(flags)
	return
}

func initConfigMain(args []string) error {
	flags, path := newInitConfigFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; edit it in place or remove it first", *path)
		}
		return err
	}
	if _, err := f.WriteString(sessionconfig.Example); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote %s -- replace every <placeholder>, then run `factoryd worker`\n", *path)
	return nil
}
