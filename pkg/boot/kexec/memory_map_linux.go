// Copyright 2015-2019 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package kexec

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/u-root/u-root/pkg/dt"
)

// RangeType defines type of a TypedRange based on the Linux
// kernel string provided by firmware memory map.
type RangeType string

// These are the range types we know Linux uses.
const (
	RangeRAM      RangeType = "System RAM"
	RangeDefault  RangeType = "Default"
	RangeACPI     RangeType = "ACPI Tables"
	RangeNVS      RangeType = "ACPI Non-volatile Storage"
	RangeReserved RangeType = "Reserved"
)

// String implements fmt.Stringer.
func (r RangeType) String() string {
	return string(r)
}

var sysfsToRangeType = map[string]RangeType{
	"System RAM":                RangeRAM,
	"Default":                   RangeDefault,
	"ACPI Tables":               RangeACPI,
	"ACPI Non-volatile Storage": RangeNVS,
	"Reserved":                  RangeReserved,
	"reserved":                  RangeReserved,
}

// TypedRange represents range of physical memory.
type TypedRange struct {
	Range
	Type RangeType
}

func (tr TypedRange) String() string {
	return fmt.Sprintf("{addr: %s, type: %s}", tr.Range, tr.Type)
}

// MemoryMap defines the layout of physical memory.
//
// MemoryMap defines which ranges in memory are usable RAM and which are
// reserved for various reasons.
type MemoryMap []TypedRange

// FilterByType only returns ranges of the given typ.
func (m MemoryMap) FilterByType(typ RangeType) Ranges {
	var rs Ranges
	for _, tr := range m {
		if tr.Type == typ {
			rs = append(rs, tr.Range)
		}
	}
	return rs
}

func (m MemoryMap) sort() {
	sort.Slice(m, func(i, j int) bool {
		return m[i].Start < m[j].Start
	})
}

// Insert a new TypedRange into the memory map, removing chunks of other ranges
// as necessary.
//
// Assumes that TypedRange is a valid range -- no checking.
func (m *MemoryMap) Insert(r TypedRange) {
	var newMap MemoryMap

	// Remove points in r from all existing physical ranges.
	for _, q := range *m {
		split := q.Range.Minus(r.Range)
		for _, r2 := range split {
			newMap = append(newMap, TypedRange{Range: r2, Type: q.Type})
		}
	}

	newMap = append(newMap, r)
	newMap.sort()
	*m = newMap
}

// MemoryMapFromFDT reads firmware provided memory map from an FDT.
func MemoryMapFromFDT(fdt *dt.FDT) (MemoryMap, error) {
	var phys MemoryMap
	addMemory := func(n *dt.Node) error {
		p, found := n.LookProperty("device_type")
		if !found {
			return nil
		}
		t, err := p.AsString()
		if err != nil || t != "memory" {
			return nil
		}
		p, found = n.LookProperty("reg")
		if found {
			r, err := p.AsRegion()
			if err != nil {
				return err
			}
			phys = append(phys, TypedRange{
				Range: Range{Start: uintptr(r.Start), Size: uint(r.Size)},
				Type:  RangeRAM,
			})
		}
		return nil
	}
	err := fdt.RootNode.Walk(addMemory)
	if err != nil {
		return nil, err
	}

	reserveMemory := func(n *dt.Node) error {
		p, found := n.LookProperty("reg")
		if found {
			r, err := p.AsRegion()
			if err != nil {
				return err
			}

			phys.Insert(TypedRange{
				Range: Range{Start: uintptr(r.Start), Size: uint(r.Size)},
				Type:  RangeReserved,
			})
		}
		return nil
	}
	resv, found := fdt.NodeByName("reserved-memory")
	if found {
		err := resv.Walk(reserveMemory)
		if err != nil {
			return nil, err
		}
	}

	for _, r := range fdt.ReserveEntries {
		phys.Insert(TypedRange{
			Range: Range{Start: uintptr(r.Address), Size: uint(r.Size)},
			Type:  RangeReserved,
		})
	}

	return phys, nil
}

var memoryMapRoot = "/sys/firmware/memmap/"

// MemoryMapFromEFI reads a firmware-provided memory map from /sys/firmware/memmap.
func MemoryMapFromEFI() (MemoryMap, error) {
	return memoryMapFromEFI(memoryMapRoot)
}

func memoryMapFromEFI(memoryMapDir string) (MemoryMap, error) {
	type memRange struct {
		// start and end addresses are inclusive
		start, end uintptr
		typ        RangeType
	}

	ranges := make(map[string]memRange)
	walker := func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		const (
			// file names
			start = "start"
			end   = "end"
			typ   = "type"
		)

		base := path.Base(name)
		if base != start && base != end && base != typ {
			return fmt.Errorf("unexpected file %q", name)
		}
		dir := path.Dir(name)

		b, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("error reading file %q: %v", name, err)
		}

		data := strings.TrimSpace(string(b))
		r := ranges[dir]
		if base == typ {
			typ, ok := sysfsToRangeType[data]
			if !ok {
				log.Printf("Sysfs file %q contains unrecognized memory map type %q, defaulting to Reserved", name, data)
				r.typ = RangeReserved
			} else {
				r.typ = typ
			}
			ranges[dir] = r
			return nil
		}

		v, err := strconv.ParseUint(data, 0, strconv.IntSize)
		if err != nil {
			return err
		}
		switch base {
		case start:
			r.start = uintptr(v)
		case end:
			r.end = uintptr(v)
		}
		ranges[dir] = r
		return nil
	}

	if err := filepath.Walk(memoryMapDir, walker); err != nil {
		return nil, err
	}

	var phys []TypedRange
	for _, r := range ranges {
		// Range's end address is exclusive, while Linux's sysfs prints
		// the end address inclusive.
		//
		// E.g. sysfs will contain
		//
		// start: 0x100, end: 0x1ff
		//
		// while we represent
		//
		// start: 0x100, size: 0x100.
		phys = append(phys, TypedRange{
			Range: RangeFromInterval(r.start, r.end+1),
			Type:  r.typ,
		})
	}
	sort.Slice(phys, func(i, j int) bool {
		return phys[i].Start < phys[j].Start
	})
	return phys, nil
}

// UEFIPayloadMemType are types used with LinuxBoot UEFI payload memory maps.
type UEFIPayloadMemType uint32

// Payload memory type (PayloadMemType) in UEFI payload.
const (
	UEFIPayloadTypeRAM      UEFIPayloadMemType = 1
	UEFIPayloadTypeDefault  UEFIPayloadMemType = 2
	UEFIPayloadTypeACPI     UEFIPayloadMemType = 3
	UEFIPayloadTypeNVS      UEFIPayloadMemType = 4
	UEFIPayloadTypeReserved UEFIPayloadMemType = 5
)

// UEFIPayloadMemoryMapEntry represent a memory map entry for a LinuxBoot UEFI payload.
type UEFIPayloadMemoryMapEntry struct {
	Start uint64
	End   uint64
	Type  UEFIPayloadMemType
}

// UEFIPayloadMemoryMap is a memory map used with LinuxBoot's UEFI payload.
type UEFIPayloadMemoryMap []UEFIPayloadMemoryMapEntry

var rangeTypeToUEFIPayloadMemType = map[RangeType]UEFIPayloadMemType{
	RangeRAM:      UEFIPayloadTypeRAM,
	RangeDefault:  UEFIPayloadTypeDefault,
	RangeACPI:     UEFIPayloadTypeACPI,
	RangeNVS:      UEFIPayloadTypeNVS,
	RangeReserved: UEFIPayloadTypeReserved,
}

func convertToUEFIPayloadMemType(rt RangeType) UEFIPayloadMemType {
	mt, ok := rangeTypeToUEFIPayloadMemType[rt]
	if !ok {
		// return reserved if range type is not recognized
		return UEFIPayloadTypeReserved
	}
	return mt
}

// ToUEFIPayloadMemoryMap converts MemoryMap to a UEFI payload memory map.
func (m *MemoryMap) ToUEFIPayloadMemoryMap() UEFIPayloadMemoryMap {
	var p UEFIPayloadMemoryMap
	for _, entry := range *m {
		p = append(p, UEFIPayloadMemoryMapEntry{
			Start: uint64(entry.Start),
			End:   uint64(entry.Start) + uint64(entry.Size) - 1,
			Type:  convertToUEFIPayloadMemType(entry.Type),
		})
	}
	return p
}

// MemoryMapFromIOMem reads the kernel-maintained memory map from /proc/iomem.
func MemoryMapFromIOMem() (MemoryMap, error) {
	return memoryMapFromIOMemFile("/proc/iomem")
}

func rangeType(s string) RangeType {
	if s == "reserved" {
		return RangeReserved
	}
	return RangeType(s)
}

func memoryMapFromIOMem(r io.Reader) (MemoryMap, error) {
	var mm MemoryMap
	b := bufio.NewScanner(r)
	for b.Scan() {
		// Format:
		//   740100000000-7401001fffff : PCI Bus 0001:01
		els := strings.Split(b.Text(), ":")
		if len(els) != 2 {
			continue
		}
		typ := strings.TrimSpace(els[1])
		addrs := strings.Split(strings.TrimSpace(els[0]), "-")
		if len(addrs) != 2 {
			continue
		}
		start, err := strconv.ParseUint(addrs[0], 16, 64)
		if err != nil {
			continue
		}
		end, err := strconv.ParseUint(addrs[1], 16, 64)
		if err != nil {
			continue
		}
		// Special case -- empty ranges are represented as "000-000"
		// even though the non-inclusive end would make that a 1-sized
		// region.
		if start == end {
			continue
		}
		mm.Insert(TypedRange{
			// end is inclusive.
			Range: RangeFromInterval(uintptr(start), uintptr(end+1)),
			Type:  rangeType(typ),
		})
	}
	if err := b.Err(); err != nil {
		return nil, err
	}
	return mm, nil
}

func memoryMapFromIOMemFile(path string) (MemoryMap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return memoryMapFromIOMem(f)
}

func rangeFromMemblockLine(s string) *Range {
	// Format:
	//    0: 0x0000004000000000..0x00000040113fffff
	els := strings.Split(s, ":")
	if len(els) != 2 {
		return nil
	}
	addrs := strings.Split(strings.TrimSpace(els[1]), "..")
	if len(addrs) != 2 {
		return nil
	}
	startS, _ := strings.CutPrefix(addrs[0], "0x")
	start, err := strconv.ParseUint(startS, 16, 64)
	if err != nil {
		return nil
	}
	endS, _ := strings.CutPrefix(addrs[1], "0x")
	end, err := strconv.ParseUint(endS, 16, 64)
	if err != nil {
		return nil
	}

	// Special case -- empty ranges are represented as "000-000"
	// even though the non-inclusive end would make that a 1-sized
	// region.
	if start == end {
		return nil
	}

	// end is inclusive.
	r := RangeFromInterval(uintptr(start), uintptr(end+1))
	return &r
}

// MemoryMapFromMemblock reads a kernel-maintained memory map from /sys/kernel/debug/memblock.
//
// memblock is only available on kernels with CONFIG_ARCH_KEEP_MEMBLOCK (and
// debugfs). Without it, the kernel only maintains memblock early during init
// as its memory allocation mechanism.
func MemoryMapFromMemblock() (MemoryMap, error) {
	m, err := os.Open("/sys/kernel/debug/memblock/memory")
	if err != nil {
		return nil, err
	}
	defer m.Close()

	r, err := os.Open("/sys/kernel/debug/memblock/reserved")
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return memoryMapFromMemblock(m, r)
}

func memoryMapFromMemblock(memory io.Reader, reserved io.Reader) (MemoryMap, error) {
	var mm MemoryMap
	b := bufio.NewScanner(memory)
	for b.Scan() {
		r := rangeFromMemblockLine(b.Text())
		if r == nil {
			continue
		}
		mm.Insert(TypedRange{
			Range: *r,
			Type:  RangeRAM,
		})
	}
	if err := b.Err(); err != nil {
		return nil, err
	}

	b = bufio.NewScanner(reserved)
	for b.Scan() {
		r := rangeFromMemblockLine(b.Text())
		if r == nil {
			continue
		}
		mm.Insert(TypedRange{
			Range: *r,
			Type:  RangeReserved,
		})
	}
	if err := b.Err(); err != nil {
		return nil, err
	}
	return mm, nil
}