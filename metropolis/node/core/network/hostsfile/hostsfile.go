// Package hostsfile implements a service which owns and writes all node-local
// files/interfaces used by the system to resolve the local node's name and the
// names of other nodes in the cluster:
//
//  1. All cluster node names are written into /etc/hosts for DNS resolution.
//  2. The local node's name is written into /etc/machine-id.
//  3. The local node's name is set as the UNIX hostname of the machine (via the
//     sethostname call).
//  4. The local node's ClusterDirectory is updated with the same set of
//     addresses as the one used in /etc/hosts.
//
// The hostsfile Service can start up in two modes: with cluster connectivity
// and without cluster connectivity. Without cluster connectivity, only
// information about the current node (as retrieved from the network service)
// will be used to populate local data. In cluster mode, information about other
// nodes is also used.
package hostsfile

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sort"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/pkg/event"
	"source.monogon.dev/metropolis/pkg/supervisor"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
)

type Config struct {
	// Network is a handle to the Network service, used to update the hostsfile
	// service with information about the local node's external IP address.
	Network *network.Service
	// Ephemeral is the root of the ephemeral storage of the node, into which the
	// service will write its managed files.
	Ephemeral *localstorage.EphemeralDirectory
	// ESP is the root of the node's EFI System Partition.
	ESP *localstorage.ESPDirectory
	// NodeID of the node the service is running on.
	NodeID string
	// Curator gRPC client authenticated as local node.
	Curator ipb.CuratorClient
	// ClusterDirectorySaved will be written with a boolean indicating whether the
	// ClusterDirectory has been successfully persisted to the ESP.
	ClusterDirectorySaved event.Value[bool]
}

// Service is the hostsfile service instance. See package-level documentation
// for more information.
type Service struct {
	Config

	// clusterC is a channel populated by the cluster sub-runnable with the newest
	// available information about the cluster nodes. It is automatically created and
	// closed by Run.
	clusterC chan nodeMap
}

type ClusterDialer func(ctx context.Context) (*grpc.ClientConn, error)

// nodeInfo contains all of a single node's data needed to build its entry in
// either hostsfile or ClusterDirectory.
type nodeInfo struct {
	// address is the node's IP address.
	address string
	// local is true if address belongs to the local node.
	local bool
}

// nodeMap is a map from node ID (effectively DNS name) to node IP address.
type nodeMap map[string]nodeInfo

// hosts generates a complete /etc/hosts file based on the contents of the
// nodeMap. Apart from the addresses in the nodeMap, entries for localhost
// pointing to 127.0.0.1 and ::1 will also be generated.
func (m nodeMap) hosts(ctx context.Context) []byte {
	var nodeIdsSorted []string
	for k, _ := range m {
		nodeIdsSorted = append(nodeIdsSorted, k)
	}
	sort.Slice(nodeIdsSorted, func(i, j int) bool {
		return nodeIdsSorted[i] < nodeIdsSorted[j]
	})

	lines := [][]byte{
		[]byte("127.0.0.1 localhost"),
		[]byte("::1 localhost"),
	}
	for _, nid := range nodeIdsSorted {
		addr := m[nid].address
		line := fmt.Sprintf("%s %s", addr, nid)
		supervisor.Logger(ctx).Infof("Hosts entry: %s", line)
		lines = append(lines, []byte(line))
	}
	lines = append(lines, []byte(""))

	return bytes.Join(lines, []byte("\n"))
}

// clusterDirectory builds a ClusterDirectory based on nodeMap contents. If m
// is empty, an empty ClusterDirectory is returned.
func (m nodeMap) clusterDirectory(ctx context.Context) *cpb.ClusterDirectory {
	var directory cpb.ClusterDirectory
	for _, ni := range m {
		// Skip local addresses.
		if ni.local {
			continue
		}

		supervisor.Logger(ctx).Infof("ClusterDirectory entry: %s", ni.address)
		addresses := []*cpb.ClusterDirectory_Node_Address{
			{Host: ni.address},
		}
		node := &cpb.ClusterDirectory_Node{
			Addresses: addresses,
		}
		directory.Nodes = append(directory.Nodes, node)
	}
	return &directory
}

func (s *Service) Run(ctx context.Context) error {
	s.ClusterDirectorySaved.Set(false)

	localC := make(chan *network.Status)
	s.clusterC = make(chan nodeMap)

	if err := supervisor.Run(ctx, "local", event.Pipe(s.Network.Value(), localC)); err != nil {
		return err
	}
	if err := supervisor.Run(ctx, "cluster", s.runCluster); err != nil {
		return err
	}

	// Immediately update machine-id and hostname, we don't need network addresses
	// for that.
	if err := s.Ephemeral.MachineID.Write([]byte(s.NodeID), 0644); err != nil {
		return fmt.Errorf("failed to write /ephemeral/machine-id: %w", err)
	}
	if err := unix.Sethostname([]byte(s.NodeID)); err != nil {
		return fmt.Errorf("failed to set runtime hostname: %w", err)
	}
	// Immediately write an /etc/hosts just containing localhost, even if we don't
	// yet have a network address.
	nodes := make(nodeMap)
	if err := s.Ephemeral.Hosts.Write(nodes.hosts(ctx), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", s.Ephemeral.Hosts.FullPath(), err)
	}

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	// Update nodeMap in a loop, issuing writes/updates when any change occurred.
	for {
		changed := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case st := <-localC:
			// Ignore spurious updates.
			if st.ExternalAddress == nil {
				continue
			}
			u := st.ExternalAddress.String()
			if nodes[s.NodeID].address == u {
				continue
			}
			supervisor.Logger(ctx).Infof("Got new local address: %s", u)
			nodes[s.NodeID] = nodeInfo{
				address: u,
				local:   true,
			}
			changed = true
		case u := <-s.clusterC:
			// Loop through the nodeMap from the cluster subrunnable, making note of what
			// changed. By design we don't care about any nodes disappearing from the
			// nodeMap: we'd rather keep stale data about nodes that don't exist any more,
			// as these might either be spurious or have a long tail of effectively still
			// being used by the local node for communications while the node gets fully
			// drained/disowned.
			//
			// MVP: we should at least log removed nodes.
			for id, info := range u {
				// We're not interested in what the cluster thinks about our local node, as that
				// might be outdated (eg. when we haven't yet reported a new local address to
				// the cluster).
				if id == s.NodeID {
					continue
				}
				if nodes[id].address == info.address {
					continue
				}
				supervisor.Logger(ctx).Infof("Got new cluster address: %s is %s", id, info.address)
				nodes[id] = info
				changed = true
			}
		}

		if !changed {
			continue
		}

		supervisor.Logger(ctx).Infof("Updating hosts file: %d nodes", len(nodes))
		if err := s.Ephemeral.Hosts.Write(nodes.hosts(ctx), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", s.Ephemeral.Hosts.FullPath(), err)
		}

		// Check that we are self-resolvable.
		if _, err := net.ResolveIPAddr("ip", s.NodeID); err != nil {
			supervisor.Logger(ctx).Errorf("Failed to self-resolve %q: %v", s.NodeID, err)
		}

		// Update this node's ClusterDirectory.
		supervisor.Logger(ctx).Info("Updating ClusterDirectory.")
		cd := nodes.clusterDirectory(ctx)
		cdirRaw, err := proto.Marshal(cd)
		if err != nil {
			return fmt.Errorf("couldn't marshal ClusterDirectory: %w", err)
		}
		if err = s.ESP.Metropolis.ClusterDirectory.Write(cdirRaw, 0644); err != nil {
			return err
		}
		unix.Sync()
		s.ClusterDirectorySaved.Set(true)
	}
}

// runCluster updates s.clusterC with the IP addresses of cluster nodes, as
// retrieved from a Curator client from the ClusterDialer. The returned map
// reflects the up-to-date view of the cluster returned from the Curator Watch
// call, including any node deletions.
func (s *Service) runCluster(ctx context.Context) error {
	w, err := s.Curator.Watch(ctx, &ipb.WatchRequest{
		Kind: &ipb.WatchRequest_NodesInCluster_{
			NodesInCluster: &ipb.WatchRequest_NodesInCluster{},
		},
	})
	if err != nil {
		return fmt.Errorf("curator watch failed: %w", err)
	}

	nodes := make(nodeMap)
	for {
		ev, err := w.Recv()
		if err != nil {
			return fmt.Errorf("receive failed: %w", err)
		}
		for _, n := range ev.Nodes {
			if n.Status == nil || n.Status.ExternalAddress == "" {
				continue
			}
			nodes[n.Id] = nodeInfo{
				address: n.Status.ExternalAddress,
				local:   false,
			}
		}
		for _, t := range ev.NodeTombstones {
			delete(nodes, t.NodeId)
		}

		// Copy nodemap before passing it over to the main goroutine. The values don't
		// need to be deep copied as they're not ever changed (only inserted).
		nodesCopy := make(nodeMap)
		for k, v := range nodes {
			nodesCopy[k] = v
		}
		s.clusterC <- nodesCopy
	}
}
