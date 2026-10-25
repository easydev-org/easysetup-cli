package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// fetchMetadata downloads the manifest and the dependency graph into a temp dir.
//
// Both are read from the main easysetup repository at run time. That is a
// deliberate choice over embedding: the binary stays a single file that never
// needs rebuilding when a setup is added, and it downloads tarballs anyway, so
// the network is already required.
func fetchMetadata(inst *Installer) (string, error) {
	work, err := os.MkdirTemp("", "easysetup-meta-*")
	if err != nil {
		return "", err
	}
	// The graph is per architecture - the store paths inside it are - so this
	// fetches the one matching the running binary. A shared name would either be
	// wrong for one architecture or need both downloaded and one thrown away.
	for _, name := range []string{"matrix-nix.yml", "closures-" + archName() + ".json"} {
		if err := inst.downloadTo(filepath.Join(work, name), rawBase+"/"+name); err != nil {
			os.RemoveAll(work)
			return "", fmt.Errorf("fetch %s: %w", name, err)
		}
	}
	return work, nil
}

// downloadTo writes url to path.
func (i *Installer) downloadTo(path, url string) error {
	body, err := i.get(url)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, body)
	return err
}

// cmdInstall installs every name given, dependencies first.
func cmdInstall(inst *Installer, man *Manifest, clo *Closures, args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("name at least one setup; 'easysetup list' shows them"))
	}

	// Resolve everything before installing anything: a typo in the third name
	// should not leave the first two installed.
	type job struct{ setup *Setup }
	var jobs []job
	queued := map[string]bool{}

	for _, name := range args {
		s, err := man.Resolve(name)
		if err != nil {
			fail(err)
		}
		for _, dir := range clo.InstallOrder(s.Dir) {
			if queued[dir] {
				continue
			}
			queued[dir] = true
			dep := man.Find(dir)
			if dep == nil {
				fail(fmt.Errorf("%s needs %s, which is not in the manifest", s.Dir, dir))
			}
			jobs = append(jobs, job{dep})
		}
	}

	var installed []string
	for _, j := range jobs {
		if err := inst.Install(j.setup); err != nil {
			fail(err)
		}
		installed = append(installed, j.setup.Dir)
	}

	if err := inst.WriteEnv(envPath); err != nil {
		fail(err)
	}
	fmt.Fprintf(inst.Out, "==> installed: %s\n", strings.Join(installed, " "))
	fmt.Fprintf(inst.Out, "    now run:  . %s\n", envPath)
}

// cmdList prints every setup, newest version last for each tool.
func cmdList(man *Manifest) {
	sorted := make([]*Setup, len(man.Setups))
	for i := range man.Setups {
		sorted[i] = &man.Setups[i]
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Dir < sorted[j].Dir })
	for _, s := range sorted {
		fmt.Printf("%-36s %s\n", s.Dir, s.Version)
	}
}

// envPath matches what the action and install.sh use.
const envPath = "/opt/setup/env.sh"

// cmdEnv writes env.sh from everything installed and prints it.
func cmdEnv(inst *Installer, args []string) {
	if len(args) > 0 {
		out := args[0]
		if err := inst.WriteEnv(out); err != nil {
			fail(err)
		}
		b, _ := os.ReadFile(out)
		fmt.Print(string(b))
		return
	}
	if err := inst.WriteEnv(envPath); err != nil {
		fail(err)
	}
	b, _ := os.ReadFile(envPath)
	fmt.Print(string(b))
}

// cmdVerify reports what is installed without changing anything.
func cmdVerify(inst *Installer, man *Manifest, args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("name at least one setup"))
	}
	bad := 0
	for _, name := range args {
		s, err := man.Resolve(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%-28s RESOLVE: %v\n", name, err)
			bad++
			continue
		}
		raw, err := os.ReadFile(inst.RegistryFile(s.Dir))
		if err != nil {
			fmt.Printf("%-28s NOT INSTALLED\n", s.Dir)
			bad++
			continue
		}
		store, dirs, _ := strings.Cut(strings.TrimSpace(string(raw)), "|")
		if store == "none" {
			fmt.Printf("%-28s installed (library set, nothing on PATH)\n", s.Dir)
			continue
		}
		if st, err := os.Stat(store); err != nil || !st.IsDir() {
			fmt.Printf("%-28s BROKEN: registry names %s, which is gone\n", s.Dir, store)
			bad++
			continue
		}
		fmt.Printf("%-28s ok  %s  bins=%s\n", s.Dir, store, dirs)
	}
	if bad > 0 {
		os.Exit(1)
	}
}

// WriteEnv generates env.sh from the registry files.
//
// It scans rather than taking the list it just installed, for the same reason
// install.sh does: this runs once per invocation, and generating from "what this
// call installed" would make a second call erase the first.
func (i *Installer) WriteEnv(path string) error {
	// The registry lives under <Root>/nix, beside the store, because that is where
	// the tarball puts it.
	nixDir := filepath.Join(i.Root, "nix")
	entries, err := os.ReadDir(nixDir)
	if err != nil {
		return err
	}
	var paths []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "setup-root-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(nixDir, e.Name()))
		if err != nil {
			continue
		}
		store, dirs, _ := strings.Cut(strings.TrimSpace(string(raw)), "|")
		if store == "" || store == "none" {
			continue
		}
		for _, d := range strings.Fields(dirs) {
			paths = append(paths, store+"/"+d)
		}
	}
	sort.Strings(paths)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# generated by easysetup — do not edit, rerun it instead\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "PATH=\"%s:${PATH}\"\n", p)
	}
	b.WriteString("export PATH\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// cmdPrepare makes /nix/store exist and be writable.
//
// The only step that needs root, and it cannot be avoided: every binary in these
// closures names /nix/store/<hash>-glibc/lib/ld-linux-x86-64.so.2 as its ELF
// interpreter, and the kernel resolves that before the program starts. No
// environment variable redirects it, so the path has to exist.
func cmdPrepare(inst *Installer) {
	const store = "/nix/store"
	if st, err := os.Stat(store); err == nil && st.IsDir() {
		if err := os.Chmod("/nix", 0o777); err == nil {
			_ = os.Chmod(store, 0o777)
			fmt.Printf("%s already exists and is writable\n", store)
			return
		}
		// Exists but we cannot chmod it: that means we are not root. Whether it is
		// already usable is what matters.
		f, err := os.CreateTemp(store, ".write-test")
		if err == nil {
			f.Close()
			os.Remove(f.Name())
			fmt.Printf("%s is writable by %d\n", store, os.Getuid())
			return
		}
		fail(fmt.Errorf(`%s exists but is not writable, and this process is not root

Every easysetup binary names /nix/store as its ELF interpreter, so the path must
exist and be usable. Run one of:

  as root:            easysetup prepare
  or in the image:    RUN mkdir -p /nix/store && chmod -R 777 /nix`, store))
	}

	// Not root: use sudo, as the shell installer does.
	//
	// `mkdir /nix` needs root because / is root-owned. A bare GitHub runner is a
	// normal user with passwordless sudo, which is the common case this missed -
	// the first version only tried the direct mkdir and failed with "mkdir /nix:
	// permission denied" while a container (root) worked, so the failure only
	// appeared on the one kind of runner that could not be tested locally.
	if err := os.MkdirAll(store, 0o777); err != nil {
		if _, lerr := exec.LookPath("sudo"); lerr != nil {
			fail(fmt.Errorf(`cannot create %s: %w

Every easysetup binary names /nix/store as its ELF interpreter, so the path must
exist. Options:

  as root:            easysetup prepare
  with sudo:          sudo easysetup prepare
  or in the image:    RUN mkdir -p /nix/store && chmod -R 777 /nix
  or use the root tag of a distroless image for this one step: gcr.io/distroless/static:latest`, store, err))
		}
		fmt.Printf("not root; creating %s with sudo\n", store)
		for _, cmd := range [][]string{
			{"sudo", "mkdir", "-p", store},
			{"sudo", "chmod", "777", "/nix"},
			{"sudo", "chmod", "777", store},
		} {
			c := exec.Command(cmd[0], cmd[1:]...)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				fail(fmt.Errorf("%s: %w", strings.Join(cmd, " "), err))
			}
		}
		fmt.Printf("created %s (mode 777)\n", store)
		return
	}
	_ = os.Chmod("/nix", 0o777)
	_ = os.Chmod(store, 0o777)
	fmt.Printf("created %s (mode 777)\n", store)
}

// ensure http is referenced even when the build tags trim something.
var _ = http.StatusOK
