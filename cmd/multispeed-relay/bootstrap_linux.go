//go:build linux

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/multispeed-app/multispeed/deploy"
	"github.com/multispeed-app/multispeed/internal/config"
	"github.com/multispeed-app/multispeed/scripts"
)

const (
	installedBinary = "/usr/local/bin/multispeed-relay"
	installedHelper = "/usr/local/lib/multispeed/relay-setup.sh"
	installedUnit   = "/etc/systemd/system/multispeed-relay.service"
)

// bootstrap turns a fresh Linux host into a running relay in one command. It
// is deliberately explicit about every change it makes to the host.
func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	server := fs.String("server", "", "public relay host:port (default: detect the public IPv4)")
	port := fs.Int("port", 7443, "relay UDP port")
	tunName := fs.String("tun", "ms0", "TUN interface name")
	dir := fs.String("dir", "/etc/multispeed", "directory for relay.json, client.json, and join.txt")
	name := fs.String("name", "client-1", "name of the first allowlisted client")
	address := fs.String("address", "10.77.0.2", "tunnel IPv4 address of the first client")
	prefix := fs.Uint("prefix", uint(config.DefaultPrefixLen), "tunnel subnet prefix length")
	mtu := fs.Uint("mtu", uint(config.DefaultTunnelMTU), "tunnel MTU")
	noService := fs.Bool("no-service", false, "install and generate configuration but do not start the service")
	noFirewall := fs.Bool("skip-firewall", false, "do not add host firewall rules")
	force := fs.Bool("force", false, "replace an existing configuration (rotates every key)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("bootstrap takes no positional arguments")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("bootstrap must run as root (try: sudo multispeed-relay bootstrap)")
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("port %d is out of range", *port)
	}
	if *prefix > 32 {
		return fmt.Errorf("prefix length must be at most 32")
	}
	if *mtu < 576 || *mtu > 9000 {
		return fmt.Errorf("MTU must be between 576 and 9000")
	}
	if *tunName == "" || len(*tunName) > 15 {
		return fmt.Errorf("TUN interface name must be 1-15 characters")
	}

	step := func(format string, a ...any) { fmt.Printf("==> "+format+"\n", a...) }

	step("Installing %s", installedBinary)
	if err := installSelf(installedBinary); err != nil {
		return err
	}
	step("Installing the network setup helper")
	if err := writeFile(installedHelper, scripts.RelaySetup, 0o755); err != nil {
		return err
	}
	if err := requireTools(); err != nil {
		return err
	}

	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", *dir, err)
	}
	relayPath := filepath.Join(*dir, "relay.json")
	clientPath := filepath.Join(*dir, "client.json")

	var joinCode string
	if _, err := os.Stat(relayPath); err == nil && !*force {
		step("Reusing the existing configuration in %s", *dir)
		var file config.Client
		if err := config.Load(clientPath, &file); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot build a join code: %v\n", err)
			fmt.Fprintln(os.Stderr, "         run with -force to generate a new configuration")
		} else {
			code, err := config.NewJoinCode(file.Server, file.RelayPublicKey, file.PrivateKey, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: cannot build a join code: %v\n", err)
			} else if joinCode, err = config.EncodeJoinCode(code); err != nil {
				fmt.Fprintf(os.Stderr, "warning: cannot build a join code: %v\n", err)
			}
		}
	} else {
		publicAddr := *server
		if strings.TrimSpace(publicAddr) == "" {
			step("Detecting the public IPv4 address")
			ip, err := publicIPv4()
			if err != nil {
				return fmt.Errorf("%w; rerun with -server YOUR_PUBLIC_IP:%d", err, *port)
			}
			publicAddr = net.JoinHostPort(ip, strconv.Itoa(*port))
			step("Public address: %s", publicAddr)
		}
		step("Generating keys and configuration")
		init, err := generateConfig(initOptions{
			Dir:     *dir,
			Server:  publicAddr,
			Listen:  "0.0.0.0:" + strconv.Itoa(*port),
			Name:    *name,
			Address: *address,
			Prefix:  uint8(*prefix),
			MTU:     uint16(*mtu),
		})
		if err != nil {
			return err
		}
		joinCode = init.JoinCode
	}

	if !*noFirewall {
		openHostFirewall(*port)
	}

	step("Configuring the TUN interface and NAT rules")
	setup := exec.Command(installedHelper, "--tun", *tunName, "--port", strconv.Itoa(*port), "--mtu", strconv.Itoa(int(*mtu)))
	setup.Stdout = os.Stdout
	setup.Stderr = os.Stderr
	if err := setup.Run(); err != nil {
		return fmt.Errorf("network setup failed: %w", err)
	}

	if !*noService {
		step("Installing and starting the systemd service")
		if err := writeFile(installedUnit, deploy.RelayService(*tunName, *port), 0o644); err != nil {
			return err
		}
		for _, args := range [][]string{
			{"daemon-reload"},
			{"enable", "multispeed-relay"},
			{"restart", "multispeed-relay"},
		} {
			if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
		}
		if !serviceActive() {
			exec.Command("journalctl", "-u", "multispeed-relay", "-n", "20", "--no-pager").Run()
			return fmt.Errorf("the relay service did not stay running; see the log above")
		}
		step("Relay service is running")
	}

	if joinCode != "" {
		fmt.Printf(`

MultiSpeed relay is ready.

Join code (treat it like a password - it contains the client private key):

  %s

Open MultiSpeed.exe on your Windows machine and paste the code above.
`, joinCode)
	} else {
		fmt.Fprintf(os.Stderr, "\nNo join code is available; run with -force to generate a new configuration.\n")
	}
	if !*noFirewall {
		fmt.Printf("If this host is in a cloud, also allow UDP %d in the provider's firewall\n(security list or network security group). For Oracle Cloud, see\noracle-free-relay.md.\n", *port)
	}
	return nil
}

// installSelf copies the running executable to path so the service does not
// depend on the directory it was uploaded to.
func installSelf(path string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if filepath.Clean(exe) == filepath.Clean(path) {
		return nil
	}
	return copyFile(exe, path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFrom(dst, in, 0o755)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	return writeFrom(path, bytes.NewReader(data), mode)
}

func writeFrom(path string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func requireTools() error {
	var missing []string
	for _, tool := range []string{"ip", "nft", "systemctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing tools: %s (install with: apt install -y iproute2 nftables)",
			strings.Join(missing, ", "))
	}
	return nil
}

// publicIPv4 asks a public address service for this host's IPv4 address.
func publicIPv4() (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	var last error
	for _, url := range []string{"https://api.ipify.org", "https://ifconfig.me/ip"} {
		resp, err := client.Get(url)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(string(body)))
		if err != nil || !addr.Is4() {
			last = fmt.Errorf("%s returned %q, not an IPv4 address", url, strings.TrimSpace(string(body)))
			continue
		}
		return addr.String(), nil
	}
	return "", fmt.Errorf("could not detect the public IPv4 address: %w", last)
}

// openHostFirewall opens the relay port on the host. Cloud-level firewalls are
// separate and can only be changed by the user.
func openHostFirewall(port int) {
	if hasTool("ufw") {
		if out, err := exec.Command("ufw", "status").CombinedOutput(); err == nil &&
			strings.Contains(string(out), "Status: active") {
			if out, err := exec.Command("ufw", "allow", fmt.Sprintf("%d/udp", port)).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: ufw allow failed: %v: %s\n", err, strings.TrimSpace(string(out)))
			} else {
				fmt.Printf("==> Opened UDP %d in ufw\n", port)
			}
		}
	}
	if !hasTool("iptables") {
		return
	}
	check := exec.Command("iptables", "-C", "INPUT", "-p", "udp", "--dport", strconv.Itoa(port), "-j", "ACCEPT")
	if check.Run() == nil {
		return
	}
	if out, err := exec.Command("iptables", "-I", "INPUT", "1", "-p", "udp", "--dport", strconv.Itoa(port), "-j", "ACCEPT").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open UDP %d in iptables: %v: %s\n", port, err, strings.TrimSpace(string(out)))
		return
	}
	fmt.Printf("==> Opened UDP %d in iptables\n", port)
	if hasTool("netfilter-persistent") {
		if err := exec.Command("netfilter-persistent", "save").Run(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not persist the firewall rule: %v\n", err)
		}
	} else {
		fmt.Fprintln(os.Stderr, "note: install iptables-persistent to keep this rule across reboots")
	}
}

func hasTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func serviceActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "multispeed-relay").Run() == nil
}
