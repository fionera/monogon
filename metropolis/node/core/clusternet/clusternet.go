// Package clusternet implements a Cluster Networking mesh service running on all
// Metropolis nodes.
//
// The mesh is based on wireguard and a centralized configuration store in the
// cluster Curator (in etcd).
//
// While the implementation is nearly generic, it currently makes an assumption
// that it is used only for Kubernetes pod networking. That has a few
// implications:
//
// First, we only have a single real route on the host into the wireguard
// networking mesh / interface, and that is configured ahead of time in the
// Service as ClusterNet. All destination addresses that should be carried by the
// mesh must thus be part of this single route. Otherwise, traffic will be able
// to flow into the node from other nodes, but will exit through another
// interface. This is used in practice to allow other host nodes (whose external
// addresses are outside the cluster network) to access the cluster network.
//
// Second, we only have a single source/owner of prefixes per node: the
// Kubernetes service. This is reflected as the LocalKubernetesPodNetwork event
// Value in Service.
package clusternet

import (
	"context"
	"fmt"
	"net"

	"github.com/cenkalti/backoff/v4"

	apb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/pkg/event"
	"source.monogon.dev/metropolis/pkg/supervisor"
	cpb "source.monogon.dev/metropolis/proto/common"
)

// Service implements the Cluster Networking Mesh. See package-level docs for
// more details.
type Service struct {
	// Curator is the gRPC client that the service will use to reach the cluster's
	// Curator, for pushing locally announced prefixes and pulling information about
	// other nodes.
	Curator apb.CuratorClient
	// ClusterNet is the prefix that will be programmed to exit through the wireguard
	// mesh.
	ClusterNet net.IPNet
	// DataDirectory is where the WireGuard key of this node will be stored.
	DataDirectory *localstorage.DataKubernetesClusterNetworkingDirectory
	// LocalKubernetesPodNetwork is an event.Value watched for prefixes that should
	// be announced into the mesh. This is to be Set by the Kubernetes service once
	// it knows about the local node's IPAM address assignment.
	LocalKubernetesPodNetwork event.Value[*Prefixes]

	// wg is the interface to all the low-level interactions with WireGuard (and
	// kernel routing). If not set, this defaults to a production implementation.
	// This can be overridden by test to a test implementation instead.
	wg wireguard
}

// Run the Service. This must be used in a supervisor Runnable.
func (s *Service) Run(ctx context.Context) error {
	if s.wg == nil {
		s.wg = &localWireguard{}
	}
	if err := s.wg.ensureOnDiskKey(s.DataDirectory); err != nil {
		return fmt.Errorf("could not ensure wireguard key: %w", err)
	}
	if err := s.wg.setup(&s.ClusterNet); err != nil {
		return fmt.Errorf("could not setup wireguard: %w", err)
	}

	supervisor.Logger(ctx).Infof("Wireguard setup complete, starting updaters...")

	if err := supervisor.Run(ctx, "pusher", s.push); err != nil {
		return err
	}
	if err := supervisor.Run(ctx, "puller", s.pull); err != nil {
		return err
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)
	<-ctx.Done()
	return ctx.Err()
}

// push is the sub-runnable responsible for letting the Curator know about what
// prefixes that are originated by this node.
func (s *Service) push(ctx context.Context) error {
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	w := s.LocalKubernetesPodNetwork.Watch()
	defer w.Close()

	for {
		// We only submit our wireguard key and prefixes when we're actually ready to
		// announce something.
		k8sPrefixes, err := w.Get(ctx)
		if err != nil {
			return fmt.Errorf("couldn't get k8s prefixes: %w", err)
		}

		err = backoff.Retry(func() error {
			_, err := s.Curator.UpdateNodeClusterNetworking(ctx, &apb.UpdateNodeClusterNetworkingRequest{
				Clusternet: &cpb.NodeClusterNetworking{
					WireguardPubkey: s.wg.key().PublicKey().String(),
					Prefixes:        k8sPrefixes.proto(),
				},
			})
			if err != nil {
				supervisor.Logger(ctx).Warningf("Could not submit cluster networking update: %v", err)
			}
			return err
		}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
		if err != nil {
			return fmt.Errorf("couldn't update curator: %w", err)
		}
	}
}

// pull is the sub-runnable responsible for fetching information about the
// cluster networking setup/status of other nodes, and programming it as
// WireGuard peers.
func (s *Service) pull(ctx context.Context) error {
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	srv, err := s.Curator.Watch(ctx, &apb.WatchRequest{
		Kind: &apb.WatchRequest_NodesInCluster_{
			NodesInCluster: &apb.WatchRequest_NodesInCluster{},
		},
	})
	if err != nil {
		return fmt.Errorf("curator watch failed: %w", err)
	}
	defer srv.CloseSend()

	nodes := newNodemap()
	for {
		ev, err := srv.Recv()
		if err != nil {
			return fmt.Errorf("curator watch recv failed: %w", err)
		}

		updated, removed := nodes.update(ctx, ev)

		for _, n := range removed {
			supervisor.Logger(ctx).Infof("Node %s removed, unconfiguring", n.id)
			if err := s.wg.unconfigurePeer(n); err != nil {
				// Do nothing and hope whatever caused this will go away at some point.
				supervisor.Logger(ctx).Errorf("Node %s couldn't be unconfigured: %v", n.id, err)
			}
		}
		var newNodes []*node
		for _, n := range updated {
			newNodes = append(newNodes, n)
			supervisor.Logger(ctx).Infof("Node %s updated: pk %s, address %s, prefixes %v", n.id, n.pubkey, n.address, n.prefixes)
		}
		succeeded := 0
		if err := s.wg.configurePeers(newNodes); err != nil {
			// If configuring all nodes at once failed, go node-by-node to make sure we've
			// done as much as possible.
			supervisor.Logger(ctx).Warningf("Bulk node update call failed, trying node-by-node..: %v", err)
			for _, n := range newNodes {
				if err := s.wg.configurePeers([]*node{n}); err != nil {
					supervisor.Logger(ctx).Errorf("Node %s failed: %v", n.id, err)
				} else {
					succeeded += 1
				}
			}
		} else {
			succeeded = len(newNodes)
		}
		supervisor.Logger(ctx).Infof("Successfully updated %d out of %d nodes", succeeded, len(newNodes))
	}
}
