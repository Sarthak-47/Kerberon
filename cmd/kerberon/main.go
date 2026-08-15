// Command kerberon is an on-call and paging system that ships as a single binary
// with no external dependencies.
package main

import (
	"fmt"
	"os"
	"runtime"

	// The timezone database is compiled into the binary. Without this, rotations
	// break on minimal container images and on Windows, which ship no tzdata.
	// Spec §7.2.
	_ "time/tzdata"
)

// Set via -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `kerberon — on-call and paging in a single binary

Usage:
  kerberon <command> [flags]

Commands:
  serve       Run the server
  validate    Check a config file and report problems
  migrate     Create or upgrade the database schema
  version     Print version information

Run "kerberon <command> -h" for details on a command.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kerberon:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	switch cmd := args[0]; cmd {
	case "version", "-v", "--version":
		printVersion()
		return nil

	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil

	case "serve", "validate", "migrate":
		// Implemented in Phase 1. See docs/ROADMAP.md.
		return fmt.Errorf("%s: not implemented yet", cmd)

	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func printVersion() {
	fmt.Printf("kerberon %s\n", version)
	fmt.Printf("  commit: %s\n", commit)
	fmt.Printf("  built:  %s\n", date)
	fmt.Printf("  go:     %s\n", runtime.Version())
	fmt.Printf("  target: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}
