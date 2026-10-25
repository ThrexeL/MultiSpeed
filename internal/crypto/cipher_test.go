package crypto

import (
	"bytes"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

func testCipher(t *testing.T) *TrafficCipher {
	t.Helper()
	key := bytes.Repeat([]byte{0x5a}, KeySize)
	c, err := NewTrafficCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testHeader(seq uint64) protocol.Header {
	return protocol.Header{Type: protocol.FrameData, SessionID: 7, PathID: 2, Seq: seq}
}

func TestSealOpenFrame(t *testing.T) {
	sender := testCipher(t)
	receiver := testCipher(t)

	for i := 0; i < 5; i++ {
		plaintext := []byte{0x45, byte(i), 0x00, 0x14}
		datagram, header, err := sender.SealFrame(testHeader(0), plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if header.Seq != uint64(i+1) {
			t.Fatalf("seq = %d, want %d", header.Seq, i+1)
		}
		frame, got, err := receiver.OpenFrame(datagram)
		if err != nil {
			t.Fatal(err)
		}
		if frame.SessionID != 7 || frame.PathID != 2 || frame.Seq != uint64(i+1) {
			t.Fatalf("header = %+v", frame.Header)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("plaintext = %x, want %x", got, plaintext)
		}
	}
}

func TestOpenFrameOutOfOrder(t *testing.T) {
	sender := testCipher(t)
	receiver := testCipher(t)

	frames := make(map[uint64][]byte)
	for seq := uint64(1); seq <= 3; seq++ {
		d, err := sender.sealAt(seq, testHeader(seq), []byte{byte(seq)})
		if err != nil {
			t.Fatal(err)
		}
		frames[seq] = d
	}
	for _, seq := range []uint64{3, 1, 2} {
		if _, _, err := receiver.OpenFrame(frames[seq]); err != nil {
			t.Fatalf("open seq %d: %v", seq, err)
		}
	}
}

func TestOpenFrameReplay(t *testing.T) {
	sender := testCipher(t)
	receiver := testCipher(t)
	d, _, err := sender.SealFrame(testHeader(0), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiver.OpenFrame(d); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiver.OpenFrame(d); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay error = %v, want ErrReplay", err)
	}
}

func TestOpenFrameOldSequence(t *testing.T) {
	receiver := testCipher(t)
	sender := testCipher(t)
	old, err := sender.sealAt(1, testHeader(1), []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := sender.sealAt(ReplayWindowBits+2, testHeader(ReplayWindowBits+2), []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiver.OpenFrame(newer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiver.OpenFrame(old); !errors.Is(err, ErrReplay) {
		t.Fatalf("old sequence error = %v, want ErrReplay", err)
	}
}

func TestOpenFrameTampered(t *testing.T) {
	sender := testCipher(t)
	receiver := testCipher(t)
	d, _, err := sender.SealFrame(testHeader(0), []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("header", func(t *testing.T) {
		bad := append([]byte(nil), d...)
		bad[16] ^= 0x01 // path ID is authenticated
		if _, _, err := receiver.OpenFrame(bad); err == nil {
			t.Fatal("accepted tampered header")
		}
	})
	t.Run("payload", func(t *testing.T) {
		bad := append([]byte(nil), d...)
		bad[len(bad)-1] ^= 0x01
		if _, _, err := receiver.OpenFrame(bad); err == nil {
			t.Fatal("accepted tampered payload")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		if _, _, err := receiver.OpenFrame(d[:len(d)-1]); err == nil {
			t.Fatal("accepted truncated frame")
		}
	})
}

func TestOpenFrameRejectsHandshake(t *testing.T) {
	c := testCipher(t)
	f, err := (protocol.Frame{Header: protocol.Header{Type: protocol.FrameHandshakeInit}, Payload: []byte("x")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.OpenFrame(f); !errors.Is(err, ErrNotEncryptedFrame) {
		t.Fatalf("error = %v, want ErrNotEncryptedFrame", err)
	}
}

func TestSealFrameRejectsHandshake(t *testing.T) {
	c := testCipher(t)
	if _, _, err := c.SealFrame(protocol.Header{Type: protocol.FrameHandshakeInit, SessionID: 1}, nil); !errors.Is(err, ErrNotEncryptedFrame) {
		t.Fatalf("error = %v", err)
	}
	if _, _, err := c.SealFrame(protocol.Header{Type: protocol.FrameData}, nil); err == nil {
		t.Fatal("accepted zero session ID")
	}
}

func TestNewTrafficCipherBadKey(t *testing.T) {
	if _, err := NewTrafficCipher([]byte{1, 2, 3}); err == nil {
		t.Fatal("accepted short key")
	}
}

func TestConcurrentOpen(t *testing.T) {
	sender := testCipher(t)
	receiver := testCipher(t)
	const n = 200
	frames := make([][]byte, n)
	for i := 0; i < n; i++ {
		d, _, err := sender.SealFrame(testHeader(0), []byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		frames[i] = d
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(d []byte) {
			defer wg.Done()
			if _, _, err := receiver.OpenFrame(d); err != nil {
				errs <- err
			}
		}(frames[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestNonceLayout(t *testing.T) {
	n := nonceForSeq(protocol.FrameData, 0x0102030405060708)
	want := [12]byte{0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}
	if n != want {
		t.Fatalf("data nonce = %x, want %x", n, want)
	}
	n = nonceForSeq(protocol.FrameControl, 0x0102030405060708)
	want[0] = 1
	if n != want {
		t.Fatalf("control nonce = %x, want %x", n, want)
	}
}

func TestIndependentSequenceSpaces(t *testing.T) {
	sender := testCipher(t)
	receiver := testCipher(t)

	d1, dh1, err := sender.SealFrame(testHeader(0), []byte("data-1"))
	if err != nil {
		t.Fatal(err)
	}
	c1, ch1, err := sender.SealFrame(protocol.Header{Type: protocol.FrameControl, SessionID: 7, PathID: 2}, []byte("control-1"))
	if err != nil {
		t.Fatal(err)
	}
	d2, dh2, err := sender.SealFrame(testHeader(0), []byte("data-2"))
	if err != nil {
		t.Fatal(err)
	}
	if dh1.Seq != 1 || dh2.Seq != 2 {
		t.Fatalf("data sequences = %d,%d", dh1.Seq, dh2.Seq)
	}
	if ch1.Seq != 1 {
		t.Fatalf("control sequence = %d", ch1.Seq)
	}
	for _, frame := range [][]byte{d1, c1, d2} {
		if _, _, err := receiver.OpenFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := receiver.OpenFrame(d1); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed data error = %v", err)
	}
}

func TestSequenceExhaustion(t *testing.T) {
	c := testCipher(t)
	c.dataSend.seq.Store(math.MaxUint64 - 1)
	if _, h, err := c.SealFrame(testHeader(0), []byte("last")); err != nil || h.Seq != math.MaxUint64 {
		t.Fatalf("last sequence: seq=%d err=%v", h.Seq, err)
	}
	if _, _, err := c.SealFrame(testHeader(0), []byte("overflow")); err == nil {
		t.Fatal("accepted sequence overflow")
	}
	if _, _, err := c.SealFrame(testHeader(0), []byte("overflow")); err == nil {
		t.Fatal("accepted sequence after exhaustion")
	}
}
