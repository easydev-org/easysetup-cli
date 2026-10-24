package main

// closures.json records the dependency graph and, per setup, the exact store
// paths its tarball carries. It is derived from the nix closures by the release
// pipeline - not written by hand - so installing in the right order is a matter
// of walking it, not of knowing anything about the packages.

import (
	"encoding/json"
	"fmt"
	"os"
)

// ClosureEntry is one setup's record.
type ClosureEntry struct {
	Store       string   `json:"store"`
	Deps        []string `json:"deps"`
	Own         []string `json:"own"`
	ClosureSize int      `json:"closure_size"`
}

// Closures is the whole file.
type Closures struct {
	Cache  string                  `json:"cache"`
	Base   []string                `json:"base"`
	Setups map[string]ClosureEntry `json:"setups"`
}

// LoadClosures reads closures.json.
func LoadClosures(path string) (*Closures, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Closures
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("closures: %w", err)
	}
	if len(c.Setups) == 0 {
		return nil, fmt.Errorf("closures: no setups in %s", path)
	}
	return &c, nil
}

// InstallOrder returns the setups to install, dependencies first.
//
// This is what makes `easysetup install composer` also fetch php: composer's
// tarball deliberately omits the php paths, so unpacking it first would leave a
// composer that cannot start.
//
// A cycle would loop forever; nix closures are a strict-subset relation and
// cannot contain one, but `seen` is kept anyway so a bad closures.json produces
// a short order rather than a hang.
func (c *Closures) InstallOrder(dir string) []string {
	var order []string
	seen := map[string]bool{}

	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, d := range c.Setups[name].Deps {
			visit(d)
		}
		order = append(order, name)
	}
	visit(dir)
	return order
}
