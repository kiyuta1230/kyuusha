package image

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestService_CreateRejectsFormatArtifactMismatch(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	if _, err := svc.Create(ctx, "tenant-a", "x", Spec{Format: FormatKernelRootfs}); err == nil {
		t.Fatal("expected validation error for KERNEL_ROOTFS with no kernel/rootfs urls")
	}
	if _, err := svc.Create(ctx, "tenant-a", "y", Spec{Format: FormatQCOW2}); err == nil {
		t.Fatal("expected validation error for QCOW2 with no disk url")
	}
	if _, err := svc.Create(ctx, "tenant-a", "z", Spec{}); err == nil {
		t.Fatal("expected validation error for unset format")
	}
}

func TestPlayground_ImageReachabilityFlipsToReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()

	svc := NewService()
	go func() {
		if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("Run: %v", err)
		}
	}()

	img, err := svc.Create(ctx, "tenant-a", "ubuntu", Spec{
		Format: FormatKernelRootfs,
		Kernel: Artifact{URL: ok.URL + "/kernel"},
		Rootfs: Artifact{URL: ok.URL + "/rootfs"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if img.Status.Phase != PhasePending {
		t.Fatalf("new Image phase = %q, want Pending", img.Status.Phase)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := svc.Get(ctx, "tenant-a", img.Meta.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status.Phase == PhaseReady {
			return
		}
		if got.Status.Phase == PhaseError {
			t.Fatalf("Image went to Error: %+v", got.Status.Conditions)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Ready, last phase = %s", got.Status.Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPlayground_ImageReachabilityFlipsToError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	svc := NewService()
	go func() {
		if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("Run: %v", err)
		}
	}()

	img, err := svc.Create(ctx, "tenant-a", "broken", Spec{
		Format: FormatQCOW2,
		Disk:   Artifact{URL: "http://127.0.0.1:1/does-not-exist"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := svc.Get(ctx, "tenant-a", img.Meta.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status.Phase == PhaseError {
			return
		}
		if got.Status.Phase == PhaseReady {
			t.Fatal("unreachable disk url unexpectedly went Ready")
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Error, last phase = %s", got.Status.Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
