package sshutil

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

// ReverseProxyConfig defines the configuration options for launching a reverse proxy of two SSH endpoints.
type ReverseProxyConfig struct {
	ServerConn     *ssh.ServerConn
	ServerChannels <-chan ssh.NewChannel
	ServerRequests <-chan *ssh.Request
	TargetConn     net.Conn
	TargetHostname string
	TargetClientConfig   *ssh.ClientConfig
}

// ReverseProxy performs a single host reverse proxy on two SSH connections.
func ReverseProxy(ctx context.Context, config ReverseProxyConfig) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	destConn, destChans, destReqs, err := ssh.NewClientConn(config.TargetConn, config.TargetHostname, config.TargetClientConfig)
	if err != nil {
		return fmt.Errorf("new client conn: %w", err)
	}

	shutdownErr := make(chan error, 1)
	go func() {
		shutdownErr <- config.ServerConn.Conn.Wait()
	}()

	go processChannels(ctx, destConn, config.ServerChannels)
	go processChannels(ctx, config.ServerConn.Conn, destChans)
	go processRequests(ctx, destConn, config.ServerRequests)
	go processRequests(ctx, config.ServerConn.Conn, destReqs)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-shutdownErr:
		return err
	}
}

// processChannels handles each ssh.NewChannel concurrently.
func processChannels(ctx context.Context, destConn ssh.Conn, chans <-chan ssh.NewChannel) {
	defer destConn.Close()
	for newCh := range chans {
		newCh := newCh
		go func() {
			if err := handleChannel(ctx, destConn, newCh); err != nil {
				// TODO
			}
		}()
	}
}

// processRequests handles each *ssh.Request in series.
func processRequests(ctx context.Context, dest requestDest, requests <-chan *ssh.Request) {
	for req := range requests {
		req := req
		if err := handleRequest(ctx, dest, req); err != nil {
			// TODO
		}
	}
}

// handleChannel performs the bicopy between the destination SSH connection and a new incoming channel.
func handleChannel(ctx context.Context, destConn ssh.Conn, newChannel ssh.NewChannel) error {
	destCh, destReqs, err := destConn.OpenChannel(newChannel.ChannelType(), newChannel.ExtraData())
	if err != nil {
		if openChanErr, ok := err.(*ssh.OpenChannelError); ok {
			_ = newChannel.Reject(openChanErr.Reason, openChanErr.Message)
		} else {
			_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())
		}
		return fmt.Errorf("open channel: %w", err)
	}
	defer destCh.Close()

	originCh, originRequests, err := newChannel.Accept()
	if err != nil {
		return fmt.Errorf("accept new channel: %w", err)
	}
	defer originCh.Close()

	// TODO(@cmoog) verify that this blocking behavior is correct.
	// As is, only one requests channel must be fully processed
	// before the ssh.Channels themselves are closed.
	requestsDone := make(chan struct{})
	go func() {
		defer func() { requestsDone <- struct{}{} }()
		processRequests(ctx, channelRequestDest{originCh}, destReqs)
	}()

	go func() {
		// TODO(@cmoog) Verify: from limited testing, this request channel does not appear to be closed
		// by the client causing this function to hang if we wait on it.
		processRequests(ctx, channelRequestDest{destCh}, originRequests)
	}()

	if err := bicopy(ctx, originCh, destCh); err != nil {
		return fmt.Errorf("bidirectional copy: %w", err)
	}
	
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-requestsDone:
		return nil
	}
}

// bicopy copies data between the two channels,
// but does not perform complete closure.
// It will block until the context is cancelled or one of the
// copies has completed.
//
// ! this is subtly different from xio.Bicopy, which
// fully closes both connections after one copy exits.
func bicopy(ctx context.Context, c1, c2 ssh.Channel) error {
	ctx1, cancel := context.WithCancel(ctx)
	defer cancel()

	copyWithCloseWrite := func(a, b ssh.Channel) {
		defer cancel()
		defer func() { _ = a.CloseWrite() }()

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(a, b)
		}()
		_, _ = io.Copy(a.Stderr(), b.Stderr())
		wg.Wait()
	}

	go copyWithCloseWrite(c1, c2)
	go copyWithCloseWrite(c2, c1)

	<-ctx1.Done()

	// ignore Copy and CloseWrite errors, only error if parent context is done
	return ctx.Err()
}

// channelRequestDest wraps the ssh.Channel type to conform with the standard SendRequest function signiture.
type channelRequestDest struct {
	ssh.Channel
}

func (c channelRequestDest) SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error) {
	ok, err := c.Channel.SendRequest(name, wantReply, payload)
	return ok, nil, err
}

// requestDest defines a resource capable of receiving requests, (global or channel).
type requestDest interface {
	SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error)
}

func handleRequest(ctx context.Context, dest requestDest, request *ssh.Request) error {
	ok, payload, err := dest.SendRequest(request.Type, request.WantReply, request.Payload)
	if err != nil {
		if request.WantReply {
			if err := request.Reply(ok, payload); err != nil {
				return fmt.Errorf("reply after send failure: %w", err)
			}
		}
		return fmt.Errorf("send request: %w", err)
	}

	if request.WantReply {
		if err := request.Reply(ok, payload); err != nil {
			return fmt.Errorf("reply: %w", err)
		}
	}
	return nil
}