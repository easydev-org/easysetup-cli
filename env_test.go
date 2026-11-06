package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envFixture builds an install tree with the given registry entries and
// closure graph, then writes env.sh.
func envFixture(t *testing.T, registry map[string]string, clo *Closures) string {
	t.Helper()
	root := t.TempDir()
	nixDir := filepath.Join(root, "nix")
	if err := os.MkdirAll(nixDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range registry {
		if err := os.WriteFile(filepath.Join(nixDir, "setup-root-"+name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inst := &Installer{Root: root, Out: os.Stderr}
	out := filepath.Join(root, "env.sh")
	if err := inst.WriteEnv(out, clo); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWriteEnvExportsLibraryPaths is the regression for the swift failure.
//
// Without these the generated env.sh only had PATH, so a compiler whose runtime
// libraries live in a SIBLING store path failed to link:
//
//	ld.bfd: libFoundation.so: undefined reference to `uloc_toLegacyKey'
//
// The `lib` directory of every installed store path (and of its dependencies)
// must be on the link path.
func TestWriteEnvExportsLibraryPaths(t *testing.T) {
	// A store path the test can actually create, so the "does this dir exist"
	// filter exercises the real branch.
	storeA := filepath.Join(t.TempDir(), "store-a")
	if err := os.MkdirAll(filepath.Join(storeA, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	storeICU := filepath.Join(t.TempDir(), "store-icu")
	if err := os.MkdirAll(filepath.Join(storeICU, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}

	clo := &Closures{
		Base: []string{},
		Setups: map[string]ClosureEntry{
			"swift-6.3.3": {
				Store: storeA,
				Deps:  []string{"base-2.44"},
				Own:   []string{storeA, storeICU}, // ICU ships in swift's own list
			},
			"base-2.44": {Store: "none"},
		},
	}
	got := envFixture(t, map[string]string{
		"swift-6.3.3": storeA + "|bin",
	}, clo)

	for _, want := range []string{"LIBRARY_PATH", "NIX_LDFLAGS"} {
		if !strings.Contains(got, want) {
			t.Errorf("env.sh does not export %s:\n%s", want, got)
		}
	}
	// LD_LIBRARY_PATH must NOT be set: it would put nix's glibc on the loader
	// path of every later process, which crashes unrelated programs. The link
	// path is enough to build a correct binary.
	if strings.Contains(got, "LD_LIBRARY_PATH=") {
		t.Errorf("env.sh must not set LD_LIBRARY_PATH (it breaks the host loader):\n%s", got)
	}
	if !strings.Contains(got, filepath.Join(storeICU, "lib")) {
		t.Errorf("ICU's lib dir (a DEPENDENCY-owned path) is missing:\n%s", got)
	}
	if !strings.Contains(got, filepath.Join(storeA, "lib")) {
		t.Errorf("the setup's own lib dir is missing:\n%s", got)
	}
	// PATH must still work exactly as before.
	if !strings.Contains(got, "PATH=\""+storeA+"/bin:") {
		t.Errorf("PATH entry missing:\n%s", got)
	}
}

// TestWriteEnvSkipsMissingLibDirs: a closure lists documentation and executable
// paths too; a -L pointing at a directory that does not exist is noise.
func TestWriteEnvSkipsMissingLibDirs(t *testing.T) {
	storeNoLib := filepath.Join(t.TempDir(), "store-nolib")
	if err := os.MkdirAll(filepath.Join(storeNoLib, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	clo := &Closures{Setups: map[string]ClosureEntry{
		"tool-1.0": {Store: storeNoLib, Own: []string{storeNoLib}},
	}}
	got := envFixture(t, map[string]string{"tool-1.0": storeNoLib + "|bin"}, clo)
	if strings.Contains(got, filepath.Join(storeNoLib, "lib")) {
		t.Errorf("a store path with no lib/ must not contribute one:\n%s", got)
	}
	// No lib dirs at all -> the export block is omitted entirely rather than
	// emitting empty exports.
	if strings.Contains(got, "LIBRARY_PATH") {
		t.Errorf("no library dirs should mean no export block:\n%s", got)
	}
}

// TestWriteEnvFollowsDependencies: a delta tarball carries only its own paths,
// so a library provided by a DEPENDENCY lives outside the setup's own store.
func TestWriteEnvFollowsDependencies(t *testing.T) {
	depStore := filepath.Join(t.TempDir(), "dep")
	if err := os.MkdirAll(filepath.Join(depStore, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainStore := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(filepath.Join(mainStore, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	clo := &Closures{Setups: map[string]ClosureEntry{
		"app-1.0": {Store: mainStore, Deps: []string{"lib-2.0"}, Own: []string{mainStore}},
		"lib-2.0": {Store: depStore, Own: []string{depStore}},
	}}
	got := envFixture(t, map[string]string{"app-1.0": mainStore + "|bin"}, clo)
	if !strings.Contains(got, filepath.Join(depStore, "lib")) {
		t.Errorf("a dependency's lib dir is missing:\n%s", got)
	}
}

// TestWriteEnvIncludesBase: base is a shared library set, not a dependency edge
// of every setup, so it must be added explicitly.
func TestWriteEnvIncludesBase(t *testing.T) {
	baseStore := filepath.Join(t.TempDir(), "base")
	if err := os.MkdirAll(filepath.Join(baseStore, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainStore := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(filepath.Join(mainStore, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	clo := &Closures{
		Base:   []string{baseStore},
		Setups: map[string]ClosureEntry{"app-1.0": {Store: mainStore, Own: []string{mainStore}}},
	}
	got := envFixture(t, map[string]string{"app-1.0": mainStore + "|bin"}, clo)
	if !strings.Contains(got, filepath.Join(baseStore, "lib")) {
		t.Errorf("base's lib dir is missing:\n%s", got)
	}
}

// TestWriteEnvWithoutClosuresStillWorks: a registry entry is authoritative for
// the setup's OWN lib dir, so env.sh must not break when the graph is absent.
func TestWriteEnvWithoutClosuresStillWorks(t *testing.T) {
	store := filepath.Join(t.TempDir(), "only")
	if err := os.MkdirAll(filepath.Join(store, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := envFixture(t, map[string]string{"solo-1.0": store + "|bin"}, nil)
	if !strings.Contains(got, "PATH=") {
		t.Errorf("PATH must always be emitted:\n%s", got)
	}
}

// TestWriteEnvLibOrderIsStable: a regenerated env.sh must be byte-identical, so
// a diff in CI means a real change.
func TestWriteEnvLibOrderIsStable(t *testing.T) {
	dir := t.TempDir()
	var stores []string
	for _, n := range []string{"a", "b", "c"} {
		s := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Join(s, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, s)
	}
	clo := &Closures{Setups: map[string]ClosureEntry{
		"t-1": {Store: stores[0], Own: stores},
	}}
	first := envFixture(t, map[string]string{"t-1": stores[0] + "|bin"}, clo)
	second := envFixture(t, map[string]string{"t-1": stores[0] + "|bin"}, clo)
	if first != second {
		t.Fatalf("env.sh is not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	// Sorted, so the order does not depend on map iteration.
	if strings.Index(first, stores[0]) > strings.Index(first, stores[2]) {
		t.Errorf("lib dirs are not sorted:\n%s", first)
	}
}
