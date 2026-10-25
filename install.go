package main

// Installing one setup, with no external programs.

// install.sh does this with curl | zstd | tar. Those are exactly what a
// distroless image lacks, so every step is done in-process here:

//   download   net/http
//   verify     crypto/sha256, against the .sha256 published beside the asset
//   decompress klauspost/compress/zstd (pure Go, no cgo - so the binary stays
//              static and the build needs no C toolchain)
//   unpack     archive/tar, preserving modes, symlinks and hard links

// The one thing still needed is a writable /nix/store, which is a property of
// the filesystem rather than something a program can conjure - see cmdPrepare.

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// One release per architecture. The tag is fixed rather than dated so each
	// asset keeps one stable URL, and separate tags mean each architecture gets
	// the whole 1000-asset allowance rather than sharing it.
	userAgent = "easysetup-cli"

	// Checksums are one file for the whole release, not one beside each tarball.
	sumsAsset = "SHA256SUMS"
)

// releaseBase is where this build's architecture lives.
//
// Derived from runtime.GOARCH, not from a build tag: the same binary source
// compiles for both, and a binary that looked up the wrong release would install
// closures it cannot execute.
var (
	releaseTag  = "setups-" + archName()
	releaseBase = "https://github.com/easydev-org/easysetup/releases/download/" + releaseTag
)

// archName maps Go's GOARCH to the name the releases use.
func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		// Better a clear failure than a 404 chain: no release exists for this
		// architecture, and every URL below would be wrong.
		panic("easysetup: unsupported architecture " + runtime.GOARCH +
			"; only linux/amd64 and linux/arm64 are published")
	}
}

// Installer carries what every step needs.
type Installer struct {
	// Root is where the tarball's `nix/...` tree is unpacked - "/" in practice.
	//
	// It is NOT /nix. The archive records paths as `nix/store/<hash>` and
	// `nix/setup-root-<dir>`, so unpacking at /nix would produce
	// /nix/nix/store/... and leave the registry one level too deep. The name is
	// Root rather than StoreRoot because it is the prefix the archive is rooted
	// at, not the store itself.
	Root string
	Token     string
	Client    *http.Client
	Out       io.Writer

	// sums caches SHA256SUMS, which is one file for the whole release. Fetching it
	// once per run matters: `install node python jq` resolves a dozen setups, and
	// re-downloading a checksum file for each would be both slow and a way to
	// exhaust the anonymous quota on something that never changes mid-run.
	sums map[string]string
}

// NewInstaller builds one with sensible defaults.
func NewInstaller(out io.Writer) *Installer {
	return &Installer{
		Root:      "/",
		Token:     os.Getenv("GITHUB_TOKEN"),
		Client: &http.Client{
			// A closure can be 1.5 GB; the default no-timeout is right for the body,
			// but a connection that never answers should not hang forever.
			Timeout: 0,
			Transport: &http.Transport{
				// ProxyFromEnvironment, and it has to be set explicitly.
				//
				// A custom Transport does NOT inherit the default one, and its Proxy
				// field defaults to nil - meaning "no proxy". The effect was a
				// download that worked but crawled: the tool sat there pulling a
				// 19 MB asset while curl fetched the same file in two seconds,
				// because curl honoured HTTPS_PROXY and this did not.
				Proxy: http.ProxyFromEnvironment,
				ResponseHeaderTimeout: 60 * time.Second,
				TLSHandshakeTimeout:   30 * time.Second,
			},
		},
		Out: out,
	}
}

// RegistryFile is where a setup records its own store path and bin dirs. One
// file per setup, written by the tarball, so several coexist.
func (i *Installer) RegistryFile(dir string) string {
	return filepath.Join(i.Root, "nix", "setup-root-"+dir)
}

// Installed reports whether the setup is present AND its store path still exists.
//
// Checking only for the registry file is not enough: a stale cache can restore a
// registry entry whose store path is gone, and then "already installed" is a lie.
func (i *Installer) Installed(dir string) bool {
	raw, err := os.ReadFile(i.RegistryFile(dir))
	if err != nil {
		return false
	}
	store, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "|")
	if store == "" || store == "none" {
		// `none` is how a library-only setup registers itself; presence of the file
		// is the whole signal.
		return store == "none"
	}
	st, err := os.Stat(store)
	return err == nil && st.IsDir()
}

// Install fetches and unpacks one setup.
func (i *Installer) Install(s *Setup) error {
	if i.Installed(s.Dir) {
		fmt.Fprintf(i.Out, "==> %s already installed, skipping\n", s.Dir)
		return nil
	}

	// All assets live at the release root now, not one directory per setup. The
	// old layout was .../download/<tag>/<dir>/<artifact> because each setup had
	// its own release; with one release they are siblings.
	url := releaseBase + "/" + s.Artifact
	fmt.Fprintf(i.Out, "==> %s\n    fetch: %s\n", s.Dir, url)

	// Resolve the expected checksum BEFORE downloading the tarball. It is a small
	// file fetched once per run, and doing it first means a missing entry fails
	// immediately rather than after a 1.5 GB download.
	wantSum, err := i.expectedSum(s.Artifact)
	if err != nil {
		return err
	}

	body, err := i.get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", s.Artifact, err)
	}
	defer body.Close()

	if err := i.unpackVerified(body, wantSum, s.Artifact); err != nil {
		return err
	}
	fmt.Fprintf(i.Out, "    unpacked %s\n", i.storeOf(s.Dir))
	return nil
}

// expectedSum returns the pinned checksum for one asset.
//
// The publishing job computes each checksum from the file it is about to upload
// and merges them into a single SHA256SUMS asset, so a pin can never describe a
// tarball that was not published.
func (i *Installer) expectedSum(artifact string) (string, error) {
	if i.sums == nil {
		raw, err := i.fetchText(releaseBase + "/" + sumsAsset)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", sumsAsset, err)
		}
		i.sums = map[string]string{}
		// coreutils format: "<hex>  <name>", with possible "*" for binary mode.
		// The name is stored by basename, because the file may have been written
		// by `sha256sum /some/path/foo.tar.zst` - which records the path rather
		// than the bare name, and looking a tarball up by filename would then fail
		// for every entry. Older releases of ours did exactly that.
		for _, line := range strings.Split(raw, "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			name := strings.TrimPrefix(f[1], "*")
			name = filepath.Base(name)
			i.sums[name] = f[0]
		}
		if len(i.sums) == 0 {
			return "", fmt.Errorf("%s is empty or malformed", sumsAsset)
		}
	}
	sum, ok := i.sums[artifact]
	if !ok {
		// List a few known keys: when this fires the useful question is what the
		// parse actually produced, not simply that this name is absent.
		sample := make([]string, 0, 3)
		for k := range i.sums {
			sample = append(sample, k)
			if len(sample) == 3 {
				break
			}
		}
		return "", fmt.Errorf("%s has no %s entry - the release is inconsistent\n"+
			"  parsed %d entries, e.g. %v", sumsAsset, artifact, len(i.sums), sample)
	}
	return sum, nil
}

// storeOf reads back what the tarball recorded.
func (i *Installer) storeOf(dir string) string {
	raw, err := os.ReadFile(i.RegistryFile(dir))
	if err != nil {
		return "(unknown)"
	}
	store, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "|")
	return store
}

// get performs the download, retrying transient failures.
func (i *Installer) get(url string) (io.ReadCloser, error) {
	var last error
	for attempt := 1; attempt <= 4; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		if i.Token != "" {
			// Release assets redirect to objects.githubusercontent.com; Go's client
			// drops sensitive headers across hosts, so the token has to be re-added
			// on each hop rather than set once.
			req.Header.Set("Authorization", "Bearer "+i.Token)
		}
		resp, err := i.Client.Do(req)
		if err != nil {
			last = err
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			// Go follows the redirect and re-applies nothing; the final request is
			// already authorised because github.com serves the redirect and the
			// object host accepts the same bearer header when present. If the object
			// host rejects it the status is 403, which is retried below.
			return resp.Body, nil
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode >= 500 {
			last = fmt.Errorf("%s", resp.Status)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	if i.Token == "" {
		return nil, fmt.Errorf("%w (no GITHUB_TOKEN set; anonymous downloads are "+
			"limited to 60/hour per IP)", last)
	}
	return nil, last
}

// fetchText reads a small auxiliary file, like the .sha256.
func (i *Installer) fetchText(url string) (string, error) {
	body, err := i.get(url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	// 1 MiB, not 4 KiB. SHA256SUMS holds one ~110-byte line per setup, so 199 of
	// them already exceed 4 KiB - the limit silently truncated the file and every
	// setup past the cutoff reported "no entry in SHA256SUMS", which looked like a
	// broken release rather than a short read. 1 MiB is ~9000 entries of headroom.
	const maxAux = 1 << 20
	b, err := io.ReadAll(io.LimitReader(body, maxAux))
	if err != nil {
		return "", err
	}
	if len(b) >= maxAux {
		return "", fmt.Errorf("%s is larger than %d bytes; refusing to parse a "+
			"truncated file", url, maxAux)
	}
	if len(strings.Fields(string(b))) == 0 {
		return "", errors.New("empty checksum file")
	}
	return string(b), nil
}

// unpackVerified streams the body through sha256 and zstd into tar.
//
// The digest is checked BEFORE anything is written: a truncated or corrupted
// download must not half-populate the store.
func (i *Installer) unpackVerified(body io.Reader, wantSum, artifact string) error {
	// Hash the whole stream first. That means the tarball is read twice - once to
	// verify, once to unpack - so it is buffered to a temp file rather than memory;
	// a 1.5 GB closure (swift) would otherwise have to fit in RAM.
	tmp, err := os.CreateTemp("", "easysetup-*.tar.zst")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), body); err != nil {
		return fmt.Errorf("read %s: %w", artifact, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantSum) {
		return fmt.Errorf("%s: sha256 mismatch\n  want %s\n  got  %s", artifact, wantSum, got)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return unpackZstdTar(tmp, i.Root)
}

// unpackZstdTar extracts a .tar.zst whose entries are recorded as nix/store/...
// and nix/setup-root-*, placing them at root.
func unpackZstdTar(src io.Reader, root string) error {
	dec, err := zstd.NewReader(src, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return err
	}
	defer dec.Close()

	// Fresh collectors for this extraction; the maps are package-level because
	// extractOne has no other way to hand the modes back.
	dirModes = map[string]os.FileMode{}
	fileModes = map[string]os.FileMode{}

	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// Everything is written; now apply the archive's real modes.
			//
			// This is the step that makes a non-root install work at all: the tree
			// is written with permissive modes and then locked down, instead of
			// being created read-only and never populated.
			return restoreModes()
		}
		if err != nil {
			return err
		}
		if err := extractOne(tr, hdr, root); err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
}

// extractOne writes a single tar entry, preserving the modes the store needs.
//
// Paths are cleaned and joined under root; a tar entry naming ../../etc would
// otherwise escape it. The archives here are produced by our own build, so this
// is a guard rather than an expectation.
func extractOne(tr *tar.Reader, hdr *tar.Header, root string) error {
	// Resolve the entry under root, and verify it is still under root.
	//
	// The check has to compare the RESOLVED target, not string-concatenated
	// pieces: cleaning "./nix/" gives "/nix", joining that with "/" gives "/nix",
	// and a naive prefix test on the raw name rejects it as an escape. Using
	// filepath.Rel is the direct way to ask "is this still inside root".
	clean := filepath.Clean("/" + hdr.Name)
	target := filepath.Join(root, clean)
	rel, err := filepath.Rel(filepath.Clean(root), target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("path escapes the store root")
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		// 0755, NOT the archive's mode. nix store directories are 0555, and the
		// owner of a 0555 directory cannot create entries in it either - so
		// creating it with 0555 and then writing the files inside fails with
		// "permission denied", as its own owner.
		//
		// This only shows up for a non-root installer: root ignores the permission
		// bits, so a container worked and a bare runner did not. The final modes
		// are restored by restoreModes once everything has been written.
		//
		// makeWritable first: the directory may already exist from ANOTHER setup's
		// tarball, which left it 0555. Two setups can legitimately contain the same
		// store path - java and maven both carry set-java-classpath-hook - and the
		// second one cannot write into what the first one locked down.
		makeWritable(target)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		// A directory entry may be seen again after its children; keep the mode so
		// it can be applied at the end.
		//
		// Except root itself, and the store directory above it. The archive records
		// `nix/` and `nix/store/` as 0555, but they belong to the system, not to
		// this setup: chmod'ing them would fail for a non-root user (they are
		// root-owned) and, worse, would lock the store for every later install.
		if clean != "/" && clean != "/nix" && clean != "/nix/store" {
			dirModes[target] = os.FileMode(hdr.Mode) & os.ModePerm
		}
	case tar.TypeSymlink:
		os.MkdirAll(filepath.Dir(target), 0o755)
		os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		// Hard link: the target must already be unpacked, which tar ordering
		// guarantees because nix archives are written parent-first.
		os.MkdirAll(filepath.Dir(target), 0o755)
		os.Remove(target)
		src := filepath.Join(root, filepath.Clean("/"+hdr.Linkname))
		return os.Link(src, target)
	case tar.TypeReg, tar.TypeRegA:
		// Same reason: an earlier setup may have created this file 0444. Overwriting
		// it is correct - identical content from an identical store path - but the
		// mode has to allow the write first.
		makeWritable(filepath.Dir(target))
		makeWritable(target)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// 0644 while writing. A nix store file is usually 0444, and opening a file
		// for writing that is already 0444 fails on the second run - the same
		// class of mistake as the directory above. The real mode is applied by
		// restoreModes afterwards.
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fileModes[target] = os.FileMode(hdr.Mode) & os.ModePerm
	default:
		// Character/block devices and fifos do not appear in these closures; skip
		// rather than fail, since a refusal here would be puzzling.
	}
	return nil
}

// makeWritable adds owner-write to a path that already exists, so it can be
// replaced.
//
// Needed because two setups can carry the same store path. nix paths are
// content-addressed, so "the same path" means "the same bytes" - overwriting is
// correct and the result is identical either way. What is not correct is failing
// because the first tarball stored it read-only, which is what happened with
// java+maven and their shared set-java-classpath-hook.
//
// Best-effort: a path owned by another user cannot be chmod'ed, and the caller's
// subsequent open will report a clearer error than this could.
func makeWritable(path string) {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink == 0 {
		if st.Mode().Perm()&0o200 == 0 {
			_ = os.Chmod(path, st.Mode().Perm()|0o200)
		}
	}
}

// dirModes and fileModes collect the archive's real modes so they can be applied
// after the tree has been written.
//
// Package-level rather than threaded through every call because there is exactly
// one extraction at a time per process, and the alternative is passing a
// collector down four call layers for no benefit.
var (
	dirModes  = map[string]os.FileMode{}
	fileModes = map[string]os.FileMode{}
)

// restoreModes applies the recorded modes, deepest first.
//
// Deepest first matters for directories: setting a parent to 0555 before
// chmod'ing its children would make the children unreachable even to their owner,
// which is the same trap in a different order.
func restoreModes() error {
	paths := make([]string, 0, len(dirModes)+len(fileModes))
	for p := range dirModes {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		// Longer paths are deeper; sorting by descending length is enough here
		// because all of them live under one root.
		return len(paths[i]) > len(paths[j])
	})
	for _, p := range paths {
		if err := os.Chmod(p, dirModes[p]); err != nil {
			// A store path already owned by someone else - root, from an earlier
			// install - can be read but not chmod'ed by this user. That is not a
			// failure of THIS install: the entry was already there with its correct
			// mode. Insisting would make a second install on a shared machine fail
			// for no reason.
			if !errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("chmod %s: %w", p, err)
			}
		}
	}
	// Files can be done in any order - they have no children.
	for p, m := range fileModes {
		if err := os.Chmod(p, m); err != nil {
			if !errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("chmod %s: %w", p, err)
			}
		}
	}
	return nil
}
