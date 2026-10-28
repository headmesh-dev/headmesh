package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Instance identifies which tailscaled to drive. The zero value is the system's default daemon; a named
// instance is a separate tailscaled (own state, socket, UDP port and TUN device) so a machine can join a
// Headmesh network while staying on another tailnet — the pattern Xcelsior's GPU hosts already use.
type Instance struct {
	Name   string
	Socket string
}

var instanceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,11}$`) // "hm-" + name must fit a 15-byte interface name

// NamedInstance returns the paths Headmesh uses for an isolated tailscaled.
func NamedInstance(name string) (Instance, error) {
	if !instanceName.MatchString(name) {
		return Instance{}, fmt.Errorf("instance name %q: lowercase letters, digits and dashes, at most 12 characters", name)
	}
	return Instance{Name: name, Socket: "/run/headmesh/tailscale-" + name + ".sock"}, nil
}

func (i Instance) args(extra ...string) []string {
	if i.Socket == "" {
		return extra
	}
	return append([]string{"--socket=" + i.Socket}, extra...)
}

// Port is the instance's WireGuard UDP port: stable per name, clear of the default 41641.
func (i Instance) Port() int { return 41650 + int(crc32.ChecksumIEEE([]byte(i.Name))%200) }

// Tailscale runs the tailscale CLI. Tests replace it.
type Tailscale interface {
	Run(ctx context.Context, inst Instance, args ...string) ([]byte, error)
}

// ExecTailscale shells out to the tailscale binary.
type ExecTailscale struct{ Binary string }

// Run implements Tailscale.
func (e ExecTailscale) Run(ctx context.Context, inst Instance, args ...string) ([]byte, error) {
	bin := e.Binary
	if bin == "" {
		bin = "tailscale"
	}
	cmd := exec.CommandContext(ctx, bin, inst.args(args...)...) //nolint:gosec // fixed binary, constructed args
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("tailscale %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// prefs is the part of `tailscale debug prefs` used to avoid hijacking an existing tailnet.
type prefs struct {
	ControlURL  string
	WantRunning bool
}

// ErrOtherTailnet means the target tailscaled is already on a different tailnet.
var ErrOtherTailnet = errors.New("this tailscaled is already connected to another tailnet")

// checkNotOnOtherTailnet refuses to repoint a daemon that is running against a different control server.
func checkNotOnOtherTailnet(ctx context.Context, ts Tailscale, inst Instance, loginServer string) error {
	out, err := ts.Run(ctx, inst, "debug", "prefs")
	if err != nil {
		return nil //nolint:nilerr // a daemon that cannot report prefs is not logged in anywhere
	}
	var p prefs
	if json.Unmarshal(out, &p) != nil {
		return nil //nolint:nilerr // unparseable prefs come from a daemon that is not logged in
	}
	if p.WantRunning && p.ControlURL != "" && strings.TrimRight(p.ControlURL, "/") != strings.TrimRight(loginServer, "/") {
		return fmt.Errorf("%w (%s); rerun with --instance NAME to join alongside it", ErrOtherTailnet, p.ControlURL)
	}
	return nil
}

// up runs `tailscale up` with the auth key passed through a private file, never on the command line.
func up(ctx context.Context, ts Tailscale, inst Instance, enr *EnrollResponse, acceptDNS bool) error {
	dir, err := os.MkdirTemp("", "headmesh-join-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	keyFile := filepath.Join(dir, "authkey")
	if err := os.WriteFile(keyFile, []byte(enr.AuthKey), 0o600); err != nil {
		return fmt.Errorf("write auth key: %w", err)
	}
	_, err = ts.Run(ctx, inst, "up",
		"--login-server="+enr.LoginServer,
		"--auth-key=file:"+keyFile,
		"--hostname="+enr.Hostname,
		"--accept-dns="+strconv.FormatBool(acceptDNS),
		"--accept-routes=false",
		"--timeout=90s",
	)
	return err
}

// selfNodeID returns the Headscale node id of this machine (Tailscale's Self.ID).
func selfNodeID(ctx context.Context, ts Tailscale, inst Instance) (uint64, error) {
	out, err := ts.Run(ctx, inst, "status", "--json")
	if err != nil {
		return 0, err
	}
	var st struct {
		Self struct {
			ID string `json:"ID"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return 0, fmt.Errorf("parse tailscale status: %w", err)
	}
	id, err := strconv.ParseUint(st.Self.ID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected node id %q from tailscale status", st.Self.ID)
	}
	return id, nil
}

// systemdUnit is installed once as /etc/systemd/system/headmesh-tailscaled@.service.
const systemdUnit = `[Unit]
Description=Tailscale daemon for Headmesh instance %i
Documentation=https://headmesh.dev/docs
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/headmesh/tailscaled/%i.env
ExecStart=/usr/sbin/tailscaled --state=/var/lib/headmesh/tailscale/%i/tailscaled.state --socket=/run/headmesh/tailscale-%i.sock --port=${PORT} --tun=${TUN}
ExecStopPost=/usr/sbin/tailscaled --cleanup --socket=/run/headmesh/tailscale-%i.sock --tun=${TUN}
Restart=on-failure
RuntimeDirectory=headmesh
RuntimeDirectoryPreserve=yes
StateDirectory=headmesh/tailscale/%i
StateDirectoryMode=0700

[Install]
WantedBy=multi-user.target
`

// ensureInstance installs and starts the isolated tailscaled for inst. It needs root.
func ensureInstance(ctx context.Context, inst Instance) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("creating instance %q needs root: rerun with sudo", inst.Name)
	}
	if err := os.MkdirAll("/etc/headmesh/tailscaled", 0o755); err != nil { //nolint:gosec // config dir, world-readable by design
		return fmt.Errorf("config dir: %w", err)
	}
	unit := "/etc/systemd/system/headmesh-tailscaled@.service"
	if _, err := os.Stat(unit); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(unit, []byte(systemdUnit), 0o644); err != nil { //nolint:gosec // systemd units are world-readable
			return fmt.Errorf("write unit: %w", err)
		}
	}
	env := fmt.Sprintf("PORT=%d\nTUN=hm-%s\n", inst.Port(), inst.Name)
	if err := os.WriteFile("/etc/headmesh/tailscaled/"+inst.Name+".env", []byte(env), 0o644); err != nil { //nolint:gosec // no secrets
		return fmt.Errorf("write instance env: %w", err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "headmesh-tailscaled@" + inst.Name + ".service"}} {
		if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed binary
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}
