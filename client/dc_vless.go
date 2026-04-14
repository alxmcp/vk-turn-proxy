package main

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"time"

	"github.com/cacggghp/vk-turn-proxy/internal/dcmux"
)

func runTelemostDataChannelVLESSMode(ctx context.Context, inviteLink, listenAddr string) error {
	return runDataChannelVLESSMode(ctx, "Telemost", connectTelemostDataChannelPeer, inviteLink, listenAddr)
}

func runJazzDataChannelVLESSMode(ctx context.Context, room, listenAddr string) error {
	return runDataChannelVLESSMode(ctx, "SaluteJazz", connectJazzDataChannelPeer, room, listenAddr)
}

func runDataChannelVLESSMode(ctx context.Context, providerName string, connectPeer dataChannelConnectFunc, room, listenAddr string) error {
	var (
		connMu sync.Mutex
		conns  = make(map[uint16]net.Conn)
	)
	closeAll := func() {
		connMu.Lock()
		defer connMu.Unlock()
		for sid, conn := range conns {
			_ = conn.Close()
			delete(conns, sid)
		}
	}

	var peer dataChannelPeer
	clientID := uint32(time.Now().UnixNano())
	mux := dcmux.New(clientID, func(frame []byte) error {
		return peer.Send(frame)
	})
	peer, err := connectPeer(ctx, room, mux.HandleFrame, func() {
		log.Printf("%s DataChannel VLESS: peer reconnected, closing active TCP streams", providerName)
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

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer func(listener net.Listener) {
		err := listener.Close()
		if err != nil {
			log.Println(err)
		}
	}(listener)
	closeOnContextDone(ctx, listener)

	log.Printf("%s DataChannel VLESS mode: listening on %s", providerName, listenAddr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				closeAll()
				return nil
			}
			log.Printf("%s DataChannel VLESS accept error: %v", providerName, err)
			continue
		}

		sid := mux.OpenStream()
		connMu.Lock()
		conns[sid] = conn
		connMu.Unlock()

		go func(streamID uint16, tcpConn net.Conn) {
			defer func() {
				connMu.Lock()
				delete(conns, streamID)
				connMu.Unlock()
				if err := mux.CloseStream(streamID); err != nil {
					log.Printf("%s DataChannel VLESS: failed to close mux stream %d: %v", providerName, streamID, err)
				}
				_ = tcpConn.Close()
				mux.CleanupStream(streamID)
			}()

			done := make(chan struct{})
			streamClosed := make(chan struct{})

			go func() {
				defer close(done)
				buf := make([]byte, 32768)
				for {
					n, readErr := tcpConn.Read(buf)
					if readErr != nil {
						return
					}
					if sendErr := mux.SendData(streamID, buf[:n]); sendErr != nil {
						return
					}
				}
			}()

			go func() {
				defer close(streamClosed)
				for {
					dataReady := mux.WaitForData(streamID)

					select {
					case <-ctx.Done():
						return
					case <-done:
						return
					case _, ok := <-dataReady:
						if !ok {
							return
						}
					}

					for {
						data := mux.ReadStream(streamID)
						if len(data) == 0 {
							break
						}
						if _, writeErr := tcpConn.Write(data); writeErr != nil {
							return
						}
					}

					if mux.StreamClosed(streamID) {
						return
					}
				}
			}()

			select {
			case <-ctx.Done():
			case <-done:
			case <-streamClosed:
			}
		}(sid, conn)
	}
}
