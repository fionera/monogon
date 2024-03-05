package roleserve

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/encoding/prototext"

	common "source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/pkg/event"
	"source.monogon.dev/metropolis/pkg/event/memory"
	"source.monogon.dev/metropolis/pkg/supervisor"
	"source.monogon.dev/metropolis/version"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

// workerStatusPush is the Status Pusher, a service responsible for sending
// UpdateNodeStatus RPCs to a cluster whenever a Curator is available.
type workerStatusPush struct {
	network *network.Service

	// localControlPlane will be read
	localControlPlane *memory.Value[*localControlPlane]
	// curatorConnection will be read.
	curatorConnection *memory.Value[*curatorConnection]
	// clusterDirectorySaved will be read.
	clusterDirectorySaved *memory.Value[bool]
}

// workerStatusPushChannels contain all the channels between the status pusher's
// 'map' runnables (waiting on Event Values) and the main loop.
type workerStatusPushChannels struct {
	// address of the node, or empty if none. Retrieved from network service.
	address           chan string
	localControlPlane chan *localControlPlane
	curatorConnection chan *curatorConnection
}

// workerStatusPushLoop runs the main loop acting on data received from
// workerStatusPushChannels.
func workerStatusPushLoop(ctx context.Context, chans *workerStatusPushChannels) error {
	status := cpb.NodeStatus{
		Version: version.Version,
	}
	var cur ipb.CuratorClient
	var nodeID string

	for {
		changed := false

		select {
		case <-ctx.Done():
			return fmt.Errorf("while waiting for map updates: %w", ctx.Err())

		case address := <-chans.address:
			if address != status.ExternalAddress {
				supervisor.Logger(ctx).Infof("Got external address: %s", address)
				status.ExternalAddress = address
				changed = true
			}

		case curCon := <-chans.curatorConnection:
			newNodeID := curCon.nodeID()
			if nodeID != newNodeID {
				supervisor.Logger(ctx).Infof("Got new NodeID: %s", newNodeID)
				nodeID = newNodeID
				changed = true
			}
			if cur == nil {
				cur = ipb.NewCuratorClient(curCon.conn)
				supervisor.Logger(ctx).Infof("Got curator connection.")
				changed = true
			}

		case lcp := <-chans.localControlPlane:
			if status.RunningCurator == nil && lcp.exists() {
				supervisor.Logger(ctx).Infof("Got new local curator state: running")
				status.RunningCurator = &cpb.NodeStatus_RunningCurator{
					Port: int32(common.CuratorServicePort),
				}
				changed = true
			}
			if status.RunningCurator != nil && !lcp.exists() {
				supervisor.Logger(ctx).Infof("Got new local curator state: not running")
				status.RunningCurator = nil
				changed = true
			}
		}

		if cur != nil && nodeID != "" && changed && status.ExternalAddress != "" {
			txt, _ := prototext.Marshal(&status)
			supervisor.Logger(ctx).Infof("Submitting status: %q", txt)
			_, err := cur.UpdateNodeStatus(ctx, &ipb.UpdateNodeStatusRequest{
				NodeId: nodeID,
				Status: &status,
			})
			if err != nil {
				return fmt.Errorf("UpdateNodeStatus failed: %w", err)
			}
		}
	}
}

func (s *workerStatusPush) run(ctx context.Context) error {
	chans := workerStatusPushChannels{
		address:           make(chan string),
		curatorConnection: make(chan *curatorConnection),
		localControlPlane: make(chan *localControlPlane),
	}

	// All the channel sends in the map runnables are preemptible by a context
	// cancelation. This is because workerStatusPushLoop can crash while processing
	// the events, requiring a restart of this runnable. Without the preemption this
	// runnable could get stuck.

	supervisor.Run(ctx, "map-network", func(ctx context.Context) error {
		nw := s.network.Status.Watch()
		defer nw.Close()

		supervisor.Signal(ctx, supervisor.SignalHealthy)
		for {
			st, err := nw.Get(ctx)
			if err != nil {
				return fmt.Errorf("getting network status failed: %w", err)
			}
			select {
			case chans.address <- st.ExternalAddress.String():
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	supervisor.Run(ctx, "pipe-local-control-plane", event.Pipe[*localControlPlane](s.localControlPlane, chans.localControlPlane))
	supervisor.Run(ctx, "pipe-curator-connection", event.Pipe[*curatorConnection](s.curatorConnection, chans.curatorConnection))

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	return workerStatusPushLoop(ctx, &chans)
}
