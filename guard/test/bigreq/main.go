// bigreq sends lnd one DecodePayReq with a huge string under a macaroon:
// the guard must refuse it without its middleware stream ending (lab only).
//
//	bigreq LND_GRPC TLS_CERT MACAROON BYTES
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/middleware"
	"google.golang.org/grpc"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "bigreq LND_GRPC TLS_CERT MACAROON BYTES")
		os.Exit(2)
	}
	n, _ := strconv.Atoi(os.Args[4])
	conn, err := middleware.Dial(os.Args[1], os.Args[2], os.Args[3])
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r lnrpc.PayReq
	err = conn.Invoke(ctx, "/lnrpc.Lightning/DecodePayReq", &lnrpc.PayReqString{PayReq: "lnbcrt" + strings.Repeat("q", n)}, &r,
		grpc.MaxCallSendMsgSize(n+1024))
	fmt.Println("result:", err)
}
