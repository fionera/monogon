package kubernetes

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"source.monogon.dev/go/net/tinylb"
	"source.monogon.dev/metropolis/node"
	oclusternet "source.monogon.dev/metropolis/node/core/clusternet"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/metropolis/node/core/network"
	"source.monogon.dev/metropolis/node/core/network/dns"
	"source.monogon.dev/metropolis/node/kubernetes/clusternet"
	"source.monogon.dev/metropolis/node/kubernetes/nfproxy"
	kpki "source.monogon.dev/metropolis/node/kubernetes/pki"
	"source.monogon.dev/metropolis/node/kubernetes/plugins/kvmdevice"
	"source.monogon.dev/metropolis/pkg/event"
	"source.monogon.dev/metropolis/pkg/event/memory"
	"source.monogon.dev/metropolis/pkg/supervisor"

	ipb "source.monogon.dev/metropolis/node/core/curator/proto/api"
)

type ConfigWorker struct {
	ServiceIPRange net.IPNet
	ClusterNet     net.IPNet
	ClusterDomain  string

	Root          *localstorage.Root
	Network       *network.Service
	NodeID        string
	CuratorClient ipb.CuratorClient
	PodNetwork    event.Value[*oclusternet.Prefixes]
}

type Worker struct {
	c ConfigWorker
}

func NewWorker(c ConfigWorker) *Worker {
	s := &Worker{
		c: c,
	}
	return s
}

func (s *Worker) Run(ctx context.Context) error {
	// Run apiproxy, which load-balances connections from worker components to this
	// cluster's api servers. This is necessary as we want to round-robin across all
	// available apiservers, and Kubernetes components do not implement client-side
	// load-balancing.
	err := supervisor.Run(ctx, "apiproxy", func(ctx context.Context) error {
		lis, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", node.KubernetesWorkerLocalAPIPort))
		if err != nil {
			return fmt.Errorf("failed to listen: %w", err)
		}
		defer lis.Close()

		v := memory.Value[tinylb.BackendSet]{}
		srv := tinylb.Server{
			Provider: &v,
			Listener: lis,
		}
		err = supervisor.Run(ctx, "updater", func(ctx context.Context) error {
			return updateLoadbalancerAPIServers(ctx, &v, s.c.CuratorClient)
		})
		if err != nil {
			return err
		}

		supervisor.Logger(ctx).Infof("Starting proxy...")
		return srv.Run(ctx)
	})
	if err != nil {
		return err
	}

	kubelet := kubeletService{
		ClusterDomain:      s.c.ClusterDomain,
		KubeletDirectory:   &s.c.Root.Data.Kubernetes.Kubelet,
		EphemeralDirectory: &s.c.Root.Ephemeral,
	}

	// Gather all required material to send over for certficiate issuance to the
	// curator...
	kwr := &ipb.IssueCertificateRequest_KubernetesWorker{}

	kubeletPK, err := kubelet.getPubkey(ctx)
	if err != nil {
		return fmt.Errorf("when getting kubelet pubkey: %w", err)
	}
	kwr.KubeletPubkey = kubeletPK

	clients := map[string]*struct {
		dir *localstorage.PKIDirectory

		sk ed25519.PrivateKey
		pk ed25519.PublicKey

		client     *kubernetes.Clientset
		informers  informers.SharedInformerFactory
		kubeconfig []byte

		certFrom func(kw *ipb.IssueCertificateResponse_KubernetesWorker) []byte
	}{
		"csi": {
			dir: &s.c.Root.Data.Kubernetes.CSIProvisioner.PKI,
			certFrom: func(kw *ipb.IssueCertificateResponse_KubernetesWorker) []byte {
				return kw.CsiProvisionerCertificate
			},
		},
		"netserv": {
			dir: &s.c.Root.Data.Kubernetes.Netservices.PKI,
			certFrom: func(kw *ipb.IssueCertificateResponse_KubernetesWorker) []byte {
				return kw.NetservicesCertificate
			},
		},
	}

	for name, c := range clients {
		if err := c.dir.GeneratePrivateKey(); err != nil {
			return fmt.Errorf("generating %s key: %w", name, err)
		}
		k, err := c.dir.ReadPrivateKey()
		if err != nil {
			return fmt.Errorf("reading %s key: %w", name, err)
		}
		c.sk = k
		c.pk = c.sk.Public().(ed25519.PublicKey)
	}
	kwr.CsiProvisionerPubkey = clients["csi"].pk
	kwr.NetservicesPubkey = clients["netserv"].pk

	// ...issue certificates...
	res, err := s.c.CuratorClient.IssueCertificate(ctx, &ipb.IssueCertificateRequest{
		Kind: &ipb.IssueCertificateRequest_KubernetesWorker_{
			KubernetesWorker: kwr,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get certificates from curator: %w", err)
	}
	kw := res.Kind.(*ipb.IssueCertificateResponse_KubernetesWorker_).KubernetesWorker

	// ...write them...
	if err := kubelet.setCertificates(kw); err != nil {
		return fmt.Errorf("failed to write kubelet certs: %w", err)
	}
	for name, c := range clients {
		if c.dir == nil {
			continue
		}
		if err := c.dir.WriteCertificates(kw.IdentityCaCertificate, c.certFrom(kw)); err != nil {
			return fmt.Errorf("failed to write %s certs: %w", name, err)
		}
	}

	// ... and set up connections.
	for name, c := range clients {
		kubeconf, err := kpki.KubeconfigRaw(kw.IdentityCaCertificate, c.certFrom(kw), c.sk, kpki.KubernetesAPIEndpointForWorker)
		if err != nil {
			return fmt.Errorf("failed to make %s kubeconfig: %w", name, err)
		}
		c.kubeconfig = kubeconf
		cs, informers, err := connectByKubeconfig(kubeconf)
		if err != nil {
			return fmt.Errorf("failed to connect with %s: %w", name, err)
		}
		c.client = cs
		c.informers = informers
	}

	// Sub-runnable which starts all parts of Kubernetes that depend on the
	// machine's external IP address. If it changes, the runnable will exit.
	// TODO(q3k): test this
	supervisor.Run(ctx, "networked", func(ctx context.Context) error {
		networkWatch := s.c.Network.Watch()
		defer networkWatch.Close()

		var status *network.Status

		supervisor.Logger(ctx).Info("Waiting for node networking...")
		for status == nil || status.ExternalAddress == nil {
			status, err = networkWatch.Get(ctx)
			if err != nil {
				return fmt.Errorf("failed to get network status: %w", err)
			}
		}
		address := status.ExternalAddress
		supervisor.Logger(ctx).Info("Node has active networking, starting apiserver/kubelet")
		kubelet.ClusterDNS = []net.IP{address}
		err := supervisor.RunGroup(ctx, map[string]supervisor.Runnable{
			"kubelet": kubelet.Run,
		})
		if err != nil {
			return fmt.Errorf("when starting kubelet: %w", err)
		}

		supervisor.Signal(ctx, supervisor.SignalHealthy)

		for status.ExternalAddress.Equal(address) {
			status, err = networkWatch.Get(ctx)
			if err != nil {
				return fmt.Errorf("when watching for network changes: %w", err)
			}
		}
		return fmt.Errorf("network configuration changed (%s -> %s)", address.String(), status.ExternalAddress.String())
	})

	csiPlugin := csiPluginServer{
		KubeletDirectory: &s.c.Root.Data.Kubernetes.Kubelet,
		VolumesDirectory: &s.c.Root.Data.Volumes,
	}

	csiProvisioner := csiProvisionerServer{
		NodeName:         s.c.NodeID,
		Kubernetes:       clients["csi"].client,
		InformerFactory:  clients["csi"].informers,
		VolumesDirectory: &s.c.Root.Data.Volumes,
	}

	clusternet := clusternet.Service{
		NodeName:   s.c.NodeID,
		Kubernetes: clients["netserv"].client,
		Prefixes:   s.c.PodNetwork,
	}

	nfproxy := nfproxy.Service{
		ClusterCIDR: s.c.ClusterNet,
		ClientSet:   clients["netserv"].client,
	}

	kvmDevicePlugin := kvmdevice.Plugin{
		KubeletDirectory: &s.c.Root.Data.Kubernetes.Kubelet,
	}

	for _, sub := range []struct {
		name     string
		runnable supervisor.Runnable
	}{
		{"csi-plugin", csiPlugin.Run},
		{"csi-provisioner", csiProvisioner.Run},
		{"clusternet", clusternet.Run},
		{"nfproxy", nfproxy.Run},
		{"kvmdeviceplugin", kvmDevicePlugin.Run},
	} {
		err := supervisor.Run(ctx, sub.name, sub.runnable)
		if err != nil {
			return fmt.Errorf("could not run sub-service %q: %w", sub.name, err)
		}
	}

	supervisor.Logger(ctx).Info("Registering K8s CoreDNS")
	clusterDNSDirective := dns.NewKubernetesDirective(s.c.ClusterDomain, clients["netserv"].kubeconfig)
	s.c.Network.ConfigureDNS(clusterDNSDirective)

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	<-ctx.Done()
	s.c.Network.ConfigureDNS(dns.CancelDirective(clusterDNSDirective))
	return nil
}

func connectByKubeconfig(kubeconfig []byte) (*kubernetes.Clientset, informers.SharedInformerFactory, error) {
	rawClientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("could not generate kubernetes client config: %w", err)
	}
	clientConfig, err := rawClientConfig.ClientConfig()
	clientSet, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("could not generate kubernetes client: %w", err)
	}
	informerFactory := informers.NewSharedInformerFactory(clientSet, 5*time.Minute)
	return clientSet, informerFactory, nil
}
