package main

// The tie-break rules, as tests rather than as comments.

// Every one of these was a real mistake at some point:

//   * a bare name meant "ambiguous" and failed, so `tools: node` stopped working
//     the moment a second node existed
//   * on a version tie the winner was whatever an unstable sort left last, which
//     picked pip-26.1.2-py3.13 over pip-26.1.2 - so `install python pip` put a
//     pip bound to 3.13 next to a 3.14 interpreter

// The manifest here is the shape that matters, not the real one: two tools with
// two versions each, and one with two builds of the SAME version.

import (
	"os"
	"path/filepath"
	"testing"
)

func loadTestManifest(t *testing.T, body string) *Manifest {
	t.Helper()
	p := filepath.Join(t.TempDir(), "matrix-nix.yml")
	if err := os.WriteFile(p, []byte("setups:\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const tieManifest = `
  - { tool: pip, attr: "nixpkgs#python314Packages.pip", version: "26.1.2" }
  - { tool: pip, attr: "nixpkgs#python313Packages.pip", version: "26.1.2", suffix: "-py3.13" }
  - { tool: python, attr: "nixpkgs#python314", version: "3.14.8" }
  - { tool: python, attr: "nixpkgs#python313", version: "3.13.16" }
  - { tool: node, attr: "nixpkgs#nodejs_26", version: "26.12.0" }
  - { tool: node, attr: "nixpkgs#nodejs_24", version: "24.21.0" }
  - { tool: go, attr: "nixpkgs#go", version: "1.26.9" }
  - { tool: go, attr: "nixpkgs#go_1_27", version: "1.27.3" }
`

func TestBareNamePicksNewest(t *testing.T) {
	m := loadTestManifest(t, tieManifest)
	for _, c := range []struct{ name, want string }{
		{"python", "python-3.14.8"},
		{"node", "node-26.12.0"},
		{"go", "go-1.27.3"},
	} {
		got, err := m.Resolve(c.name)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Dir != c.want {
			t.Errorf("%s resolved to %s, want %s", c.name, got.Dir, c.want)
		}
	}
}

// The one that broke `install python pip`.
func TestVersionTiePicksUnsuffixed(t *testing.T) {
	m := loadTestManifest(t, tieManifest)
	got, err := m.Resolve("pip")
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != "pip-26.1.2" {
		t.Fatalf("bare pip resolved to %s, want pip-26.1.2 - the suffixed build "+
			"belongs to python 3.13, so installing it next to python 3.14 leaves "+
			"the interpreter without a pip", got.Dir)
	}
	// And it must be reachable explicitly when that is what you want.
	suffixed, err := m.Resolve("pip-py3.13")
	if err != nil {
		t.Fatal(err)
	}
	if suffixed.Dir != "pip-26.1.2-py3.13" {
		t.Fatalf("pip-py3.13 resolved to %s", suffixed.Dir)
	}
}

func TestVersionPrefix(t *testing.T) {
	m := loadTestManifest(t, tieManifest)
	for _, c := range []struct{ name, want string }{
		{"python-3.13", "python-3.13.16"},
		{"node-24", "node-24.21.0"},
		{"go-1.26", "go-1.26.9"},
	} {
		got, err := m.Resolve(c.name)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Dir != c.want {
			t.Errorf("%s resolved to %s, want %s", c.name, got.Dir, c.want)
		}
	}
}

// A tool whose name is a prefix of another must not resolve to it: `go` picking
// up `go-tools` made the ambiguity message claim go had three versions.
func TestPrefixDoesNotLeakAcrossTools(t *testing.T) {
	m := loadTestManifest(t, tieManifest+`
  - { tool: go-tools, attr: "nixpkgs#gotools", version: "2026.2.2" }
`)
	got, err := m.Resolve("go-tools")
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != "go-tools-2026.2.2" {
		t.Fatalf("go-tools resolved to %s", got.Dir)
	}
	// And `go` still means go, not the pair.
	got, err = m.Resolve("go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tool != "go" {
		t.Fatalf("go resolved to %s, which is a different tool", got.Dir)
	}
}

func TestUnknownName(t *testing.T) {
	m := loadTestManifest(t, tieManifest)
	if _, err := m.Resolve("nonexistent"); err == nil {
		t.Fatal("an unknown name must be an error, not a silent pick")
	}
}
