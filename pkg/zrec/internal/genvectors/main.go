// Command genvectors writes the Contract §4.5 zrec test vectors
// (vectors.json and the .zrec files it lists) into a directory:
//
//	go run ./pkg/zrec/internal/genvectors -out testdata/zrec/v1
//
// Stale .zrec files in the directory that the generator no longer produces
// are removed. The output is deterministic; see package vectorgen.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/apikdech/zrelay-v2/pkg/zrec/internal/vectorgen"
)

func main() {
	out := flag.String("out", "testdata/zrec/v1", "output directory")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "genvectors:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	files, err := vectorgen.Generate()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := files[e.Name()]; !ok && strings.HasSuffix(e.Name(), ".zrec") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("genvectors: wrote %d files to %s\n", len(names), dir)
	return nil
}
