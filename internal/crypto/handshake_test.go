package crypto

import (
	"net/netip"
	"testing"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

type handshakeFixture struct {
	client      KeyPair
	relay       KeyPair
	clientHS    *ClientHandshake
	serverHS    *ServerHandshake
	initMsg     []byte
	clientPeer  []byte
	info        protocol.SessionInfo
	responseMsg []byte
	serverKeys  TrafficKeys
	clientKeys  TrafficKeys
	clientInfo  protocol.SessionInfo
}

func newHandshakeFixture(t *testing.T) *handshakeFixture {
	t.Helper()
	f := &handshakeFixture{}
	var err error
	f.client, err = GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	f.relay, err = GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	f.clientHS, err = NewClientHandshake(f.client.Private, f.relay.Public)
	if err != nil {
		t.Fatal(err)
	}
	f.initMsg, err = f.clientHS.Init()
	if err != nil {
		t.Fatal(err)
	}
	f.serverHS, err = NewServerHandshake(f.relay.Private)
	if err != nil {
		t.Fatal(err)
	}
	f.clientPeer, err = f.serverHS.ReadInit(f.initMsg)
	if err != nil {
		t.Fatal(err)
	}
	f.info = protocol.SessionInfo{
		SessionID: 0x1020304050607080,
		Address:   netip.MustParseAddr("10.77.0.2"),
		PrefixLen: 16,
		MTU:       1400,
		DNS:       netip.MustParseAddr("1.1.1.1"),
	}
	f.responseMsg, f.serverKeys, err = f.serverHS.Finish(f.info)
	if err != nil {
		t.Fatal(err)
	}
	f.clientInfo, f.clientKeys, err = f.clientHS.Finish(f.responseMsg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestHandshake(t *testing.T) {
	f := newHandshakeFixture(t)
	if !EqualKeys(f.clientPeer, f.client.Public) {
		t.Fatal("relay did not recover the client public key")
	}
	if f.clientInfo != f.info {
		t.Fatalf("client info = %+v, want %+v", f.clientInfo, f.info)
	}
	if f.clientKeys.ClientToServer != f.serverKeys.ClientToServer {
		t.Fatal("client-to-server keys differ")
	}
	if f.clientKeys.ServerToClient != f.serverKeys.ServerToClient {
		t.Fatal("server-to-client keys differ")
	}
	if f.clientKeys.ClientToServer == f.clientKeys.ServerToClient {
		t.Fatal("directional keys are identical")
	}
	if len(f.initMsg) < 64 || len(f.responseMsg) < 64 {
		t.Fatalf("unexpected handshake message sizes: %d/%d", len(f.initMsg), len(f.responseMsg))
	}
}

func TestHandshakeTamperedInit(t *testing.T) {
	f := newHandshakeFixture(t)
	bad := append([]byte(nil), f.initMsg...)
	bad[len(bad)-1] ^= 1
	server, err := NewServerHandshake(f.relay.Private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.ReadInit(bad); err == nil {
		t.Fatal("accepted tampered handshake init")
	}
}

func TestHandshakeTamperedResponse(t *testing.T) {
	client, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	relay, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientHS, err := NewClientHandshake(client.Private, relay.Public)
	if err != nil {
		t.Fatal(err)
	}
	initMsg, err := clientHS.Init()
	if err != nil {
		t.Fatal(err)
	}
	serverHS, err := NewServerHandshake(relay.Private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverHS.ReadInit(initMsg); err != nil {
		t.Fatal(err)
	}
	info := protocol.SessionInfo{
		SessionID: 1,
		Address:   netip.MustParseAddr("10.77.0.2"),
		PrefixLen: 16,
		MTU:       1400,
	}
	response, _, err := serverHS.Finish(info)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), response...)
	bad[len(bad)-1] ^= 1
	if _, _, err := clientHS.Finish(bad); err == nil {
		t.Fatal("accepted tampered handshake response")
	}
}

func TestHandshakeWrongRelayKey(t *testing.T) {
	client, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	relay, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	impostor, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientHS, err := NewClientHandshake(client.Private, impostor.Public)
	if err != nil {
		t.Fatal(err)
	}
	initMsg, err := clientHS.Init()
	if err != nil {
		t.Fatal(err)
	}
	serverHS, err := NewServerHandshake(relay.Private)
	if err != nil {
		t.Fatal(err)
	}
	// The handshake must fail at one side or the other. It must never produce
	// matching traffic keys with an untrusted relay.
	if _, err := serverHS.ReadInit(initMsg); err == nil {
		info := protocol.SessionInfo{
			SessionID: 1,
			Address:   netip.MustParseAddr("10.77.0.2"),
			PrefixLen: 16,
			MTU:       1400,
		}
		if resp, _, err := serverHS.Finish(info); err == nil {
			if _, _, err := clientHS.Finish(resp); err == nil {
				t.Fatal("completed handshake with the wrong relay key")
			}
		}
	}
}

func TestHandshakeRejectsBadSessionInfo(t *testing.T) {
	client, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	relay, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientHS, err := NewClientHandshake(client.Private, relay.Public)
	if err != nil {
		t.Fatal(err)
	}
	initMsg, err := clientHS.Init()
	if err != nil {
		t.Fatal(err)
	}
	serverHS, err := NewServerHandshake(relay.Private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverHS.ReadInit(initMsg); err != nil {
		t.Fatal(err)
	}
	// A zero session ID is invalid and must be rejected before any response is
	// encrypted.
	info := protocol.SessionInfo{
		Address:   netip.MustParseAddr("10.77.0.2"),
		PrefixLen: 16,
		MTU:       1400,
	}
	if _, _, err := serverHS.Finish(info); err == nil {
		t.Fatal("server accepted invalid session info")
	}
}

func TestHandshakeStateOrder(t *testing.T) {
	client, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	relay, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	hs, err := NewClientHandshake(client.Private, relay.Public)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hs.Finish([]byte{1, 2, 3}); err == nil {
		t.Fatal("finished before init")
	}
	if _, err := hs.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.Init(); err == nil {
		t.Fatal("sent init twice")
	}
}
