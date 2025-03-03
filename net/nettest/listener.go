// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nettest

import (
	"context"
	"net"
	"strings"
	"sync"
)

const (
	bufferSize = 256 * 1024
)

// Listener is a net.Listener using using NewConn to create pairs of network
// connections connected in memory using a buffered pipe. It also provides a
// Dial method to establish new connections.
type Listener struct {
	addr      connAddr
	ch        chan Conn
	closeOnce sync.Once
	closed    chan struct{}
}

// Listen returns a new Listener for the provided address.
func Listen(addr string) *Listener {
	return &Listener{
		addr:   connAddr(addr),
		ch:     make(chan Conn),
		closed: make(chan struct{}),
	}
}

// Addr implements net.Listener.Addr.
func (l *Listener) Addr() net.Addr {
	return l.addr
}

// Close closes the pipe listener.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

// Accept blocks until a new connection is available or the listener is closed.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Dial connects to the listener using the provided context.
// The provided Context must be non-nil. If the context expires before the
// connection is complete, an error is returned. Once successfully connected
// any expiration of the context will not affect the connection.
func (l *Listener) Dial(ctx context.Context, network, addr string) (_ net.Conn, err error) {
	if !strings.HasSuffix(network, "tcp") {
		return nil, net.UnknownNetworkError(network)
	}
	if connAddr(addr) != l.addr {
		return nil, &net.AddrError{
			Err:  "invalid address",
			Addr: addr,
		}
	}
	c, s := NewConn(addr, bufferSize)
	defer func() {
		if err != nil {
			c.Close()
			s.Close()
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	case l.ch <- s:
		return c, nil
	}
}
