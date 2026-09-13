// Command compute-agent runs the NATS side of compute-agent: it accepts
// vm.create/vm.delete commands and boots real VMM processes -- Firecracker
// for driver_hint=FIRECRACKER (internal/compute-agent/fcvmm,
// docs/specs/firecracker-boot.md) and cloud-hypervisor for
// driver_hint=CLOUD_HYPERVISOR (internal/compute-agent/chvmm,
// docs/specs/cloud-hypervisor-boot.md).
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kiyuta1230/kyuusha/internal/compute"
	computeagent "github.com/kiyuta1230/kyuusha/internal/compute-agent"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/chvmm"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/fcvmm"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/resourcemetrics"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/volumeref"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address, for Hypervisor self-registration")
	hypervisor := flag.String("hypervisor", "", "this hypervisor's ID (required)")
	bootstrapTokenFile := flag.String("bootstrap-token-file", "", "path to a zone-scoped bootstrap token (required; see 'kyuusha hypervisor bootstrap-token create'). Its zone claim, not any locally-configured value, becomes this Hypervisor's zone")
	vcpu := flag.Int("vcpu", 8, "allocatable vCPU capacity to report")
	memoryMB := flag.Int64("memory-mb", 16384, "allocatable memory capacity to report, in MB")
	drivers := flag.String("drivers", "FIRECRACKER", "comma-separated VMM drivers this hypervisor supports (FIRECRACKER|CLOUD_HYPERVISOR)")
	heartbeat := flag.Duration("heartbeat", 5*time.Second, "heartbeat interval")
	metricsAddr := flag.String("metrics-addr", ":9094", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	fcBin := flag.String("firecracker-bin", "firecracker", "firecracker binary jailer execs into for driver_hint=FIRECRACKER VMs")
	fcCacheDir := flag.String("fc-cache-dir", "/var/lib/kyuusha/fc-cache", "directory caching downloaded kernel/rootfs artifacts, shared across VMs")
	fcRunDir := flag.String("fc-run-dir", "/var/lib/kyuusha/fc-run", "directory holding each running VM's console log (everything else lives inside its jail, see -fc-jail-chroot-base-dir)")
	fcJailerBin := flag.String("fc-jailer-bin", "jailer", "jailer binary every driver_hint=FIRECRACKER VM is exec'd through -- see docs/specs/firecracker-boot.md \"jailer\"")
	fcJailChrootBaseDir := flag.String("fc-jail-chroot-base-dir", "/var/lib/kyuusha/fc-jail", "jailer's --chroot-base-dir: parent of <exec-file-basename>/<vm_id>/root for every VM's jail")
	fcJailUID := flag.Uint("fc-jail-uid", 123, "uid jailer drops privileges to before exec'ing Firecracker inside its jail -- shared by every VM this compute-agent boots (see the fcvmm package doc comment)")
	fcJailGID := flag.Uint("fc-jail-gid", 100, "gid jailer drops privileges to before exec'ing Firecracker inside its jail -- shared by every VM this compute-agent boots (see the fcvmm package doc comment)")
	chBin := flag.String("ch-bin", "cloud-hypervisor", "cloud-hypervisor binary to exec for driver_hint=CLOUD_HYPERVISOR VMs (see internal/compute-agent/chvmm)")
	chCacheDir := flag.String("ch-cache-dir", "/var/lib/kyuusha/ch-cache", "directory caching downloaded kernel/rootfs artifacts for driver_hint=CLOUD_HYPERVISOR VMs")
	chRunDir := flag.String("ch-run-dir", "/var/lib/kyuusha/ch-run", "directory holding each running driver_hint=CLOUD_HYPERVISOR VM's writable rootfs copy and console log")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented when dialing compute (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA compute's certificate must chain to")
	storageConnections := flag.String("storage-connections", "", "comma-separated storage connections this host already has established, name[:local_path][,name[:local_path]...] -- an iSCSI/NVMe-oF session already logged in (no local_path needed: Volumes on it are discovered under /dev/disk/by-id/) or an NFS export already mounted (local_path is its mount point). Sent to compute at self-registration and used locally by internal/compute-agent/volumeref to find each Volume's already-visible device/file at boot time; kyuusha never logs in, mounts, or exports anything itself (see docs/architecture.md「訂正: 責務の境界を...」)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if *hypervisor == "" {
		slog.Error("-hypervisor is required")
		os.Exit(1)
	}
	if *bootstrapTokenFile == "" {
		slog.Error("-bootstrap-token-file is required")
		os.Exit(1)
	}
	bootstrapTokenBytes, err := os.ReadFile(*bootstrapTokenFile)
	if err != nil {
		slog.Error("read bootstrap token", "path", *bootstrapTokenFile, "err", err)
		os.Exit(1)
	}
	bootstrapToken := strings.TrimSpace(string(bootstrapTokenBytes))

	connections, connectionProtos := parseStorageConnections(*storageConnections)

	// Built once, before Agent and before /metrics/resources' collector,
	// since both need the exact same map[string]vmm.VMM: the collector
	// reads Running() from each driver, and Agent dispatches Boot/Stop/
	// Destroy against the same instances.
	vmmDrivers := map[string]vmm.VMM{
		string(compute.VmmDriverFirecracker): &fcvmm.Manager{
			BinPath:            *fcBin,
			CacheDir:           *fcCacheDir,
			RunDir:             *fcRunDir,
			JailerBinPath:      *fcJailerBin,
			JailChrootBaseDir:  *fcJailChrootBaseDir,
			JailUID:            uint32(*fcJailUID),
			JailGID:            uint32(*fcJailGID),
			StorageConnections: connections,
		},
		string(compute.VmmDriverCloudHypervisor): &chvmm.Manager{
			BinPath:            *chBin,
			CacheDir:           *chCacheDir,
			RunDir:             *chRunDir,
			StorageConnections: connections,
		},
	}

	// Must happen before any Firecracker process is ever forked (Boot's
	// exec.Command): a child forked while this process still resides
	// directly in the (cgroupns-scoped) root cgroup inherits that placement
	// permanently, which then blocks cgroup.Apply's controller delegation
	// for every VM after it. See internal/compute-agent/cgroup's doc
	// comment on Init. Best-effort -- a host/container without usable
	// cgroup v2 delegation just runs with VMs unconstrained.
	if err := cgroup.Init(); err != nil {
		slog.Warn("cgroup delegation not available, VMs will boot unconstrained", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "compute-agent", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("compute-agent")
	if err != nil {
		slog.Error("setup metrics", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownMetrics(shutdownCtx)
	}()
	// /metrics/resources is a separate registry/handler from /metrics
	// (telemetry.SetupMetrics' own otel-backed one): these are payload
	// (VM) metrics, not this process' own system metrics, and deliberately
	// kept out of the resource_version/Watch-visible object model -- see
	// docs/architecture.md「払い出したリソース自身のメトリクス」.
	resourceRegistry := prometheus.NewRegistry()
	resourceRegistry.MustRegister(resourcemetrics.New(*hypervisor, vmmDrivers))

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metricsHandler)
		mux.Handle("/metrics/resources", promhttp.HandlerFor(resourceRegistry, promhttp.HandlerOpts{}))
		if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
			slog.Error("metrics server stopped", "err", err)
		}
	}()

	nc, err := nats.Connect(*natsURL)
	if err != nil {
		slog.Error("connect to nats", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		slog.Error("create jetstream context", "err", err)
		os.Exit(1)
	}

	// compute-agent -> compute is mTLS-authenticated (proves "this is some
	// kyuusha service"); Register additionally carries a zone-scoped
	// bootstrap token (see BootstrapToken below and
	// docs/specs/hypervisor-bootstrap.md), verified by compute. Individual
	// hypervisor identity/revocation remains a separate, still-undone
	// follow-up (docs/open-questions.md).
	clientCreds, err := mtls.ClientCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		slog.Error("load mTLS client credentials", "err", err)
		os.Exit(1)
	}
	computeConn, err := grpc.NewClient(*computeAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()

	agent := &computeagent.Agent{
		Hypervisor:              *hypervisor,
		NC:                      nc,
		JS:                      js,
		HeartbeatInterval:       *heartbeat,
		Hypervisors:             computev1.NewHypervisorServiceClient(computeConn),
		BootstrapToken:          bootstrapToken,
		AllocatableVCPU:         int32(*vcpu),
		AllocatableMemoryMB:     *memoryMB,
		SupportedDrivers:        strings.Split(*drivers, ","),
		StorageConnections:      connectionProtos,
		LocalStorageConnections: connections,
		Drivers:                 vmmDrivers,
	}
	slog.Info("compute-agent: starting", "hypervisor", *hypervisor)
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

// parseStorageConnections turns -storage-connections' name[:local_path]
// entries into both forms this compute-agent needs: a local
// volumeref.Connections map (consumed by fcvmm/chvmm's Managers at VM
// boot time, and by handleVerifyVolume to answer block-storage) and the
// []*computev1.StorageConnection this same information travels as in
// RegisterHypervisorRequest. Both are built from a single flag value,
// rather than kept as two separately-specified inputs, since they describe
// the exact same fact (what this host already has connected) from two
// different callers' point of view.
//
// "Static" (docs/open-questions.md「Hypervisorのストレージ接続自己申告を
// 動的化すべきか」) means this is checked once, here, at startup -- not
// never: an entry with a local_path (NFS-shaped; ISCSI/NVME_OF entries
// have none, see volumeref.Resolve's fixed /dev/disk/by-id/ convention, so
// there's nothing connection-specific to check for those at this level) is
// verified to actually exist as a directory right now, and dropped (with a
// warning, not a fatal error -- the other declared connections may still
// be fine) if it doesn't. Never re-checked again afterward; that's the
// "static" part.
func parseStorageConnections(raw string) (volumeref.Connections, []*computev1.StorageConnection) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	conns := make(volumeref.Connections)
	var protos []*computev1.StorageConnection
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, localPath, _ := strings.Cut(entry, ":")
		if localPath != "" {
			info, err := os.Stat(localPath)
			if err != nil {
				slog.Warn("compute-agent: dropping declared storage connection, local_path check failed", "name", name, "local_path", localPath, "err", err)
				continue
			}
			if !info.IsDir() {
				slog.Warn("compute-agent: dropping declared storage connection, local_path is not a directory", "name", name, "local_path", localPath)
				continue
			}
		}
		conns[name] = localPath
		protos = append(protos, &computev1.StorageConnection{Name: name, LocalPath: localPath})
	}
	return conns, protos
}
