package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// keyCmd manages the encrypted side-storage keystore. Provider keys are never
// written plaintext: `key add` encrypts under the master key and stores it.
func keyCmd(args []string) {
	// Manual flag scan (Go's flag pkg stops at the first positional arg, so it
	// cannot handle "key add <ref> --secret-env X").
	var cfgPath, keyfile, keystore, secretFile, secretEnv string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			i++
			if i < len(args) {
				return args[i]
			}
			return ""
		}
		switch a {
		case "--config", "-config":
			cfgPath = next()
		case "--keyfile", "-keyfile":
			keyfile = next()
		case "--keystore", "-keystore":
			keystore = next()
		case "--secret-file", "-secret-file":
			secretFile = next()
		case "--secret-env", "-secret-env":
			secretEnv = next()
		default:
			rest = append(rest, a)
		}
	}

	if len(rest) == 0 {
		fmt.Println("usage: tallowctl key add <ref> | tallowctl key rm <ref> | tallowctl key list")
		os.Exit(2)
	}
	action := rest[0]

	// Resolve master-key + keystore paths (config supplies defaults).
	masterKeyfile, ksPath := keyfile, keystore
	if masterKeyfile == "" || ksPath == "" {
		cfg, err := config.Load(resolveConfigPath(cfgPath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
			os.Exit(1)
		}
		if masterKeyfile == "" {
			masterKeyfile = cfg.Secret.Keyfile
		}
		if ksPath == "" {
			ksPath = cfg.Secret.Keystore
		}
	}

	mk, err := secret.LoadMasterKey(masterKeyfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
		os.Exit(1)
	}
	box, err := secret.NewBox(mk)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
		os.Exit(1)
	}
	st, err := secret.LoadStore(ksPath, box)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
		os.Exit(1)
	}

	switch action {
	case "add":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: tallowctl key add <ref>")
			os.Exit(2)
		}
		ref := rest[1]
		val, err := readSecret(secretFile, secretEnv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
			os.Exit(1)
		}
		val = strings.TrimSpace(val)
		if val == "" {
			fmt.Fprintln(os.Stderr, "tallowctl: empty key")
			os.Exit(1)
		}
		if err := st.Set(ref, val); err != nil {
			fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("stored key %q (encrypted) in %s\n", ref, ksPath)
	case "rm":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: tallowctl key rm <ref>")
			os.Exit(2)
		}
		if err := st.Delete(rest[1]); err != nil {
			fmt.Fprintf(os.Stderr, "tallowctl: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("removed key %q\n", rest[1])
	case "list":
		refs := st.List()
		sort.Strings(refs)
		for _, r := range refs {
			fmt.Println(r)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown key action %q\n", action)
		os.Exit(2)
	}
}

// readSecret obtains the key bytes from a file, stdin, or an env var.
func readSecret(file, env string) (string, error) {
	if env != "" {
		if v := os.Getenv(env); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("env var %q is empty", env)
	}
	if file == "" {
		return "", fmt.Errorf("provide --secret-file or --secret-env")
	}
	if file == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
