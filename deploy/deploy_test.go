package deploy

import (
	"strings"
	"testing"
)

func TestRelayServiceKeepsDefaults(t *testing.T) {
	unit := string(RelayService("ms0", 7443))
	if !strings.Contains(unit, "-tun ms0") {
		t.Fatalf("unit does not contain the default TUN interface:\n%s", unit)
	}
	if !strings.Contains(unit, "--port 7443") {
		t.Fatalf("unit does not contain the default port:\n%s", unit)
	}
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/multispeed-relay run") {
		t.Fatalf("unit has an unexpected ExecStart:\n%s", unit)
	}
}

func TestRelayServiceCustomTunAndPort(t *testing.T) {
	unit := string(RelayService("bond0", 9443))
	if strings.Contains(unit, "ms0") {
		t.Fatalf("unit still mentions ms0:\n%s", unit)
	}
	if strings.Contains(unit, "7443") {
		t.Fatalf("unit still mentions 7443:\n%s", unit)
	}
	if !strings.Contains(unit, "--tun bond0 --port 9443") {
		t.Fatalf("ExecStartPre was not rewritten:\n%s", unit)
	}
	if !strings.Contains(unit, "-tun bond0") {
		t.Fatalf("ExecStart was not rewritten:\n%s", unit)
	}
}
