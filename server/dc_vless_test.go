package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestEnqueueBackendDataReturnsWhenQueueIsFull(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}()

	stream := newDCBackendStream(context.Background(), clientConn)
	defer stream.Close()

	stream.writeCh = make(chan []byte, 1)
	stream.writeCh <- []byte("busy")

	done := make(chan error, 1)
	go func() {
		done <- enqueueBackendData(stream, []byte("next"))
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "queue full") {
			t.Fatalf("enqueueBackendData() error = %v, want queue full", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("enqueueBackendData() blocked on a full queue")
	}
}

func TestEnqueueBackendDataReturnsCanceledForClosedStream(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}()

	stream := newDCBackendStream(context.Background(), clientConn)
	stream.Close()

	err := enqueueBackendData(stream, []byte("next"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("enqueueBackendData() error = %v, want context canceled", err)
	}
}

func TestDCBackendStreamWriteReturnsNetErrClosedAfterClose(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}()

	stream := newDCBackendStream(context.Background(), clientConn)
	stream.Close()

	err := stream.write([]byte("next"))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stream.write() error = %v, want net.ErrClosed", err)
	}
}
