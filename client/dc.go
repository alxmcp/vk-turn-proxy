package main

import (
	"context"
	"errors"
	"log"
	"net"
	"sync/atomic"

	"github.com/cacggghp/vk-turn-proxy/internal/jazz"
	"github.com/cacggghp/vk-turn-proxy/internal/telemost"
)

type dataChannelPeer interface {
	Send([]byte) error
	Close() error
}

type dataChannelConnectFunc func(context.Context, string, func([]byte), func()) (dataChannelPeer, error)

func connectTelemostDataChannelPeer(ctx context.Context, room string, onData func([]byte), onReconnect func()) (dataChannelPeer, error) {
	return telemost.NewConnectedPeer(ctx, room, onData, onReconnect)
}

func connectJazzDataChannelPeer(ctx context.Context, room string, onData func([]byte), onReconnect func()) (dataChannelPeer, error) {
	return jazz.NewConnectedPeer(ctx, room, onData, onReconnect)
}

func runTelemostDataChannelMode(ctx context.Context, inviteLink, listenAddr string) error {
	return runDataChannelMode(ctx, "Telemost", connectTelemostDataChannelPeer, inviteLink, listenAddr)
}

func runJazzDataChannelMode(ctx context.Context, room, listenAddr string) error {
	return runDataChannelMode(ctx, "SaluteJazz", connectJazzDataChannelPeer, room, listenAddr)
}

func runDataChannelMode(ctx context.Context, providerName string, connectPeer dataChannelConnectFunc, room, listenAddr string) error {
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
	peer, err := connectPeer(ctx, room, func(data []byte) {
		addr, ok := activeLocalPeer.Load().(net.Addr)
		if !ok || addr == nil {
			return
		}
		if _, writeErr := listenConn.WriteTo(data, addr); writeErr != nil {
			log.Printf("%s DataChannel: failed to write local packet: %v", providerName, writeErr)
		}
	}, nil)
	if err != nil {
		return err
	}
	defer func(peer dataChannelPeer) {
		err := peer.Close()
		if err != nil {
			log.Println(err)
		}
	}(peer)

	closeOnContextDone(ctx, listenConn)

	log.Printf("%s DataChannel mode: listening on %s", providerName, listenAddr)

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
			log.Printf("%s DataChannel: dropped outbound packet (%d bytes): %v", providerName, n, err)
		}
	}
}
