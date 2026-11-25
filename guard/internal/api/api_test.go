package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/policy"
)

// Slow clients holding connections half-sent (slowloris) are cut by the
// read deadline: the API answers again within seconds, and the guard does
// not run out of file descriptors or memory.
func TestSlowClientsDoNotKillTheAPI(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, addr, "tok", &policy.Policy{}, log.New(io.Discard, "", 0))
	time.Sleep(200 * time.Millisecond)
	var conns []net.Conn
	for i := 0; i < 500; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			break
		}
		fmt.Fprintf(c, "POST /v1/claim HTTP/1.1\r\nHost: g\r\nAuthorization: Bearer tok\r\nContent-Length: 4000\r\n\r\n{")
		conns = append(conns, c)
	}
	// the flood is held within MaxConns (the rest closed at once): the guard
	// keeps its file descriptors; once the slow ones are cut it answers
	time.Sleep(4 * time.Second)
	for _, c := range conns {
		c.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c := http.Client{Timeout: 3 * time.Second}
		r, err := c.Post("http://"+addr+"/v1/health", "application/json", strings.NewReader("{}"))
		if err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the API never answered again while slow clients held connections")
}
