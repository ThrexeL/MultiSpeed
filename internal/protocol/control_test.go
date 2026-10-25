package protocol

import (
	"bytes"
	"testing"
)

func TestControlRoundTrip(t *testing.T) {
	hello, err := NewJSONControl(ControlPathHello, PathHello{
		Name: "Wi-Fi", Kind: LinkWiFi, MTU: 1500, Weight: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := hello.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseControl(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != ControlPathHello {
		t.Fatalf("type = %s", got.Type)
	}
	var decoded PathHello
	if err := got.DecodeJSON(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != (PathHello{Name: "Wi-Fi", Kind: LinkWiFi, MTU: 1500, Weight: 60}) {
		t.Fatalf("decoded = %+v", decoded)
	}

	ping, err := NewTokenControl(ControlPing, 0x1122334455667788)
	if err != nil {
		t.Fatal(err)
	}
	b, err = ping.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err = ParseControl(b)
	if err != nil {
		t.Fatal(err)
	}
	token, err := got.Token()
	if err != nil {
		t.Fatal(err)
	}
	if token != 0x1122334455667788 {
		t.Fatalf("token = %#x", token)
	}
}

func TestControlRejectsInvalid(t *testing.T) {
	if _, err := ParseControl(nil); err == nil {
		t.Fatal("accepted empty control")
	}
	unknown, err := ParseControl([]byte{99, 1, 2})
	if err != nil {
		t.Fatalf("rejected unknown control type: %v", err)
	}
	if unknown.Type != 99 || !bytes.Equal(unknown.Body, []byte{1, 2}) {
		t.Fatalf("unknown control = %+v", unknown)
	}
	if _, err := (Control{Type: ControlPing, Body: []byte{1, 2}}).Token(); err == nil {
		t.Fatal("accepted short token")
	}
	if _, err := NewTokenControl(ControlKeepalive, 1); err == nil {
		t.Fatal("created token for keepalive")
	}
	if _, err := (Control{Type: ControlKeepalive, Body: bytes.Repeat([]byte{0}, MaxControlPayload)}).Marshal(); err == nil {
		t.Fatal("accepted oversized control")
	}
}

func TestParseLinkKind(t *testing.T) {
	tests := map[string]LinkKind{
		"Wi-Fi":     LinkWiFi,
		"wifi":      LinkWiFi,
		"5G":        LinkCellular,
		"ethernet":  LinkEthernet,
		"Starlink":  LinkStarlink,
		"satellite": LinkSatellite,
		"garbage":   LinkUnknown,
	}
	for in, want := range tests {
		if got := ParseLinkKind(in); got != want {
			t.Fatalf("ParseLinkKind(%q) = %q, want %q", in, got, want)
		}
	}
}
