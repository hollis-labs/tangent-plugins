// Command portfolio-storage evaluates owned snapshots, never the live writer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/storage"
)

func run(args []string) error {
	flags := flag.NewFlagSet("portfolio-storage", flag.ContinueOnError)
	path := flags.String("database", "", "absolute shadow SQLite file")
	source := flags.String("source-copy", "", "absolute owned copy containing all seven JSON files")
	output := flags.String("export-copy", "", "new absolute output directory (must not exist)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: portfolio-storage -database /absolute/shadow.db [-source-copy /owned/copy] [-export-copy /new/dir] import|verify|export")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := storage.Open(ctx, *path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	switch flags.Arg(0) {
	case "import", "verify":
		snapshot, err := storage.ReadSnapshot(*source)
		if err != nil {
			return err
		}
		if flags.Arg(0) == "verify" {
			return db.Verify(ctx, snapshot)
		}
		applied, err := db.Import(ctx, snapshot)
		if err != nil {
			return err
		}
		fmt.Printf("import applied=%t\n", applied)
		return nil
	case "export":
		if !filepath.IsAbs(*output) {
			return errors.New("new absolute export directory required")
		}
		files, err := db.Export(ctx)
		if err != nil {
			return err
		}
		if err = os.Mkdir(*output, 0700); err != nil {
			return err
		}
		for name, body := range files {
			if err = os.WriteFile(filepath.Join(*output, name+".json"), append(body, '\n'), 0600); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("unknown command")
	}
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
