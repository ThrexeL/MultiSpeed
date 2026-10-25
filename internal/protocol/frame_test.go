package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	tests := []Frame{
		{Header: Header{Type: FrameHandshakeInit}, Payload: []byte("noise-1")},
		{Header: Header{Type: FrameHandshakeResponse, SessionID: 0, PathID: 0, Seq: 0}, Payload: []byte("noise-2")},
		{Header: Header{Type: FrameData, SessionID: 42, PathID: 7, Seq: 1<<40 + 9}, Payload: []byte{0x45, 0, 0, 20}},
		{Header: Header{Type: FrameControl, Flags: 0, SessionID: 99, PathID: 255, Seq: 123456789}, Payload: nil},
	}
	for _, want := range tests {
		encoded, err := want.Marshal()
		if err != nil {
			t.Fatalf("Marshal(%s): %v", want.Type, err)
		}
		got, err := ParseFrame(encoded)
		if err != nil {
			t.Fatalf("ParseFrame(%s): %v", want.Type, err)
		}
		if got.Type != want.Type || got.Flags != want.Flags ||
			got.SessionID != want.SessionID || got.PathID != want.PathID ||
			got.Seq != want.Seq {
			t.Fatalf("header mismatch: got %+v want %+v", got.Header, want.Header)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("payload mismatch: got %x want %x", got.Payload, want.Payload)
		}
	}
}

func TestFrameHeaderLayout(t *testing.T) {
	f := Frame{Header: Header{Type: FrameData, Flags: 0x0102, SessionID: 0x1122334455667788, PathID: 9, Seq: 0x99aabbccddeeff00}, Payload: []byte{1, 2, 3}}
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != HeaderSize+3 {
		t.Fatalf("length = %d", len(b))
	}
	if got := string(b[0:4]); got != Magic {
		t.Fatalf("magic = %q", got)
	}
	if b[4] != Version {
		t.Fatalf("version = %d", b[4])
	}
	if b[5] != byte(FrameData) {
		t.Fatalf("type = %d", b[5])
	}
	if got := binary.BigEndian.Uint16(b[6:8]); got != 0x0102 {
		t.Fatalf("flags = %#x", got)
	}
	if got := binary.BigEndian.Uint64(b[8:16]); got != 0x1122334455667788 {
		t.Fatalf("session = %#x", got)
	}
	if b[16] != 9 {
		t.Fatalf("path = %d", b[16])
	}
	if b[17] != 0 || b[18] != 0 || b[19] != 0 {
		t.Fatalf("reserved = %x", b[17:20])
	}
	if got := binary.BigEndian.Uint64(b[20:28]); got != 0x99aabbccddeeff00 {
		t.Fatalf("seq = %#x", got)
	}
	if !bytes.Equal(b[28:], []byte{1, 2, 3}) {
		t.Fatalf("payload = %x", b[28:])
	}
}

func TestParseFrameRejectsInvalid(t *testing.T) {
	valid, err := (Frame{Header: Header{Type: FrameData, SessionID: 1, Seq: 1}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	clone := func() []byte { return append([]byte(nil), valid...) }

	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"short", valid[:HeaderSize-1]},
		{"bad magic", append([]byte{'X'}, valid[1:]...)},
		{"bad version", func() []byte { b := clone(); b[4] = 9; return b }()},
		{"unknown type", func() []byte { b := clone(); b[5] = 99; return b }()},
		{"too large", make([]byte, MaxDatagramSize+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseFrame(tt.in); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAppendTo(t *testing.T) {
	f := Frame{Header: Header{Type: FrameControl, SessionID: 5, Seq: 6}, Payload: []byte("abc")}
	prefix := []byte{0xaa, 0xbb}
	out, err := f.AppendTo(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[:2], prefix) {
		t.Fatalf("prefix = %x", out[:2])
	}
	got, err := ParseFrame(out[2:])
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != 5 || got.Seq != 6 || string(got.Payload) != "abc" {
		t.Fatalf("frame = %+v payload=%q", got.Header, got.Payload)
	}
}

func FuzzParseFrame(f *testing.F) {
	seeds := [][]byte{
		{},
		[]byte(Magic),
		append([]byte(Magic), 1, 3, 0, 0),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		// ParseFrame must never panic, regardless of input.
		fr, err := ParseFrame(b)
		if err != nil {
			return
		}
		if fr.Type == 0 || fr.Payload == nil && len(b) > HeaderSize {
			t.Fatalf("invalid parsed frame: %+v", fr)
		}
	})
}
