package main

import (
	"context"
	"log"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"source.monogon.dev/metropolis/cli/metroctl/core"
	clicontext "source.monogon.dev/metropolis/cli/pkg/context"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/metropolis/node/core/rpc/resolver"
	apb "source.monogon.dev/metropolis/proto/api"
)

var takeownershipCommand = &cobra.Command{
	Use:   "takeownership",
	Short: "Takes ownership of a new Metropolis cluster",
	Long: `This takes ownership of a new Metropolis cluster by asking the new
cluster to issue an owner certificate to for the owner key generated by a
previous invocation of metroctl install on this machine. A single cluster
endpoint must be provided with the --endpoints parameter.`,
	Args: cobra.ExactArgs(0),
	Run:  doTakeOwnership,
}

func doTakeOwnership(cmd *cobra.Command, _ []string) {
	if len(flags.clusterEndpoints) != 1 {
		log.Fatalf("takeownership requires a single cluster endpoint to be provided with the --endpoints parameter.")
	}

	// Retrieve the cluster owner's private key, and use it to construct
	// ephemeral credentials. Then, dial the cluster.
	opk, err := core.GetOwnerKey(flags.configPath)
	if err == core.NoCredentialsError {
		log.Fatalf("Owner key does not exist. takeownership needs to be executed on the same system that has previously installed the cluster using metroctl install.")
	}
	if err != nil {
		log.Fatalf("Couldn't get owner's key: %v", err)
	}
	ctx := clicontext.WithInterrupt(context.Background())
	opts, err := core.DialOpts(ctx, connectOptions())
	if err != nil {
		log.Fatalf("While configuring cluster dial opts: %v", err)
	}
	creds, err := rpc.NewEphemeralCredentials(opk, rpc.WantInsecure())
	if err != nil {
		log.Fatalf("While generating ephemeral credentials: %v", err)
	}
	opts = append(opts, grpc.WithTransportCredentials(creds))

	cc, err := grpc.Dial(resolver.MetropolisControlAddress, opts...)
	if err != nil {
		log.Fatalf("While dialing the cluster: %v", err)
	}
	aaa := apb.NewAAAClient(cc)

	ownerCert, err := rpc.RetrieveOwnerCertificate(ctx, aaa, opk)
	if err != nil {
		log.Fatalf("Failed to retrive owner certificate from cluster: %v", err)
	}

	if err := core.WriteOwnerCertificate(flags.configPath, ownerCert.Certificate[0]); err != nil {
		log.Printf("Failed to store retrieved owner certificate: %v", err)
		log.Fatalln("Sorry, the cluster has been lost as taking ownership cannot be repeated. Fix the reason the file couldn't be written and reinstall the node.")
	}
	log.Print("Successfully retrieved owner credentials! You now own this cluster. Setting up kubeconfig now...")

	// If the user has metroctl in their path, use the metroctl from path as
	// a credential plugin. Otherwise use the path to the currently-running
	// metroctl.
	metroctlPath := "metroctl"
	if _, err := exec.LookPath("metroctl"); err != nil {
		metroctlPath, err = os.Executable()
		if err != nil {
			log.Fatalf("Failed to create kubectl entry as metroctl is neither in PATH nor can its absolute path be determined: %v", err)
		}
	}
	// TODO(q3k, issues/144): this only works as long as all nodes are kubernetes controller
	// nodes. This won't be the case for too long. Figure this out.
	configName := "metroctl"
	if err := core.InstallKubeletConfig(metroctlPath, connectOptions(), configName, flags.clusterEndpoints[0]); err != nil {
		log.Fatalf("Failed to install metroctl/k8s integration: %v", err)
	}
	log.Printf("Success! kubeconfig is set up. You can now run kubectl --context=%s ... to access the Kubernetes cluster.", configName)
}

func init() {
	rootCmd.AddCommand(takeownershipCommand)
}
