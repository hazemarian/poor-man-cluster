package stackdrift

import (
	"context"
	"fmt"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestContentHashMatchesStoreConfigHash pins stackdrift.ContentHash to
// store.ConfigHash: the hash recorded by the store and the hash stamped on
// services must stay byte-identical, or drift detection breaks silently.
func TestContentHashMatchesStoreConfigHash(t *testing.T) {
	samples := []string{
		"",
		"a",
		"hello world",
		"services:\n  web:\n    image: nginx\n",
		"some longer content with $dollars$ and\nnewlines\tand tabs",
	}
	for _, s := range samples {
		if got, want := ContentHash([]byte(s)), store.ConfigHash(s); got != want {
			t.Errorf("ContentHash(%q) = %s, store.ConfigHash = %s — the two MUST stay identical", s, got, want)
		}
	}
}

// fakeClient is a minimal runtime.Client for the InSync table: it only
// implements ServiceList; the embedded interface panics on anything else
// (which these tests never touch).
type fakeClient struct {
	runtime.Client
	svcs []runtime.Service
	err  error
}

func (f *fakeClient) ServiceList(context.Context) ([]runtime.Service, error) {
	return f.svcs, f.err
}

// freshCompose renders a minimal two-service compose carrying the rendered-hash
// label on every service ("" hash → a label-free render that predates the
// label).
func freshCompose(hash string) []byte {
	return []byte(fmt.Sprintf(`version: "3.9"
services:
  web:
    image: nginx
    deploy:
      labels:
        io.pmcluster.rendered_hash: "%s"
  worker:
    image: nginx
    deploy:
      labels:
        io.pmcluster.rendered_hash: "%s"
`, hash, hash))
}

func svc(stack, name, hash string) runtime.Service {
	return runtime.Service{
		Name:   stack + "_" + name,
		Stack:  stack,
		Labels: map[string]string{runtime.RenderedHashLabel: hash},
	}
}

func TestInSync(t *testing.T) {
	const stack = "demo"
	const h = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name       string
		client     runtime.Client
		fresh      []byte
		wantInSync bool
		wantReason string
	}{
		{
			name:       "nil client is in sync",
			client:     nil,
			fresh:      freshCompose(h),
			wantInSync: true,
		},
		{
			name:       "absent stack drifts",
			client:     &fakeClient{},
			fresh:      freshCompose(h),
			wantInSync: false,
			wantReason: "absent from the swarm",
		},
		{
			name: "missing required service drifts",
			client: &fakeClient{svcs: []runtime.Service{
				svc(stack, "web", h),
			}},
			fresh:      freshCompose(h),
			wantInSync: false,
			wantReason: "a required service is missing",
		},
		{
			name: "drifted label drifts",
			client: &fakeClient{svcs: []runtime.Service{
				svc(stack, "web", h),
				svc(stack, "worker", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			}},
			fresh:      freshCompose(h),
			wantInSync: false,
			wantReason: "drifted from the rendered compose",
		},
		{
			name: "partial labels drift",
			client: &fakeClient{svcs: []runtime.Service{
				svc(stack, "web", h),
				{Name: stack + "_worker", Stack: stack, Labels: map[string]string{}},
			}},
			fresh:      freshCompose(h),
			wantInSync: false,
			wantReason: "some services are missing the rendered-hash label (partial deploy)",
		},
		{
			name: "all unlabeled drifts when render requires the label",
			client: &fakeClient{svcs: []runtime.Service{
				{Name: stack + "_web", Stack: stack, Labels: map[string]string{}},
				{Name: stack + "_worker", Stack: stack, Labels: map[string]string{}},
			}},
			fresh:      freshCompose(h),
			wantInSync: false,
			wantReason: "no live service carries the rendered-hash label (the labelled render was not applied)",
		},
		{
			name: "extra live service ignored",
			client: &fakeClient{svcs: []runtime.Service{
				svc(stack, "web", h),
				svc(stack, "worker", h),
				svc(stack, "legacy-gone", h), // render no longer produces this
			}},
			fresh:      freshCompose(h),
			wantInSync: true,
		},
		{
			name: "matching labels in sync",
			client: &fakeClient{svcs: []runtime.Service{
				svc(stack, "web", h),
				svc(stack, "worker", h),
			}},
			fresh:      freshCompose(h),
			wantInSync: true,
		},
		{
			name: "render predates label compares service set only",
			client: &fakeClient{svcs: []runtime.Service{
				{Name: stack + "_web", Stack: stack, Labels: map[string]string{}},
				{Name: stack + "_worker", Stack: stack, Labels: map[string]string{}},
			}},
			fresh:      freshCompose(""),
			wantInSync: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inSync, reason, err := InSync(context.Background(), tt.client, stack, tt.fresh)
			if err != nil {
				t.Fatalf("InSync() err = %v, want nil", err)
			}
			if inSync != tt.wantInSync {
				t.Fatalf("InSync() = %v (reason %q), want %v", inSync, reason, tt.wantInSync)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestInSync_ErrorReturned(t *testing.T) {
	boom := fmt.Errorf("docker API down")
	_, _, err := InSync(context.Background(), &fakeClient{err: boom}, "demo", freshCompose("a"))
	if err == nil {
		t.Fatal("InSync() with a ServiceList error should return the error, got nil")
	}
}

func TestInSync_BadComposeError(t *testing.T) {
	_, _, err := InSync(context.Background(), &fakeClient{}, "demo", []byte(": : not valid yaml:"))
	if err == nil {
		t.Fatal("InSync() with unparseable compose should return an error, got nil")
	}
}
