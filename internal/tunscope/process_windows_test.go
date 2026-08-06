//go:build windows

package tunscope

import (
	"encoding/binary"
	"net/netip"
	"testing"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"golang.org/x/sys/windows"
)

func windowsTableBuffer(row []byte) []byte {
	buffer := make([]byte, 4+len(row))
	binary.LittleEndian.PutUint32(buffer[:4], 1)
	copy(buffer[4:], row)
	return buffer
}

func TestParseWindowsTCP4Table(t *testing.T) {
	row := make([]byte, 24)
	binary.LittleEndian.PutUint32(row[0:4], 5)
	copy(row[4:8], []byte{198, 18, 0, 1})
	binary.BigEndian.PutUint16(row[8:10], 53001)
	copy(row[12:16], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(row[16:18], 443)
	binary.LittleEndian.PutUint32(row[20:24], 4242)
	flows, err := parseWindowsTCPTable(windowsTableBuffer(row), windows.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(flows))
	}
	flow := flows[0]
	if flow.pid != 4242 || flow.network != M.TCP || flow.local.String() != "198.18.0.1:53001" || flow.remote.String() != "8.8.8.8:443" {
		t.Fatalf("unexpected flow: %+v", flow)
	}
}

func TestParseWindowsTCP6Table(t *testing.T) {
	row := make([]byte, 56)
	local := netip.MustParseAddr("fd7a:6d61:6374:756e::1").As16()
	remote := netip.MustParseAddr("2606:4700:4700::1111").As16()
	copy(row[0:16], local[:])
	binary.BigEndian.PutUint16(row[20:22], 53002)
	copy(row[24:40], remote[:])
	binary.BigEndian.PutUint16(row[44:46], 853)
	binary.LittleEndian.PutUint32(row[48:52], 5)
	binary.LittleEndian.PutUint32(row[52:56], 4343)
	flows, err := parseWindowsTCPTable(windowsTableBuffer(row), windows.AF_INET6)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 || flows[0].pid != 4343 || flows[0].local.Port() != 53002 || flows[0].remote.Addr() != netip.MustParseAddr("2606:4700:4700::1111") {
		t.Fatalf("unexpected IPv6 flow: %+v", flows)
	}
}

func TestParseWindowsUDP4Table(t *testing.T) {
	row := make([]byte, 12)
	copy(row[0:4], []byte{0, 0, 0, 0})
	binary.BigEndian.PutUint16(row[4:6], 5353)
	binary.LittleEndian.PutUint32(row[8:12], 4444)
	flows, err := parseWindowsUDPTable(windowsTableBuffer(row), windows.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 || flows[0].pid != 4444 || flows[0].local.Port() != 5353 || !flows[0].local.Addr().IsUnspecified() {
		t.Fatalf("unexpected UDP flow: %+v", flows)
	}
}

func TestWindowsTableRejectsTruncatedRows(t *testing.T) {
	buffer := make([]byte, 8)
	binary.LittleEndian.PutUint32(buffer[:4], 2)
	if _, err := windowsTableRows(buffer, 24); err == nil {
		t.Fatal("expected truncated owner table to fail")
	}
}

func TestDecideWindowsOwner(t *testing.T) {
	metadata := &M.Metadata{
		Network: M.TCP,
		SrcIP:   netip.MustParseAddr("198.18.0.1"),
		SrcPort: 53001,
		DstIP:   netip.MustParseAddr("8.8.8.8"),
		DstPort: 443,
	}
	selectedFlow := windowsFlow{
		pid: 101, network: M.TCP,
		local:  netip.MustParseAddrPort("198.18.0.1:53001"),
		remote: netip.MustParseAddrPort("8.8.8.8:443"),
	}
	directFlow := selectedFlow
	directFlow.pid = 202

	tests := []struct {
		name      string
		snapshot  windowsProcessSnapshot
		selfPID   int
		want      processDecision
		wantFound bool
	}{
		{
			name: "selected",
			snapshot: windowsProcessSnapshot{
				processes: map[int]windowsProcess{101: {known: true}},
				selected:  map[int]bool{101: true},
				flows:     []windowsFlow{selectedFlow},
			},
			want: processProxy, wantFound: true,
		},
		{
			name: "known direct",
			snapshot: windowsProcessSnapshot{
				processes: map[int]windowsProcess{202: {known: true}},
				selected:  map[int]bool{},
				flows:     []windowsFlow{directFlow},
			},
			want: processDirect, wantFound: true,
		},
		{
			name: "selected direct conflict",
			snapshot: windowsProcessSnapshot{
				processes: map[int]windowsProcess{101: {known: true}, 202: {known: true}},
				selected:  map[int]bool{101: true},
				flows:     []windowsFlow{selectedFlow, directFlow},
			},
			want: processReject, wantFound: true,
		},
		{
			name: "engine loop",
			snapshot: windowsProcessSnapshot{
				processes: map[int]windowsProcess{101: {known: true}},
				selected:  map[int]bool{},
				flows:     []windowsFlow{selectedFlow},
			},
			selfPID: 101, want: processReject, wantFound: true,
		},
		{
			name: "owner not yet enumerated",
			snapshot: windowsProcessSnapshot{
				processes: map[int]windowsProcess{},
				selected:  map[int]bool{},
				flows:     []windowsFlow{selectedFlow},
			},
			want: processUnknown, wantFound: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found, _ := decideWindowsOwner(metadata, test.snapshot, test.selfPID, 7)
			if got != test.want || found != test.wantFound {
				t.Fatalf("got (%v, %v), want (%v, %v)", got, found, test.want, test.wantFound)
			}
		})
	}
}

func TestSelectWindowsProcessesIncludesDescendants(t *testing.T) {
	processes := map[int]windowsProcess{
		10: {path: `c:\program files\google\chrome\application\chrome.exe`, known: true},
		11: {parentPID: 10, known: true},
		12: {parentPID: 11, known: true},
		20: {path: `c:\windows\system32\notepad.exe`, known: true},
	}
	selected := selectWindowsProcesses(processes, []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`})
	if !selected[10] || !selected[11] || !selected[12] {
		t.Fatalf("selected process tree is incomplete: %#v", selected)
	}
	if selected[20] {
		t.Fatal("unrelated process was selected")
	}
}
