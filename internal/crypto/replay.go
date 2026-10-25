package crypto

// ReplayWindowBits is the size of the anti-replay window.
const ReplayWindowBits = 2048

const replayWords = ReplayWindowBits / 64

// replayWindow implements a sliding bitmap of recently seen sequence numbers.
// Bit i represents maxSeen-i. It is not safe for concurrent use; callers must
// serialize access.
type replayWindow struct {
	maxSeen uint64
	bits    [replayWords]uint64
	used    bool
}

// Accept reports whether seq is new and within the window. It does not record
// the sequence number; call Mark after successful authentication.
func (w *replayWindow) Accept(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if !w.used {
		return true
	}
	if seq > w.maxSeen {
		return true
	}
	diff := w.maxSeen - seq
	if diff >= ReplayWindowBits {
		return false
	}
	word, bit := diff/64, diff%64
	return w.bits[word]&(uint64(1)<<bit) == 0
}

// Mark records seq as seen. It must be called only after Accept(seq) returned
// true and the frame authenticated successfully.
func (w *replayWindow) Mark(seq uint64) {
	if seq == 0 {
		return
	}
	if !w.used {
		w.used = true
		w.maxSeen = seq
		w.set(0)
		return
	}
	if seq > w.maxSeen {
		w.shift(seq - w.maxSeen)
		w.maxSeen = seq
		w.set(0)
		return
	}
	diff := w.maxSeen - seq
	if diff >= ReplayWindowBits {
		return
	}
	w.set(diff)
}

func (w *replayWindow) set(diff uint64) {
	w.bits[diff/64] |= uint64(1) << (diff % 64)
}

// shift moves every recorded sequence number toward older positions so bit 0
// remains reserved for the new maximum.
func (w *replayWindow) shift(n uint64) {
	if n >= ReplayWindowBits {
		clear(w.bits[:])
		return
	}
	wordShift := int(n / 64)
	bitShift := uint(n % 64)
	if wordShift > 0 {
		for i := replayWords - 1; i >= 0; i-- {
			src := i - wordShift
			if src >= 0 {
				w.bits[i] = w.bits[src]
			} else {
				w.bits[i] = 0
			}
		}
	}
	if bitShift == 0 {
		return
	}
	for i := replayWords - 1; i > 0; i-- {
		w.bits[i] = w.bits[i]<<bitShift | w.bits[i-1]>>(64-bitShift)
	}
	w.bits[0] <<= bitShift
}
