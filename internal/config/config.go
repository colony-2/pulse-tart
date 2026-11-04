// Package config contains the typed configuration shared by both backends.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var Err = errors.New("invalid provider configuration")

const DefaultCPU int64 = 4
const DefaultMemoryMiB int64 = 8192
const WorkerNameLabel = "org.cirruslabs.orchard.worker-name"

// Secret redacts formatted credentials. Explicit string conversion exposes bytes.
type Secret string

func (Secret) String() string   { return "[REDACTED]" }
func (Secret) GoString() string { return "[REDACTED]" }

// Config is identical for Orchard and direct Tart. Paths are absolute; the host
// owns configuration loading. A nil GuestBundle selects the embedded bundle.
type Config struct {
	OwnerID          string
	StateDir         string
	Bases            []Base
	Slots            []SlotGroup
	GuestBundle      []byte
	PollInterval     time.Duration
	BootstrapTimeout time.Duration
}

// Base names a concrete macOS ARM64 environment and its explicitly declared
// aliases. Image must be a digest-pinned Tart OCI reference. OSVersion is checked
// inside the template before it can become ready. No aliases are inferred.
type Base struct {
	Name        string
	Aliases     []string
	Image       string
	OSVersion   string
	SSHUsername string
	SSHPassword Secret
	// Optional authorized_keys-format SSH server public key for direct Tart.
	SSHHostKey []byte
	// Capabilities are restricted to built-in probes: nix, git, node, pnpm, uv, python.
	Capabilities []string
}

type SlotGroup struct {
	Name      string
	Count     int
	Base      string
	CPU       int64
	MemoryMiB int64
}

// Orchard adds connectivity and placement only; the execution configuration is shared.
type Orchard struct {
	URL               string
	Worker            string
	WorkerLabels      map[string]string
	Username          string
	Token             Secret
	CACert            []byte
	AllowInsecureHTTP bool
	RequestTimeout    time.Duration
}

type Tart struct {
	// Binary defaults to tart on PATH. Home defaults to ~/.tart.
	Binary         string
	Home           string
	CommandTimeout time.Duration
	SSHTimeout     time.Duration
}

// Settings is implementation-only: the public Config has no transport mode.
type Settings struct {
	Config
	Kind         string
	Endpoint     string
	Worker       string
	WorkerLabels map[string]string
}

func (c Settings) Scope() string {
	h := sha256.Sum256([]byte(c.Kind + "\x00" + c.Endpoint + "\x00" + c.OwnerID))
	return hex.EncodeToString(h[:])
}
func (c Config) Capacity() int {
	total := 0
	for _, s := range c.Slots {
		total += s.Count
	}
	return total
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var digestImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:/_-]*@sha256:[a-f0-9]{64}$`)
var osVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
var capabilities = map[string]bool{"nix": true, "git": true, "node": true, "pnpm": true, "uv": true, "python": true}

func Name(s string) bool { return namePattern.MatchString(s) }

// Normalize validates and deep-copies all retained input. Defaults apply equally
// to both backends. Counts have no default: zero explicitly drains a group.
func Normalize(c Config) (Config, error) {
	if !Name(c.OwnerID) || len(c.OwnerID) > 64 || !filepath.IsAbs(c.StateDir) || len(c.Bases) == 0 || len(c.Slots) == 0 {
		return Config{}, Err
	}
	c.StateDir = filepath.Clean(c.StateDir)
	c.GuestBundle = slices.Clone(c.GuestBundle)
	c.Bases = slices.Clone(c.Bases)
	c.Slots = slices.Clone(c.Slots)
	if c.PollInterval == 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.BootstrapTimeout == 0 {
		c.BootstrapTimeout = 45 * time.Minute
	}
	if c.PollInterval < time.Second || c.PollInterval > time.Minute || c.BootstrapTimeout < time.Minute || c.BootstrapTimeout > 2*time.Hour {
		return Config{}, Err
	}
	seen := map[string]bool{}
	for i := range c.Bases {
		b := &c.Bases[i]
		if !Name(b.Name) || seen[b.Name] || !digestImage.MatchString(b.Image) || !osVersion.MatchString(b.OSVersion) || b.SSHPassword == "" {
			return Config{}, Err
		}
		seen[b.Name] = true
		b.Aliases = slices.Clone(b.Aliases)
		b.Capabilities = slices.Clone(b.Capabilities)
		b.SSHHostKey = slices.Clone(b.SSHHostKey)
		for _, a := range b.Aliases {
			if !Name(a) {
				return Config{}, Err
			}
		}
		for _, cap := range b.Capabilities {
			if !capabilities[cap] {
				return Config{}, Err
			}
		}
		slices.Sort(b.Capabilities)
		b.Capabilities = slices.Compact(b.Capabilities)
		if b.SSHUsername == "" {
			b.SSHUsername = "admin"
		}
		if !Name(b.SSHUsername) || len(b.SSHPassword) > 16384 {
			return Config{}, Err
		}
	}
	// An alias must not shadow another concrete environment's identity.
	for _, b := range c.Bases {
		for _, alias := range b.Aliases {
			if alias != b.Name && seen[alias] {
				return Config{}, Err
			}
		}
	}
	groups := map[string]bool{}
	for i := range c.Slots {
		s := &c.Slots[i]
		if !Name(s.Name) || len(s.Name) > 32 || groups[s.Name] || s.Count < 0 || s.Count > 1000 || !seen[s.Base] {
			return Config{}, Err
		}
		groups[s.Name] = true
		if s.CPU == 0 {
			s.CPU = DefaultCPU
		}
		if s.MemoryMiB == 0 {
			s.MemoryMiB = DefaultMemoryMiB
		}
		if s.CPU < 1 || s.CPU > 256 || s.MemoryMiB < 1024 || s.MemoryMiB > 1048576 {
			return Config{}, Err
		}
	}
	if c.Capacity() > 1000 {
		return Config{}, Err
	}
	return c, nil
}
func NormalizeURL(raw string, allowHTTP bool) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP)) {
		return "", Err
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/v1") {
		u.Path += "/v1"
	}
	u.RawPath = ""
	return u.String(), nil
}
