package provider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"golang.org/x/crypto/ssh"
	"io/fs"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type dockerBackend interface {
	Exec(context.Context, string, string, []byte) ([]byte, error)
	Stop(context.Context, string, string) error
	Endpoint(context.Context, string, string, int) (bool, error)
}

// PairRecord owns unused warm capacity as well as accepted allocations. No lease
// is stored here: handoff travels over management SSH stdin, only to Primary.
type PairRecord struct {
	ID, SlotID, ConfigHash, Base, Profile, Phase   string
	Primary, Engine                                backend.Spec
	PrimaryUID, EngineUID, PrimaryHash, EngineHash string
	PrimaryDispatched, EngineDispatched            bool
	Port                                           int
	EngineID, LaunchID, DeliveryHash               string
	Created                                        time.Time
}

func (p *Provider) dockerGroup(slot string) config.SlotGroup {
	for _, g := range p.Config.Slots {
		if strings.HasPrefix(slot, g.Name+"-") {
			suffix := strings.TrimPrefix(slot, g.Name+"-")
			if len(suffix) == 4 {
				if _, e := strconv.Atoi(suffix); e == nil {
					return g
				}
			}
		}
	}
	return config.SlotGroup{}
}
func (p *Provider) dockerHash(g config.SlotGroup) string {
	// Pool growth does not change an existing slot's execution contract.
	g.Count = 0
	return p.Store.hash(struct {
		Base     config.Base
		Group    config.SlotGroup
		Engine   config.EngineProfile
		Endpoint config.Endpoint
	}{p.base(g.Base), g, p.Config.Engine(g.DockerProfile), p.Config.DockerEndpoint})
}
func (p *Provider) dockerMatch(g config.SlotGroup, l api.Launch) bool {
	if g.DockerProfile == "" {
		return false
	}
	b := p.base(g.Base)
	b.Capabilities = append(slices.Clone(b.Capabilities), "docker")
	return baseMatches(b, l) && g.CPU*1000 >= l.CPUMillis && (g.MemoryMiB-512)*(1<<20) >= l.MemoryBytes
}
func shell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func fileScript(path string, data []byte, linux bool) string {
	decode := "-D"
	if linux {
		decode = "-d"
	}
	return fmt.Sprintf("printf '%%s' %s | base64 %s | sudo -n tee %s >/dev/null\nsudo -n chmod 600 %s\n", shell(base64.StdEncoding.EncodeToString(data)), decode, shell(path), shell(path))
}
func keyPair() ([]byte, []byte, error) {
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return nil, nil, e
	}
	block, e := ssh.MarshalPrivateKey(priv, "")
	if e != nil {
		return nil, nil, e
	}
	key, e := ssh.NewPublicKey(pub)
	if e != nil {
		return nil, nil, e
	}
	return pem.EncodeToMemory(block), ssh.MarshalAuthorizedKey(key), nil
}
func (p *Provider) asset(name string) ([]byte, error) { return fs.ReadFile(p.Assets, "scripts/"+name) }
func (p *Provider) engineStartup(profile config.EngineProfile, host, client []byte) (string, error) {
	script, e := p.asset("docker-engine.sh")
	if e != nil {
		return "", e
	}
	s := "#!/bin/bash\nset -eu\nset +x\numask 077\nsudo -n install -d -m 700 /var/lib/pulse-docker\n"
	s += fileScript("/var/lib/pulse-docker/prepare.sh", script, true)
	if len(host) > 0 {
		s += fileScript("/var/lib/pulse-docker/host-key", host, true) + fileScript("/var/lib/pulse-docker/authorized_keys", client, true)
	}
	// systemd owns preparation across management-process disconnection. No lease.
	s += fmt.Sprintf("if ! sudo -n systemctl cat pulse-docker-prepare.service >/dev/null 2>&1; then sudo -n systemd-run --unit=pulse-docker-prepare --service-type=oneshot --property=RemainAfterExit=yes /bin/bash /var/lib/pulse-docker/prepare.sh %s %d; fi\n", shell(profile.EngineVersion), profile.DiskGiB)
	s += "for ((i=0;i<900;i++)); do\n if sudo -n systemctl is-failed --quiet pulse-docker-prepare; then exit 1; fi\n if sudo -n journalctl -u pulse-docker-prepare --no-pager -o cat | grep -q '^PULSE_DOCKER_ENGINE_READY '; then sudo -n journalctl -u pulse-docker-prepare --no-pager -o cat | grep '^PULSE_DOCKER_ENGINE_READY '; exit 0; fi\n sleep 1\ndone\nexit 1\n"
	// Replace Orchard's login shell: Ubuntu's .bash_logout can fail without a TTY.
	return "exec /bin/bash <<'PULSE_DOCKER_STARTUP'\n" + s + "PULSE_DOCKER_STARTUP\n", nil
}
func (p *Provider) newPair(g config.SlotGroup, slot string, template *SlotRecord) (*PairRecord, error) {
	b := p.base(g.Base)
	ep := p.Config.Engine(g.DockerProfile)
	pair := &PairRecord{ID: newID(), SlotID: slot, ConfigHash: p.dockerHash(g), Base: g.Base, Profile: g.DockerProfile, Phase: "reserved", Created: time.Now().UTC()}
	pair.Primary = p.sourceSpec(b, g.CPU, g.MemoryMiB)
	pair.Primary.Name = "pulse-" + p.Config.Scope()[:12] + "-" + pair.ID + "-primary"
	pair.Primary.StartupScript.Content = "#!/bin/bash\ntrue\n"
	if b.OS == "linux" {
		image := p.Store.data.Images[p.baseImageName(b.Name)]
		if image == nil || image.Phase != "ready" {
			return nil, nil
		}
		pair.Primary.Image = image.LocalName
		script, e := p.engineStartup(ep, nil, nil)
		if e != nil {
			return nil, e
		}
		pair.Primary.StartupScript.Content = script
	} else {
		if template == nil || template.Phase != "ready" {
			return nil, nil
		}
		pair.Primary.Image = template.LocalName
		used := map[int]bool{}
		for _, v := range p.Store.data.Pairs {
			if v.Phase != "gone" {
				used[v.Port] = true
			}
		}
		for port := p.Config.DockerEndpoint.PortMin; port <= p.Config.DockerEndpoint.PortMax; port++ {
			if !used[port] {
				pair.Port = port
				break
			}
		}
		if pair.Port == 0 {
			return nil, nil
		}
		client, clientPub, e := keyPair()
		if e != nil {
			return nil, e
		}
		host, hostPub, e := keyPair()
		if e != nil {
			return nil, e
		}
		pair.Engine = p.sourceSpec(config.Base{OS: "linux", Image: ep.Image, SSHUsername: ep.SSHUsername, SSHPassword: ep.SSHPassword}, ep.CPU, ep.MemoryMiB)
		pair.Engine.Name = "pulse-" + p.Config.Scope()[:12] + "-" + pair.ID + "-engine"
		pair.Engine.StartupScript.Content, e = p.engineStartup(ep, host, clientPub)
		if e != nil {
			return nil, e
		}
		conf, _ := json.Marshal(map[string]string{"Address": net.JoinHostPort(p.Config.DockerEndpoint.Address, strconv.Itoa(pair.Port)), "User": "pulseengine", "KeyFile": "/var/lib/pulse-docker/client-key", "HostKeyFile": "/var/lib/pulse-docker/host-key.pub", "Socket": "/var/lib/pulse-docker/docker.sock"})
		pair.Primary.StartupScript.Content = "#!/bin/bash\nset -eu\nset +x\numask 077\nsudo -n install -d -m 700 /var/lib/pulse-docker\n" + fileScript("/var/lib/pulse-docker/client-key", client, false) + fileScript("/var/lib/pulse-docker/host-key.pub", hostPub, false) + fileScript("/var/lib/pulse-docker/relay.json", conf, false)
	}
	pair.PrimaryHash = p.Store.hash(normalized(pair.Primary))
	pair.EngineHash = p.Store.hash(normalized(pair.Engine))
	return pair, nil
}
func (p *Provider) pairMatches(pair *PairRecord, v backend.VM, primary bool) bool {
	spec, uid, hash := pair.Engine, pair.EngineUID, pair.EngineHash
	if primary {
		spec, uid, hash = pair.Primary, pair.PrimaryUID, pair.PrimaryHash
	}
	return v.Name == spec.Name && v.UID != "" && (uid == "" || v.UID == uid) && p.Store.hash(normalized(v.Spec)) == hash && (v.Worker == "" || v.Worker == p.Config.Worker)
}
func (p *Provider) pairVM(ctx context.Context, pair *PairRecord, primary bool) (backend.VM, bool, error) {
	name := pair.Engine.Name
	if primary {
		name = pair.Primary.Name
	}
	if name == "" {
		return backend.VM{}, false, nil
	}
	v, e := p.Backend.Get(ctx, name)
	if backend.IsStatus(e, 404) {
		return v, false, nil
	}
	if e != nil {
		return v, false, ErrBackend
	}
	if !p.pairMatches(pair, v, primary) {
		pair.Phase = "quarantine"
		_ = p.Store.save()
		return v, false, fmt.Errorf("Docker allocation identity mismatch")
	}
	if primary {
		pair.PrimaryUID = v.UID
	} else {
		pair.EngineUID = v.UID
	}
	return v, true, nil
}
func (p *Provider) pairEngineReady(ctx context.Context, pair *PairRecord) (bool, error) {
	name := pair.Engine.Name
	if name == "" {
		name = pair.Primary.Name
	}
	ev, e := p.Backend.(templateBackend).Events(ctx, name)
	if e != nil {
		return false, ErrBackend
	}
	for _, v := range ev {
		for _, line := range strings.Split(v.Payload+"\n"+v.Message, "\n") {
			if strings.HasPrefix(line, "PULSE_DOCKER_ENGINE_READY ") {
				id := strings.TrimSpace(strings.TrimPrefix(line, "PULSE_DOCKER_ENGINE_READY "))
				if id != "" {
					pair.EngineID = id
					return true, nil
				}
			}
		}
	}
	return false, nil
}
func (p *Provider) configurePrimary(ctx context.Context, pair *PairRecord) error {
	b := p.base(pair.Base)
	linux := b.OS == "linux"
	script, e := p.asset("docker-primary.sh")
	if e != nil {
		return e
	}
	content := "set -eu\nset +x\n" + fileScript("/var/lib/pulse-docker/primary.sh", script, linux) + "sudo -n /bin/bash /var/lib/pulse-docker/primary.sh\n"
	_, e = p.Backend.(dockerBackend).Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte(content))
	return e
}
func (p *Provider) primaryReady(ctx context.Context, pair *PairRecord) bool {
	b := p.base(pair.Base)
	g := p.dockerGroup(pair.SlotID)
	// Both the OS/resource facts and the exact engine identity must be observed.
	code := fmt.Sprintf(`import json,os,platform,subprocess
r=lambda *a:subprocess.check_output(a,text=True).strip()
assert platform.machine() in ('arm64','aarch64')
assert r('/opt/pulse-tart/bin/c2j','version')
assert r('sudo','-n','-u','pulseworker','/opt/pulse-tart/bin/docker','--host','unix:///var/run/docker.sock','info','--format','{{.ID}}') == %s
`, strconv.Quote(pair.EngineID))
	if b.OS == "linux" {
		code += fmt.Sprintf("assert platform.system() == 'Linux'\nassert os.cpu_count() >= %d\nassert int(next(l.split()[1] for l in open('/proc/meminfo') if l.startswith('MemTotal:')))*1024 >= %d\nassert 'VERSION_ID=\"%s\"' in open('/etc/os-release').read()\n", g.CPU, (g.MemoryMiB-512)*(1<<20), b.OSVersion)
	} else {
		code += fmt.Sprintf("assert r('/usr/bin/sw_vers','-productVersion') == '%s'\nassert int(r('/usr/sbin/sysctl','-n','hw.ncpu')) == %d\nassert int(r('/usr/sbin/sysctl','-n','hw.memsize')) == %d\nsaved=json.load(open('/var/db/pulse-tart-template/ready'))\nassert r('/usr/sbin/sysctl','-n','kern.bootsessionuuid') == saved['boot_id']\nassert saved['mac'] in r('/sbin/ifconfig','en0')\n", b.OSVersion, g.CPU, g.MemoryMiB*(1<<20))
	}
	for _, cap := range b.Capabilities {
		tool := cap
		if tool == "python" {
			tool = "python3"
		}
		path := "/opt/pulse-tart/bin/" + tool
		if tool == "nix" {
			path = "/nix/var/nix/profiles/default/bin/nix"
			if b.OS == "linux" {
				path = "/usr/bin/nix"
			}
		}
		code += "assert r(" + strconv.Quote(path) + ",'--version')\n"
	}
	code += "print('PULSE_DOCKER_PRIMARY_READY')\n"
	out, e := p.Backend.(dockerBackend).Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte("set -eu\ncd /tmp\nsudo -n /opt/pulse-tart/bin/python3 - <<'PULSE_CHECK'\n"+code+"PULSE_CHECK\n"))
	return e == nil && strings.Contains(string(out), "PULSE_DOCKER_PRIMARY_READY")
}
func (p *Provider) reclaimPair(ctx context.Context, pair *PairRecord) error {
	b := p.Backend.(dockerBackend)
	uncertainMissing := false
	// No new allocation can reuse this slot or port until BOTH deletions confirm.
	for _, primary := range []bool{true, false} {
		spec, uid := pair.Engine, pair.EngineUID
		if primary {
			spec, uid = pair.Primary, pair.PrimaryUID
		}
		if spec.Name == "" {
			continue
		}
		v, exists, e := p.pairVM(ctx, pair, primary)
		if e != nil {
			return e
		}
		if !exists {
			if uid == "" { // A dispatched create with no response remains uncertain.
				dispatched := pair.PrimaryDispatched
				if !primary {
					dispatched = pair.EngineDispatched
				}
				if dispatched {
					uncertainMissing = true
				}
			}
			continue
		}
		if !v.Stopped() {
			return b.Stop(ctx, v.Name, v.UID)
		}
		if e = p.Backend.Delete(ctx, v.Name); e != nil && !backend.IsStatus(e, 404) {
			return ErrBackend
		}
		if _, exists, e = p.pairVM(ctx, pair, primary); e != nil {
			return e
		} else if exists {
			return nil
		}
	}
	if pair.EngineUID != "" {
		if b, ok := p.Backend.(interface {
			RemoveEndpoint(context.Context, string, string) error
		}); ok {
			if e := b.RemoveEndpoint(ctx, pair.Engine.Name, pair.EngineUID); e != nil {
				return e
			}
		}
	}
	if uncertainMissing {
		return p.Store.save()
	}
	pair.Phase = "gone"
	if r := p.Store.data.Records[pair.LaunchID]; r != nil {
		r.Phase = "done"
	}
	// Release stored transport credentials only after all native resources are gone.
	pair.Primary.StartupScript.Content = ""
	pair.Engine.StartupScript.Content = ""
	return p.Store.save()
}
func (p *Provider) reconcilePairs(ctx context.Context) error {
	if p.Config.Docker == nil && len(p.Store.data.Pairs) == 0 {
		return nil
	}
	b, ok := p.Backend.(dockerBackend)
	if !ok {
		return fmt.Errorf("backend does not support Docker allocations")
	}
	ids := []string{}
	for id := range p.Store.data.Pairs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Lower limits retire idle workers before accepting more work. Active jobs
	// keep their reservations until cleanup, even if the new limit is smaller.
	if !p.fitsJobCounts(p.jobReservations()) {
		for _, id := range ids {
			if p.Store.data.Pairs[id].Phase == "reclaiming" {
				if e := p.reclaimPair(ctx, p.Store.data.Pairs[id]); e != nil {
					return e
				}
				return nil
			}
		}
		for _, id := range ids {
			pair := p.Store.data.Pairs[id]
			if pair.LaunchID == "" && pair.Phase != "gone" && pair.Phase != "quarantine" {
				pair.Phase = "reclaiming"
				return p.Store.save()
			}
		}
	}
	for _, id := range ids {
		pair := p.Store.data.Pairs[id]
		if pair.Phase == "gone" || pair.Phase == "quarantine" {
			continue
		}
		if pair.Phase == "reclaiming" {
			if e := p.reclaimPair(ctx, pair); e != nil {
				return e
			}
			continue
		}
		g := p.dockerGroup(pair.SlotID)
		// Configuration changes drain unused warm capacity; never kill an active job.
		if pair.LaunchID == "" && (g.Count == 0 || pair.ConfigHash != p.dockerHash(g) || time.Since(pair.Created) > p.dockerReadyTimeout() && pair.Phase != "ready") {
			pair.Phase = "reclaiming"
			if e := p.Store.save(); e != nil {
				return e
			}
			continue
		}
		if pair.Phase == "reserved" {
			available, e := p.canStartVMs(ctx)
			if e != nil {
				return e
			}
			if !available {
				continue
			}
			primary := pair.Engine.Name == ""
			spec := pair.Engine
			if primary {
				spec = pair.Primary
				pair.PrimaryDispatched = true
				pair.Phase = "primary-dispatch"
			} else {
				pair.EngineDispatched = true
				pair.Phase = "engine-dispatch"
			}
			if e := p.Store.save(); e != nil {
				return e
			}
			v, e := p.Backend.Create(ctx, spec)
			if e == nil && p.pairMatches(pair, v, primary) {
				if primary {
					pair.PrimaryUID = v.UID
				} else {
					pair.EngineUID = v.UID
				}
			}
			if e = p.Store.save(); e != nil {
				return e
			}
			continue
		}
		primary, hasPrimary, e := p.pairVM(ctx, pair, true)
		if e != nil {
			return e
		}
		if pair.PrimaryDispatched && ((!hasPrimary && pair.PrimaryUID != "") || (hasPrimary && primary.Stopped() && primary.PowerState != "suspended" && (primary.Status == "failed" || pair.Phase == "ready" || pair.Phase == "running" || pair.Phase == "handoff-intent"))) {
			pair.Phase = "reclaiming"
			if e = p.Store.save(); e != nil {
				return e
			}
			continue
		}
		if pair.Engine.Name != "" {
			engine, exists, e := p.pairVM(ctx, pair, false)
			if e != nil {
				return e
			}
			if !exists || (engine.Stopped() && engine.Status == "failed") {
				if pair.LaunchID == "" && pair.EngineUID != "" {
					pair.Phase = "reclaiming"
					if e = p.Store.save(); e != nil {
						return e
					}
				}
				continue
			}
			if pair.Phase == "engine-dispatch" {
				ready, e := p.pairEngineReady(ctx, pair)
				if e != nil {
					return e
				}
				if !ready {
					continue
				}
				ready, e = b.Endpoint(ctx, pair.Engine.Name, pair.EngineUID, pair.Port)
				if e != nil {
					return e
				}
				if !ready {
					continue
				}
				available, e := p.canStartVMs(ctx)
				if e != nil {
					return e
				}
				if !available {
					continue
				}
				pair.Phase = "primary-dispatch"
				pair.PrimaryDispatched = true
				if e = p.Store.save(); e != nil {
					return e
				}
				v, e := p.Backend.Create(ctx, pair.Primary)
				if e == nil && p.pairMatches(pair, v, true) {
					pair.PrimaryUID = v.UID
				}
				if e = p.Store.save(); e != nil {
					return e
				}
				continue
			}
			// Ensure the same endpoint survives worker/helper restart, including after handoff.
			if _, e = b.Endpoint(ctx, pair.Engine.Name, pair.EngineUID, pair.Port); e != nil {
				return e
			}
		}
		if !hasPrimary {
			continue
		}
		switch pair.Phase {
		case "primary-dispatch", "configuring":
			if pair.Engine.Name == "" {
				ready, e := p.pairEngineReady(ctx, pair)
				if e != nil {
					return e
				}
				if !ready {
					continue
				}
			}
			pair.Phase = "configuring"
			if e = p.Store.save(); e != nil {
				return e
			}
			if e = p.configurePrimary(ctx, pair); e != nil {
				continue
			}
			if !p.primaryReady(ctx, pair) {
				continue
			}
			pair.Phase = "ready"
		case "handoff-intent":
			// Observe the exact durable receipt. Never resubmit the envelope after ambiguity.
			script := "set -eu\ntest \"$(sudo -n cat /var/db/pulse-tart/fingerprint)\" = " + shell(pair.DeliveryHash) + "\nsudo -n /bin/bash /opt/pulse-tart-image/bootstrap-guest.sh\necho PULSE_HANDOFF_OK\n"
			out, e := b.Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte(script))
			if e == nil && strings.Contains(string(out), "PULSE_HANDOFF_OK") {
				pair.Phase = "running"
				r := p.Store.data.Records[pair.LaunchID]
				r.Outcome = api.Accepted
				r.Phase = "active"
			}
		}
		if e = p.Store.save(); e != nil {
			return e
		}
	}
	if p.Config.Docker == nil {
		return nil
	}
	// Finish a partial pair before dispatching another; avoid filling a small
	// worker with companions while all primaries wait for capacity.
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "gone" && pair.Phase != "ready" && pair.Phase != "running" && pair.Phase != "handoff-intent" {
			return nil
		}
	}
	for _, g := range p.Config.Slots {
		if p.preferredGroup != "" && time.Now().Before(p.preferUntil) && g.Name != p.preferredGroup {
			continue
		}
		if g.DockerProfile == "" {
			continue
		}
		for i := 1; i <= g.Count; i++ {
			slot := fmt.Sprintf("%s-%04d", g.Name, i)
			if p.slotBusy(slot) {
				continue
			}
			if p.base(g.Base).OS != "linux" {
				s := p.Store.data.Slots[slot]
				if s == nil || s.Phase != "ready" {
					continue
				}
				v, e := p.Backend.Get(ctx, s.Name)
				if e != nil {
					return ErrBackend
				}
				if !p.matchesSlot(s, v) || !suspended(v) {
					return ErrBackend
				}
			}
			pair, e := p.newPair(g, slot, p.Store.data.Slots[slot])
			if e != nil {
				return e
			}
			if pair == nil {
				continue
			}
			available, e := p.canReserveJobs(ctx, pair.Primary)
			if e != nil {
				return e
			}
			if !available {
				continue
			}
			p.Store.data.Pairs[pair.ID] = pair
			return p.Store.save()
		}
	}
	return nil
}
func (p *Provider) submitDocker(ctx context.Context, l api.Launch, hash string) (api.Submission, bool) {
	wants := slices.Contains(l.Capabilities, "docker")
	compatible := false
	ordinary := false
	for _, g := range p.Config.Slots {
		if p.dockerMatch(g, l) {
			compatible = true
		}
		if g.DockerProfile == "" && baseMatches(p.base(g.Base), l) && g.CPU*1000 >= l.CPUMillis && g.MemoryMiB*(1<<20) >= l.MemoryBytes {
			ordinary = true
		}
	}
	if !wants && (!compatible || ordinary) {
		return api.Submission{}, false
	}
	if !compatible || l.TimeoutSeconds != 0 || l.StartBefore != nil || l.ScratchBytes != 0 {
		return result(l.LaunchID, api.Unsupported), true
	}
	if !p.fitsJobCounts(p.jobReservations()) {
		return result(l.LaunchID, api.NoCapacity), true
	}
	// Preserve configured base preference; stable smallest fitting slot within it.
	candidates := []*PairRecord{}
	for _, pair := range p.Store.data.Pairs {
		g := p.dockerGroup(pair.SlotID)
		if pair.Phase == "ready" && pair.LaunchID == "" && g.Count > 0 && pair.ConfigHash == p.dockerHash(g) && p.dockerMatch(g, l) {
			candidates = append(candidates, pair)
		}
	}
	pref := map[string]int{}
	for i, b := range p.Config.Bases {
		pref[b.Name] = i
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if pref[a.Base] != pref[b.Base] {
			return pref[a.Base] < pref[b.Base]
		}
		if a.Primary.CPU != b.Primary.CPU {
			return a.Primary.CPU < b.Primary.CPU
		}
		if a.Primary.Memory != b.Primary.Memory {
			return a.Primary.Memory < b.Primary.Memory
		}
		return a.SlotID < b.SlotID
	})
	for _, pair := range candidates {
		v, exists, e := p.pairVM(ctx, pair, true)
		if e != nil {
			return result(l.LaunchID, api.Unavailable), true
		}
		if !exists || v.Stopped() {
			continue
		}
		if pair.Engine.Name != "" {
			v, exists, e = p.pairVM(ctx, pair, false)
			if e != nil {
				return result(l.LaunchID, api.Unavailable), true
			}
			if !exists || v.Stopped() {
				continue
			}
		}
		if !p.primaryReady(ctx, pair) {
			return result(l.LaunchID, api.Unavailable), true
		}
		b := p.base(pair.Base)
		if wants {
			b.Capabilities = append(slices.Clone(b.Capabilities), "docker")
		}
		proc := workerProcess(l, b, pair.Primary.CPU, pair.Primary.Memory-512)
		proc.Env["DOCKER_HOST"] = "unix:///var/run/docker.sock"
		proc.Env["TESTCONTAINERS_HOST_OVERRIDE"] = "127.0.0.1"
		proc.Env["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"] = "/var/run/docker.sock"
		proc.Env["TMPDIR"] = proc.WorkingDir + "/work/tmp"
		payload, _ := json.Marshal(envelope{1, l.LaunchID, proc})
		sum := sha256.Sum256(payload)
		pair.DeliveryHash = hex.EncodeToString(sum[:])
		pair.LaunchID = l.LaunchID
		pair.Phase = "handoff-intent"
		r := &Record{LaunchID: l.LaunchID, Fingerprint: hash, Name: pair.Primary.Name, UID: pair.PrimaryUID, NativeHash: pair.PrimaryHash, Job: l.Job, Created: time.Now().UTC(), Outcome: api.Unknown, Phase: "dispatch", SlotID: pair.SlotID, PairID: pair.ID}
		p.Store.data.Records[l.LaunchID] = r
		p.preferredGroup = ""
		if p.Store.save() != nil {
			return result(l.LaunchID, api.Unknown), true
		}
		decode := "-D"
		if b.OS == "linux" {
			decode = "-d"
		}
		script := fmt.Sprintf("set -eu\nset +x\numask 077\nprintf '%%s' %s | base64 %s | sudo -n /opt/pulse-tart/bin/pulse-tart-guest ingest\nsudo -n /bin/bash /opt/pulse-tart-image/bootstrap-guest.sh\necho PULSE_HANDOFF_OK\n", shell(base64.StdEncoding.EncodeToString(payload)), decode)
		out, e := p.Backend.(dockerBackend).Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte(script))
		if e == nil && strings.Contains(string(out), "PULSE_HANDOFF_OK") {
			r.Outcome = api.Accepted
			r.Phase = "active"
			pair.Phase = "running"
		}
		if p.Store.save() != nil {
			return result(l.LaunchID, api.Unknown), true
		}
		return result(l.LaunchID, r.Outcome), true
	}
	if err := p.requestJobRoom(ctx, l); err != nil {
		return result(l.LaunchID, api.Unavailable), true
	}
	return result(l.LaunchID, api.NoCapacity), true
}

func (p *Provider) dockerReadyTimeout() time.Duration {
	if p.Config.Docker != nil {
		return p.Config.Docker.ReadyTimeout
	}
	return p.Config.BootstrapTimeout
}

func (p *Provider) pairInstances(vs []backend.VM, q api.ListRequest, after string) ([]api.Instance, error) {
	byName := map[string]backend.VM{}
	for _, v := range vs {
		byName[v.Name] = v
	}
	out := []api.Instance{}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase == "gone" || pair.LaunchID == "" {
			continue
		}
		r := p.Store.data.Records[pair.LaunchID]
		if r == nil {
			return nil, ErrState
		}
		if q.LaunchID != "" && q.LaunchID != r.LaunchID {
			continue
		}
		for _, primary := range []bool{true, false} {
			name := pair.Engine.Name
			role := "engine"
			if primary {
				name = pair.Primary.Name
				role = "primary"
			}
			if name == "" || name <= after {
				continue
			}
			v, ok := byName[name]
			if !ok {
				continue
			}
			if !p.pairMatches(pair, v, primary) {
				return nil, fmt.Errorf("Docker resource identity mismatch")
			}
			state := "starting"
			if pair.Phase == "running" {
				state = "running"
			}
			if pair.Phase == "reclaiming" || v.Stopped() {
				state = "stopping"
			}
			created := r.Created
			out = append(out, api.Instance{ID: name, LaunchID: r.LaunchID, Job: r.Job, State: state, CreatedAt: &created, Refs: []string{p.Config.Kind + ":" + name, "role:" + role}})
		}
	}
	return out, nil
}

// DockerStatus excludes credentials, guest payloads and journal fingerprints.
type DockerStatus struct{ ID, Slot, Phase, Primary, PrimaryUID, Engine, EngineUID, LaunchID string }

func (p *Provider) DockerAllocations() ([]DockerStatus, error) {
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	if p.Store.broken {
		return nil, ErrState
	}
	out := []DockerStatus{}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "gone" {
			out = append(out, DockerStatus{pair.ID, pair.SlotID, pair.Phase, pair.Primary.Name, pair.PrimaryUID, pair.Engine.Name, pair.EngineUID, pair.LaunchID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, nil
}
