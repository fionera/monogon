package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"source.monogon.dev/metropolis/cli/metroctl/core"
	common "source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/proto/api"
)

var nodeMetricsCmd = &cobra.Command{
	Short: "Get metrics from node",
	Long: `Get metrics from node.

Node metrics are exported in the Prometheus format, and can be collected by any
number of metrics collection software compatible with said format.

This helper tool can be used to manually fetch metrics from a node using the same
credentials as used to manage the cluster, and is designed to be used as a
troubleshooting tool when a proper metrics collection system has not been set up
for the cluster.

A node ID and exporter must be provided. Currently available exporters are:

  - node: node_exporter metrics for the node
  - etcd: etcd metrics, if the node is running the cluster control plane
  - kubernetes-scheduler, kubernetes-controller-manager, kubernetes-apiserver:
    metrics for kubernetes control plane components, if the node runs the
    Kubernetes control plane
  - containerd: containerd metrics, if the node is a Kubernetes worker

`,
	Use:  "metrics [node-id] [exporter]",
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// First connect to the main management service and figure out the node's IP
		// address.
		cc := dialAuthenticated(ctx)
		mgmt := api.NewManagementClient(cc)
		nodes, err := core.GetNodes(ctx, mgmt, fmt.Sprintf("node.id == %q", args[0]))
		if err != nil {
			return fmt.Errorf("when getting node info: %w", err)
		}

		if len(nodes) == 0 {
			return fmt.Errorf("no such node")
		}
		if len(nodes) > 1 {
			return fmt.Errorf("expression matched more than one node")
		}
		n := nodes[0]
		if n.Status == nil || n.Status.ExternalAddress == "" {
			return fmt.Errorf("node has no external address")
		}

		client := http.Client{
			Transport: newAuthenticatedNodeHTTPTransport(ctx, n.Id),
		}
		res, err := client.Get(fmt.Sprintf("https://%s/metrics/%s", net.JoinHostPort(n.Status.ExternalAddress, common.MetricsPort.PortString()), args[1]))
		if err != nil {
			return fmt.Errorf("metrics HTTP request failed: %v", err)
		}
		defer res.Body.Close()
		_, err = io.Copy(os.Stdout, res.Body)
		return err
	},
}

func init() {
	nodeCmd.AddCommand(nodeMetricsCmd)
}
