package main

// easysetup - install easysetup toolchains on an image that has no shell.

// install.sh needs sh, curl, zstd and python3 to bootstrap itself. A distroless
// image has none of those, so the script cannot run there at all. This binary is
// the same installer with those dependencies compiled in: one static file, no
// interpreter, no package manager.

// It still needs a writable /nix/store - that is a filesystem property, not
// something a program can create from nothing. `easysetup prepare` reports
// exactly what to do when it cannot.

import (
	"fmt"
	"os"
	"path/filepath"
)

const rawBase = "https://raw.githubusercontent.com/easydev-org/easysetup/main"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage()
		return
	}

	// `prepare` runs before anything else and needs no metadata: it is the step
	// that makes the store usable, and fetching a manifest first would fail on an
	// image with no /nix at all.
	inst := NewInstaller(os.Stdout)
	if cmd == "prepare" {
		cmdPrepare(inst)
		return
	}

	// The manifest and the dependency graph are fetched at run time, never
	// embedded: embedding would mean recompiling the binary whenever a setup is
	// added, and it downloads a tarball anyway, so the network is required
	// regardless.
	work, err := fetchMetadata(inst)
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(work)

	man, err := LoadManifest(filepath.Join(work, "matrix-nix.yml"))
	if err != nil {
		fail(err)
	}
	clo, err := LoadClosures(filepath.Join(work, "closures.json"))
	if err != nil {
		fail(err)
	}

	switch cmd {
	case "install":
		cmdInstall(inst, man, clo, args)
	case "list":
		cmdList(man)
	case "env":
		cmdEnv(inst, args)
	case "verify":
		cmdVerify(inst, man, args)
	default:
		fmt.Fprintf(os.Stderr, "easysetup: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `easysetup - install toolchains from prebuilt nix closures

  easysetup install <name>...   download, verify and unpack, with dependencies
  easysetup list                every setup that can be installed
  easysetup env [path]          write env.sh (default /opt/setup/env.sh) and print it
  easysetup prepare             make /nix/store exist and be writable (needs root)
  easysetup verify <name>...    check an install without changing anything

A bare tool name means the newest version: "node" is node-26, "node-24" is 24.
Python's tools need a suffix when both interpreters are present: "mypy-py3.13".

Set GITHUB_TOKEN to use the authenticated download quota; anonymous downloads
are limited to 60/hour per IP, which one multi-tool job can exhaust.
`)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "easysetup: %v\n", err)
	os.Exit(1)
}
