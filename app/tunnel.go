package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/karimra/gnmic/types"
	"github.com/karimra/gnmic/utils"
	tpb "github.com/openconfig/grpctunnel/proto/tunnel"
	"github.com/openconfig/grpctunnel/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func (a *App) initTunnelServer(tsc tunnel.ServerConfig) error {
	if !a.Config.UseTunnelServer {
		return nil
	}
	err := a.Config.GetTunnelServer()
	if err != nil {
		return err
	}
	go func() {
		err = a.startTunnelServer(tsc)
		if err != nil {
			a.Logger.Printf("failed to start tunnel server: %v", err)
		}
	}()
	return nil
}

func (a *App) startTunnelServer(tsc tunnel.ServerConfig) error {
	if a.Config.TunnelServer == nil {
		return nil
	}
	var err error
	a.tunServer, err = tunnel.NewServer(tsc)
	if err != nil {
		a.Logger.Printf("failed to create a tunnel server: %v", err)
		return err

	}
	// create tunnel server options
	opts, err := a.gRPCTunnelServerOpts()
	if err != nil {
		a.Logger.Printf("failed to build gRPC tunnel server options: %v", err)
		return err
	}
	a.grpcTunnelSrv = grpc.NewServer(opts...)
	// register the tunnel service with the grpc server
	tpb.RegisterTunnelServer(a.grpcTunnelSrv, a.tunServer)
	//
	var l net.Listener
	network := "tcp"
	addr := a.Config.TunnelServer.Address
	if strings.HasPrefix(a.Config.TunnelServer.Address, "unix://") {
		network = "unix"
		addr = strings.TrimPrefix(addr, "unix://")
	}

	ctx, cancel := context.WithCancel(a.ctx)
	for {
		l, err = net.Listen(network, addr)
		if err != nil {
			a.Logger.Printf("failed to start gRPC tunnel server listener: %v", err)
			time.Sleep(time.Second)
			continue
		}
		break
	}
	go func() {
		err = a.grpcTunnelSrv.Serve(l)
		if err != nil {
			a.Logger.Printf("gRPC tunnel server shutdown: %v", err)
		}
		cancel()
	}()
	defer a.grpcTunnelSrv.Stop()
	for range ctx.Done() {
	}
	return ctx.Err()
}

func (a *App) gRPCTunnelServerOpts() ([]grpc.ServerOption, error) {
	opts := make([]grpc.ServerOption, 0)
	if a.Config.TunnelServer.EnableMetrics && a.reg != nil {
		grpcMetrics := grpc_prometheus.NewServerMetrics()
		opts = append(opts,
			grpc.StreamInterceptor(grpcMetrics.StreamServerInterceptor()),
			grpc.UnaryInterceptor(grpcMetrics.UnaryServerInterceptor()),
		)
		a.reg.MustRegister(grpcMetrics)
	}

	tlscfg, err := utils.NewTLSConfig(
		a.Config.TunnelServer.CaFile,
		a.Config.TunnelServer.CertFile,
		a.Config.TunnelServer.KeyFile,
		a.Config.TunnelServer.SkipVerify,
		true,
	)
	if err != nil {
		return nil, err
	}
	if tlscfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlscfg)))
	}

	return opts, nil
}

func (a *App) tunServerAddTargetHandler(tt tunnel.Target) error {
	a.Logger.Printf("tunnel server discovered target %+v", tt)
	a.ttm.Lock()
	a.tunTargets[tt.ID] = tt
	a.ttm.Unlock()
	return nil
}

func (a *App) tunServerAddTargetSubscribeHandler(tt tunnel.Target) error {
	a.tunServerAddTargetHandler(tt)
	if len(a.Config.TunnelServer.Targets) == 0 {
		return nil
	}
	tc := a.getTunnelTargetMatch(tt)
	a.AddTargetConfig(tc)
	a.initTarget(tc.Name)
	a.targetsChan <- a.Targets[tc.Name]
	a.wg.Add(1)
	go a.subscribeStream(a.ctx, tc.Name)
	return nil
}

func (a *App) tunServerDeleteTargetHandler(tt tunnel.Target) error {
	a.Logger.Printf("tunnel server target %+v deregistered", tt)
	a.ttm.Lock()
	defer a.ttm.Unlock()
	if cfn, ok := a.tunTargetCfn[tt.ID]; ok {
		cfn()
		delete(a.tunTargetCfn, tt.ID)
		delete(a.tunTargets, tt.ID)
		a.configLock.Lock()
		delete(a.Config.Targets, tt.ID)
		a.configLock.Unlock()
	}
	return nil
}

func (a *App) tunServerRegisterHandler(ss tunnel.ServerSession) error {
	return nil
}

func (a *App) tunServerHandler(ss tunnel.ServerSession, rwc io.ReadWriteCloser) error {
	return nil
}

// tunDialerFn is used to build a grpc Option that sets a custom dialer for tunnel targets.
func (a *App) tunDialerFn(ctx context.Context, tName string) func(context.Context, string) (net.Conn, error) {
	return func(_ context.Context, _ string) (net.Conn, error) {
		a.ttm.RLock()
		tt, ok := a.tunTargets[tName]
		a.ttm.RUnlock()
		if !ok {
			return nil, fmt.Errorf("unknown tunnel target %q", tName)
		}
		a.Logger.Printf("dialing tunnel connection for tunnel target %q", tName)
		conn, err := tunnel.ServerConn(ctx, a.tunServer, &tt)
		if err != nil {
			a.Logger.Printf("failed dialing tunnel connection for target %q: %v", tName, err)
		}
		return conn, err
	}
}

func (a *App) getTunnelTargetMatch(tt tunnel.Target) *types.TargetConfig {
	for _, tm := range a.Config.TunnelServer.Targets {
		// check if the discovered target matches one of the configured types
		ok, err := regexp.MatchString(tm.Type, tt.Type)
		if err != nil {
			a.Logger.Printf("regex %q eval failed with string %q: %v", tm.Type, tt.Type, err)
			continue
		}
		if !ok {
			continue
		}
		// check if the discovered target matches one of the configured IDs
		ok, err = regexp.MatchString(tm.Match, tt.ID)
		if err != nil {
			a.Logger.Printf("regex %q eval failed with string %q: %v", tm.Match, tt.ID, err)
			continue
		}
		if !ok {
			continue
		}
		// target has a match
		if a.Config.Debug {
			a.Logger.Printf("target %+v matches %+v", tt, tm)
		}
		tc := new(types.TargetConfig)
		*tc = tm.Config
		tc.Name = tt.ID
		err = a.Config.SetTargetConfigDefaults(tc)
		if err != nil {
			a.Logger.Printf("failed to set target %q config defaults: %v", tt.ID, err)
			continue
		}
		tc.Address = tc.Name
		return tc
	}
	// no matchs defined or target didn't match any of the configured ones.
	// create a default target config
	tc := &types.TargetConfig{Name: tt.ID}
	err := a.Config.SetTargetConfigDefaults(tc)
	if err != nil {
		a.Logger.Printf("failed to set target %q config defaults: %v", tt.ID, err)
		return nil
	}
	tc.Address = tc.Name
	return tc
}
