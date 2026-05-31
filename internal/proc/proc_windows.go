//go:build windows

package proc

import (
	"fmt"
	"net/netip"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows IPHLPAPI constants.
const (
	tcpTableOwnerPidAll = 5 // TCP_TABLE_OWNER_PID_ALL
	afInet              = 2 // AF_INET
)

// mibTCPRowOwnerPID mirrors MIB_TCPROW_OWNER_PID. Ports/addresses are in
// network byte order; state is little-endian. Layout must match the C struct.
type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32 // low 16 bits, network byte order
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

var (
	iphlpapi              = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTbl = iphlpapi.NewProc("GetExtendedTcpTable")
	tblMu                 sync.Mutex
	tblBuf                []byte
)

// ownerExe finds the PID owning the local TCP endpoint whose port matches the
// client's port (the connection to our loopback proxy), then resolves the exe.
func ownerExe(client netip.AddrPort) (string, error) {
	pid, ok := pidForLocalPort(client.Port())
	if !ok {
		return "", nil
	}
	return exeForPID(pid)
}

// pidForLocalPort scans the IPv4 owner-PID TCP table for a row whose LOCAL port
// equals port (the client side of the loopback connection lives on this port).
func pidForLocalPort(port uint16) (uint32, bool) {
	tblMu.Lock()
	defer tblMu.Unlock()

	var size uint32
	// First call to size the buffer.
	r0, _, _ := procGetExtendedTcpTbl.Call(
		0, uintptr(unsafe.Pointer(&size)), 0, // pTcpTable, pdwSize, bOrder
		uintptr(afInet), uintptr(tcpTableOwnerPidAll), 0,
	)
	const errInsufficientBuffer = 122
	if r0 != 0 && r0 != errInsufficientBuffer {
		return 0, false
	}
	if size == 0 {
		return 0, false
	}
	if uint32(len(tblBuf)) < size {
		tblBuf = make([]byte, size)
	}
	r0, _, _ = procGetExtendedTcpTbl.Call(
		uintptr(unsafe.Pointer(&tblBuf[0])), uintptr(unsafe.Pointer(&size)), 0,
		uintptr(afInet), uintptr(tcpTableOwnerPidAll), 0,
	)
	if r0 != 0 {
		return 0, false
	}

	// MIB_TCPTABLE_OWNER_PID: { DWORD dwNumEntries; MIB_TCPROW_OWNER_PID table[] }.
	// All row fields are DWORDs (4-byte aligned), so the array begins right after
	// the count DWORD at offset 4 — no extra padding.
	numEntries := *(*uint32)(unsafe.Pointer(&tblBuf[0]))
	rowSize := unsafe.Sizeof(mibTCPRowOwnerPID{})
	base := unsafe.Pointer(&tblBuf[0])
	rowsPtr := unsafe.Add(base, unsafe.Sizeof(uint32(0)))

	for i := uint32(0); i < numEntries; i++ {
		row := (*mibTCPRowOwnerPID)(unsafe.Add(rowsPtr, uintptr(i)*rowSize))
		// LocalPort is network byte order in the low 16 bits.
		lp := uint16((row.LocalPort&0xff)<<8 | (row.LocalPort&0xff00)>>8)
		if lp == port {
			return row.OwningPID, true
		}
	}
	return 0, false
}

// exeForPID returns the full image path of a process by PID.
func exeForPID(pid uint32) (string, error) {
	if pid == 0 {
		return "", nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", nil // protected/exited process — treat as unknown
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", fmt.Errorf("proc: QueryFullProcessImageName: %w", err)
	}
	return windows.UTF16ToString(buf[:n]), nil
}
