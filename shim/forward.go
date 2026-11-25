package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// forwards parses "28332=knots:28332,28333=knots:28333": listen port on the
// left, target on the right.
func parseForwards(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "=")
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("bad forward %q, want port=host:port", part)
		}
		out[":"+strings.TrimPrefix(from, ":")] = to
	}
	return out, nil
}

// maxForwardConns caps the connections relayed on one forward at once.
const maxForwardConns = 16

// forward relays TCP connections on listen to target, byte for byte. The
// swap backend finds the node's ZMQ endpoints through getzmqnotifications
// and connects to them at the RPC host, which is this shim, so the shim
// passes them through.
func forward(ctx context.Context, listen, target string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	log.Printf("forwarding %s to %s", listen, target)
	conns := make(chan struct{}, maxForwardConns)
	backoff := 5 * time.Millisecond
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Out of file descriptors and the like pass; keep serving RPC
			// and try again, as net/http does
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || isTemporary(err) {
				log.Printf("forward %s: accept: %v; retrying in %s", listen, err, backoff)
				time.Sleep(backoff)
				backoff = min(2*backoff, time.Second)
				continue
			}
			return err
		}
		backoff = 5 * time.Millisecond
		// Kiloswap: a few subscribers at most (the backend's handful of
		// ZMQ sockets); more connections are closed at once
		select {
		case conns <- struct{}{}:
			go func() {
				defer func() { <-conns }()
				relay(ctx, conn, target)
			}()
		default:
			log.Printf("forward %s: over %d connections, closing one", listen, maxForwardConns)
			conn.Close()
		}
	}
}

func isTemporary(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.ENOMEM)
}

func relay(ctx context.Context, conn net.Conn, target string) {
	defer conn.Close()
	var d net.Dialer
	up, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		log.Printf("forward to %s: %v", target, err)
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = copyWithDeadline(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	go copyHalf(up, conn)
	go func() {
		// Kiloswap: a subscriber that stops reading is dropped (both ends
		// closed), never buffered for: the node's ZMQ queue (hwm) drops
		// for it instead
		copyHalf(conn, up)
		conn.Close()
		up.Close()
	}()
	wg.Wait()
}

// writeTimeout is how long one write to a peer may block (nanoseconds).
var writeTimeout atomic.Int64

func init() { writeTimeout.Store(int64(10 * time.Second)) }

// copyWithDeadline is io.Copy with a deadline on each write.
func copyWithDeadline(dst, src net.Conn) (int64, error) {
	buf := make([]byte, 32<<10)
	var n int64
	for {
		r, rerr := src.Read(buf)
		if r > 0 {
			_ = dst.SetWriteDeadline(time.Now().Add(time.Duration(writeTimeout.Load())))
			w, werr := dst.Write(buf[:r])
			n += int64(w)
			if werr != nil {
				if ne, ok := werr.(net.Error); ok && ne.Timeout() {
					log.Printf("forward: dropping a peer that stopped reading")
				}
				return n, werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return n, nil
			}
			return n, rerr
		}
	}
}
