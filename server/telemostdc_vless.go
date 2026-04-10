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
	"github.com/cacggghp/vk-turn-proxy/internal/telemost"
)

type telemostBackendStream struct {
	conn      net.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	writeCh   chan []byte
	closeOnce sync.Once
	connMu    sync.Mutex
	closed    atomic.Bool
}

func newTelemostBackendStream(parent context.Context, conn net.Conn) *telemostBackendStream {
	ctx, cancel := context.WithCancel(parent)
	return &telemostBackendStream{
		conn:    conn,
		ctx:     ctx,
		cancel:  cancel,
		writeCh: make(chan []byte, 32),
	}
}

func (s *telemostBackendStream) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		s.connMu.Lock()
		_ = s.conn.Close()
		s.connMu.Unlock()
	})
}

func enqueueBackendData(stream *telemostBackendStream, data []byte) error {
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

func (s *telemostBackendStream) write(data []byte) error {
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

func handleTelemostBackendStream(streamID uint16, stream *telemostBackendStream, mux *dcmux.Multiplexer, closeStream func(uint16), closeMuxStream func(uint16)) {
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
				if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
					if telemost.DebugEnabled() {
						log.Printf("Telemost DataChannel VLESS backend stream closed: %v", readErr)
					}
				} else {
					log.Printf("Telemost DataChannel VLESS backend read error: %v", readErr)
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
					log.Printf("Telemost DataChannel VLESS backend write error: %v", err)
					return
				}
			}
		}
	}()

	wg.Wait()
}

func runTelemostDataChannelVLESSMode(ctx context.Context, inviteLink, connectAddr string) error {
	var (
		connMu sync.Mutex
		conns  = make(map[uint16]*telemostBackendStream)
	)

	var peer *telemost.Peer
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
		streams := make([]*telemostBackendStream, 0, len(conns))
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
			log.Printf("Telemost DataChannel VLESS server: failed to close mux stream %d: %v", sid, err)
		}
	}

	getOrCreateBackendStream := func(sid uint16) (*telemostBackendStream, error) {
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

		stream = newTelemostBackendStream(ctx, conn)

		connMu.Lock()
		if existing := conns[sid]; existing != nil {
			connMu.Unlock()
			stream.Close()
			return existing, nil
		}
		conns[sid] = stream
		connMu.Unlock()

		go handleTelemostBackendStream(sid, stream, mux, closeStream, closeMuxStream)
		return stream, nil
	}

	peer, err := telemost.NewConnectedPeer(ctx, inviteLink, mux.HandleFrame, func() {
		if telemost.DebugEnabled() {
			log.Printf("Telemost DataChannel VLESS server: peer reconnected, closing active backend streams")
		}
		closeAll()
		mux.Reset()
	})
	if err != nil {
		return err
	}
	defer func(peer *telemost.Peer) {
		err := peer.Close()
		if err != nil {
			log.Println(err)
		}
	}(peer)

	log.Printf("Telemost DataChannel VLESS server: forwarding to %s", connectAddr)
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
					log.Printf("Telemost DataChannel VLESS backend dial error: %v", err)
					closeMuxStream(sid)
					continue
				}
				if err := enqueueBackendData(stream, data); err != nil {
					if !errors.Is(err, context.Canceled) {
						log.Printf("Telemost DataChannel VLESS backend stream %d stalled: %v", sid, err)
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
