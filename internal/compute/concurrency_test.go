package compute

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestService_ConcurrentCreateNeverOverchargesTenantQuota fires many
// concurrent Create calls at the same tenant against a tight max_vms quota
// -- the actual correctness property usageMu exists to guarantee (see its
// doc comment on the Service struct). Unlike hypervisor_service_test.go's
// existing capacity race tests (which race reserveHypervisorCapacity
// directly), this is the first test that puts concurrent load through the
// real entrypoint, Service.Create, exercising usageMu itself
// (docs/architecture.md「Quota設計」/ the production-roadmap's "quota/
// スケジューラの並行性・負荷テスト" item). Kept as a normal, fast,
// deterministic correctness test -- see BenchmarkService_
// CreateUnderQuotaContention below for that item's other half (whether
// usageMu's single global lock scales), which is deliberately not a Test.
func TestService_ConcurrentCreateNeverOverchargesTenantQuota(t *testing.T) {
	ctx := context.Background()
	const tenant = "tenant-a"
	const quota = 5
	const attempts = 30

	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu: 1000, MaxMemoryMb: 1_000_000, MaxVms: quota, MaxVcpuPerVm: 8, MaxMemoryMbPerVm: 8192,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	var wg sync.WaitGroup
	var succeeded, quotaExceeded, otherErr int64
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Create(ctx, tenant, fmt.Sprintf("vm-%d", i), VirtualMachineSpec{
				ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
			})
			switch {
			case err == nil:
				atomic.AddInt64(&succeeded, 1)
			case errors.Is(err, ErrQuotaExceeded):
				atomic.AddInt64(&quotaExceeded, 1)
			default:
				atomic.AddInt64(&otherErr, 1)
				t.Errorf("unexpected Create error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if otherErr != 0 {
		t.Fatalf("%d Create calls failed with an error other than ErrQuotaExceeded", otherErr)
	}
	if succeeded != quota {
		t.Fatalf("succeeded = %d, want exactly quota (%d)", succeeded, quota)
	}
	if quotaExceeded != attempts-quota {
		t.Fatalf("quotaExceeded = %d, want %d", quotaExceeded, attempts-quota)
	}

	// The in-memory usage bookkeeping and the actual persisted VM count in
	// etcd must agree exactly: a real over/under-charge race would show up
	// as a mismatch here even if the success/quotaExceeded counts above
	// happened to add up by coincidence.
	if usage := svc.usage[tenant]; usage.VMCount != quota {
		t.Fatalf("usage.VMCount = %d, want %d", usage.VMCount, quota)
	}
	vms, err := svc.store.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) != quota {
		t.Fatalf("persisted VM count = %d, want %d", len(vms), quota)
	}
}

// slowImageClient wraps FakeImageClient with an artificial per-call delay,
// standing in for a real image service's network round trip. Needed for
// BenchmarkService_CreateUnderQuotaContention to actually demonstrate
// usageMu's lock-hold cost: FakeImageClient alone returns near-instantly,
// which would make the benchmark measure raw mutex overhead instead of the
// "does Create's throughput scale under realistic external-call latency"
// question the roadmap item is actually about (Create's doc comment in
// service.go: image/subnet/volume validation, the identity quota lookup,
// and the admission webhook call all happen while usageMu is held).
type slowImageClient struct {
	FakeImageClient
	delay time.Duration
}

func (f *slowImageClient) Get(ctx context.Context, req *imagev1.GetImageRequest, opts ...grpc.CallOption) (*imagev1.Image, error) {
	time.Sleep(f.delay)
	return f.FakeImageClient.Get(ctx, req, opts...)
}

// BenchmarkService_CreateUnderQuotaContention measures how Create's
// throughput scales with concurrency given usageMu's single global lock
// (see the Service struct's own doc comment: "a real scalability
// bottleneck at higher creates/sec than this system's target scale
// implies... sharding it per tenant is a reasonable follow-up if that ever
// matters"). Run manually, e.g.:
//
//	go test ./internal/compute/ -run '^$' -bench BenchmarkService_CreateUnderQuotaContention -benchtime=3s -cpu=1,4,16
//
// A roughly flat ns/op across -cpu values (instead of dropping as more
// CPUs are made available) is exactly what "usageMu doesn't scale" looks
// like in practice: every Create still runs its slow section one at a
// time, no matter how many goroutines are runnable. If that's ever
// actually observed at a traffic volume that matters, the lock's own doc
// comment already names the fix (shard tenant_usage's bookkeeping, and the
// mutex with it, per tenant instead of one global lock for every tenant at
// once).
//
// Deliberately not a Test: Go only runs Benchmark* functions when -bench
// is passed explicitly, so this never runs under CI's plain `go test
// ./...` (build-test job) and never affects its timing -- see
// docs/open-questions.md「quota/スケジューラの並行性・負荷テスト」on why
// this stays a manual, on-demand measurement rather than a CI gate: its
// numbers are latency/throughput characteristics, not a pass/fail
// correctness property, and shared CI runners don't give consistent
// enough hardware for a benchmark threshold to mean anything.
func BenchmarkService_CreateUnderQuotaContention(b *testing.B) {
	ctx := context.Background()
	const tenant = "tenant-a"

	svc, err := NewService(ctx, resourcetest.Client(b), &FakeTenantClient{Quota: UnlimitedQuota()},
		&slowImageClient{delay: 2 * time.Millisecond}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		b.Fatalf("NewService: %v", err)
	}

	var counter int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&counter, 1)
			if _, err := svc.Create(ctx, tenant, fmt.Sprintf("vm-%d", n), VirtualMachineSpec{
				ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
			}); err != nil {
				b.Fatalf("Create: %v", err)
			}
		}
	})
}
