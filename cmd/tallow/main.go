// Command tallow is the LLM gateway core: a self-hosted, OpenAI-compatible
// proxy that routes requests across the user's providers and keys.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/setup"
)

// version is overridden at build time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	cfgPath := flag.String("config", "", "path to config.toml (defaults to $TALLOW_CONFIG or ./config.toml)")
	showVersion := flag.Bool("version", false, "print version and exit")
	allowPrivate := flag.Bool("allow-private-upstream", false, "allow providers whose base URL is a private/loopback address")
	flag.Parse()

	if *showVersion {
		fmt.Println("tallow", version)
		return
	}
	if *allowPrivate {
		config.SetAllowPrivateBaseURL(true)
	}

	path := *cfgPath
	if path == "" {
		if v := os.Getenv("TALLOW_CONFIG"); v != "" {
			path = v
		} else {
			path = "config.toml"
		}
	}

	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "tallow: %v\n", err)
			os.Exit(1)
		}
		if err := setup.Run(path, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "tallow: first-run setup: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("tallow: wrote new configuration %s\n", path)
	}

	a, err := app.New(path, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tallow: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("tallow %s\n  client API:  http://%s\n  admin socket: %s\n  config:       %s\n",
		version, a.ListenAddr(), a.AdminSocket(), a.CfgPath)

	if err := a.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "tallow: %v\n", err)
		os.Exit(1)
	}
}
