//go:build windows

package tunscope

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"golang.org/x/sys/windows"
)

const (
	windowsTCPTableOwnerPIDAll = 5
	windowsUDPTableOwnerPID    = 1
	windowsTCPStateClosed      = 1
	windowsTCPStateListen      = 2
	windowsTCPStateDeleteTCB   = 12
	windowsSnapshotFreshness   = 75 * time.Millisecond
)

var (
	ipHelperDLL              = windows.NewLazySystemDLL("iphlpapi.dll")
	getExtendedTCPTableProc  = ipHelperDLL.NewProc("GetExtendedTcpTable")
	getExtendedUDPTableProc  = ipHelperDLL.NewProc("GetExtendedUdpTable")
	processKernel32DLL       = windows.NewLazySystemDLL("kernel32.dll")
	getPackageFamilyNameProc = processKernel32DLL.NewProc("GetPackageFamilyName")
	windowsOwnerRetryDelays  = [...]time.Duration{
		3 * time.Millisecond,
		8 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
	}
)

type windowsProcess struct {
	parentPID     int
	name          string
	path          string
	packageFamily string
	known         bool
}

type windowsFlow struct {
	pid     int
	network M.Network
	local   netip.AddrPort
	remote  netip.AddrPort
}

type windowsProcessSnapshot struct {
	processes map[int]windowsProcess
	selected  map[int]bool
	flows     []windowsFlow
}

type windowsRefresh struct {
	done chan struct{}
	err  error
}

type windowsProcessMatcher struct {
	mu                sync.Mutex
	configuredTargets []string
	targets           []string
	packageFamilies   []string
	snapshot          windowsProcessSnapshot
	updatedAt         time.Time
	generation        uint64
	refreshing        *windowsRefresh
	selfPID           int
	now               func() time.Time
	sleep             func(time.Duration)
	snapshotSystem    func([]string, []string) (windowsProcessSnapshot, error)
}

func newProcessMatcher(applicationPaths, packageFamilies []string) (processMatcher, error) {
	if len(applicationPaths) == 0 && len(packageFamilies) == 0 {
		return nil, fmt.Errorf("at least one application is required")
	}
	if err := validateApplicationTargetCount(applicationPaths, packageFamilies); err != nil {
		return nil, err
	}
	validatedFamilies, err := validatePackageFamilyNames(packageFamilies)
	if err != nil {
		return nil, err
	}
	if len(validatedFamilies) > 0 {
		if err := getPackageFamilyNameProc.Find(); err != nil {
			return nil, fmt.Errorf("load Windows package identity API: %w", err)
		}
	}
	configured := make([]string, 0, len(applicationPaths))
	targets := make([]string, 0, len(applicationPaths))
	for _, path := range applicationPaths {
		clean := filepath.Clean(path)
		resolved, err := filepath.EvalSymlinks(clean)
		if err != nil {
			return nil, fmt.Errorf("resolve application path %q: %w", clean, err)
		}
		configured = append(configured, clean)
		targets = append(targets, normalizeWindowsPath(resolved))
	}
	matcher := &windowsProcessMatcher{
		configuredTargets: configured,
		targets:           targets,
		packageFamilies:   validatedFamilies,
		selfPID:           os.Getpid(),
		now:               time.Now,
		sleep:             time.Sleep,
	}
	matcher.snapshotSystem = snapshotWindowsSystem
	return matcher, nil
}

func (m *windowsProcessMatcher) Decide(metadata *M.Metadata) (processDecision, error) {
	if metadata == nil {
		return processUnknown, fmt.Errorf("process metadata is nil")
	}
	decision, found, generation := m.cachedDecision(metadata)
	if found && decision != processDirect {
		return decision, nil
	}
	if err := m.refresh(found, generation); err != nil {
		return processUnknown, err
	}
	decision, found, generation = m.cachedDecision(metadata)
	if found {
		return decision, nil
	}
	for _, delay := range windowsOwnerRetryDelays {
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

func (m *windowsProcessMatcher) Close() error { return nil }

func (m *windowsProcessMatcher) refresh(force bool, observedGeneration uint64) error {
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
	if !force && !m.updatedAt.IsZero() && now.Sub(m.updatedAt) < windowsSnapshotFreshness {
		m.mu.Unlock()
		return nil
	}
	inFlight := &windowsRefresh{done: make(chan struct{})}
	m.refreshing = inFlight
	configured := append([]string(nil), m.configuredTargets...)
	targets := append([]string(nil), m.targets...)
	packageFamilies := append([]string(nil), m.packageFamilies...)
	snapshotSystem := m.snapshotSystem
	m.mu.Unlock()

	targets = refreshWindowsTargets(configured, targets)
	snapshot, err := snapshotSystem(targets, packageFamilies)
	completedAt := m.now()

	m.mu.Lock()
	if err == nil {
		m.targets = targets
		m.snapshot = snapshot
		m.updatedAt = completedAt
		m.generation++
	}
	inFlight.err = err
	m.refreshing = nil
	close(inFlight.done)
	m.mu.Unlock()
	return err
}

func (m *windowsProcessMatcher) cachedDecision(metadata *M.Metadata) (processDecision, bool, uint64) {
	m.mu.Lock()
	snapshot := m.snapshot
	generation := m.generation
	m.mu.Unlock()
	return decideWindowsOwner(metadata, snapshot, m.selfPID, generation)
}

func refreshWindowsTargets(configured, known []string) []string {
	result := append([]string(nil), known...)
	seen := make(map[string]struct{}, len(result))
	for _, target := range result {
		seen[target] = struct{}{}
	}
	for _, configuredPath := range configured {
		resolved, err := filepath.EvalSymlinks(configuredPath)
		if err != nil {
			continue
		}
		normalized := normalizeWindowsPath(resolved)
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

func snapshotWindowsSystem(targets, packageFamilies []string) (windowsProcessSnapshot, error) {
	processes, err := snapshotWindowsProcesses()
	if err != nil {
		return windowsProcessSnapshot{}, err
	}
	flows, err := snapshotWindowsFlows()
	if err != nil {
		return windowsProcessSnapshot{}, err
	}
	enrichWindowsProcessIdentities(processes, flows, targets, len(packageFamilies) > 0)
	selected := selectWindowsProcesses(processes, targets, packageFamilies)
	return windowsProcessSnapshot{processes: processes, selected: selected, flows: flows}, nil
}

func snapshotWindowsProcesses() (map[int]windowsProcess, error) {
	handle, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("list Windows processes: %w", err)
	}
	defer windows.CloseHandle(handle)

	processes := make(map[int]windowsProcess)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(handle, &entry)
	for err == nil {
		pid := int(entry.ProcessID)
		if pid > 0 {
			process := windowsProcess{
				parentPID: int(entry.ParentProcessID),
				name:      strings.ToLower(windows.UTF16ToString(entry.ExeFile[:])),
				known:     true,
			}
			processes[pid] = process
		}
		err = windows.Process32Next(handle, &entry)
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("enumerate Windows processes: %w", err)
	}
	return processes, nil
}

func enrichWindowsProcessIdentities(processes map[int]windowsProcess, flows []windowsFlow, targets []string, queryPackageFamily bool) {
	targetNames := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetNames[strings.ToLower(filepath.Base(target))] = struct{}{}
	}
	candidates := make(map[int]struct{})
	// Only flow owners and their ancestors can affect this snapshot's routing
	// decisions. Limiting handle queries to that set avoids scanning every
	// process for package identity on each short-lived owner-table refresh.
	for _, flow := range flows {
		pid := flow.pid
		visiting := make(map[int]struct{})
		for pid > 0 {
			if _, duplicate := visiting[pid]; duplicate {
				break
			}
			visiting[pid] = struct{}{}
			if _, alreadyIncluded := candidates[pid]; alreadyIncluded {
				break
			}
			process, found := processes[pid]
			if !found {
				break
			}
			candidates[pid] = struct{}{}
			if process.parentPID == pid {
				break
			}
			pid = process.parentPID
		}
	}
	for pid := range candidates {
		process := processes[pid]
		_, queryPath := targetNames[process.name]
		if !queryPath && !queryPackageFamily {
			continue
		}
		process.path, process.packageFamily, _ = queryWindowsProcessIdentity(pid, queryPath, queryPackageFamily)
		processes[pid] = process
	}
}

func queryWindowsProcessIdentity(pid int, queryPath, queryPackageFamily bool) (string, string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", "", err
	}
	defer windows.CloseHandle(handle)
	var path, packageFamily string
	var pathErr, packageErr error
	if queryPath {
		path, pathErr = windowsProcessPathFromHandle(handle)
		path = normalizeWindowsPath(path)
	}
	if queryPackageFamily {
		packageFamily, packageErr = windowsPackageFamilyNameFromHandle(handle)
	}
	return path, packageFamily, errors.Join(pathErr, packageErr)
}

func queryWindowsProcessPath(pid int) (string, error) {
	path, _, err := queryWindowsProcessIdentity(pid, true, false)
	return path, err
}

func windowsPackageFamilyNameFromHandle(handle windows.Handle) (string, error) {
	var length uint32
	result, _, _ := getPackageFamilyNameProc.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&length)),
		0,
	)
	if result == uintptr(windows.APPMODEL_ERROR_NO_PACKAGE) {
		return "", nil
	}
	if result != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
		return "", syscall.Errno(result)
	}
	if length == 0 || length > 256 {
		return "", fmt.Errorf("Windows package family name has invalid length %d", length)
	}
	buffer := make([]uint16, length)
	result, _, _ = getPackageFamilyNameProc.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&length)),
		uintptr(unsafe.Pointer(&buffer[0])),
	)
	if result != 0 {
		return "", syscall.Errno(result)
	}
	return windows.UTF16ToString(buffer), nil
}

func selectWindowsProcesses(processes map[int]windowsProcess, targets, packageFamilies []string) map[int]bool {
	targetSet := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetSet[normalizeWindowsPath(target)] = struct{}{}
	}
	packageSet := make(map[string]struct{}, len(packageFamilies))
	for _, packageFamily := range packageFamilies {
		packageSet[strings.ToLower(packageFamily)] = struct{}{}
	}
	selected := make(map[int]bool, len(processes))
	visiting := make(map[int]bool)
	var isSelected func(int) bool
	isSelected = func(pid int) bool {
		if value, found := selected[pid]; found {
			return value
		}
		if visiting[pid] {
			return false
		}
		process, found := processes[pid]
		if !found {
			return false
		}
		visiting[pid] = true
		_, pathSelected := targetSet[normalizeWindowsPath(process.path)]
		_, packageSelected := packageSet[strings.ToLower(process.packageFamily)]
		value := pathSelected || packageSelected
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

func snapshotWindowsFlows() ([]windowsFlow, error) {
	var flows []windowsFlow
	for _, family := range []uint32{windows.AF_INET, windows.AF_INET6} {
		tcp, err := readWindowsOwnerTable(getExtendedTCPTableProc, family, windowsTCPTableOwnerPIDAll)
		if err != nil {
			if family == windows.AF_INET6 && errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
				continue
			}
			return nil, fmt.Errorf("snapshot Windows TCP owners: %w", err)
		}
		parsedTCP, err := parseWindowsTCPTable(tcp, family)
		if err != nil {
			return nil, err
		}
		flows = append(flows, parsedTCP...)

		udp, err := readWindowsOwnerTable(getExtendedUDPTableProc, family, windowsUDPTableOwnerPID)
		if err != nil {
			if family == windows.AF_INET6 && errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
				continue
			}
			return nil, fmt.Errorf("snapshot Windows UDP owners: %w", err)
		}
		parsedUDP, err := parseWindowsUDPTable(udp, family)
		if err != nil {
			return nil, err
		}
		flows = append(flows, parsedUDP...)
	}
	return flows, nil
}

func readWindowsOwnerTable(proc *windows.LazyProc, family, tableClass uint32) ([]byte, error) {
	if err := proc.Find(); err != nil {
		return nil, err
	}
	var size uint32
	result, _, _ := proc.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		1,
		uintptr(family),
		uintptr(tableClass),
		0,
	)
	if result != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) && result != 0 {
		return nil, syscall.Errno(result)
	}
	if size < 4 {
		size = 4
	}
	for attempts := 0; attempts < 3; attempts++ {
		buffer := make([]byte, size)
		result, _, _ = proc.Call(
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(unsafe.Pointer(&size)),
			1,
			uintptr(family),
			uintptr(tableClass),
			0,
		)
		if result == 0 {
			return buffer, nil
		}
		if result != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, syscall.Errno(result)
		}
	}
	return nil, fmt.Errorf("owner table kept changing while it was read")
}

func parseWindowsTCPTable(buffer []byte, family uint32) ([]windowsFlow, error) {
	rowSize := 24
	if family == windows.AF_INET6 {
		rowSize = 56
	}
	rows, err := windowsTableRows(buffer, rowSize)
	if err != nil {
		return nil, fmt.Errorf("parse Windows TCP owner table: %w", err)
	}
	flows := make([]windowsFlow, 0, len(rows))
	for _, row := range rows {
		var state, pid uint32
		var local, remote netip.AddrPort
		if family == windows.AF_INET {
			state = binary.LittleEndian.Uint32(row[0:4])
			local = netip.AddrPortFrom(addr4FromWindowsRow(row[4:8]), binary.BigEndian.Uint16(row[8:10]))
			remote = netip.AddrPortFrom(addr4FromWindowsRow(row[12:16]), binary.BigEndian.Uint16(row[16:18]))
			pid = binary.LittleEndian.Uint32(row[20:24])
		} else {
			state = binary.LittleEndian.Uint32(row[48:52])
			local = netip.AddrPortFrom(addr16FromWindowsRow(row[0:16]), binary.BigEndian.Uint16(row[20:22]))
			remote = netip.AddrPortFrom(addr16FromWindowsRow(row[24:40]), binary.BigEndian.Uint16(row[44:46]))
			pid = binary.LittleEndian.Uint32(row[52:56])
		}
		if state == windowsTCPStateClosed || state == windowsTCPStateListen || state == windowsTCPStateDeleteTCB || local.Port() == 0 || pid == 0 {
			continue
		}
		flows = append(flows, windowsFlow{pid: int(pid), network: M.TCP, local: local, remote: remote})
	}
	return flows, nil
}

func parseWindowsUDPTable(buffer []byte, family uint32) ([]windowsFlow, error) {
	rowSize := 12
	if family == windows.AF_INET6 {
		rowSize = 28
	}
	rows, err := windowsTableRows(buffer, rowSize)
	if err != nil {
		return nil, fmt.Errorf("parse Windows UDP owner table: %w", err)
	}
	flows := make([]windowsFlow, 0, len(rows))
	for _, row := range rows {
		var pid uint32
		var local netip.AddrPort
		if family == windows.AF_INET {
			local = netip.AddrPortFrom(addr4FromWindowsRow(row[0:4]), binary.BigEndian.Uint16(row[4:6]))
			pid = binary.LittleEndian.Uint32(row[8:12])
		} else {
			local = netip.AddrPortFrom(addr16FromWindowsRow(row[0:16]), binary.BigEndian.Uint16(row[20:22]))
			pid = binary.LittleEndian.Uint32(row[24:28])
		}
		if local.Port() == 0 || pid == 0 {
			continue
		}
		flows = append(flows, windowsFlow{pid: int(pid), network: M.UDP, local: local})
	}
	return flows, nil
}

func windowsTableRows(buffer []byte, rowSize int) ([][]byte, error) {
	if len(buffer) < 4 || rowSize <= 0 {
		return nil, fmt.Errorf("owner table is truncated")
	}
	count := int(binary.LittleEndian.Uint32(buffer[:4]))
	if count > (len(buffer)-4)/rowSize {
		return nil, fmt.Errorf("owner table declares %d rows but only %d fit", count, (len(buffer)-4)/rowSize)
	}
	rows := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		offset := 4 + i*rowSize
		rows = append(rows, buffer[offset:offset+rowSize])
	}
	return rows, nil
}

func addr4FromWindowsRow(value []byte) netip.Addr {
	return netip.AddrFrom4([4]byte{value[0], value[1], value[2], value[3]})
}

func addr16FromWindowsRow(value []byte) netip.Addr {
	var address [16]byte
	copy(address[:], value)
	return netip.AddrFrom16(address)
}

func decideWindowsOwner(metadata *M.Metadata, snapshot windowsProcessSnapshot, selfPID int, generation uint64) (processDecision, bool, uint64) {
	exact := make([]windowsFlow, 0, 2)
	local := make([]windowsFlow, 0, 2)
	for _, flow := range snapshot.flows {
		if flow.network != metadata.Network || flow.local.Port() != metadata.SrcPort || !windowsAddressMatches(flow.local.Addr(), metadata.SrcIP) {
			continue
		}
		local = append(local, flow)
		if flow.remote.Port() != 0 && flow.remote.Port() == metadata.DstPort && windowsAddressMatches(flow.remote.Addr(), metadata.DstIP) {
			exact = append(exact, flow)
		}
	}
	candidates := local
	if len(exact) > 0 {
		candidates = exact
	}
	if len(candidates) == 0 {
		return processUnknown, false, generation
	}
	hasReject, hasProxy, hasUnknown, hasDirect := false, false, false, false
	seenPID := make(map[int]struct{}, len(candidates))
	for _, flow := range candidates {
		if _, duplicate := seenPID[flow.pid]; duplicate {
			continue
		}
		seenPID[flow.pid] = struct{}{}
		switch {
		case flow.pid == selfPID:
			hasReject = true
		case snapshot.selected[flow.pid]:
			hasProxy = true
		case snapshot.processes[flow.pid].known:
			hasDirect = true
		default:
			hasUnknown = true
		}
	}
	switch {
	case hasReject:
		return processReject, true, generation
	case hasProxy && (hasDirect || hasUnknown):
		return processReject, true, generation
	case hasProxy:
		return processProxy, true, generation
	case hasUnknown:
		// A flow may appear in the owner table just before its process appears
		// in the Toolhelp snapshot. Report a miss so Decide performs its short
		// retry sequence before falling back to unknown-direct.
		return processUnknown, false, generation
	case hasDirect:
		return processDirect, true, generation
	default:
		return processUnknown, false, generation
	}
}

func windowsAddressMatches(socketAddress, packetAddress netip.Addr) bool {
	if !socketAddress.IsValid() || socketAddress.IsUnspecified() {
		return true
	}
	if !packetAddress.IsValid() {
		return false
	}
	return socketAddress.Unmap() == packetAddress.Unmap()
}
