// Package blocks is the catalog of building blocks a project can enable:
// shared services such as MariaDB (ADR 0013) and per-sandbox services such
// as Redis (ADR 0015). The catalog is Go code, not
// a file an agent or user could edit; projects only choose a profile and
// tags (versions) within it.
//
// The package uses only the standard library, so wsm-helper can import it
// to validate the service and tag of a request.
package blocks

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Kinds of blocks.
const (
	// KindShared: one instance per (block, tag), shared by every project
	// whose ceiling allows the tag.
	KindShared = "shared"
	// KindPerSandbox: one instance per sandbox (M3d).
	KindPerSandbox = "per-sandbox"
)

// Block describes one catalog entry.
type Block struct {
	ID   string
	Kind string
	// Image is the repository without a tag; the tag comes from the ceiling.
	Image string
	// ContainerPort is the port the service listens on inside its container.
	ContainerPort int
	// Profiles are the allowed profiles; the first one is the default.
	Profiles []string
	// MinTag is the oldest version a ceiling may list, as MAJOR.MINOR.
	MinTag string
}

// DefaultProfile returns the block's default profile.
func (b Block) DefaultProfile() string { return b.Profiles[0] }

// HasProfile reports whether p is one of the block's profiles.
func (b Block) HasProfile(p string) bool {
	for _, q := range b.Profiles {
		if q == p {
			return true
		}
	}
	return false
}

// ImageRef returns the image reference for tag. The tag must be valid.
func (b Block) ImageRef(tag string) string { return b.Image + ":" + tag }

// catalog is the fixed list of blocks.
var catalog = []Block{
	{
		ID:            "mariadb",
		Kind:          KindShared,
		Image:         "docker.io/library/mariadb",
		ContainerPort: 3306,
		Profiles:      []string{"dev"},
		MinTag:        "10.11",
	},
	{
		ID:            "redis",
		Kind:          KindPerSandbox,
		Image:         "docker.io/library/redis",
		ContainerPort: 6379,
		Profiles:      []string{"dev"},
		MinTag:        "7.2",
	},
}

// Lookup returns the block with the given ID.
func Lookup(id string) (Block, bool) {
	for _, b := range catalog {
		if b.ID == id {
			b.Profiles = append([]string(nil), b.Profiles...)
			return b, true
		}
	}
	return Block{}, false
}

// All returns every block in the catalog.
func All() []Block {
	out := make([]Block, 0, len(catalog))
	for _, b := range catalog {
		b.Profiles = append([]string(nil), b.Profiles...)
		out = append(out, b)
	}
	return out
}

// LookupShared returns the shared block with the given ID.
func LookupShared(id string) (Block, error) {
	b, ok := Lookup(id)
	if !ok {
		return Block{}, fmt.Errorf("unknown block %q", id)
	}
	if b.Kind != KindShared {
		return Block{}, fmt.Errorf("block %q is not a shared service", id)
	}
	return b, nil
}

// Tag limits.
const (
	// MaxTags is the most tags a project ceiling may list per block.
	MaxTags = 8
	// maxTagLen is the longest possible valid tag: "999.999.999".
	maxTagLen = 11
)

// ValidTagFormat checks that tag is MAJOR.MINOR or MAJOR.MINOR.PATCH:
// decimal numbers without leading zeros, at most three digits each. There
// is no "latest" and no suffix. It does not check a block's minimum; use
// Block.ValidTag for that.
func ValidTagFormat(tag string) error {
	_, err := parseTag(tag)
	return err
}

// ValidTag checks the format of tag and that it is at least b.MinTag.
func (b Block) ValidTag(tag string) error {
	nums, err := parseTag(tag)
	if err != nil {
		return err
	}
	min, err := parseTag(b.MinTag)
	if err != nil {
		return fmt.Errorf("block %s: bad minimum tag: %w", b.ID, err)
	}
	if nums[0] < min[0] || (nums[0] == min[0] && nums[1] < min[1]) {
		return fmt.Errorf("tag %q: the oldest supported version of %s is %s", tag, b.ID, b.MinTag)
	}
	return nil
}

// parseTag parses a tag in the format ValidTagFormat describes.
func parseTag(tag string) ([]int, error) {
	if len(tag) == 0 || len(tag) > maxTagLen {
		return nil, fmt.Errorf("tag %q: use MAJOR.MINOR or MAJOR.MINOR.PATCH", tag)
	}
	parts := strings.Split(tag, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return nil, fmt.Errorf("tag %q: use MAJOR.MINOR or MAJOR.MINOR.PATCH", tag)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, ok := number(p)
		if !ok {
			return nil, fmt.Errorf("tag %q: use MAJOR.MINOR or MAJOR.MINOR.PATCH with numbers of 1-3 digits and no leading zeros", tag)
		}
		nums[i] = n
	}
	return nums, nil
}

// number parses a decimal number of 1-3 ASCII digits without a leading zero.
func number(s string) (int, bool) {
	if len(s) == 0 || len(s) > 3 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Slug turns a valid tag into a name part: "11.8" becomes "11-8".
func Slug(tag string) string { return strings.ReplaceAll(tag, ".", "-") }

// ValidateCeiling checks a project ceiling for a block: a known block, a
// profile of that block and 1 to MaxTags valid tags without duplicates.
// An empty profile is replaced by the default; the result is the profile
// to store.
func ValidateCeiling(blockID, profile string, tags []string) (string, error) {
	b, ok := Lookup(blockID)
	if !ok {
		return "", fmt.Errorf("unknown block %q", blockID)
	}
	if profile == "" {
		profile = b.DefaultProfile()
	}
	if !b.HasProfile(profile) {
		return "", fmt.Errorf("block %s has no profile %q", blockID, profile)
	}
	if len(tags) == 0 {
		return "", errors.New("give at least one tag")
	}
	if len(tags) > MaxTags {
		return "", fmt.Errorf("give at most %d tags", MaxTags)
	}
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		if err := b.ValidTag(t); err != nil {
			return "", err
		}
		if seen[t] {
			return "", fmt.Errorf("tag %q is listed twice", t)
		}
		seen[t] = true
	}
	return profile, nil
}
