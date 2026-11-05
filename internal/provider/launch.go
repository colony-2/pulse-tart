package provider

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	compute "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/config"
)

var launchID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
func validLaunch(l compute.Launch) bool {
	if !launchID.MatchString(l.LaunchID) || !config.BaseSelector(l.Base) {
		return false
	}
	for _, q := range []int64{l.CPUMillis, l.MemoryBytes, l.ScratchBytes} {
		if q < 0 || q > compute.MaxQuantity {
			return false
		}
	}
	if l.TimeoutSeconds < 0 || l.TimeoutSeconds > 31536000 || len(l.Lease) == 0 || len(l.Lease) > 1<<20 {
		return false
	}
	for _, id := range []string{l.Job.InstanceID, l.Job.TenantID, l.Job.JobID} {
		if id == "" || len(id) > 4096 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
			return false
		}
	}
	if len(l.Job.JobDB) > 8192 || !utf8.ValidString(l.Job.JobDB) {
		return false
	}
	u, e := url.Parse(l.Job.JobDB)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	if len(l.Capabilities) > 256 {
		return false
	}
	for _, c := range l.Capabilities {
		if c == "" || len(c) > 256 || !utf8.ValidString(c) || strings.ContainsRune(c, 0) {
			return false
		}
	}
	return true
}
func copyLaunch(l compute.Launch) compute.Launch {
	l.Capabilities = slices.Clone(l.Capabilities)
	slices.Sort(l.Capabilities)
	l.Capabilities = slices.Compact(l.Capabilities)
	if l.StartBefore != nil {
		v := *l.StartBefore
		l.StartBefore = &v
	}
	return l
}
func (p *Provider) base(name string) config.Base {
	for _, b := range p.Config.Bases {
		if b.Name == name {
			return b
		}
	}
	return config.Base{}
}
func baseMatches(b config.Base, l compute.Launch) bool {
	if !matchBase(l.Base, b.Name) {
		return false
	}
	for _, c := range l.Capabilities {
		if !slices.Contains(b.Capabilities, c) {
			return false
		}
	}
	return true
}

// matchBase treats '*' as zero or more arbitrary characters, including '/'.
// Every other character is literal; filepath.Match would impose extra syntax.
func matchBase(selector, name string) bool {
	parts := strings.Split(selector, "*")
	if len(parts) == 1 {
		return selector == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(name, part)
		if i < 0 {
			return false
		}
		name = name[i+len(part):]
	}
	return strings.HasSuffix(name, parts[len(parts)-1])
}

type process struct {
	Command    []string          `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	WorkingDir string            `json:"working_dir"`
	StdinBytes []byte            `json:"stdin_bytes"`
}
type envelope struct {
	Version  int     `json:"version"`
	LaunchID string  `json:"launch_id"`
	Process  process `json:"process"`
}

func workerProcess(l compute.Launch, b config.Base, cpu, memory int64) process {
	args := []string{"run", "with-lease", "--jobdb", l.Job.JobDB, "--job-id", l.Job.JobID, "--lease-file", "-", "--ci", "--input-mode", "fail", "--execution-cpu", strconv.FormatInt(cpu*1000, 10) + "m", "--execution-memory", strconv.FormatInt(memory, 10) + "Mi", "--execution-platform", "darwin/arm64", "--execution-image", b.Image, "--execution-image-digest", strings.Split(b.Image, "@")[1]}
	for _, cap := range b.Capabilities {
		args = append(args, "--execution-capability", cap)
	}
	return process{Command: []string{"/opt/pulse-tart/bin/c2j"}, Args: args, Env: map[string]string{"PULSE_LAUNCH_ID": l.LaunchID, "PULSE_BASE": b.Name, "PULSE_JOBDB_INSTANCE_ID": l.Job.InstanceID, "PULSE_TENANT_ID": l.Job.TenantID}, WorkingDir: "/Users/pulseworker", StdinBytes: []byte(l.Lease)}
}

// JSON replaces invalid UTF-8 in strings. Hash the opaque credential as bytes
// so two distinct leases can never become the same assignment fingerprint.
func (p *Provider) launchHash(l compute.Launch) string {
	lease := []byte(l.Lease)
	l.Lease = ""
	return p.Store.hash(struct {
		Launch compute.Launch
		Lease  []byte
	}{l, lease})
}
