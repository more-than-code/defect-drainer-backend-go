package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/joe/defect-drainer-go/internal/env"
	"github.com/joe/defect-drainer-go/internal/paths"
)

// version is set via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if cwd, err := os.Getwd(); err == nil {
		if root := paths.FindModuleRoot(cwd); root != "" {
			if _, err := env.LoadEnvFile(filepath.Join(root, ".env")); err != nil {
				fmt.Fprintf(os.Stderr, "load .env: %v\n", err)
				return 1
			}
		}
	}

	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(version)
		return 0
	case "serve", "healthcheck", "worker":
		fmt.Fprintf(os.Stderr, "%s: not implemented\n", cmd)
		return 1
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n%s", cmd, usage())
		return 2
	}
}

func usage() string {
	return `defect-drainer — agent-harness control plane

Usage:
  defect-drainer serve         control plane (not implemented in PR 0)
  defect-drainer healthcheck   GET /health (not implemented in PR 0)
  defect-drainer worker        reserved (not implemented)
  defect-drainer version       print build version
`
}
