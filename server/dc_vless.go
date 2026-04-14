package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cacggghp/vk-turn-proxy/internal/dcmux"
)

type dcBackendStream struct {
	conn      net.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	writeCh   chan []byte
	closeOnce sync.Once
	connMu    sync.Mutex
	closed    atomic.Bool
}

func newDCBackendStream(parent context.Context, conn net.Conn) *dcBackendStream {
	ctx, cancel := context.WithCancel(parent)
	return &dcBackendStream{
		conn:    conn,
		ctx:     ctx,
		cancel:  cancel,
		writeCh: make(chan []byte, 32),
	}
}

func (s *dcBackendStream) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		s.connMu.Lock()
		_ = s.conn.Close()
		s.connMu.Unlock()
	})
}

func enqueueBackendData(stream *dcBackendStream, data []byte) error {
	if stream.closed.Load() {
		return context.Canceled
	}

	select {
	case <-stream.ctx.Done():
		return context.Canceled
	case stream.writeCh <- data:
		return nil
	default:
		return fmt.Errorf("backend write queue full")
	}
}

func (s *dcBackendStream) write(data []byte) error {
	if s.closed.Load() {
		return net.ErrClosed
	}

	s.connMu.Lock()
	defer s.connMu.Unlock()

	if s.closed.Load() {
		return net.ErrClosed
	}

	if err := s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set backend write deadline: %w", err)
	}
	if _, err := s.conn.Write(data); err != nil {
		return fmt.Errorf("write backend data: %w", err)
	}

	return nil
}

func handleDCBackendStream(streamID uint16, stream *dcBackendStream, mux *dcmux.Multiplexer, closeStream func(uint16), closeMuxStream func(uint16)) {
	defer closeStream(streamID)
	defer closeMuxStream(streamID)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer stream.cancel()

		buf := make([]byte, 32768)
		for {
			n, readErr := stream.conn.Read(buf)
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, net.ErrClosed) {
					log.Printf("DataChannel VLESS backend read error: %v", readErr)
				}
				return
			}
			if sendErr := mux.SendData(streamID, buf[:n]); sendErr != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer stream.cancel()

		for {
			select {
			case <-stream.ctx.Done():
				return
			case data := <-stream.writeCh:
				if err := stream.write(data); err != nil {
					if errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) || stream.closed.Load() {
						return
					}
					log.Printf("DataChannel VLESS backend write error: %v", err)
					return
				}
			}
		}
	}()

	wg.Wait()
}

func runTelemostDataChannelVLESSMode(ctx context.Context, inviteLink, connectAddr string) error {
	return runDataChannelVLESSMode(ctx, "Telemost", connectTelemostDataChannelPeer, inviteLink, connectAddr)
}

func runJazzDataChannelVLESSMode(ctx context.Context, room, connectAddr string) error {
	return runDataChannelVLESSMode(ctx, "SaluteJazz", connectJazzDataChannelPeer, room, connectAddr)
}

func runDataChannelVLESSMode(ctx context.Context, providerName string, connectPeer dataChannelConnectFunc, room, connectAddr string) error {
	var (
		connMu sync.Mutex
		conns  = make(map[uint16]*dcBackendStream)
	)

	var peer dataChannelPeer
	mux := dcmux.New(0, func(frame []byte) error {
		return peer.Send(frame)
	})

	closeStream := func(sid uint16) {
		connMu.Lock()
		stream := conns[sid]
		delete(conns, sid)
		connMu.Unlock()
		if stream != nil {
			stream.Close()
		}
	}

	closeAll := func() {
		connMu.Lock()
		streams := make([]*dcBackendStream, 0, len(conns))
		for sid, stream := range conns {
			streams = append(streams, stream)
			delete(conns, sid)
		}
		connMu.Unlock()

		for _, stream := range streams {
			stream.Close()
		}
	}

	closeMuxStream := func(sid uint16) {
		if mux.StreamClosed(sid) {
			return
		}
		if err := mux.CloseStream(sid); err != nil {
			log.Printf("%s DataChannel VLESS server: failed to close mux stream %d: %v", providerName, sid, err)
		}
	}

	getOrCreateBackendStream := func(sid uint16) (*dcBackendStream, error) {
		connMu.Lock()
		stream := conns[sid]
		connMu.Unlock()
		if stream != nil {
			return stream, nil
		}

		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", connectAddr)
		if err != nil {
			return nil, err
		}

		stream = newDCBackendStream(ctx, conn)

		connMu.Lock()
		if existing := conns[sid]; existing != nil {
			connMu.Unlock()
			stream.Close()
			return existing, nil
		}
		conns[sid] = stream
		connMu.Unlock()

		go handleDCBackendStream(sid, stream, mux, closeStream, closeMuxStream)
		return stream, nil
	}

	peer, err := connectPeer(ctx, room, mux.HandleFrame, func() {
		log.Printf("%s DataChannel VLESS server: peer reconnected, closing active backend streams", providerName)
		closeAll()
		mux.Reset()
	})
	if err != nil {
		return err
	}
	defer func(peer dataChannelPeer) {
		err := peer.Close()
		if err != nil {
			log.Println(err)
		}
	}(peer)

	log.Printf("%s DataChannel VLESS server: forwarding to %s", providerName, connectAddr)
	activityCh := mux.WaitForActivity()

	for {
		select {
		case <-ctx.Done():
			closeAll()
			return nil
		case <-activityCh:
		}

		for _, sid := range mux.GetStreams() {
			data := mux.ReadStream(sid)
			if len(data) > 0 {
				stream, err := getOrCreateBackendStream(sid)
				if err != nil {
					log.Printf("%s DataChannel VLESS backend dial error: %v", providerName, err)
					closeMuxStream(sid)
					continue
				}
				if err := enqueueBackendData(stream, data); err != nil {
					if !errors.Is(err, context.Canceled) {
						log.Printf("%s DataChannel VLESS backend stream %d stalled: %v", providerName, sid, err)
					}
					closeStream(sid)
					closeMuxStream(sid)
					continue
				}
			}

			if mux.StreamClosed(sid) {
				closeStream(sid)
				mux.CleanupStream(sid)
			}
		}
	}
}
