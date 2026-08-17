package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joe/defect-drainer-go/internal/api"
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
	case "serve":
		return runServe()
	case "healthcheck":
		return runHealthcheck()
	case "worker":
		fmt.Fprintf(os.Stderr, "worker: not implemented\n")
		return 1
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n%s", cmd, usage())
		return 2
	}
}

func listenAddr() (host, port string) {
	host = env.EnvDrainer("HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port = env.EnvDrainer("PORT")
	if port == "" {
		port = "8788"
	}
	return host, port
}

func runServe() int {
	app, err := api.Build(api.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	defer app.Close()

	host, port := listenAddr()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: listen: %v\n", err)
		return 1
	}
	fmt.Printf("defect-drainer http://%s  evidenceRoot=%s  db=%s\n",
		ln.Addr().String(), app.Roots.DefectsRoot, paths.DbPath(app.Roots.DataRoot))

	srv := &http.Server{Handler: app.Handler}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		_ = srv.Close()
		return 0
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "serve: %v\n", err)
			return 1
		}
		return 0
	}
}

func runHealthcheck() int {
	host, port := listenAddr()
	url := "http://" + net.JoinHostPort(host, port) + "/health"
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP %d\n", res.StatusCode)
		return 1
	}
	var parsed struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || !parsed.OK {
		fmt.Fprintf(os.Stderr, "healthcheck: ok is not true\n")
		return 1
	}
	return 0
}

func usage() string {
	return `defect-drainer — agent-harness control plane

Usage:
  defect-drainer serve         control plane (lock + SQLite + HTTP)
  defect-drainer healthcheck   GET /health on HOST:PORT (exit 0 iff 200 and ok:true)
  defect-drainer worker        reserved (not implemented)
  defect-drainer version       print build version
`
}
