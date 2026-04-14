package rtcutil

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

type fakeDataChannel struct {
	buffered uint64
	state    webrtc.DataChannelState
	sent     []byte
}

func (dc *fakeDataChannel) BufferedAmount() uint64 {
	return dc.buffered
}

func (dc *fakeDataChannel) ReadyState() webrtc.DataChannelState {
	return dc.state
}

func (dc *fakeDataChannel) Send(data []byte) error {
	dc.sent = append([]byte(nil), data...)
	return nil
}

type fakeWebSocketConn struct {
	pongHandler func(string) error
	deadlines   []time.Time
}

func (conn *fakeWebSocketConn) SetPongHandler(handler func(string) error) {
	conn.pongHandler = handler
}

func (conn *fakeWebSocketConn) SetReadDeadline(deadline time.Time) error {
	conn.deadlines = append(conn.deadlines, deadline)
	return nil
}

func TestSendDataChannelSendsWhenOpen(t *testing.T) {
	t.Parallel()

	dc := &fakeDataChannel{state: webrtc.DataChannelStateOpen}
	payload := []byte("payload")

	if err := SendDataChannel(dc, payload); err != nil {
		t.Fatalf("SendDataChannel() err = %v, want nil", err)
	}
	if !bytes.Equal(dc.sent, payload) {
		t.Fatalf("sent payload = %q, want %q", dc.sent, payload)
	}
}

func TestSendDataChannelReturnsNotReady(t *testing.T) {
	t.Parallel()

	dc := &fakeDataChannel{state: webrtc.DataChannelStateClosing}

	if err := SendDataChannel(dc, []byte("payload")); err == nil {
		t.Fatal("SendDataChannel() err = nil, want error")
	}
}

func TestConfigureWebSocketReadDeadline(t *testing.T) {
	t.Parallel()

	conn := &fakeWebSocketConn{}

	if err := ConfigureWebSocketReadDeadline(conn, time.Minute); err != nil {
		t.Fatalf("ConfigureWebSocketReadDeadline() err = %v, want nil", err)
	}
	if conn.pongHandler == nil {
		t.Fatal("pong handler was not configured")
	}
	if len(conn.deadlines) != 1 {
		t.Fatalf("deadline updates = %d, want 1", len(conn.deadlines))
	}
	if err := conn.pongHandler(""); err != nil {
		t.Fatalf("pong handler err = %v, want nil", err)
	}
	if len(conn.deadlines) != 2 {
		t.Fatalf("deadline updates after pong = %d, want 2", len(conn.deadlines))
	}
}
