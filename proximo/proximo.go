package proximo

import (
	"crypto/tls"
	"time"

	"google.golang.org/grpc/keepalive"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"

	"github.com/uw-labs/substrate"
)

// KeepAlive provides configuration for the gRPC keep alive
type KeepAlive struct {
	// Time the interval at which a keep alive is performed
	Time time.Duration
	// TimeOut the duration in which a keep alive is deemed to have failed if no response is received
	Timeout time.Duration
}

type dialConfig struct {
	broker         string
	insecure       bool
	keepAlive      *KeepAlive
	maxRecvMsgSize int
}

const defaultMaxRecvMsgSize = 1024 * 1024 * 64

func dialProximo(conf dialConfig) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	if conf.keepAlive != nil {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    conf.keepAlive.Time,
			Timeout: conf.keepAlive.Timeout,
		}))
	}

	if conf.insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(new(tls.Config))))
	}

	maxRecvMsgSize := defaultMaxRecvMsgSize
	if conf.maxRecvMsgSize > 0 {
		maxRecvMsgSize = conf.maxRecvMsgSize
	}
	opts = append(opts, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgSize)))

	conn, err := grpc.Dial(conf.broker, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial %s", conf.broker)
	}

	return conn, nil
}

func proximoStatus(conn *grpc.ClientConn) (*substrate.Status, error) {
	switch state := conn.GetState(); state {
	case connectivity.Idle, connectivity.Ready:
		return &substrate.Status{Working: true}, nil
	case connectivity.Connecting:
		return &substrate.Status{Working: true, Problems: []string{"connecting"}}, nil
	case connectivity.TransientFailure:
		return &substrate.Status{Working: true, Problems: []string{"transient failure"}}, nil
	case connectivity.Shutdown:
		return &substrate.Status{Working: false, Problems: []string{"connection shutdown"}}, nil
	default:
		return nil, errors.Errorf("unknown connection state: %s", state)
	}
}
