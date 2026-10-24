package main

// checksum exists so the action does not need sha256sum.

// The action has to verify the easysetup binary it just downloaded, and on a
// minimal image the tools usually used for that - sha256sum, shasum - may not be
// there. One of them will always be present though: the binary being verified.

// The limit is worth stating plainly. Checking a binary with itself catches a
// truncated or corrupted download, which is the failure that actually happens -
// a lost connection, a proxy that cut the body short. It does NOT prove the
// binary is the one we published, because an attacker who can replace the
// download can replace the checksum published beside it. Real tamper-proofing
// would need a signed checksum or a pin inside the action, and neither is worth
// the maintenance for a file downloaded over TLS from our own releases.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdChecksum prints or verifies a file's sha256.
//
//	easysetup checksum <file>                    print the hex digest
//	easysetup checksum --expect <hex> <file>     verify, exit 1 on mismatch
//	easysetup checksum --check <sumfile> <file>  verify against a file written
//	                                             by `sha256sum` ("<hex>  <name>")
func cmdChecksum(args []string) {
	var expect, check string
	var file string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--expect":
			i++
			if i >= len(args) {
				fail(fmt.Errorf("--expect needs a hex digest"))
			}
			expect = args[i]
		case "--check":
			i++
			if i >= len(args) {
				fail(fmt.Errorf("--check needs a file"))
			}
			check = args[i]
		default:
			file = args[i]
		}
	}
	if file == "" {
		fail(fmt.Errorf("usage: easysetup checksum [--expect HEX | --check SUMFILE] <file>"))
	}

	if check != "" {
		raw, err := os.ReadFile(check)
		if err != nil {
			fail(err)
		}
		// Same format sha256sum writes: the digest, whitespace, then a name that
		// may carry a leading '*' for binary mode.
		fields := strings.Fields(string(raw))
		if len(fields) == 0 {
			fail(fmt.Errorf("%s is empty", check))
		}
		expect = fields[0]
	}

	got, err := fileSHA256(file)
	if err != nil {
		fail(err)
	}
	if expect == "" {
		fmt.Println(got)
		return
	}
	if !strings.EqualFold(got, expect) {
		fmt.Fprintf(os.Stderr, "checksum mismatch for %s\n  want %s\n  got  %s\n",
			file, expect, got)
		os.Exit(1)
	}
	fmt.Printf("%s: OK\n", file)
}

// fileSHA256 streams the file through sha256, so a 1.5 GB asset does not have to
// fit in memory.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
