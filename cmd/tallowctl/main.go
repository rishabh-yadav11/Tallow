// Command tallowctl is the TUI dashboard and management CLI for Tallow. It
// talks to the gateway strictly over the admin Unix-socket HTTP interface.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		dashboardCmd([]string{})
		return
	}
	switch os.Args[1] {
	case "dashboard", "dash", "tui":
		dashboardCmd(os.Args[2:])
	case "key":
		keyCmd(os.Args[2:])
	case "reload":
		reloadCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`tallowctl — Tallow dashboard and management CLI

Usage:
  tallowctl                  run the dashboard TUI
  tallowctl dashboard        same as above
  tallowctl key add <ref>    encrypt + store a provider key (see flags)
  tallowctl key rm <ref>     remove a stored key
  tallowctl key list         list stored keystore refs
  tallowctl reload           request the gateway hot-reload its config

Key management flags (key add/rm/list):
  --config      config.toml path (default $TALLOW_CONFIG or ./config.toml)
  --secret-file path to read the key from ("-" = stdin; key add only)
  --secret-env  env var holding the key (key add only)
`)
}

// resolveConfigPath returns the config path from flags/env/default.
func resolveConfigPath(over string) string {
	if over != "" {
		return over
	}
	if v := os.Getenv("TALLOW_CONFIG"); v != "" {
		return v
	}
	return "config.toml"
}

// unixHTTPClient builds an HTTP client that dials a Unix socket.
func unixHTTPClient(socket string) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func dashboardCmd(args []string) {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config.toml path")
	socket := fs.String("socket", "", "admin socket path (overrides config)")
	_ = fs.Parse(args)

	sock := *socket
	if sock == "" {
		cfg, err := config.Load(resolveConfigPath(*cfgPath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
			os.Exit(1)
		}
		sock = cfg.Server.AdminSocket
	}
	if err := runTUI(sock); err != nil {
		fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
		os.Exit(1)
	}
}

func reloadCmd(args []string) {
	fs := flag.NewFlagSet("reload", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config.toml path")
	socket := fs.String("socket", "", "admin socket path (overrides config)")
	_ = fs.Parse(args)

	sock := *socket
	if sock == "" {
		cfg, err := config.Load(resolveConfigPath(*cfgPath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
			os.Exit(1)
		}
		sock = cfg.Server.AdminSocket
	}
	c := unixHTTPClient(sock)
	req, _ := http.NewRequest(http.MethodPost, "http://tallow/reload", nil)
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tallowctl: reload: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "tallowctl: reload: %s\n", resp.Status)
		os.Exit(1)
	}
	fmt.Println("reloaded")
}
