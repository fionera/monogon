// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"

	common "source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/cluster"
	"source.monogon.dev/metropolis/node/core/curator"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/localstorage/declarative"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/node/core/roleserve"
	timesvc "source.monogon.dev/metropolis/node/core/time"
	"source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/metropolis/pkg/logtree"
	"source.monogon.dev/metropolis/pkg/supervisor"
	"source.monogon.dev/metropolis/pkg/tpm"
	apb "source.monogon.dev/metropolis/proto/api"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Init panicked:", r)
			debug.PrintStack()
		}
		unix.Sync()
		// TODO(lorenz): Switch this to Reboot when init panics are less likely
		// Best effort, nothing we can do if this fails except printing the
		// error to the console.
		if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
			panic(fmt.Sprintf("failed to halt node: %v\n", err))
		}
	}()

	// Set up logger for Metropolis. Currently logs everything to stderr.
	lt := logtree.New()
	reader, err := lt.Read("", logtree.WithChildren(), logtree.WithStream())
	if err != nil {
		panic(fmt.Errorf("could not set up root log reader: %v", err))
	}
	go func() {
		for {
			p := <-reader.Stream
			fmt.Fprintf(os.Stderr, "%s\n", p.String())
		}
	}()

	// Initial logger. Used until we get to a supervisor.
	logger := lt.MustLeveledFor("init")

	// Set up basic mounts
	err = setupMounts(logger)
	if err != nil {
		panic(fmt.Errorf("could not set up basic mounts: %w", err))
	}

	// Linux kernel default is 4096 which is far too low. Raise it to 1M which
	// is what gVisor suggests.
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 1048576, Max: 1048576}); err != nil {
		logger.Fatalf("Failed to raise rlimits: %v", err)
	}

	logger.Info("Starting Metropolis node init")

	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel)

	if err := tpm.Initialize(logger); err != nil {
		logger.Fatalf("Failed to initialize TPM 2.0: %v", err)
	}

	networkSvc := network.New()
	timeSvc := timesvc.New()

	// This function initializes a headless Delve if this is a debug build or
	// does nothing if it's not
	initializeDebugger(networkSvc)

	// Prepare local storage.
	root := &localstorage.Root{}
	if err := declarative.PlaceFS(root, "/"); err != nil {
		panic(fmt.Errorf("when placing root FS: %w", err))
	}

	// trapdoor is a channel used to signal to the init service that a very
	// low-level, unrecoverable failure occured. This causes a GURU MEDITATION
	// ERROR visible to the end user.
	trapdoor := make(chan struct{})

	// Make context for supervisor. We cancel it when we reach the trapdoor.
	ctxS, ctxC := context.WithCancel(context.Background())

	// Start root initialization code as a supervisor one-shot runnable. This
	// means waiting for the network, starting the cluster manager, and then
	// starting all services related to the node's roles.
	// TODO(q3k): move this to a separate 'init' service.
	supervisor.New(ctxS, func(ctx context.Context) error {
		// Start storage and network - we need this to get anything else done.
		if err := root.Start(ctx); err != nil {
			return fmt.Errorf("cannot start root FS: %w", err)
		}
		if err := supervisor.Run(ctx, "network", networkSvc.Run); err != nil {
			return fmt.Errorf("when starting network: %w", err)
		}
		if err := supervisor.Run(ctx, "time", timeSvc.Run); err != nil {
			return fmt.Errorf("when starting time: %w", err)
		}

		// Start cluster manager. This kicks off cluster membership machinery,
		// which will either start a new cluster, enroll into one or join one.
		m := cluster.NewManager(root, networkSvc)
		if err := supervisor.Run(ctx, "enrolment", m.Run); err != nil {
			return fmt.Errorf("when starting enrolment: %w", err)
		}

		// Wait until the node finds a home in the new cluster.
		watcher := m.Watch()
		status, err := watcher.GetHome(ctx)
		if err != nil {
			close(trapdoor)
			return fmt.Errorf("new couldn't find home in new cluster, aborting: %w", err)
		}

		// Here starts some hairy stopgap code. In the future, not all nodes will have
		// direct access to etcd (ie. the ability to retrieve an etcd client via
		// status.ConsensusClient).
		// However, we are not ready to implement this yet, as that would require
		// moving more logic into the curator (eg. some of the Kubernetes PKI logic).
		//
		// For now, we keep Kubernetes PKI initialization logic here, and just assume
		// that every node will have direct access to etcd.

		// Retrieve namespaced etcd KV clients for the two main direct etcd users:
		// - Curator
		// - Kubernetes PKI
		ckv, err := status.ConsensusClient(cluster.ConsensusUserCurator)
		if err != nil {
			close(trapdoor)
			return fmt.Errorf("failed to retrieve consensus curator client: %w", err)
		}
		kkv, err := status.ConsensusClient(cluster.ConsensusUserKubernetesPKI)
		if err != nil {
			close(trapdoor)
			return fmt.Errorf("failed to retrieve consensus kubernetes PKI client: %w", err)
		}

		// TODO(q3k): restart curator on credentials change?

		// Start cluster curator. The cluster curator is responsible for lifecycle
		// management of the cluster.
		// In the future, this will only be started on nodes that run etcd.
		c := curator.New(curator.Config{
			Etcd:            ckv,
			NodeCredentials: status.Credentials,
			// TODO(q3k): make this configurable?
			LeaderTTL: time.Second * 5,
			Directory: &root.Ephemeral.Curator,
		})
		if err := supervisor.Run(ctx, "curator", c.Run); err != nil {
			close(trapdoor)
			return fmt.Errorf("when starting curator: %w", err)
		}

		// We are now in a cluster. We can thus access our 'node' object and
		// start all services that we should be running.
		logger.Info("Enrolment success, continuing startup.")

		// Ensure Kubernetes PKI objects exist in etcd. In the future, this logic will
		// be implemented in the curator.
		kpki := pki.New(lt.MustLeveledFor("pki.kubernetes"), kkv)
		if err := kpki.EnsureAll(ctx); err != nil {
			close(trapdoor)
			return fmt.Errorf("failed to ensure kubernetes PKI present: %w", err)
		}

		// Start the role service. The role service connects to the curator and runs
		// all node-specific role code (eg. Kubernetes services).
		//   supervisor.Logger(ctx).Infof("Starting role service...")
		rs := roleserve.New(roleserve.Config{
			CuratorDial: c.DialCluster,
			StorageRoot: root,
			Network:     networkSvc,
			KPKI:        kpki,
			NodeID:      status.Credentials.ID(),
		})
		if err := supervisor.Run(ctx, "role", rs.Run); err != nil {
			close(trapdoor)
			return fmt.Errorf("failed to start role service: %w", err)
		}

		// Start the node debug service.
		supervisor.Logger(ctx).Infof("Starting debug service...")
		dbg := &debugService{
			roleserve:       rs,
			logtree:         lt,
			traceLock:       make(chan struct{}, 1),
			ephemeralVolume: &root.Ephemeral.Containerd,
		}
		dbgSrv := grpc.NewServer()
		apb.RegisterNodeDebugServiceServer(dbgSrv, dbg)
		dbgLis, err := net.Listen("tcp", fmt.Sprintf(":%d", common.DebugServicePort))
		if err != nil {
			return fmt.Errorf("failed to listen on debug service: %w", err)
		}
		if err := supervisor.Run(ctx, "debug", supervisor.GRPCServer(dbgSrv, dbgLis, false)); err != nil {
			return fmt.Errorf("failed to start debug service: %w", err)
		}

		supervisor.Signal(ctx, supervisor.SignalHealthy)
		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	}, supervisor.WithExistingLogtree(lt))

	// We're PID1, so orphaned processes get reparented to us to clean up
	for {
		select {
		case <-trapdoor:
			// If the trapdoor got closed, we got stuck early enough in the
			// boot process that we can't do anything about it. Display a
			// generic error message until we handle error conditions better.
			ctxC()
			log.Printf("                  ########################")
			log.Printf("                  # GURU MEDIATION ERROR #")
			log.Printf("                  ########################")
			log.Printf("")
			log.Printf("Metropolis encountered an uncorrectable error and this node must be")
			log.Printf("restarted.")
			log.Printf("")
			log.Printf("(Error condition: init trapdoor closed)")
			log.Printf("")
			select {}

		case sig := <-signalChannel:
			switch sig {
			case unix.SIGCHLD:
				var status unix.WaitStatus
				var rusage unix.Rusage
				for {
					res, err := unix.Wait4(-1, &status, unix.WNOHANG, &rusage)
					if err != nil && err != unix.ECHILD {
						logger.Errorf("Failed to wait on orphaned child: %v", err)
						break
					}
					if res <= 0 {
						break
					}
				}
			case unix.SIGURG:
				// Go 1.14 introduced asynchronous preemption, which uses
				// SIGURG.
				// In order not to break backwards compatibility in the
				// unlikely case of an application actually using SIGURG on its
				// own, they're not filtering them.
				// (https://github.com/golang/go/issues/37942)
				logger.V(5).Info("Ignoring SIGURG")
			// TODO(lorenz): We can probably get more than just SIGCHLD as init, but I can't think
			// of any others right now, just log them in case we hit any of them.
			default:
				logger.Warningf("Got unexpected signal %s", sig.String())
			}
		}
	}
}

// nodeCertificate creates a node key/certificate for a foreign node. This is
// duplicated code with localstorage's PKIDirectory EnsureSelfSigned, but is
// temporary (and specific to 'golden tickets').
func (s *debugService) nodeCertificate() (cert, key []byte, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		err = fmt.Errorf("failed to generate key: %w", err)
		return
	}

	key, err = x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		err = fmt.Errorf("failed to marshal key: %w", err)
		return
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 127)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		err = fmt.Errorf("failed to generate serial number: %w", err)
		return
	}

	template := localstorage.CertificateForNode(pubKey)
	template.SerialNumber = serialNumber

	cert, err = x509.CreateCertificate(rand.Reader, &template, &template, pubKey, privKey)
	if err != nil {
		err = fmt.Errorf("could not sign certificate: %w", err)
		return
	}
	return
}
