package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestProbeEntryAudioHash(t *testing.T) {
	entry := &probeEntry{closed: make(chan struct{})}
	entry.feed([]byte("abc"))
	entry.feed([]byte("def"))
	sum := sha256.Sum256([]byte("abcdef"))
	if got := hex.EncodeToString(entry.audio.Sum(nil)); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha mismatch: %s", got)
	}
	if entry.audioLen != 6 {
		t.Fatalf("audioLen = %d", entry.audioLen)
	}
	entry.markClosed()
	entry.markClosed()
	select {
	case <-entry.closed:
	default:
		t.Fatal("closed channel not closed")
	}
}
