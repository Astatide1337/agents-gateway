package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
)

// boundedTestQuotaProvider is intentionally not a production implementation.
// It models the admission/release contract without requiring privileged
// filesystem quota ioctls in unit tests. Live Podman tests remain responsible
// for proving that the tmpfs mount returns ENOSPC.
type boundedTestQuotaProvider struct {
	mu       sync.Mutex
	root     string
	capacity int64
	used     int64
	active   map[string]*boundedTestQuotaLease
}

func newBoundedTestQuotaProvider(root string, capacity int64) *boundedTestQuotaProvider {
	return &boundedTestQuotaProvider{root: root, capacity: capacity, active: make(map[string]*boundedTestQuotaLease)}
}

func (*boundedTestQuotaProvider) Name() string    { return "bounded-test" }
func (*boundedTestQuotaProvider) Validate() error { return nil }

func (p *boundedTestQuotaProvider) Provision(ctx context.Context, request WorkspaceQuotaRequest) (WorkspaceQuotaLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceQuotaRequest(request); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if request.Bytes > p.capacity-p.used {
		return nil, ErrWorkspaceQuotaExhausted
	}
	path, err := prepareWorkspace(p.root, request.ID)
	if err != nil {
		return nil, err
	}
	lease := &boundedTestQuotaLease{
		provider: p, path: path,
		mount: WorkspaceQuotaMount{Kind: WorkspaceQuotaTmpfs, Bytes: request.Bytes, UID: request.UID, GID: request.GID},
		bytes: request.Bytes,
	}
	p.used += request.Bytes
	p.active[path] = lease
	return lease, nil
}

func (p *boundedTestQuotaProvider) Reconcile(_ context.Context, root string) error {
	return cleanupOrphanWorkspaces(root)
}

func (p *boundedTestQuotaProvider) release(lease *boundedTestQuotaLease) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.active[lease.path]; !ok {
		return nil
	}
	delete(p.active, lease.path)
	p.used -= lease.bytes
	return os.RemoveAll(lease.path)
}

type boundedTestQuotaLease struct {
	provider *boundedTestQuotaProvider
	path     string
	mount    WorkspaceQuotaMount
	bytes    int64
	once     sync.Once
	err      error
}

func (l *boundedTestQuotaLease) Path() string               { return l.path }
func (l *boundedTestQuotaLease) Mount() WorkspaceQuotaMount { return l.mount }
func (l *boundedTestQuotaLease) Release() error {
	l.once.Do(func() { l.err = l.provider.release(l) })
	return l.err
}

func TestBoundedTestQuotaExhaustionAndReuse(t *testing.T) {
	provider := newBoundedTestQuotaProvider(t.TempDir(), 10)
	first, err := provider.Provision(context.Background(), WorkspaceQuotaRequest{Root: provider.root, ID: "run-one", Bytes: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Provision(context.Background(), WorkspaceQuotaRequest{Root: provider.root, ID: "run-two", Bytes: 4}); !errors.Is(err, ErrWorkspaceQuotaExhausted) {
		t.Fatalf("expected bounded quota exhaustion, got %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Path()); !os.IsNotExist(err) {
		t.Fatalf("released quota workspace remains: %v", err)
	}
	second, err := provider.Provision(context.Background(), WorkspaceQuotaRequest{Root: provider.root, ID: "run-two", Bytes: 10})
	if err != nil {
		t.Fatalf("released capacity was not reusable: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedTestQuotaRejectsAdversarialRequests(t *testing.T) {
	provider := newBoundedTestQuotaProvider(t.TempDir(), 100)
	for _, request := range []WorkspaceQuotaRequest{
		{Root: provider.root, ID: "../escape", Bytes: 1},
		{Root: provider.root, ID: "run-zero", Bytes: 0},
		{Root: provider.root, ID: "run-negative", Bytes: -1},
		{Root: "/", ID: "run-root", Bytes: 1},
	} {
		if _, err := provider.Provision(context.Background(), request); err == nil {
			t.Fatalf("adversarial quota request was accepted: %#v", request)
		}
	}
	if len(provider.active) != 0 || provider.used != 0 {
		t.Fatalf("rejected requests changed quota state: active=%d used=%d", len(provider.active), provider.used)
	}
}

func TestBoundedTestQuotaConcurrentAdmissionNeverOvercommits(t *testing.T) {
	provider := newBoundedTestQuotaProvider(t.TempDir(), 10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var leases []WorkspaceQuotaLease
	var successes int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			lease, err := provider.Provision(context.Background(), WorkspaceQuotaRequest{
				Root: provider.root, ID: "run-concurrent-" + strconv.Itoa(index), Bytes: 3,
			})
			if err != nil {
				if !errors.Is(err, ErrWorkspaceQuotaExhausted) {
					t.Errorf("unexpected concurrent admission error: %v", err)
				}
				return
			}
			mu.Lock()
			successes++
			leases = append(leases, lease)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if successes > 3 {
		t.Fatalf("bounded provider overcommitted: admitted %d leases", successes)
	}
	for _, lease := range leases {
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.used != 0 || len(provider.active) != 0 {
		t.Fatalf("concurrent cleanup leaked quota: used=%d active=%d", provider.used, len(provider.active))
	}
}

func TestPodmanTmpfsQuotaMountRejectsPortableOptionRegression(t *testing.T) {
	plan, err := BuildHardenedSpec(testSandboxSpec(runner.BackendPodman), "/var/lib/agw-runner/workspaces/run-fixed", "podman")
	if err != nil {
		t.Fatal(err)
	}
	args, err := podmanArgsWithQuota("agw-fixed", plan, WorkspaceQuotaMount{
		Kind: WorkspaceQuotaTmpfs, Bytes: plan.Resources.DiskBytes, UID: 65532, GID: 65532,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "uid=") || strings.Contains(joined, "gid=") {
		t.Fatalf("Podman 4.9-incompatible tmpfs ownership options returned: %q", joined)
	}
	if !strings.Contains(joined, "--tmpfs\x00/workspace:rw,nosuid,nodev,size=2147483648,mode=01777") {
		t.Fatalf("workspace quota mount is not the portable isolated tmpfs form: %q", joined)
	}
}

func TestPodmanQuotaMountFailsClosedForInvalidProjectSource(t *testing.T) {
	if _, err := podmanArgsWithQuota("agw-fixed", HardenedSpec{
		Runtime: "podman", Image: "image@sha256:" + strings.Repeat("a", 64),
		RunAsUser: "1000", Resources: runner.ResourceLimits{DiskBytes: 1},
	}, WorkspaceQuotaMount{Kind: WorkspaceQuotaProject, Source: filepath.Join(t.TempDir(), "missing"), Bytes: 1, UID: 1000, GID: 1000}); err == nil {
		t.Fatal("invalid project quota source was accepted")
	}
}

type quotaStartFailureFactory struct{}

func (quotaStartFailureFactory) Start(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (PodmanProcess, error) {
	return nil, errors.New("simulated Podman start failure")
}

func TestPodmanProvisioningReleasesQuotaWhenRuntimeStartFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("rootless Podman backend is intentionally unavailable to root")
	}
	root := t.TempDir()
	provider := newBoundedTestQuotaProvider(root, 1<<20)
	backend, err := NewRootlessPodmanBackend(PodmanConfig{
		WorkspaceRoot: root, Factory: quotaStartFailureFactory{}, WorkspaceQuota: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := testSandboxSpec(runner.BackendPodman)
	spec.RunAsUser = strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	if _, err := backend.Start(context.Background(), spec); err == nil {
		t.Fatal("simulated Podman start failure was not returned")
	}
	provider.mu.Lock()
	active, used := len(provider.active), provider.used
	provider.mu.Unlock()
	if active != 0 || used != 0 {
		t.Fatalf("failed start leaked quota allocation: active=%d used=%d", active, used)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed start left workspace entries: %#v", entries)
	}
}
