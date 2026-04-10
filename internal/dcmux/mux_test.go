package dcmux

import (
	"encoding/binary"
	"testing"
)

func TestCleanupStreamRemovesClosedStream(t *testing.T) {
	t.Parallel()

	mux := New(1, func([]byte) error { return nil })
	mux.maxStreams = 1

	dataReady := mux.WaitForData(7)
	mux.HandleFrame(testFrame(1, 7, 0, []byte("hello")))
	if got := mux.GetStreams(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("GetStreams() = %v, want [7]", got)
	}
	select {
	case <-dataReady:
	default:
		t.Fatal("expected dataReady signal after HandleFrame")
	}
	select {
	case <-mux.WaitForActivity():
	default:
		t.Fatal("expected activity signal after HandleFrame")
	}

	mux.CleanupStream(7)

	if _, ok := <-dataReady; ok {
		t.Fatal("dataReady channel stayed open after CleanupStream")
	}

	if got := mux.GetStreams(); len(got) != 0 {
		t.Fatalf("GetStreams() after CleanupStream = %v, want empty", got)
	}

	mux.HandleFrame(testFrame(1, 8, 0, []byte("world")))
	if got := mux.GetStreams(); len(got) != 1 || got[0] != 8 {
		t.Fatalf("GetStreams() after reusing capacity = %v, want [8]", got)
	}
}

func testFrame(clientID uint32, sid uint16, seq uint32, payload []byte) []byte {
	frame := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(frame[0:4], clientID)
	binary.BigEndian.PutUint16(frame[4:6], sid)
	binary.BigEndian.PutUint16(frame[6:8], uint16(len(payload)))
	binary.BigEndian.PutUint32(frame[8:12], seq)
	copy(frame[12:], payload)
	return frame
}
