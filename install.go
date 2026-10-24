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
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// Every setup is an asset of ONE release, tagged `setups`. The tag is fixed
	// rather than dated so each asset keeps one stable URL: a dated tag would make
	// every publish a new release, and an unchanged setup would drop out of
	// /releases/latest the moment anything else changed.
	releaseTag  = "setups"
	releaseBase = "https://github.com/easydev-org/easysetup/releases/download/" + releaseTag
	userAgent   = "easysetup-cli"

	// Checksums are one file for the whole release, not one beside each tarball.
	sumsAsset = "SHA256SUMS"
)

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

	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
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
		if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm); err != nil {
			return err
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
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&os.ModePerm)
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
	default:
		// Character/block devices and fifos do not appear in these closures; skip
		// rather than fail, since a refusal here would be puzzling.
	}
	return nil
}
