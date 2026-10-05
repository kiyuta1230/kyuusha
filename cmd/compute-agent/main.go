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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/imagestore"
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
	imageCacheDir := flag.String("image-cache-dir", "/var/lib/kyuusha/image-cache", "directory caching downloaded kernel/rootfs artifacts, digest-verified and shared across both VMM drivers and every VM they boot (see internal/compute-agent/imagestore)")
	imageCacheMaxMB := flag.Int64("image-cache-max-mb", 20480, "soft cap on -image-cache-dir's total size, in MiB; the sweep loop evicts least-recently-used blobs not currently in use by a running VM to stay at or under it (see internal/compute-agent/imagestore.Store.Sweep). 0 disables eviction (the cache grows unbounded)")
	imageCacheSweepInterval := flag.Duration("image-cache-sweep-interval", 10*time.Minute, "how often the image cache eviction sweep runs")
	fcRunDir := flag.String("fc-run-dir", "/var/lib/kyuusha/fc-run", "directory holding each running VM's console log (everything else lives inside its jail, see -fc-jail-chroot-base-dir)")
	fcJailerBin := flag.String("fc-jailer-bin", "jailer", "jailer binary every driver_hint=FIRECRACKER VM is exec'd through -- see docs/specs/firecracker-boot.md \"jailer\"")
	fcJailChrootBaseDir := flag.String("fc-jail-chroot-base-dir", "/var/lib/kyuusha/fc-jail", "jailer's --chroot-base-dir: parent of <exec-file-basename>/<vm_id>/root for every VM's jail")
	fcJailUID := flag.Uint("fc-jail-uid", 123, "uid jailer drops privileges to before exec'ing Firecracker inside its jail -- shared by every VM this compute-agent boots (see the fcvmm package doc comment)")
	fcJailGID := flag.Uint("fc-jail-gid", 100, "gid jailer drops privileges to before exec'ing Firecracker inside its jail -- shared by every VM this compute-agent boots (see the fcvmm package doc comment)")
	chBin := flag.String("ch-bin", "cloud-hypervisor", "cloud-hypervisor binary to exec for driver_hint=CLOUD_HYPERVISOR VMs (see internal/compute-agent/chvmm)")
	chFirmwarePath := flag.String("ch-firmware-path", "/usr/local/share/kyuusha/CLOUDHV.fd", "edk2 UEFI firmware (CLOUDHV.fd) passed to --firmware when booting a QCOW2 Image (see docs/specs/cloud-hypervisor-boot.md \"QCOW2起動\"); unused for KERNEL_ROOTFS Images")
	chRunDir := flag.String("ch-run-dir", "/var/lib/kyuusha/ch-run", "directory holding each running driver_hint=CLOUD_HYPERVISOR VM's writable rootfs copy and console log")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented when dialing compute (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA compute's certificate must chain to")
	storageConnections := flag.String("storage-connections", "", "comma-separated storage connections this host already has established, name[:local_path][,name[:local_path]...] -- an iSCSI/NVMe-oF session already logged in (no local_path needed: Volumes on it are discovered under /dev/disk/by-id/) or an NFS export already mounted (local_path is its mount point). Sent to compute at self-registration and used locally by internal/compute-agent/volumeref to find each Volume's already-visible device/file at boot time; kyuusha never logs in, mounts, or exports anything itself (see docs/architecture.md「訂正: 責務の境界を...」)")
	pciDevices := flag.String("pci-devices", "", "comma-separated PCI devices this host has already bound to vfio-pci and makes available for passthrough, pci_address:vendor_id:device_id[,pci_address:vendor_id:device_id...] (e.g. 0000:3b:00.0:10de:1c03) -- only meaningful with CLOUD_HYPERVISOR in -drivers (Firecracker has no PCI bus). Sent to compute at self-registration (see internal/compute/hypervisor_service.go's reservePciDevices); each address is verified to actually be vfio-pci-bound and dropped (with a warning) otherwise, same defensive spirit as -storage-connections' local_path check")
	networkAttachBin := flag.String("network-attach-bin", "", "path to an external VNAP plugin binary (see internal/compute-agent/netsetup and docs/architecture.md \"VMのネットワーク接続をCNIのようにプラガブルにすべきか\") that Wire/DeleteTap delegate the local tap-to-switch attach/detach step to, invoked as '<bin> attach|detach' with a JSON payload on stdin. Empty (the default) keeps the built-in Linux bridge implementation")
	securityBackendBin := flag.String("security-backend-bin", "", "path to an external SNAP (Security Network Attach Protocol) plugin binary (see internal/compute-agent/snap and docs/specs/snap.md) that SecurityGroup enforcement delegates to, invoked as '<bin> attach|detach|update_sets' with a JSON payload on stdin -- a separate contract from -network-attach-bin/VNAP (ACL enforcement is independent of tap-to-switch wiring). Empty (the default) keeps the built-in nftacl implementation, which assumes the built-in Linux bridge wiring (bridge-family nftables rules) -- an operator using a non-bridge VNAP wiring plugin needs a paired SNAP plugin here too")
	migrationRegistry := flag.String("migration-registry", "", "OCI registry host[:port] this compute-agent pushes a VM's current root disk to for Migrate(transfer_root_disk=true), and deletes temporary migration artifacts from afterward. Empty (the default) disables root disk transfer on this host -- a push request then fails explicitly rather than silently doing nothing (see docs/specs/virtual-machine.md \"ルートディスク転送\")")
	migrationRegistryRef := flag.String("migration-registry-ref", "", "OCI registry host[:port] to embed in a pushed artifact's URL, if different from -migration-registry -- same split as `kyuusha image build`'s -registry/-registry-ref (default: same as -migration-registry)")
	migrationRegistryPlainHTTP := flag.Bool("migration-registry-plain-http", false, "push/delete against -migration-registry over plain HTTP instead of HTTPS")
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
	availableDevices := parsePciDevices(*pciDevices)
	numaNodes, numaTopology, err := detectNumaTopology()
	if err != nil {
		slog.Warn("compute-agent: NUMA topology detection failed, spec.numa_pinned VMs will never be schedulable on this host", "err", err)
	}

	// Built once, before Agent and before /metrics/resources' collector,
	// since both need the exact same map[string]vmm.VMM: the collector
	// reads Running() from each driver, and Agent dispatches Boot/Stop/
	// Destroy against the same instances.
	// One Store shared by both drivers, so the same Image is never
	// downloaded or held twice regardless of which driver_hint a VM
	// requests -- see internal/compute-agent/imagestore and
	// docs/architecture.md「イメージのローカル管理: containerdのcontent
	// store/snapshotterへの移行検討」.
	imageStore := &imagestore.Store{Dir: *imageCacheDir}

	vmmDrivers := map[string]vmm.VMM{
		string(compute.VmmDriverFirecracker): &fcvmm.Manager{
			BinPath:            *fcBin,
			ImageStore:         imageStore,
			RunDir:             *fcRunDir,
			JailerBinPath:      *fcJailerBin,
			JailChrootBaseDir:  *fcJailChrootBaseDir,
			JailUID:            uint32(*fcJailUID),
			JailGID:            uint32(*fcJailGID),
			StorageConnections: connections,
			NetworkAttachBin:   *networkAttachBin,
			SecurityBackendBin: *securityBackendBin,
			NumaTopology:       numaTopology,
		},
		string(compute.VmmDriverCloudHypervisor): &chvmm.Manager{
			BinPath:            *chBin,
			FirmwarePath:       *chFirmwarePath,
			ImageStore:         imageStore,
			RunDir:             *chRunDir,
			StorageConnections: connections,
			NetworkAttachBin:   *networkAttachBin,
			SecurityBackendBin: *securityBackendBin,
			NumaTopology:       numaTopology,
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

	// Keeps -image-cache-dir bounded -- see internal/compute-agent/
	// imagestore's package doc and Sweep's doc comment. maxBytes <= 0
	// (imageCacheMaxMB == 0) makes Sweep itself a no-op, so no separate
	// on/off branch is needed here.
	imageCacheMaxBytes := *imageCacheMaxMB * 1024 * 1024
	go func() {
		ticker := time.NewTicker(*imageCacheSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				freed, err := imageStore.Sweep(imageCacheMaxBytes)
				if err != nil {
					slog.Warn("image cache sweep failed", "err", err)
				} else if freed > 0 {
					slog.Info("image cache sweep evicted least-recently-used blobs", "freed_bytes", freed)
				}
			}
		}
	}()

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
		Hypervisor:                 *hypervisor,
		NC:                         nc,
		JS:                         js,
		HeartbeatInterval:          *heartbeat,
		Hypervisors:                computev1.NewHypervisorServiceClient(computeConn),
		BootstrapToken:             bootstrapToken,
		AllocatableVCPU:            int32(*vcpu),
		AllocatableMemoryMB:        *memoryMB,
		SupportedDrivers:           strings.Split(*drivers, ","),
		StorageConnections:         connectionProtos,
		LocalStorageConnections:    connections,
		AvailableDevices:           availableDevices,
		NumaNodes:                  numaNodes,
		MigrationRegistry:          *migrationRegistry,
		MigrationRegistryRef:       *migrationRegistryRef,
		MigrationRegistryPlainHTTP: *migrationRegistryPlainHTTP,
		Drivers:                    vmmDrivers,
		SecurityBackendBin:         *securityBackendBin,
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

// cutLast splits s at the last occurrence of sep, returning (the part after
// it, the part before it, true), or ("", s, false) if sep doesn't occur.
func cutLast(s, sep string) (tail, head string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", s, false
	}
	return s[i+len(sep):], s[:i], true
}

// vfioPciDriverName is the kernel driver every declared -pci-devices entry
// must actually be bound to -- checked below via /sys/bus/pci/devices/<addr>/
// driver's symlink target, the same "declared" claim the OS itself can
// independently confirm before compute-agent forwards it as fact (see
// parseStorageConnections' local_path check for the same defensive shape).
const vfioPciDriverName = "vfio-pci"

// parsePciDevices turns -pci-devices' pci_address:vendor_id:device_id
// entries into the RegisterHypervisorRequest.available_devices this
// compute-agent self-reports. An entry whose pci_address isn't actually
// bound to vfio-pci right now is dropped (with a warning, not a fatal
// error) rather than trusted at face value -- a misconfigured or stale
// declaration would otherwise let compute schedule a passthrough VM onto a
// device the host can't actually hand to a guest.
func parsePciDevices(raw string) []*computev1.PciDevice {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var devices []*computev1.PciDevice
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Split from the right: a pci_address (e.g. "0000:00:01.0") itself
		// contains colons, so a naive strings.Split(entry, ":") on the whole
		// entry would shatter it -- vendor_id/device_id never do, so the
		// last two ":"-separated fields are always exactly those, and
		// whatever's left is the (possibly colon-containing) pci_address.
		deviceID, rest, ok := cutLast(entry, ":")
		var vendorID, pciAddress string
		if ok {
			vendorID, pciAddress, ok = cutLast(rest, ":")
		}
		if !ok {
			slog.Warn("compute-agent: dropping malformed -pci-devices entry, want pci_address:vendor_id:device_id", "entry", entry)
			continue
		}
		driverLink, err := os.Readlink("/sys/bus/pci/devices/" + pciAddress + "/driver")
		if err != nil {
			slog.Warn("compute-agent: dropping declared pci device, driver check failed", "pci_address", pciAddress, "err", err)
			continue
		}
		if filepath.Base(driverLink) != vfioPciDriverName {
			slog.Warn("compute-agent: dropping declared pci device, not bound to vfio-pci", "pci_address", pciAddress, "bound_driver", filepath.Base(driverLink))
			continue
		}
		devices = append(devices, &computev1.PciDevice{PciAddress: pciAddress, VendorId: vendorID, DeviceId: deviceID, NumaNode: readPCINumaNode(pciAddress)})
	}
	return devices
}

// readPCINumaNode reads /sys/bus/pci/devices/<addr>/numa_node -- best-effort
// input to NUMA-pinned scheduling (see hypervisor.proto's PciDevice.
// numa_node), so a missing/malformed file is not fatal to declaring the
// device at all, just to co-locating it with a NUMA-pinned VM's vCPUs.
// The kernel itself reports -1 for "no NUMA affinity known" (a single-node
// host, or a bus that doesn't expose one), which is also this function's
// own fallback for a read/parse failure -- same sentinel either way.
func readPCINumaNode(pciAddress string) int32 {
	b, err := os.ReadFile("/sys/bus/pci/devices/" + pciAddress + "/numa_node")
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return int32(n)
}

// detectNumaTopology reads /sys/devices/system/node/node<N>/{cpulist,meminfo}
// for every NUMA node the host kernel reports, building both the
// self-report compute-agent sends at Register (numaNodes) and the local
// node-id -> host-CPU-list map fcvmm/chvmm need to actually pin a VM's
// cgroup (topology, see fcvmm.Manager/chvmm.Manager's NumaTopology field).
// Best-effort: a host without /sys/devices/system/node at all (a container
// without it mounted, or a non-Linux/unusual kernel) returns (nil, nil, err)
// -- callers should log and continue with NUMA pinning simply unavailable,
// the same "host already has it, agent just reports it" trust level as
// every other self-report in this file, not a fatal startup error.
func detectNumaTopology() ([]*computev1.NumaNode, map[int32][]int32, error) {
	const sysNode = "/sys/devices/system/node"
	entries, err := os.ReadDir(sysNode)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", sysNode, err)
	}
	var nodes []*computev1.NumaNode
	topology := make(map[int32][]int32)
	for _, e := range entries {
		nodeID, ok := strings.CutPrefix(e.Name(), "node")
		if !ok {
			continue
		}
		id, err := strconv.Atoi(nodeID)
		if err != nil {
			continue
		}
		dir := filepath.Join(sysNode, e.Name())
		cpus, err := parseCPUList(dir + "/cpulist")
		if err != nil {
			slog.Warn("compute-agent: reading NUMA node cpulist failed, dropping this node from self-report", "node_id", id, "err", err)
			continue
		}
		memoryMB, err := parseNodeMemTotalMB(dir + "/meminfo")
		if err != nil {
			slog.Warn("compute-agent: reading NUMA node meminfo failed, dropping this node from self-report", "node_id", id, "err", err)
			continue
		}
		nodes = append(nodes, &computev1.NumaNode{NodeId: int32(id), Cpus: cpus, MemoryMb: memoryMB})
		topology[int32(id)] = cpus
	}
	return nodes, topology, nil
}

// parseCPUList reads a /sys cpulist-format file ("0-3,8,10-11") and expands
// it to individual logical CPU ids.
func parseCPUList(path string) ([]int32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cpus []int32
	for _, part := range strings.Split(strings.TrimSpace(string(b)), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		loN, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("malformed cpulist entry %q: %w", part, err)
		}
		if !isRange {
			cpus = append(cpus, int32(loN))
			continue
		}
		hiN, err := strconv.Atoi(hi)
		if err != nil {
			return nil, fmt.Errorf("malformed cpulist entry %q: %w", part, err)
		}
		for c := loN; c <= hiN; c++ {
			cpus = append(cpus, int32(c))
		}
	}
	return cpus, nil
}

// parseNodeMemTotalMB reads a NUMA node's meminfo file's first line
// ("Node <N> MemTotal:       <kB> kB") and converts it to MB.
func parseNodeMemTotalMB(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	firstLine, _, _ := strings.Cut(string(b), "\n")
	fields := strings.Fields(firstLine)
	if len(fields) < 4 || fields[2] != "MemTotal:" {
		return 0, fmt.Errorf("malformed meminfo first line %q", firstLine)
	}
	kB, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed MemTotal value in %q: %w", firstLine, err)
	}
	return kB / 1024, nil
}
