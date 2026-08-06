//go:build darwin && cgo

package tunscope

/*
#cgo LDFLAGS: -lproc
#include <arpa/inet.h>
#include <libproc.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>

typedef struct {
	int pid;
	int protocol;
	int family;
	unsigned short local_port;
	unsigned short remote_port;
	unsigned char local_addr[16];
	unsigned char remote_addr[16];
} tunscope_flow;

static int tunscope_snapshot_flows(const int *pids, int pid_count, tunscope_flow **result) {
	int capacity = 256;
	int count = 0;
	tunscope_flow *flows = (tunscope_flow *)calloc((size_t)capacity, sizeof(tunscope_flow));
	if (flows == NULL) return -1;

	for (int p = 0; p < pid_count; p++) {
		int pid = pids[p];
		int buffer_size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
		if (buffer_size <= 0) continue;
		struct proc_fdinfo *fds = (struct proc_fdinfo *)malloc((size_t)buffer_size);
		if (fds == NULL) continue;
		int used = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, buffer_size);
		if (used <= 0) {
			free(fds);
			continue;
		}
		int fd_count = used / PROC_PIDLISTFD_SIZE;
		for (int i = 0; i < fd_count; i++) {
			if (fds[i].proc_fdtype != PROX_FDTYPE_SOCKET) continue;
			struct socket_fdinfo socket_info;
			memset(&socket_info, 0, sizeof(socket_info));
			int bytes = proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDSOCKETINFO,
				&socket_info, PROC_PIDFDSOCKETINFO_SIZE);
			if (bytes != PROC_PIDFDSOCKETINFO_SIZE) continue;
			if (socket_info.psi.soi_family != AF_INET && socket_info.psi.soi_family != AF_INET6) continue;
			if (socket_info.psi.soi_protocol != IPPROTO_TCP && socket_info.psi.soi_protocol != IPPROTO_UDP) continue;

			struct in_sockinfo *in = NULL;
			if (socket_info.psi.soi_kind == SOCKINFO_TCP) {
				in = &socket_info.psi.soi_proto.pri_tcp.tcpsi_ini;
			} else if (socket_info.psi.soi_kind == SOCKINFO_IN) {
				in = &socket_info.psi.soi_proto.pri_in;
			}
			if (in == NULL || ntohs((uint16_t)in->insi_lport) == 0) continue;

			if (count == capacity) {
				capacity *= 2;
				tunscope_flow *grown = (tunscope_flow *)realloc(flows, (size_t)capacity * sizeof(tunscope_flow));
				if (grown == NULL) {
					free(fds);
					free(flows);
					return -1;
				}
				flows = grown;
			}

			tunscope_flow *flow = &flows[count++];
			memset(flow, 0, sizeof(*flow));
			flow->pid = pid;
			flow->protocol = socket_info.psi.soi_protocol;
			flow->family = socket_info.psi.soi_family;
			flow->local_port = ntohs((uint16_t)in->insi_lport);
			flow->remote_port = ntohs((uint16_t)in->insi_fport);
			if (flow->family == AF_INET) {
				memcpy(flow->local_addr, &in->insi_laddr.ina_46.i46a_addr4, 4);
				memcpy(flow->remote_addr, &in->insi_faddr.ina_46.i46a_addr4, 4);
			} else {
				memcpy(flow->local_addr, &in->insi_laddr.ina_6, 16);
				memcpy(flow->remote_addr, &in->insi_faddr.ina_6, 16);
			}
		}
		free(fds);
	}

	*result = flows;
	return count;
}
*/
import "C"

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"golang.org/x/sys/unix"
)

type cachedProcess struct {
	path      string
	parentPID int
	startSec  int64
	startUsec int64
}

type darwinFlow struct {
	pid     int
	network M.Network
	local   netip.AddrPort
	remote  netip.AddrPort
}

const (
	darwinSnapshotFreshness = 75 * time.Millisecond
	darwinRecentFlowTTL     = 750 * time.Millisecond
	// Chromium retries a rejected QUIC flow for several seconds. Retain a
	// positively identified selected/self tuple across that retry window so a
	// later packet cannot decay to unknown-direct and bypass TCP-only mode.
	darwinSelectedFlowTTL = 30 * time.Second
)

var darwinOwnerRetryDelays = [...]time.Duration{
	3 * time.Millisecond,
	8 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
}

type darwinProcessSnapshot struct {
	processes map[int]cachedProcess
	flows     []darwinFlow
}

type darwinFlowDecisions struct {
	proxyUntil   time.Time
	rejectUntil  time.Time
	unknownUntil time.Time
	directUntil  time.Time
}

type darwinFlowBucket struct {
	network   M.Network
	localPort uint16
}

type darwinRecentFlowIndex map[darwinFlowBucket]map[darwinFlow]darwinFlowDecisions

func (d *darwinFlowDecisions) remember(decision processDecision, until time.Time) {
	switch decision {
	case processProxy:
		if until.After(d.proxyUntil) {
			d.proxyUntil = until
		}
	case processReject:
		if until.After(d.rejectUntil) {
			d.rejectUntil = until
		}
	case processUnknown:
		if until.After(d.unknownUntil) {
			d.unknownUntil = until
		}
	case processDirect:
		if until.After(d.directUntil) {
			d.directUntil = until
		}
	}
}

func (d darwinFlowDecisions) decide(now time.Time) (processDecision, bool) {
	if now.Before(d.rejectUntil) {
		return processReject, true
	}
	if now.Before(d.proxyUntil) {
		return processProxy, true
	}
	if now.Before(d.unknownUntil) {
		return processUnknown, true
	}
	if now.Before(d.directUntil) {
		return processDirect, true
	}
	return processUnknown, false
}

type darwinRefresh struct {
	done chan struct{}
	err  error
}

type darwinProcessMatcher struct {
	mu                sync.Mutex
	configuredTargets []string
	targets           []string
	processes         map[int]cachedProcess
	selected          map[int]bool
	recentFlows       darwinRecentFlowIndex
	updatedAt         time.Time
	generation        uint64
	refreshing        *darwinRefresh
	selfPID           int
	snapshot          func(map[int]cachedProcess) (darwinProcessSnapshot, error)
	now               func() time.Time
	sleep             func(time.Duration)
}

func newProcessMatcher(applicationPaths []string) (processMatcher, error) {
	if len(applicationPaths) == 0 {
		return nil, fmt.Errorf("at least one application is required")
	}
	configuredTargets := make([]string, 0, len(applicationPaths))
	targets := make([]string, 0, len(applicationPaths))
	for _, path := range applicationPaths {
		configured := filepath.Clean(path)
		resolved, err := filepath.EvalSymlinks(configured)
		if err != nil {
			return nil, err
		}
		configuredTargets = append(configuredTargets, configured)
		targets = append(targets, resolved)
	}
	return &darwinProcessMatcher{
		configuredTargets: configuredTargets,
		targets:           targets,
		processes:         make(map[int]cachedProcess),
		selected:          make(map[int]bool),
		recentFlows:       make(darwinRecentFlowIndex),
		selfPID:           os.Getpid(),
		snapshot:          snapshotDarwinProcesses,
		now:               time.Now,
		sleep:             time.Sleep,
	}, nil
}

func (m *darwinProcessMatcher) Decide(metadata *M.Metadata) (processDecision, error) {
	// Proxy and loop-guard decisions are safe to reuse immediately. Direct is
	// never reused without a new snapshot: a selected UDP socket can otherwise
	// inherit the five-tuple of a direct socket which closed just after even a
	// very recent snapshot.
	decision, found, generation := m.cachedDecision(metadata)
	if found && decision != processDirect {
		return decision, nil
	}

	force := found && decision == processDirect
	if err := m.refresh(force, generation); err != nil {
		return processUnknown, err
	}
	decision, found, generation = m.cachedDecision(metadata)
	if found {
		return decision, nil
	}

	for _, delay := range darwinOwnerRetryDelays {
		m.sleep(delay)
		if err := m.refresh(true, generation); err != nil {
			return processUnknown, err
		}
		decision, found, generation = m.cachedDecision(metadata)
		if found {
			return decision, nil
		}
	}
	return processUnknown, nil
}

func (m *darwinProcessMatcher) Close() error { return nil }

// refresh performs at most one system-wide snapshot at a time. A forced retry
// names the generation on which it missed. If another caller already advanced
// that generation, its completed refresh is shared rather than scanning again.
func (m *darwinProcessMatcher) refresh(force bool, observedGeneration uint64) error {
	m.mu.Lock()
	now := m.now()
	if force && m.generation != observedGeneration {
		m.mu.Unlock()
		return nil
	}
	if inFlight := m.refreshing; inFlight != nil {
		m.mu.Unlock()
		<-inFlight.done
		return inFlight.err
	}
	if !force && !m.updatedAt.IsZero() && now.Sub(m.updatedAt) < darwinSnapshotFreshness {
		m.mu.Unlock()
		return nil
	}

	inFlight := &darwinRefresh{done: make(chan struct{})}
	m.refreshing = inFlight
	previous := m.processes
	previousFlows := m.recentFlows
	configuredTargets := append([]string(nil), m.configuredTargets...)
	targets := append([]string(nil), m.targets...)
	snapshot := m.snapshot
	m.mu.Unlock()

	result, err := snapshot(previous)
	var selected map[int]bool
	var recentFlows darwinRecentFlowIndex
	completedAt := m.now()
	if err == nil {
		targets = refreshResolvedTargets(configuredTargets, targets)
		selected = selectProcesses(result.processes, targets)
		recentFlows = buildDarwinRecentFlowIndex(previousFlows, result, selected, m.selfPID, completedAt)
	}

	m.mu.Lock()
	if err == nil {
		m.processes = result.processes
		m.selected = selected
		m.recentFlows = recentFlows
		m.targets = targets
		m.updatedAt = completedAt
		m.generation++
	}
	inFlight.err = err
	m.refreshing = nil
	close(inFlight.done)
	m.mu.Unlock()
	return err
}

func snapshotDarwinProcesses(previous map[int]cachedProcess) (darwinProcessSnapshot, error) {
	processList, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return darwinProcessSnapshot{}, fmt.Errorf("list processes: %w", err)
	}

	active := make(map[int]cachedProcess, len(processList))
	pids := make([]C.int, 0, len(processList))
	for i := range processList {
		entry := &processList[i]
		pid := int(entry.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		startSec := int64(entry.Proc.P_starttime.Sec)
		startUsec := int64(entry.Proc.P_starttime.Usec)
		cached, ok := previous[pid]
		if processCacheNeedsRefresh(cached, ok, startSec, startUsec) {
			cached = cachedProcess{
				path:      processPath(pid),
				parentPID: int(entry.Eproc.Ppid),
				startSec:  startSec,
				startUsec: startUsec,
			}
		} else {
			cached.parentPID = int(entry.Eproc.Ppid)
		}
		active[pid] = cached
		pids = append(pids, C.int(pid))
	}
	var raw *C.tunscope_flow
	count := 0
	if len(pids) > 0 {
		count = int(C.tunscope_snapshot_flows((*C.int)(unsafe.Pointer(&pids[0])), C.int(len(pids)), &raw))
	}
	if count < 0 {
		return darwinProcessSnapshot{}, fmt.Errorf("snapshot process sockets failed")
	}
	if raw != nil {
		defer C.free(unsafe.Pointer(raw))
	}
	flows := make([]darwinFlow, 0, count)
	for _, item := range unsafe.Slice(raw, count) {
		flow, ok := convertDarwinFlow(item)
		if ok {
			flows = append(flows, flow)
		}
	}
	return darwinProcessSnapshot{processes: active, flows: flows}, nil
}

func processCacheNeedsRefresh(cached cachedProcess, found bool, startSec, startUsec int64) bool {
	return !found || cached.startSec != startSec || cached.startUsec != startUsec || cached.path == ""
}

func processPath(pid int) string {
	buffer := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	length := int(C.proc_pidpath(C.int(pid), unsafe.Pointer(&buffer[0]), C.uint32_t(len(buffer))))
	if length <= 0 {
		return ""
	}
	return string(buffer[:length])
}

func refreshResolvedTargets(configured, known []string) []string {
	result := append([]string(nil), known...)
	seen := make(map[string]struct{}, len(result))
	for _, target := range result {
		seen[target] = struct{}{}
	}
	for _, configuredPath := range configured {
		resolved, err := filepath.EvalSymlinks(configuredPath)
		if err != nil {
			// Updaters may replace Current non-atomically. Retain the last valid
			// target and try again on the next process snapshot.
			continue
		}
		if _, exists := seen[resolved]; exists {
			continue
		}
		seen[resolved] = struct{}{}
		result = append(result, resolved)
	}
	return result
}

func selectProcesses(processes map[int]cachedProcess, targets []string) map[int]bool {
	selected := make(map[int]bool, len(processes))
	visiting := make(map[int]bool)
	var isSelected func(int) bool
	isSelected = func(pid int) bool {
		if value, ok := selected[pid]; ok {
			return value
		}
		if visiting[pid] {
			return false
		}
		process, ok := processes[pid]
		if !ok {
			return false
		}
		visiting[pid] = true
		value := pathIsSelected(process.path, targets)
		if !value && process.parentPID > 0 && process.parentPID != pid {
			value = isSelected(process.parentPID)
		}
		delete(visiting, pid)
		selected[pid] = value
		return value
	}
	for pid := range processes {
		isSelected(pid)
	}
	return selected
}

func pathIsSelected(path string, targets []string) bool {
	for _, target := range targets {
		if path == target {
			return true
		}
		if strings.EqualFold(filepath.Ext(target), ".app") && strings.HasPrefix(path, target+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func buildDarwinRecentFlowIndex(previous darwinRecentFlowIndex, snapshot darwinProcessSnapshot, selected map[int]bool, selfPID int, observedAt time.Time) darwinRecentFlowIndex {
	// The published index is immutable. Clone and prune it while the expensive
	// system snapshot is in flight so readers only hold the matcher lock long
	// enough to fetch one source-port bucket.
	index := make(darwinRecentFlowIndex, len(previous))
	for bucket, flows := range previous {
		active := make(map[darwinFlow]darwinFlowDecisions, len(flows))
		for flow, decisions := range flows {
			// Direct is valid only for the snapshot which observed it. Proxy and
			// fail-closed loop-guard observations survive briefly because those are
			// safe when a short-lived socket disappears before the next snapshot.
			decisions.directUntil = time.Time{}
			if _, found := decisions.decide(observedAt); found {
				active[flow] = decisions
			}
		}
		if len(active) > 0 {
			index[bucket] = active
		}
	}

	for _, flow := range snapshot.flows {
		decision := processUnknown
		remember := true
		switch {
		case flow.pid == selfPID:
			// A physical-interface socket created by this engine must never be
			// processed a second time. Keep this distinct from a normal lookup
			// miss, because normal unknown owners intentionally stay direct.
			decision = processReject
		case selected[flow.pid]:
			decision = processProxy
		default:
			if process, known := snapshot.processes[flow.pid]; known && process.path != "" {
				decision = processDirect
			} else {
				// Do not turn a transient proc_pidpath failure into a cached owner.
				// Leaving it unmatched makes Decide retry snapshots, then fail closed.
				remember = false
			}
		}
		if !remember {
			continue
		}
		until := observedAt.Add(darwinRecentFlowTTL)
		if decision == processProxy || decision == processReject {
			until = observedAt.Add(darwinSelectedFlowTTL)
		}

		bucket := darwinFlowBucket{network: flow.network, localPort: flow.local.Port()}
		flows := index[bucket]
		if flows == nil {
			flows = make(map[darwinFlow]darwinFlowDecisions)
			index[bucket] = flows
		}
		decisions := flows[flow]
		decisions.remember(decision, until)
		flows[flow] = decisions
	}
	return index
}

func (m *darwinProcessMatcher) cachedDecision(metadata *M.Metadata) (processDecision, bool, uint64) {
	m.mu.Lock()
	now := m.now()
	generation := m.generation
	flows := m.recentFlows[darwinFlowBucket{network: metadata.Network, localPort: metadata.SrcPort}]
	m.mu.Unlock()

	// Buckets are immutable after publication, so matching does not serialize
	// unrelated Decide calls on the global matcher lock.
	hasProxy := false
	hasReject := false
	hasUnknown := false
	hasDirect := false
	for flow, decisions := range flows {
		decision, active := decisions.decide(now)
		if !active || !flowExactlyMatchesMetadata(flow, metadata) {
			continue
		}
		switch decision {
		case processProxy:
			hasProxy = true
		case processReject:
			hasReject = true
		case processUnknown:
			hasUnknown = true
		case processDirect:
			hasDirect = true
		}
	}
	if decision, found := prioritizedDarwinDecision(hasReject, hasProxy, hasUnknown, hasDirect); found {
		return decision, true, generation
	}

	// A socket's remote endpoint can be temporarily absent or lag the packet
	// during connect. Fall back to its local endpoint only if every live cached
	// candidate agrees. Any selected/direct/self conflict fails closed.
	var fallback processDecision
	fallbackFound := false
	for flow, decisions := range flows {
		decision, active := decisions.decide(now)
		if !active || !flowLocalMatchesMetadata(flow, metadata) {
			continue
		}
		if !fallbackFound {
			fallback = decision
			fallbackFound = true
			continue
		}
		if fallback != decision {
			return processReject, true, generation
		}
	}
	if fallbackFound {
		return fallback, true, generation
	}
	return processUnknown, false, generation
}

func prioritizedDarwinDecision(hasReject, hasProxy, hasUnknown, hasDirect bool) (processDecision, bool) {
	switch {
	case hasReject:
		return processReject, true
	case hasProxy && (hasDirect || hasUnknown):
		return processReject, true
	case hasProxy:
		return processProxy, true
	case hasUnknown:
		return processUnknown, true
	case hasDirect:
		return processDirect, true
	default:
		return processUnknown, false
	}
}

func flowExactlyMatchesMetadata(flow darwinFlow, metadata *M.Metadata) bool {
	return flowLocalMatchesMetadata(flow, metadata) &&
		flow.local.Addr().IsValid() && !flow.local.Addr().IsUnspecified() &&
		flow.remote.Port() == metadata.DstPort && flow.remote.Port() != 0 &&
		flow.remote.Addr().IsValid() && !flow.remote.Addr().IsUnspecified() &&
		addressMatches(flow.remote.Addr(), metadata.DstIP)
}

func flowLocalMatchesMetadata(flow darwinFlow, metadata *M.Metadata) bool {
	return flow.network == metadata.Network &&
		flow.local.Port() == metadata.SrcPort &&
		addressMatches(flow.local.Addr(), metadata.SrcIP)
}

func addressMatches(socketAddress, packetAddress netip.Addr) bool {
	if !socketAddress.IsValid() || socketAddress.IsUnspecified() {
		return true
	}
	return socketAddress.Unmap() == packetAddress.Unmap()
}

func convertDarwinFlow(item C.tunscope_flow) (darwinFlow, bool) {
	var network M.Network
	switch int(item.protocol) {
	case C.IPPROTO_TCP:
		network = M.TCP
	case C.IPPROTO_UDP:
		network = M.UDP
	default:
		return darwinFlow{}, false
	}
	local, remote, ok := darwinAddresses(item)
	if !ok {
		return darwinFlow{}, false
	}
	return darwinFlow{
		pid:     int(item.pid),
		network: network,
		local:   netip.AddrPortFrom(local, uint16(item.local_port)),
		remote:  netip.AddrPortFrom(remote, uint16(item.remote_port)),
	}, true
}

func darwinAddresses(item C.tunscope_flow) (netip.Addr, netip.Addr, bool) {
	switch int(item.family) {
	case C.AF_INET:
		var localBytes, remoteBytes [4]byte
		for i := 0; i < 4; i++ {
			localBytes[i] = byte(item.local_addr[i])
			remoteBytes[i] = byte(item.remote_addr[i])
		}
		return netip.AddrFrom4(localBytes), netip.AddrFrom4(remoteBytes), true
	case C.AF_INET6:
		var localBytes, remoteBytes [16]byte
		for i := 0; i < 16; i++ {
			localBytes[i] = byte(item.local_addr[i])
			remoteBytes[i] = byte(item.remote_addr[i])
		}
		return netip.AddrFrom16(localBytes), netip.AddrFrom16(remoteBytes), true
	default:
		return netip.Addr{}, netip.Addr{}, false
	}
}
