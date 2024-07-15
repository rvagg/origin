package mock

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	clock "github.com/jonboulle/clockwork"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	localClient "github.com/drand/drand-cli/client"
	"github.com/drand/drand/v2/common"
	"github.com/drand/drand/v2/common/chain"
	"github.com/drand/drand/v2/common/client"
	dhttp "github.com/drand/drand/v2/handler/http"
	"github.com/drand/drand/v2/protobuf/drand"
	"github.com/drand/drand/v2/test/mock"

	"github.com/drand/drand/v2/crypto"
)

// NewMockHTTPPublicServer creates a mock drand HTTP server for testing.
func NewMockHTTPPublicServer(t *testing.T, badSecondRound bool, sch *crypto.Scheme, clk clock.Clock) (string, *chain.Info, context.CancelFunc, func(bool)) {
	t.Helper()

	server := mock.NewMockServer(t, badSecondRound, sch, clk)
	client := Proxy(server)

	ctx, cancel := context.WithCancel(context.Background())

	handler, err := dhttp.New(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	var chainInfo *chain.Info
	for i := 0; i < 3; i++ {
		protoInfo, err := server.ChainInfo(ctx, &drand.ChainInfoRequest{})
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		chainInfo, err = chain.InfoFromProto(protoInfo)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		break
	}
	if chainInfo == nil {
		t.Fatal("could not use server after 3 attempts.")
	}

	handler.RegisterNewBeaconHandler(client, chainInfo.HashString())

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	httpServer := http.Server{Handler: handler.GetHTTPHandler(), ReadHeaderTimeout: 3 * time.Second}
	go httpServer.Serve(listener)

	return listener.Addr().String(), chainInfo, func() {
		httpServer.Shutdown(context.Background())
		cancel()
	}, server.(mock.Service).EmitRand
}

// drandProxy is used as a proxy between a Public service (e.g. the node as a server)
// and a Public Client (the client consumed by the HTTP API)
type drandProxy struct {
	r drand.PublicServer
}

// Proxy wraps a server interface into a client interface so it can be queried
func Proxy(s drand.PublicServer) client.Client {
	return &drandProxy{s}
}

// String returns the name of this proxy.
func (d *drandProxy) String() string {
	return "Proxy"
}

// Get returns randomness at a requested round
func (d *drandProxy) Get(ctx context.Context, round uint64) (client.Result, error) {
	resp, err := d.r.PublicRand(ctx, &drand.PublicRandRequest{Round: round})
	if err != nil {
		return nil, err
	}
	return &localClient.RandomData{
		Rnd:               resp.GetRound(),
		Random:            crypto.RandomnessFromSignature(resp.GetSignature()),
		Sig:               resp.GetSignature(),
		PreviousSignature: resp.GetPreviousSignature(),
	}, nil
}

// Watch returns new randomness as it becomes available.
func (d *drandProxy) Watch(ctx context.Context) <-chan client.Result {
	proxy := newStreamProxy(ctx)
	go func() {
		err := d.r.PublicRandStream(&drand.PublicRandRequest{}, proxy)
		if err != nil {
			proxy.Close()
		}
	}()
	return proxy.outgoing
}

// Info returns the parameters of the chain this client is connected to.
// The public key, when it started, and how frequently it updates.
func (d *drandProxy) Info(ctx context.Context) (*chain.Info, error) {
	info, err := d.r.ChainInfo(ctx, &drand.ChainInfoRequest{})
	if err != nil {
		return nil, err
	}
	return chain.InfoFromProto(info)
}

// RoundAt will return the most recent round of randomness that will be available
// at time for the current client.
func (d *drandProxy) RoundAt(t time.Time) uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := d.Info(ctx)
	if err != nil {
		return 0
	}
	return common.CurrentRound(t.Unix(), info.Period, info.GenesisTime)
}

func (d *drandProxy) Close() error {
	return nil
}

// streamProxy directly relays messages of the PublicRandResponse stream.
type streamProxy struct {
	ctx      context.Context
	cancel   context.CancelFunc
	outgoing chan client.Result
}

func newStreamProxy(ctx context.Context) *streamProxy {
	ctx, cancel := context.WithCancel(ctx)
	s := streamProxy{
		ctx:      ctx,
		cancel:   cancel,
		outgoing: make(chan client.Result, 1),
	}
	return &s
}

func (s *streamProxy) Send(next *drand.PublicRandResponse) error {
	d := common.Beacon{
		Round:       next.Round,
		Signature:   next.Signature,
		PreviousSig: next.PreviousSignature,
	}
	select {
	case s.outgoing <- &d:
		return nil
	case <-s.ctx.Done():
		close(s.outgoing)
		return s.ctx.Err()
	default:
		return nil
	}
}

func (s *streamProxy) Close() {
	s.cancel()
}

/* implement the grpc stream interface. not used since messages passed directly. */

func (s *streamProxy) SetHeader(metadata.MD) error {
	return nil
}
func (s *streamProxy) SendHeader(metadata.MD) error {
	return nil
}
func (s *streamProxy) SetTrailer(metadata.MD) {}

func (s *streamProxy) Context() context.Context {
	return peer.NewContext(s.ctx, &peer.Peer{Addr: &net.UnixAddr{}})
}
func (s *streamProxy) SendMsg(_ interface{}) error {
	return nil
}
func (s *streamProxy) RecvMsg(_ interface{}) error {
	return nil
}

func (s *streamProxy) Header() (metadata.MD, error) {
	return nil, nil
}

func (s *streamProxy) Trailer() metadata.MD {
	return nil
}
func (s *streamProxy) CloseSend() error {
	return nil
}
