#!/usr/bin/env bash
# Configure a Linux host as a MultiSpeed relay: TUN interface, IPv4
# forwarding, source NAT, and client isolation.
#
# This script is intentionally narrow in scope. It does not install MultiSpeed,
# configure a DNS resolver, or manage the relay service. Run it as root.
set -euo pipefail

TUN="ms0"
SUBNET="10.77.0.0/16"
RELAY_IP="10.77.0.1"
PORT="7443"
MTU="1400"
WAN_IF=""
TEARDOWN=0

usage() {
  cat <<'EOF'
Usage: relay-setup.sh [options]

Options:
  --tun NAME        TUN interface name (default: ms0)
  --subnet CIDR     Tunnel subnet (default: 10.77.0.0/16)
  --relay-ip IP     Relay address inside the tunnel (default: 10.77.0.1)
  --port PORT       Relay UDP port for the firewall rule (default: 7443)
  --mtu MTU         Tunnel MTU (default: 1400)
  --wan-if NAME     Internet-facing interface (default: auto-detect)
  --teardown        Remove the TUN interface, nftables table, and sysctl file
  -h, --help        Show this help

The relay host must also allow the UDP port through its cloud firewall or
security group. This script only manages the local nftables table.
EOF
}

die() {
  echo "relay-setup: $*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tun) TUN="${2:?--tun requires a value}"; shift 2 ;;
    --subnet) SUBNET="${2:?--subnet requires a value}"; shift 2 ;;
    --relay-ip) RELAY_IP="${2:?--relay-ip requires a value}"; shift 2 ;;
    --port) PORT="${2:?--port requires a value}"; shift 2 ;;
    --mtu) MTU="${2:?--mtu requires a value}"; shift 2 ;;
    --wan-if) WAN_IF="${2:?--wan-if requires a value}"; shift 2 ;;
    --teardown) TEARDOWN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "must run as root"

if [[ ! "$SUBNET" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/([0-9]{1,2})$ ]]; then
  die "invalid subnet: $SUBNET"
fi
PREFIX="${BASH_REMATCH[1]}"
[[ "$PREFIX" -ge 1 && "$PREFIX" -le 32 ]] || die "invalid prefix length: $PREFIX"
[[ "$MTU" =~ ^[0-9]+$ ]] || die "invalid MTU: $MTU"
[[ "$PORT" =~ ^[0-9]+$ ]] || die "invalid port: $PORT"

if (( TEARDOWN )); then
  nft delete table ip multispeed 2>/dev/null || true
  ip link delete dev "$TUN" 2>/dev/null || true
  rm -f /etc/sysctl.d/99-multispeed.conf
  echo "Removed the nftables table, $TUN, and /etc/sysctl.d/99-multispeed.conf."
  echo "IPv4 forwarding remains enabled until the next reboot; change it manually if needed."
  exit 0
fi

command -v ip >/dev/null 2>&1 || die "iproute2 is required"
command -v nft >/dev/null 2>&1 || die "nftables is required (apt install nftables)"
command -v sysctl >/dev/null 2>&1 || die "sysctl is required"

if [[ -z "$WAN_IF" ]]; then
  WAN_IF="$(ip -4 route show default | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") {print $(i + 1); exit}}')"
fi
[[ -n "$WAN_IF" ]] || die "could not detect the default WAN interface; pass --wan-if"
ip link show dev "$WAN_IF" >/dev/null 2>&1 || die "interface $WAN_IF does not exist"

modprobe tun 2>/dev/null || true
[[ -c /dev/net/tun ]] || die "/dev/net/tun is missing; enable the tun kernel module"

if ip link show dev "$TUN" >/dev/null 2>&1; then
  echo "Reusing existing interface $TUN."
else
  ip tuntap add dev "$TUN" mode tun
fi
ip link set dev "$TUN" mtu "$MTU"
ip addr replace "$RELAY_IP/$PREFIX" dev "$TUN"
ip link set dev "$TUN" up

cat >/etc/sysctl.d/99-multispeed.conf <<'EOF'
# Managed by MultiSpeed. The relay needs IPv4 forwarding.
net.ipv4.ip_forward = 1
EOF
sysctl -q -p /etc/sysctl.d/99-multispeed.conf

# Replace our table atomically. The forward chain isolates tunnel clients from
# each other and from the relay's local networks, and permits only traffic that
# enters or leaves through the WAN interface.
nft delete table ip multispeed 2>/dev/null || true
nft -f - <<EOF
table ip multispeed {
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "$TUN" drop
    iifname "$WAN_IF" udp dport $PORT accept
  }

  chain forward {
    type filter hook forward priority filter; policy accept;
    iifname "$TUN" oifname "$WAN_IF" accept
    iifname "$TUN" drop
    oifname "$TUN" ct state established,related accept
    oifname "$TUN" drop
  }

  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    oifname "$WAN_IF" ip saddr $SUBNET masquerade
  }
}
EOF

cat <<EOF

Relay network setup complete:
  TUN:        $TUN ($RELAY_IP/$PREFIX, mtu $MTU)
  WAN:        $WAN_IF
  NAT:        $SUBNET -> $WAN_IF (masquerade)
  UDP port:   $PORT
  Isolation:  client-to-client and client-to-local forwarding is dropped

Make sure your provider firewall and the host firewall allow UDP $PORT.

Start the relay with:
  multispeed-relay run -config relay.json -tun $TUN

Tear down with:
  $0 --teardown
EOF
