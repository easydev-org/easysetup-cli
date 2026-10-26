package main

// The manifest is the same file the shell installer reads, parsed by the same
// rules. Keeping them identical matters: a name that resolves one way in
// install.sh and another here would install a different version depending on
// which tool the user happened to run.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Setup is one publishable unit.
type Setup struct {
	Tool    string `yaml:"tool"`
	Attr    string `yaml:"attr"`
	Version string `yaml:"version"`
	// Suffix disambiguates two entries that share a version: python313Packages.mypy
	// and python314Packages.mypy are both 2.1.0, so one is "mypy-2.1.0" and the
	// other "mypy-2.1.0-py3.13".
	Suffix string `yaml:"suffix"`
	Bins   string `yaml:"bins"`

	Dir      string `yaml:"-"`
	Artifact string `yaml:"-"`
}

// Manifest is the parsed matrix-nix.yml.
type Manifest struct {
	Setups []Setup `yaml:"setups"`
}

// Load parses the manifest and fills in the derived fields the way matrixlib2.py
// does, so directory names and asset names are identical between the two.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if len(m.Setups) == 0 {
		return nil, fmt.Errorf("manifest: no setups in %s", path)
	}
	for i := range m.Setups {
		s := &m.Setups[i]
		if s.Tool == "" || s.Attr == "" || s.Version == "" {
			return nil, fmt.Errorf("manifest: entry missing tool/attr/version: %+v", *s)
		}
		s.Dir = fmt.Sprintf("%s-%s%s", s.Tool, s.Version, s.Suffix)
		if s.Bins == "" {
			s.Bins = "bin"
		}
		s.Artifact = s.Dir + "-debian-trixie-amd64.tar.zst"
	}
	return &m, nil
}

// Resolve turns what the user typed into exactly one setup.
//
// The rules mirror matrixlib2.resolve deliberately, including the order:
//
//	swift-6.3.3   full directory
//	mypy-py3.13   tool plus suffix, for entries sharing a version
//	node-24       version prefix
//	node          the NEWEST version of that tool
//
// A bare tool name means the newest rather than an error, because erroring made
// the most obvious thing to type the one thing that failed.
func (m *Manifest) Resolve(name string) (*Setup, error) {
	for i := range m.Setups {
		if m.Setups[i].Dir == name {
			return &m.Setups[i], nil
		}
	}

	// Version prefix: node-24 matches node-24.21.0. The boundary after the version
	// has to be a `.` or `-` or end of string - matching "node-24-" would never hit,
	// because the directory carries another segment.
	var asDir []*Setup
	for i := range m.Setups {
		d := m.Setups[i].Dir
		if d == name || strings.HasPrefix(d, name+".") || strings.HasPrefix(d, name+"-") {
			asDir = append(asDir, &m.Setups[i])
		}
	}
	if len(asDir) == 1 {
		return asDir[0], nil
	}

	// Tool plus suffix: mypy-py3.13.
	var bySuffix []*Setup
	for i := range m.Setups {
		s := &m.Setups[i]
		suf := strings.TrimPrefix(s.Suffix, "-")
		if suf != "" && name == s.Tool+"-"+suf {
			bySuffix = append(bySuffix, s)
		}
	}
	if len(bySuffix) == 1 {
		return bySuffix[0], nil
	}

	// Bare tool name: the newest version.
	var asTool []*Setup
	for i := range m.Setups {
		if m.Setups[i].Tool == name {
			asTool = append(asTool, &m.Setups[i])
		}
	}
	if len(asTool) == 1 {
		return asTool[0], nil
	}
	if len(asTool) > 1 {
		// Newest version wins; on a tie the UNSUFFIXED entry wins.
		//
		// The tie is not hypothetical: pip-26.1.2 (for python 3.14) and
		// pip-26.1.2-py3.13 (for 3.13) share a version string, because the pip
		// release is the same for both. sort.Slice is not stable, so the winner
		// was whichever the sort happened to leave last - and it picked the 3.13
		// build. `install python pip` then installed python 3.14 alongside a pip
		// bound to 3.13, and `python3` had no pip module at all.
		//
		// Sort ASCENDING by (version, suffixed?), then take the last - so the
		// winner is the highest version, and on a tie the UNSUFFIXED one.
		//
		// The tie-break has to sort the suffixed entry EARLIER for the last element
		// to be the plain one: `less(suffixed, plain)` must be true. Writing it the
		// other way round (as a first attempt did) leaves the suffixed entry last
		// and picks exactly the wrong one.
		sort.SliceStable(asTool, func(i, j int) bool {
			if asTool[i].Version != asTool[j].Version {
				return VersionLess(asTool[i].Version, asTool[j].Version)
			}
			// Equal version: suffixed sorts before plain, so plain ends up last.
			return asTool[i].Suffix != "" && asTool[j].Suffix == ""
		})
		return asTool[len(asTool)-1], nil
	}

	hits := asDir
	if len(hits) < 2 {
		hits = bySuffix
	}
	if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.Dir
		}
		return nil, fmt.Errorf("%q is ambiguous: %s", name, strings.Join(names, ", "))
	}
	return nil, fmt.Errorf("no setup matches %q", name)
}

var versionPart = regexp.MustCompile(`^(\d+)$`)
var digits = regexp.MustCompile(`\d+`)

// VersionLess compares versions numerically, so 1.10 sorts after 1.9 - which
// string order gets backwards. A trailing +N build tag is a tie-break, and a
// non-numeric segment sorts low rather than panicking.
func VersionLess(a, b string) bool {
	ka, kb := versionKey(a), versionKey(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i] != kb[i] {
			return ka[i] < kb[i]
		}
	}
	return len(ka) < len(kb)
}

func versionKey(v string) []int {
	main, build, _ := strings.Cut(v, "+")
	var out []int
	for _, part := range strings.FieldsFunc(main, func(r rune) bool {
		return r == '.' || r == '-'
	}) {
		if m := versionPart.FindStringSubmatch(part); m != nil {
			n, _ := strconv.Atoi(m[1])
			out = append(out, n)
		} else {
			out = append(out, -1)
		}
	}
	for _, d := range digits.FindAllString(build, -1) {
		n, _ := strconv.Atoi(d)
		out = append(out, n)
	}
	return out
}

// Find returns the setup whose Dir matches exactly, or nil.
func (m *Manifest) Find(dir string) *Setup {
	for i := range m.Setups {
		if m.Setups[i].Dir == dir {
			return &m.Setups[i]
		}
	}
	return nil
}
