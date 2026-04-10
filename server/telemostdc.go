package main

import (
	"context"
	"errors"
	"log"
	"net"

	"github.com/cacggghp/vk-turn-proxy/internal/telemost"
)

func runTelemostDataChannelMode(ctx context.Context, inviteLink, connectAddr string) error {
	backendConn, err := net.Dial("udp", connectAddr)
	if err != nil {
		return err
	}
	defer func(backendConn net.Conn) {
		err := backendConn.Close()
		if err != nil {
			log.Println(err)
		}
	}(backendConn)

	peer, err := telemost.NewConnectedPeer(ctx, inviteLink, func(data []byte) {
		if _, writeErr := backendConn.Write(data); writeErr != nil {
			log.Printf("Telemost DataChannel: failed to write backend packet: %v", writeErr)
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

	closeOnContextDone(ctx, backendConn)

	log.Printf("Telemost DataChannel mode: forwarding to %s", connectAddr)

	buf := make([]byte, 2048)
	for {
		n, err := backendConn.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		if err := peer.Send(buf[:n]); err != nil {
			log.Printf("Telemost DataChannel: dropped backend packet (%d bytes): %v", n, err)
		}
	}
}
