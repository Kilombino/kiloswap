// Package middleware registers the guard with lnd as RPC middleware and
// answers lnd's interceptions. Adapted from the donations guard of
// paulscode/lightning-fork-swap (AGPL-3.0).
package middleware

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/policy"
)

// Name is how the guard registers itself with lnd.
const Name = "kiloswap-guard"

var streamDesc = &grpc.StreamDesc{
	StreamName: "RegisterRPCMiddleware", ServerStreams: true, ClientStreams: true,
}

const method = "/lnrpc.Lightning/RegisterRPCMiddleware"

// Dial opens lnd's gRPC port, trusting only its certificate, and sends the
// guard's own macaroon (register middleware, read the wallet balance).
func Dial(target, certPath, macaroonPath string) (*grpc.ClientConn, error) {
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		return nil, fmt.Errorf("no certificate in %s", certPath)
	}
	mac, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient(target,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "")),
		grpc.WithPerRPCCredentials(macaroonCreds(hex.EncodeToString(mac))),
		// lnd forwards every caveat call and answer: a big one must not end
		// the stream (requests over MaxRequestBytes are refused when judged)
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageBytes)))
}

type macaroonCreds string

func (m macaroonCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": string(m)}, nil
}
func (macaroonCreds) RequireTransportSecurity() bool { return true }

// Serve registers with lnd and answers its interceptions until the stream
// ends. Requests are judged by the policy; responses pass unchanged.
func Serve(ctx context.Context, conn grpc.ClientConnInterface, p *policy.Policy, logger *log.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := conn.NewStream(ctx, streamDesc, method)
	if err != nil {
		return err
	}
	if err := stream.SendMsg(&lnrpc.RPCMiddlewareResponse{
		MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Register{
			Register: &lnrpc.MiddlewareRegistration{
				MiddlewareName: Name, CustomMacaroonCaveatName: policy.CaveatName,
			},
		},
	}); err != nil {
		return err
	}
	// lnd waits a few seconds for each answer: judgments run side by side
	// (a slow one must not hold back the others), each with a deadline
	// shorter than lnd's, and the answers share the stream under a lock
	var send sync.Mutex
	slots := make(chan struct{}, MaxConcurrent)
	reply := func(msgID uint64, feedback *lnrpc.InterceptFeedback) error {
		send.Lock()
		defer send.Unlock()
		return stream.SendMsg(&lnrpc.RPCMiddlewareResponse{
			RefMsgId:          msgID,
			MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Feedback{Feedback: feedback},
		})
	}
	errs := make(chan error, 1)
	for {
		req := new(lnrpc.RPCMiddlewareRequest)
		if err := stream.RecvMsg(req); err != nil {
			return err
		}
		select {
		case err := <-errs:
			return err
		default:
		}
		if _, ok := req.InterceptType.(*lnrpc.RPCMiddlewareRequest_RegComplete); ok {
			logger.Printf("registered with lnd for the %q caveat", policy.CaveatName)
			p.Lifecycle("guard_registered")
			// a new registration (lnd restarted?): ids of earlier calls mean
			// nothing any more
			p.ResetPending()
			continue
		}
		if _, ok := req.InterceptType.(*lnrpc.RPCMiddlewareRequest_Response); ok {
			// answers are never refused for being busy: an allowed call's
			// answer must reach the ledger (a lockup sent, a payment done)
			go func(req *lnrpc.RPCMiddlewareRequest) {
				if err := reply(req.MsgId, judge(ctx, p, req, logger)); err != nil {
					select {
					case errs <- err:
					default:
					}
					cancel()
				}
			}(req)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			// too many judgments at once: refuse at once rather than queue
			p.Stall("too many requests at once")
			if err := reply(req.MsgId, &lnrpc.InterceptFeedback{Error: "kiloswap guard: busy"}); err != nil {
				return err
			}
			continue
		}
		go func(req *lnrpc.RPCMiddlewareRequest) {
			defer func() { <-slots }()
			feedback := judge(ctx, p, req, logger)
			if err := reply(req.MsgId, feedback); err != nil {
				select {
				case errs <- err:
				default:
				}
				cancel()
			}
		}(req)
	}
}

// MaxMessageBytes is the largest message read from lnd (the backend's
// answers are small: no DescribeGraph; requests over 256 KiB stop at
// lnd-proxy), well within the guard's memory; MaxRequestBytes the largest
// request judged (a bigger one is refused, not parsed).
const (
	MaxMessageBytes = 8 << 20
	MaxRequestBytes = 64 << 10
)

// MaxConcurrent is how many interceptions are judged at once.
const MaxConcurrent = 32

// JudgeTimeout is how long one judgment may take: lnd gives up on an
// answer after a few seconds, and a late "allowed" must never happen.
var JudgeTimeout = 1500 * time.Millisecond

// Judge is what judge needs of the policy (an interface for the tests).
type Judge interface {
	Stream(uri string) error
	Intercept(ctx context.Context, condition string, requestID uint64, m *lnrpc.RPCMessage) error
	Answered(requestID uint64, m *lnrpc.RPCMessage)
	Stall(why string)
	Forget(requestID uint64)
	Replacement(requestID uint64) []byte
}

func judge(ctx context.Context, p Judge, req *lnrpc.RPCMiddlewareRequest, logger *log.Logger) *lnrpc.InterceptFeedback {
	feedback := &lnrpc.InterceptFeedback{}
	switch t := req.InterceptType.(type) {
	case *lnrpc.RPCMiddlewareRequest_StreamAuth:
		if err := p.Stream(t.StreamAuth.GetMethodFullUri()); err != nil {
			feedback.Error = err.Error()
			logger.Printf("refused stream %s: %v", t.StreamAuth.GetMethodFullUri(), err)
		}
	case *lnrpc.RPCMiddlewareRequest_Request:
		if n := len(t.Request.GetSerialized()); n > MaxRequestBytes {
			feedback.Error = fmt.Sprintf("kiloswap guard: request of %d bytes, over %d", n, MaxRequestBytes)
			logger.Printf("refused %s: %s", t.Request.GetMethodFullUri(), feedback.Error)
			return feedback
		}
		jctx, cancel := context.WithTimeout(ctx, JudgeTimeout)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.Intercept(jctx, req.CustomCaveatCondition, req.RequestId, t.Request) }()
		var err error
		select {
		case err = <-done:
		case <-jctx.Done():
			err = fmt.Errorf("kiloswap guard: no judgment within %v", JudgeTimeout)
			p.Stall(fmt.Sprintf("%s took over %v", t.Request.GetMethodFullUri(), JudgeTimeout))
			// the late judgment, whatever it says, must not stay recorded
			go func() { <-done; p.Forget(req.RequestId) }()
		}
		if err != nil {
			feedback.Error = err.Error()
			logger.Printf("refused %s: %v", t.Request.GetMethodFullUri(), err)
		} else {
			if j := policy.Allowed[t.Request.GetMethodFullUri()]; j {
				logger.Printf("allowed %s (request %d)", t.Request.GetMethodFullUri(), req.RequestId)
			}
			if b := p.Replacement(req.RequestId); b != nil {
				feedback.ReplaceResponse, feedback.ReplacementSerialized = true, b
			}
		}
	case *lnrpc.RPCMiddlewareRequest_Response:
		p.Answered(req.RequestId, t.Response)
	default:
		feedback.Error = "kiloswap guard: unknown interception"
	}
	return feedback
}

// Run keeps the guard registered: after any error it registers again,
// waiting longer each time (up to a minute). While it is not registered,
// lnd refuses every request made with the backend's macaroon.
func Run(ctx context.Context, conn grpc.ClientConnInterface, p *policy.Policy, logger *log.Logger) {
	wait := time.Second
	for {
		start := time.Now()
		err := Serve(ctx, conn, p, logger)
		if ctx.Err() != nil {
			return
		}
		logger.Printf("stream ended: %v", err)
		p.Lifecycle("guard_stream_ended")
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < time.Minute {
			wait *= 2
		}
	}
}
