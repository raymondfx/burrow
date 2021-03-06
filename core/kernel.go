// Copyright 2017 Monax Industries Limited
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	kitlog "github.com/go-kit/kit/log"
	bcm "github.com/hyperledger/burrow/blockchain"
	"github.com/hyperledger/burrow/consensus/tendermint"
	"github.com/hyperledger/burrow/consensus/tendermint/query"
	"github.com/hyperledger/burrow/event"
	"github.com/hyperledger/burrow/execution"
	"github.com/hyperledger/burrow/execution/events/pbevents"
	"github.com/hyperledger/burrow/execution/pbtransactor"
	"github.com/hyperledger/burrow/genesis"
	"github.com/hyperledger/burrow/keys"
	"github.com/hyperledger/burrow/keys/pbkeys"
	"github.com/hyperledger/burrow/logging"
	"github.com/hyperledger/burrow/logging/structure"
	"github.com/hyperledger/burrow/process"
	"github.com/hyperledger/burrow/rpc"
	"github.com/hyperledger/burrow/rpc/metrics"
	"github.com/hyperledger/burrow/rpc/rpcevents"
	"github.com/hyperledger/burrow/rpc/rpctransactor"
	"github.com/hyperledger/burrow/rpc/tm"
	"github.com/hyperledger/burrow/rpc/v0"
	v0_server "github.com/hyperledger/burrow/rpc/v0/server"
	"github.com/hyperledger/burrow/txs"
	tm_config "github.com/tendermint/tendermint/config"
	tm_types "github.com/tendermint/tendermint/types"
	dbm "github.com/tendermint/tmlibs/db"
	"google.golang.org/grpc/reflection"
)

const (
	CooldownMilliseconds              = 1000
	ServerShutdownTimeoutMilliseconds = 1000
	LoggingCallerDepth                = 5
)

// Kernel is the root structure of Burrow
type Kernel struct {
	// Expose these public-facing interfaces to allow programmatic extension of the Kernel by other projects
	Emitter        event.Emitter
	Service        *rpc.Service
	Launchers      []process.Launcher
	State          *execution.State
	Blockchain     bcm.BlockchainInfo
	Logger         *logging.Logger
	processes      map[string]process.Process
	shutdownNotify chan struct{}
	shutdownOnce   sync.Once
}

func NewKernel(ctx context.Context, keyClient keys.KeyClient, privValidator tm_types.PrivValidator,
	genesisDoc *genesis.GenesisDoc, tmConf *tm_config.Config, rpcConfig *rpc.RPCConfig, keyConfig *keys.KeysConfig,
	keyStore *keys.KeyStore, exeOptions []execution.ExecutionOption, logger *logging.Logger) (*Kernel, error) {

	logger = logger.WithScope("NewKernel()").With(structure.TimeKey, kitlog.DefaultTimestampUTC)
	tmLogger := logger.With(structure.CallerKey, kitlog.Caller(LoggingCallerDepth+1))
	logger = logger.WithInfo(structure.CallerKey, kitlog.Caller(LoggingCallerDepth))
	stateDB := dbm.NewDB("burrow_state", dbm.GoLevelDBBackend, tmConf.DBDir())

	blockchain, err := bcm.LoadOrNewBlockchain(stateDB, genesisDoc, logger)
	if err != nil {
		return nil, fmt.Errorf("error creating or loading blockchain state: %v", err)
	}

	var state *execution.State
	// These should be in sync unless we are at the genesis block
	if blockchain.LastBlockHeight() > 0 {
		state, err = execution.LoadState(stateDB, blockchain.AppHashAfterLastBlock())
		if err != nil {
			return nil, fmt.Errorf("could not load persisted execution state at hash 0x%X: %v",
				blockchain.AppHashAfterLastBlock(), err)
		}
	} else {
		state, err = execution.MakeGenesisState(stateDB, genesisDoc)
	}

	txCodec := txs.NewAminoCodec()
	tmGenesisDoc := tendermint.DeriveGenesisDoc(genesisDoc)
	checker := execution.NewBatchChecker(state, blockchain.Tip, logger)

	emitter := event.NewEmitter(logger)
	committer := execution.NewBatchCommitter(state, blockchain.Tip, emitter, logger, exeOptions...)
	tmNode, err := tendermint.NewNode(tmConf, privValidator, tmGenesisDoc, blockchain, checker, committer, txCodec,
		tmLogger)
	if err != nil {
		return nil, err
	}
	transactor := execution.NewTransactor(blockchain.Tip, emitter, tmNode.MempoolReactor().BroadcastTx, txCodec,
		logger)

	nameRegState := state
	accountState := state
	service := rpc.NewService(ctx, accountState, nameRegState, checker, emitter, blockchain, keyClient, transactor,
		query.NewNodeView(tmNode, txCodec), logger)

	launchers := []process.Launcher{
		{
			Name:    "Profiling Server",
			Enabled: rpcConfig.Profiler.Enabled,
			Launch: func() (process.Process, error) {
				debugServer := &http.Server{
					Addr: ":6060",
				}
				go func() {
					err := debugServer.ListenAndServe()
					if err != nil {
						logger.InfoMsg("Error from pprof debug server", structure.ErrorKey, err)
					}
				}()
				return debugServer, nil
			},
		},
		{
			Name:    "Database",
			Enabled: true,
			Launch: func() (process.Process, error) {
				// Just close database
				return process.ShutdownFunc(func(ctx context.Context) error {
					stateDB.Close()
					return nil
				}), nil
			},
		},
		{
			Name:    "Tendermint",
			Enabled: true,
			Launch: func() (process.Process, error) {
				err := tmNode.Start()
				if err != nil {
					return nil, fmt.Errorf("error starting Tendermint node: %v", err)
				}
				subscriber := fmt.Sprintf("TendermintFireHose-%s-%s", genesisDoc.ChainName, genesisDoc.ChainID())
				// Multiplex Tendermint and EVM events

				err = tendermint.PublishAllEvents(ctx, tendermint.EventBusAsSubscribable(tmNode.EventBus()), subscriber,
					emitter)
				if err != nil {
					return nil, fmt.Errorf("could not subscribe to Tendermint events: %v", err)
				}
				return process.ShutdownFunc(func(ctx context.Context) error {
					err := tmNode.Stop()
					// Close tendermint database connections using our wrapper
					defer tmNode.Close()
					if err != nil {
						return err
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tmNode.Quit():
						logger.InfoMsg("Tendermint Node has quit, closing DB connections...")
						return nil
					}
					return err
				}), nil
			},
		},
		{
			Name:    "RPC/tm",
			Enabled: rpcConfig.TM.Enabled,
			Launch: func() (process.Process, error) {
				server, err := tm.StartServer(service, "/websocket", rpcConfig.TM.ListenAddress, emitter, logger)
				if err != nil {
					return nil, err
				}
				return server, nil
			},
		},
		{
			Name:    "RPC/metrics",
			Enabled: rpcConfig.Metrics.Enabled,
			Launch: func() (process.Process, error) {
				server, err := metrics.StartServer(service, rpcConfig.Metrics.MetricsPath, rpcConfig.Metrics.ListenAddress, rpcConfig.Metrics.BlockSampleSize, logger)
				if err != nil {
					return nil, err
				}
				return server, nil
			},
		},
		{
			Name:    "RPC/V0",
			Enabled: rpcConfig.V0.Enabled,
			Launch: func() (process.Process, error) {
				codec := v0.NewTCodec()
				jsonServer := v0.NewJSONServer(v0.NewJSONService(codec, service, logger))
				websocketServer := v0_server.NewWebSocketServer(rpcConfig.V0.Server.WebSocket.MaxWebSocketSessions,
					v0.NewWebsocketService(codec, service, logger), logger)

				serveProcess, err := v0_server.NewServeProcess(rpcConfig.V0.Server, logger, jsonServer, websocketServer)
				if err != nil {
					return nil, err
				}
				err = serveProcess.Start()
				if err != nil {
					return nil, err
				}
				return serveProcess, nil
			},
		},
		{
			Name:    "RPC/GRPC",
			Enabled: rpcConfig.GRPC.Enabled,
			Launch: func() (process.Process, error) {
				listen, err := net.Listen("tcp", rpcConfig.GRPC.ListenAddress)
				if err != nil {
					return nil, err
				}

				grpcServer := rpc.NewGRPCServer(logger)
				var ks *keys.KeyStore
				if keyStore != nil {
					ks = keyStore
				}

				if keyConfig.GRPCServiceEnabled {
					if keyStore == nil {
						ks = keys.NewKeyStore(keyConfig.KeysDirectory, keyConfig.AllowBadFilePermissions, logger)
					}
					pbkeys.RegisterKeysServer(grpcServer, ks)
				}

				pbtransactor.RegisterTransactorServer(grpcServer, rpctransactor.NewTransactorServer(service.Transactor(),
					service.MempoolAccounts(), state, txCodec))

				pbevents.RegisterEventsServer(grpcServer, rpcevents.NewEventsServer(rpc.NewSubscriptions(service)))

				pbevents.RegisterExecutionEventsServer(grpcServer, rpcevents.NewExecutionEventsServer(state, emitter,
					blockchain.Tip))

				// Provides metadata about services registered
				reflection.Register(grpcServer)

				go grpcServer.Serve(listen)

				return process.ShutdownFunc(func(ctx context.Context) error {
					grpcServer.Stop()
					// listener is closed for us
					return nil
				}), nil
			},
		},
	}

	return &Kernel{
		Emitter:        emitter,
		Service:        service,
		Launchers:      launchers,
		State:          state,
		Blockchain:     blockchain,
		Logger:         logger,
		processes:      make(map[string]process.Process),
		shutdownNotify: make(chan struct{}),
	}, nil
}

// Boot the kernel starting Tendermint and RPC layers
func (kern *Kernel) Boot() error {
	for _, launcher := range kern.Launchers {
		if launcher.Enabled {
			srvr, err := launcher.Launch()
			if err != nil {
				return fmt.Errorf("error launching %s server: %v", launcher.Name, err)
			}

			kern.processes[launcher.Name] = srvr
		}
	}
	go kern.supervise()
	return nil
}

// Wait for a graceful shutdown
func (kern *Kernel) WaitForShutdown() {
	// Supports multiple goroutines waiting for shutdown since channel is closed
	<-kern.shutdownNotify
}

// Supervise kernel once booted
func (kern *Kernel) supervise() {
	// TODO: Consider capturing kernel panics from boot and sending them here via a channel where we could
	// perform disaster restarts of the kernel; rejoining the network as if we were a new node.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	sig := <-signals
	kern.Logger.InfoMsg(fmt.Sprintf("Caught %v signal so shutting down", sig),
		"signal", sig.String())
	kern.Shutdown(context.Background())
}

// Stop the kernel allowing for a graceful shutdown of components in order
func (kern *Kernel) Shutdown(ctx context.Context) (err error) {
	kern.shutdownOnce.Do(func() {
		logger := kern.Logger.WithScope("Shutdown")
		logger.InfoMsg("Attempting graceful shutdown...")
		logger.InfoMsg("Shutting down servers")
		ctx, cancel := context.WithTimeout(ctx, ServerShutdownTimeoutMilliseconds*time.Millisecond)
		defer cancel()
		// Shutdown servers in reverse order to boot
		for i := len(kern.Launchers) - 1; i >= 0; i-- {
			name := kern.Launchers[i].Name
			srvr, ok := kern.processes[name]
			if ok {
				logger.InfoMsg("Shutting down server", "server_name", name)
				sErr := srvr.Shutdown(ctx)
				if sErr != nil {
					logger.InfoMsg("Failed to shutdown server",
						"server_name", name,
						structure.ErrorKey, sErr)
					if err == nil {
						err = sErr
					}
				}
			}
		}
		logger.InfoMsg("Shutdown complete")
		structure.Sync(kern.Logger.Info)
		structure.Sync(kern.Logger.Trace)
		// We don't want to wait for them, but yielding for a cooldown Let other goroutines flush
		// potentially interesting final output (e.g. log messages)
		time.Sleep(time.Millisecond * CooldownMilliseconds)
		close(kern.shutdownNotify)
	})
	return
}
