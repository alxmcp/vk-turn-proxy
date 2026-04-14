package rtcutil

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	dataChannelBufferLimit = 256 * 1024
	dataChannelSendTimeout = 2 * time.Second
	dataChannelPollDelay   = 10 * time.Millisecond
)

type DataChannel interface {
	BufferedAmount() uint64
	ReadyState() webrtc.DataChannelState
	Send([]byte) error
}

type WebSocketConn interface {
	SetPongHandler(func(string) error)
	SetReadDeadline(time.Time) error
}

func SendDataChannel(dc DataChannel, data []byte) error {
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("datachannel not ready")
	}

	start := time.Now()
	for dc.BufferedAmount() > dataChannelBufferLimit {
		if time.Since(start) > dataChannelSendTimeout {
			return fmt.Errorf("datachannel buffer is full")
		}
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			return fmt.Errorf("datachannel not ready")
		}
		time.Sleep(dataChannelPollDelay)
	}

	return dc.Send(data)
}

func ConfigureWebSocketReadDeadline(ws WebSocketConn, timeout time.Duration) error {
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(timeout))
	})
	return ws.SetReadDeadline(time.Now().Add(timeout))
}
