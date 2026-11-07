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
	"unicode/utf8"
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
// owns configuration loading. C2JVersion is used only to prepare missing images.
type Config struct {
	OwnerID          string
	StateDir         string
	Bases            []Base // Most preferred first; discovery preserves this order.
	Slots            []SlotGroup
	C2JVersion       string // Defaults to latest published release; existing images are reused.
	PollInterval     time.Duration
	BootstrapTimeout time.Duration
	Docker           *Docker
}

// Base names a concrete ARM64 environment. Image must be a digest-pinned
// Tart OCI reference. OSVersion is checked inside the template before readiness.
type Base struct {
	Name        string
	OS          string // darwin (default) or linux; ARM64 only.
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
	Name          string
	Count         int
	Base          string
	CPU           int64
	MemoryMiB     int64
	DockerProfile string // Empty disables Docker for this group.
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
	DockerEndpoint    Endpoint
}

type Tart struct {
	// Binary defaults to tart on PATH. Home defaults to ~/.tart.
	Binary         string
	Home           string
	CommandTimeout time.Duration
	SSHTimeout     time.Duration
	DockerEndpoint Endpoint
	PythonBinary   string // Worker-local bridge supervisor; defaults to /usr/bin/python3.
}

// Settings is implementation-only: the public Config has no transport mode.
type Settings struct {
	Config
	Kind           string
	Endpoint       string
	Worker         string
	WorkerLabels   map[string]string
	DockerEndpoint Endpoint
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

// BaseSelector follows the shared API's label grammar. Only '*' is special.
func BaseSelector(s string) bool {
	return s != "" && len(s) <= 4096 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00 \t\r\n")
}

// Normalize validates and deep-copies all retained input. Defaults apply equally
// to both backends. Counts have no default: zero explicitly drains a group.
func Normalize(c Config) (Config, error) {
	if !Name(c.OwnerID) || len(c.OwnerID) > 64 || !filepath.IsAbs(c.StateDir) || len(c.Bases) == 0 || len(c.Slots) == 0 {
		return Config{}, Err
	}
	c.StateDir = filepath.Clean(c.StateDir)
	if c.C2JVersion == "" {
		c.C2JVersion = "latest"
	}
	if len(c.C2JVersion) > 200 || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`).MatchString(c.C2JVersion) {
		return Config{}, Err
	}
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
		if b.OS == "" {
			b.OS = "darwin"
		}
		if b.OS != "darwin" && b.OS != "linux" {
			return Config{}, Err
		}
		if !BaseSelector(b.Name) || strings.Contains(b.Name, "*") || seen[b.Name] || !digestImage.MatchString(b.Image) || !osVersion.MatchString(b.OSVersion) || b.SSHPassword == "" {
			return Config{}, Err
		}
		seen[b.Name] = true
		b.Capabilities = slices.Clone(b.Capabilities)
		b.SSHHostKey = slices.Clone(b.SSHHostKey)
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
	if err := normalizeDocker(&c); err != nil {
		return Config{}, err
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
