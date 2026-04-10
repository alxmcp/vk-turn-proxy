package main

import (
	"context"
	"errors"
	"log"
	"net"
	"sync/atomic"

	"github.com/cacggghp/vk-turn-proxy/internal/telemost"
)

func runTelemostDataChannelMode(ctx context.Context, inviteLink, listenAddr string) error {
	listenConn, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return err
	}
	defer func(listenConn net.PacketConn) {
		err := listenConn.Close()
		if err != nil {
			log.Println(err)
		}
	}(listenConn)

	var activeLocalPeer atomic.Value
	peer, err := telemost.NewConnectedPeer(ctx, inviteLink, func(data []byte) {
		addr, ok := activeLocalPeer.Load().(net.Addr)
		if !ok || addr == nil {
			return
		}
		if _, writeErr := listenConn.WriteTo(data, addr); writeErr != nil {
			log.Printf("Telemost DataChannel: failed to write local packet: %v", writeErr)
		}
	}, nil)
	if err != nil {
		return err
	}
	defer func(peer *telemost.Peer) {
		err := peer.Close()
		if err != nil {
			log.Println(err)
		}
	}(peer)

	closeOnContextDone(ctx, listenConn)

	log.Printf("Telemost DataChannel mode: listening on %s", listenAddr)

	buf := make([]byte, 2048)
	for {
		n, addr, err := listenConn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		activeLocalPeer.Store(addr)
		if err := peer.Send(buf[:n]); err != nil {
			log.Printf("Telemost DataChannel: dropped outbound packet (%d bytes): %v", n, err)
		}
	}
}
