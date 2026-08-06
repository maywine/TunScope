//go:build darwin && cgo

package tunscope

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
)

func TestDarwinMatcherFindsSelectedTCPFlow(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit a local listener: %v", err)
		}
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("listener did not accept the test connection")
	}
	defer server.Close()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := newProcessMatcher([]string{executable})
	if err != nil {
		t.Fatal(err)
	}
	defer matcher.Close()
	// The real engine must always trigger its self loop guard. This integration
	// test uses the test process as a stand-in for an independently selected app.
	matcher.(*darwinProcessMatcher).selfPID = -1

	source := netip.MustParseAddrPort(client.LocalAddr().String())
	destination := netip.MustParseAddrPort(client.RemoteAddr().String())
	decision, err := matcher.Decide(&M.Metadata{
		Network: M.TCP,
		SrcIP:   source.Addr(),
		SrcPort: source.Port(),
		DstIP:   destination.Addr(),
		DstPort: destination.Port(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision != processProxy {
		t.Fatalf("decision = %v, want processProxy", decision)
	}
}

func TestDarwinMatcherFindsSelectedUDPFlow(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit a local listener: %v", err)
		}
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.DialUDP("udp4", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := newProcessMatcher([]string{executable})
	if err != nil {
		t.Fatal(err)
	}
	defer matcher.Close()
	matcher.(*darwinProcessMatcher).selfPID = -1

	source := netip.MustParseAddrPort(client.LocalAddr().String())
	destination := netip.MustParseAddrPort(client.RemoteAddr().String())
	decision, err := matcher.Decide(&M.Metadata{
		Network: M.UDP,
		SrcIP:   source.Addr(),
		SrcPort: source.Port(),
		DstIP:   destination.Addr(),
		DstPort: destination.Port(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision != processProxy {
		t.Fatalf("decision = %v, want processProxy", decision)
	}
}

func TestDarwinMatcherInheritsSelectionFromParent(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit a local listener: %v", err)
		}
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	command := exec.Command("/usr/bin/nc", "-d", "127.0.0.1", fmt.Sprint(port))
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := newProcessMatcher([]string{executable})
	if err != nil {
		t.Fatal(err)
	}
	defer matcher.Close()

	source := netip.MustParseAddrPort(connection.RemoteAddr().String())
	destination := netip.MustParseAddrPort(connection.LocalAddr().String())
	decision, err := matcher.Decide(&M.Metadata{
		Network: M.TCP,
		SrcIP:   source.Addr(),
		SrcPort: source.Port(),
		DstIP:   destination.Addr(),
		DstPort: destination.Port(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision != processProxy {
		t.Fatalf("decision = %v, want processProxy for child process", decision)
	}
}

const testDarwinSelectedApp = "/Applications/Selected.app"

func testDarwinFlow(pid int, network M.Network) darwinFlow {
	return darwinFlow{
		pid:     pid,
		network: network,
		local:   netip.MustParseAddrPort("192.0.2.10:41000"),
		remote:  netip.MustParseAddrPort("203.0.113.20:443"),
	}
}

func testDarwinMetadata(network M.Network) *M.Metadata {
	return &M.Metadata{
		Network: network,
		SrcIP:   netip.MustParseAddr("192.0.2.10"),
		SrcPort: 41000,
		DstIP:   netip.MustParseAddr("203.0.113.20"),
		DstPort: 443,
	}
}

func testDarwinSelectedProcess() cachedProcess {
	return cachedProcess{path: testDarwinSelectedApp + "/Contents/MacOS/helper"}
}

func newTestDarwinMatcher(now func() time.Time, snapshot func(map[int]cachedProcess) (darwinProcessSnapshot, error)) *darwinProcessMatcher {
	return &darwinProcessMatcher{
		targets:     []string{testDarwinSelectedApp},
		processes:   make(map[int]cachedProcess),
		selected:    make(map[int]bool),
		recentFlows: make(darwinRecentFlowIndex),
		selfPID:     9000,
		snapshot:    snapshot,
		now:         now,
		sleep:       func(time.Duration) {},
	}
}

func testDarwinSelectedSnapshot(network M.Network) darwinProcessSnapshot {
	return darwinProcessSnapshot{
		processes: map[int]cachedProcess{42: testDarwinSelectedProcess()},
		flows:     []darwinFlow{testDarwinFlow(42, network)},
	}
}

func TestDarwinMatcherConcurrentMissesShareSnapshotWithoutHoldingLock(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	var scans atomic.Int32
	matcher := newTestDarwinMatcher(func() time.Time { return fixedNow }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
		if scans.Add(1) == 1 {
			close(started)
		}
		<-release
		return testDarwinSelectedSnapshot(M.TCP), nil
	})

	const callers = 16
	decisions := make([]processDecision, callers)
	errors := make([]error, callers)
	startAll := make(chan struct{})
	var callersDone sync.WaitGroup
	callersDone.Add(callers)
	for i := 0; i < callers; i++ {
		go func(index int) {
			defer callersDone.Done()
			<-startAll
			decisions[index], errors[index] = matcher.Decide(testDarwinMetadata(M.TCP))
		}(i)
	}
	close(startAll)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("system snapshot did not start")
	}
	lockAvailable := make(chan struct{})
	go func() {
		matcher.mu.Lock()
		matcher.mu.Unlock()
		close(lockAvailable)
	}()
	select {
	case <-lockAvailable:
	case <-time.After(250 * time.Millisecond):
		close(release)
		t.Fatal("matcher lock was held while the system snapshot was blocked")
	}
	if got := scans.Load(); got != 1 {
		close(release)
		t.Fatalf("snapshots while first refresh is in flight = %d, want 1", got)
	}
	close(release)

	done := make(chan struct{})
	go func() {
		callersDone.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent decisions did not finish")
	}
	if got := scans.Load(); got != 1 {
		t.Fatalf("shared snapshots = %d, want 1", got)
	}
	for i := range decisions {
		if errors[i] != nil {
			t.Fatalf("caller %d: %v", i, errors[i])
		}
		if decisions[i] != processProxy {
			t.Fatalf("caller %d decision = %v, want processProxy", i, decisions[i])
		}
	}
}

func TestDarwinMatcherRetriesWithoutHoldingLockDuringSleep(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	var scans int
	matcher := newTestDarwinMatcher(func() time.Time { return fixedNow }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
		scans++
		if scans <= len(darwinOwnerRetryDelays) {
			return darwinProcessSnapshot{processes: make(map[int]cachedProcess)}, nil
		}
		return testDarwinSelectedSnapshot(M.TCP), nil
	})

	sleeping := make(chan struct{})
	resume := make(chan struct{})
	var firstSleep sync.Once
	var delays []time.Duration
	matcher.sleep = func(delay time.Duration) {
		delays = append(delays, delay)
		firstSleep.Do(func() {
			close(sleeping)
			<-resume
		})
	}

	decisionResult := make(chan processDecision, 1)
	errorResult := make(chan error, 1)
	go func() {
		decision, err := matcher.Decide(testDarwinMetadata(M.TCP))
		decisionResult <- decision
		errorResult <- err
	}()
	select {
	case <-sleeping:
	case <-time.After(2 * time.Second):
		t.Fatal("matcher did not enter retry sleep")
	}
	lockAvailable := make(chan struct{})
	go func() {
		matcher.mu.Lock()
		matcher.mu.Unlock()
		close(lockAvailable)
	}()
	select {
	case <-lockAvailable:
	case <-time.After(250 * time.Millisecond):
		close(resume)
		t.Fatal("matcher lock was held during retry sleep")
	}
	close(resume)

	if err := <-errorResult; err != nil {
		t.Fatal(err)
	}
	if decision := <-decisionResult; decision != processProxy {
		t.Fatalf("decision = %v, want processProxy", decision)
	}
	if scans != 1+len(darwinOwnerRetryDelays) {
		t.Fatalf("snapshots = %d, want %d", scans, 1+len(darwinOwnerRetryDelays))
	}
	if len(delays) != len(darwinOwnerRetryDelays) {
		t.Fatalf("retry sleeps = %v, want %v", delays, darwinOwnerRetryDelays)
	}
	var retryWindow time.Duration
	for i, delay := range darwinOwnerRetryDelays {
		if delays[i] != delay {
			t.Fatalf("retry delay %d = %v, want %v", i, delays[i], delay)
		}
		retryWindow += delay
	}
	if retryWindow < 50*time.Millisecond || retryWindow > 80*time.Millisecond {
		t.Fatalf("retry window = %v, want 50-80ms", retryWindow)
	}
}

func TestDarwinMatcherRecentSelectedFlowSurvivesProcessExitThenExpires(t *testing.T) {
	current := time.Unix(1_700_000_000, 0)
	var scans int
	matcher := newTestDarwinMatcher(func() time.Time { return current }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
		scans++
		if scans == 1 {
			return testDarwinSelectedSnapshot(M.TCP), nil
		}
		return darwinProcessSnapshot{processes: make(map[int]cachedProcess)}, nil
	})
	metadata := testDarwinMetadata(M.TCP)

	decision, err := matcher.Decide(metadata)
	if err != nil || decision != processProxy {
		t.Fatalf("initial decision = %v, err = %v, want processProxy", decision, err)
	}
	current = current.Add(darwinSnapshotFreshness + time.Millisecond)
	if err := matcher.refresh(false, 0); err != nil {
		t.Fatal(err)
	}
	decision, err = matcher.Decide(metadata)
	if err != nil || decision != processProxy {
		t.Fatalf("decision after process exit = %v, err = %v, want cached processProxy", decision, err)
	}

	current = current.Add(darwinRecentFlowTTL)
	decision, err = matcher.Decide(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if decision != processProxy {
		t.Fatalf("selected decision after short flow TTL = %v, want processProxy", decision)
	}

	current = current.Add(darwinSelectedFlowTTL)
	decision, err = matcher.Decide(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if decision != processUnknown {
		t.Fatalf("decision after selected-flow TTL = %v, want processUnknown", decision)
	}
}

func TestDarwinMatcherDirectDecisionIsAlwaysReconfirmed(t *testing.T) {
	current := time.Unix(1_700_000_000, 0)
	var scans int
	present := true
	directSnapshot := darwinProcessSnapshot{
		processes: map[int]cachedProcess{77: {path: "/usr/bin/direct"}},
		flows:     []darwinFlow{testDarwinFlow(77, M.TCP)},
	}
	matcher := newTestDarwinMatcher(func() time.Time { return current }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
		scans++
		if present {
			return directSnapshot, nil
		}
		return darwinProcessSnapshot{processes: make(map[int]cachedProcess)}, nil
	})
	metadata := testDarwinMetadata(M.TCP)

	decision, err := matcher.Decide(metadata)
	if err != nil || decision != processDirect {
		t.Fatalf("initial decision = %v, err = %v, want processDirect", decision, err)
	}
	decision, err = matcher.Decide(metadata)
	if err != nil || decision != processDirect || scans != 2 {
		t.Fatalf("reconfirmed decision = %v, err = %v, scans = %d; want direct from a second snapshot", decision, err, scans)
	}

	present = false
	decision, err = matcher.Decide(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if decision != processUnknown {
		t.Fatalf("decision after direct flow disappeared = %v, want processUnknown", decision)
	}
}

func TestDarwinMatcherLocalEndpointFallbackRequiresConsistentDecision(t *testing.T) {
	tests := []struct {
		name      string
		processes map[int]cachedProcess
		flows     []darwinFlow
		selfPID   int
		want      processDecision
	}{
		{
			name: "same selected decision",
			processes: map[int]cachedProcess{
				41: testDarwinSelectedProcess(),
				42: testDarwinSelectedProcess(),
			},
			flows:   []darwinFlow{testDarwinFlow(41, M.TCP), testDarwinFlow(42, M.TCP)},
			selfPID: 9000,
			want:    processProxy,
		},
		{
			name: "selected and direct conflict",
			processes: map[int]cachedProcess{
				41: testDarwinSelectedProcess(),
				77: {path: "/usr/bin/direct"},
			},
			flows:   []darwinFlow{testDarwinFlow(41, M.TCP), testDarwinFlow(77, M.TCP)},
			selfPID: 9000,
			want:    processReject,
		},
		{
			name: "selected self is still loop guard",
			processes: map[int]cachedProcess{
				41: testDarwinSelectedProcess(),
			},
			flows:   []darwinFlow{testDarwinFlow(41, M.TCP)},
			selfPID: 41,
			want:    processReject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixedNow := time.Unix(1_700_000_000, 0)
			flows := append([]darwinFlow(nil), tt.flows...)
			for i := range flows {
				// Force the local-only path by presenting a remote endpoint which
				// does not yet agree with the intercepted packet.
				flows[i].remote = netip.MustParseAddrPort("198.51.100.30:8443")
			}
			matcher := newTestDarwinMatcher(func() time.Time { return fixedNow }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
				return darwinProcessSnapshot{processes: tt.processes, flows: flows}, nil
			})
			matcher.selfPID = tt.selfPID
			decision, err := matcher.Decide(testDarwinMetadata(M.TCP))
			if err != nil {
				t.Fatal(err)
			}
			if decision != tt.want {
				t.Fatalf("decision = %v, want %v", decision, tt.want)
			}
		})
	}
}

func TestDarwinMatcherStaleUDPProxyAndUnknownOutrankReusedDirectTuple(t *testing.T) {
	tests := []struct {
		name    string
		oldPID  int
		selfPID int
		want    processDecision
	}{
		{name: "stale selected conflicts with current direct", oldPID: 42, selfPID: 9000, want: processReject},
		{name: "self loop guard", oldPID: 9000, selfPID: 9000, want: processReject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := time.Unix(1_700_000_000, 0)
			stage := 0
			matcher := newTestDarwinMatcher(func() time.Time { return current }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
				switch stage {
				case 0:
					process := testDarwinSelectedProcess()
					return darwinProcessSnapshot{
						processes: map[int]cachedProcess{tt.oldPID: process},
						flows:     []darwinFlow{testDarwinFlow(tt.oldPID, M.UDP)},
					}, nil
				default:
					return darwinProcessSnapshot{
						processes: map[int]cachedProcess{77: {path: "/usr/bin/direct"}},
						flows:     []darwinFlow{testDarwinFlow(77, M.UDP)},
					}, nil
				}
			})
			matcher.selfPID = tt.selfPID
			metadata := testDarwinMetadata(M.UDP)
			if _, err := matcher.Decide(metadata); err != nil {
				t.Fatal(err)
			}

			stage = 1
			current = current.Add(darwinSnapshotFreshness + time.Millisecond)
			if err := matcher.refresh(false, 0); err != nil {
				t.Fatal(err)
			}
			decision, found, _ := matcher.cachedDecision(metadata)
			if !found || decision != tt.want {
				t.Fatalf("reused UDP tuple decision = %v, found = %v, want %v", decision, found, tt.want)
			}

			current = current.Add(darwinSelectedFlowTTL)
			if err := matcher.refresh(false, 0); err != nil {
				t.Fatal(err)
			}
			decision, found, _ = matcher.cachedDecision(metadata)
			if !found || decision != processDirect {
				t.Fatalf("decision after stale safety entry expired = %v, found = %v, want processDirect", decision, found)
			}
		})
	}
}

func TestDarwinMatcherDecisionIndexSeparatesNetworkAndSourcePort(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	matcher := newTestDarwinMatcher(func() time.Time { return fixedNow }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
		return darwinProcessSnapshot{}, nil
	})
	matcher.updatedAt = fixedNow
	until := fixedNow.Add(time.Second)
	targetFlow := testDarwinFlow(42, M.TCP)
	targetDecisions := darwinFlowDecisions{}
	targetDecisions.remember(processProxy, until)
	matcher.recentFlows[darwinFlowBucket{network: M.TCP, localPort: 41000}] = map[darwinFlow]darwinFlowDecisions{
		targetFlow: targetDecisions,
	}

	conflict := darwinFlowDecisions{}
	conflict.remember(processUnknown, until)
	matcher.recentFlows[darwinFlowBucket{network: M.UDP, localPort: 41000}] = map[darwinFlow]darwinFlowDecisions{
		testDarwinFlow(9000, M.UDP): conflict,
	}
	otherPortFlow := testDarwinFlow(9000, M.TCP)
	otherPortFlow.local = netip.MustParseAddrPort("192.0.2.10:42000")
	matcher.recentFlows[darwinFlowBucket{network: M.TCP, localPort: 42000}] = map[darwinFlow]darwinFlowDecisions{
		otherPortFlow: conflict,
	}

	decision, found, _ := matcher.cachedDecision(testDarwinMetadata(M.TCP))
	if !found || decision != processProxy {
		t.Fatalf("indexed decision = %v, found = %v, want processProxy", decision, found)
	}
}

func TestRefreshResolvedTargetsTracksMovingCurrentSymlink(t *testing.T) {
	root := t.TempDir()
	version1 := filepath.Join(root, "1.0", "Updater.app")
	version2 := filepath.Join(root, "2.0", "Updater.app")
	if err := os.MkdirAll(version1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(version2, 0755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "Current")
	if err := os.Symlink(filepath.Join(root, "1.0"), current); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(current, "Updater.app")
	resolved1, err := filepath.EvalSymlinks(configured)
	if err != nil {
		t.Fatal(err)
	}
	targets := refreshResolvedTargets([]string{configured}, []string{resolved1})

	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "2.0"), current); err != nil {
		t.Fatal(err)
	}
	targets = refreshResolvedTargets([]string{configured}, targets)
	resolved2, err := filepath.EvalSymlinks(configured)
	if err != nil {
		t.Fatal(err)
	}
	if !pathIsSelected(filepath.Join(resolved1, "Contents", "MacOS", "Updater"), targets) {
		t.Fatal("previous updater process stopped matching after Current moved")
	}
	if !pathIsSelected(filepath.Join(resolved2, "Contents", "MacOS", "Updater"), targets) {
		t.Fatal("new updater process did not match the refreshed Current target")
	}
}

func TestDarwinMatcherRetriesTransientEmptyProcessPath(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	var scans int
	matcher := newTestDarwinMatcher(func() time.Time { return fixedNow }, func(map[int]cachedProcess) (darwinProcessSnapshot, error) {
		scans++
		process := cachedProcess{}
		if scans > 1 {
			process = testDarwinSelectedProcess()
		}
		return darwinProcessSnapshot{
			processes: map[int]cachedProcess{42: process},
			flows:     []darwinFlow{testDarwinFlow(42, M.TCP)},
		}, nil
	})
	decision, err := matcher.Decide(testDarwinMetadata(M.TCP))
	if err != nil {
		t.Fatal(err)
	}
	if decision != processProxy || scans != 2 {
		t.Fatalf("decision = %v after %d snapshots, want processProxy after retry", decision, scans)
	}
}

func TestProcessCacheNeedsRefresh(t *testing.T) {
	cached := cachedProcess{path: "/usr/bin/app", startSec: 10, startUsec: 20}
	tests := []struct {
		name      string
		cached    cachedProcess
		found     bool
		startSec  int64
		startUsec int64
		want      bool
	}{
		{name: "missing", cached: cached, found: false, startSec: 10, startUsec: 20, want: true},
		{name: "empty path", cached: cachedProcess{startSec: 10, startUsec: 20}, found: true, startSec: 10, startUsec: 20, want: true},
		{name: "PID reused", cached: cached, found: true, startSec: 11, startUsec: 20, want: true},
		{name: "same process", cached: cached, found: true, startSec: 10, startUsec: 20, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := processCacheNeedsRefresh(tt.cached, tt.found, tt.startSec, tt.startUsec); got != tt.want {
				t.Fatalf("processCacheNeedsRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}
