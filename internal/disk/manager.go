package disk

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type DiskInfo struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	SizeBytes  int64      `json:"size_bytes"`
	SizeHuman  string     `json:"size_human"`
	Type       string     `json:"type"`
	Mountpoint string     `json:"mountpoint"`
	Model      string     `json:"model"`
	Serial     string     `json:"serial"`
	Tran       string     `json:"tran"`
	Rota       bool       `json:"rota"`
	Removable  bool       `json:"rm"`
	Fstype     string     `json:"fstype"`
	Label      string     `json:"label"`
	IsSystem   bool       `json:"is_system"`
	IsMounted  bool       `json:"is_mounted"`
	Children   []DiskInfo `json:"children"`
}

type LsblkDevice struct {
	Name string      `json:"name"`
	Size json.Number `json:"size"`
	Type string      `json:"type"`
	// Mountpoint is the legacy singular field: on util-linux ≥ 2.37 it can
	// be null when a device has multiple mountpoints (bind mounts, btrfs
	// subvolumes) — mountpoints below is the authoritative array then.
	// Pointers throughout because lsblk emits null for absent values.
	Mountpoint  *string       `json:"mountpoint"`
	Mountpoints []*string     `json:"mountpoints"`
	Model       *string       `json:"model"`
	Serial      *string       `json:"serial"`
	Tran        *string       `json:"tran"`
	Rota        *bool         `json:"rota"`
	Rm          *bool         `json:"rm"`
	Fstype      *string       `json:"fstype"`
	Label       *string       `json:"label"`
	Children    []LsblkDevice `json:"children"`
}

type LsblkOutput struct {
	Blockdevices []LsblkDevice `json:"blockdevices"`
}

var systemMountPoints = map[string]bool{
	"/": true, "/mnt": true, "/boot": true, "/boot/efi": true, "/efi": true,
}

func GetLocalDisks() ([]DiskInfo, error) {
	// MOUNTPOINTS (plural) carries every mount of a device; older lsblk
	// builds reject the unknown column, so fall back to the classic set.
	out, err := exec.Command("lsblk", "-Jb", "-o",
		"NAME,SIZE,TYPE,MOUNTPOINTS,MOUNTPOINT,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL").Output()
	if err != nil {
		out, err = exec.Command("lsblk", "-Jb", "-o",
			"NAME,SIZE,TYPE,MOUNTPOINT,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL").Output()
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("lsblk: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	return ParseJSON(string(out))
}

// ParseJSON parses lsblk -J output into DiskInfo records.
func ParseJSON(raw string) ([]DiskInfo, error) {
	var output LsblkOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return nil, err
	}
	var result []DiskInfo
	for _, dev := range output.Blockdevices {
		// ram/zram/loop devices have no business being dd sources or
		// targets (loop already reports its own type; ram0 reports "disk").
		if isVirtualDevice(dev.Name) {
			continue
		}
		info := processDevice(dev, nil)
		if info != nil {
			result = append(result, *info)
		}
	}
	return result, nil
}

func isVirtualDevice(name string) bool {
	for _, p := range []string{"loop", "ram", "zram"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func processDevice(dev LsblkDevice, parentRemovable *bool) *DiskInfo {
	sizeBytes, err := dev.Size.Int64()
	if err != nil {
		// lsblk -b always emits byte counts; a non-numeric size means
		// malformed output — drop the device instead of offering a
		// silent "0 B" disk.
		return nil
	}
	mps := allMountpoints(dev)
	model := ""
	if dev.Model != nil {
		model = *dev.Model
	}
	serial := ""
	if dev.Serial != nil {
		serial = *dev.Serial
	}
	tran := ""
	if dev.Tran != nil {
		tran = *dev.Tran
	}
	fstype := ""
	if dev.Fstype != nil {
		fstype = *dev.Fstype
	}
	label := ""
	if dev.Label != nil {
		label = *dev.Label
	}

	rota := false
	if dev.Rota != nil {
		rota = *dev.Rota
	}
	// Partitions normally carry the same rm flag as their disk; inherit it
	// when the row lacks one so removable-disk logic works per-partition.
	removable := false
	switch {
	case dev.Rm != nil:
		removable = *dev.Rm
	case parentRemovable != nil:
		removable = *parentRemovable
	}

	path := "/dev/" + dev.Name

	mp := ""
	if len(mps) > 0 {
		mp = mps[0]
	}

	disk := DiskInfo{
		Name:       dev.Name,
		Path:       path,
		SizeBytes:  sizeBytes,
		SizeHuman:  FormatBytes(sizeBytes),
		Type:       dev.Type,
		Mountpoint: mp,
		Model:      model,
		Serial:     serial,
		Tran:       tran,
		Rota:       rota,
		Removable:  removable,
		Fstype:     fstype,
		Label:      label,
		IsMounted:  len(mps) > 0,
		IsSystem:   false,
		Children:   []DiskInfo{},
	}

	for _, m := range mps {
		if systemMountPoints[m] || strings.HasPrefix(m, "/boot") {
			disk.IsSystem = true
			break
		}
	}

	for _, childDev := range dev.Children {
		child := processDevice(childDev, &removable)
		if child == nil {
			continue
		}
		disk.Children = append(disk.Children, *child)
		if child.IsSystem {
			disk.IsSystem = true
		}
		if child.IsMounted {
			disk.IsMounted = true
		}
	}

	// Conservative safety net for the dd-target listing: a mounted
	// partition on a non-removable disk almost always means a running
	// system — flag it as such even when the mountpoint is somewhere
	// unexpected (/var/lib/docker, /home, ...).
	if !disk.IsSystem && disk.IsMounted && !removable {
		disk.IsSystem = true
	}

	return &disk
}

// allMountpoints merges the legacy singular mountpoint with the plural
// mountpoints array, dropping nulls and duplicates.
func allMountpoints(dev LsblkDevice) []string {
	seen := map[string]bool{}
	var mps []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		mps = append(mps, s)
	}
	if dev.Mountpoint != nil {
		add(*dev.Mountpoint)
	}
	for _, m := range dev.Mountpoints {
		if m != nil {
			add(*m)
		}
	}
	return mps
}

func FindDisk(disks []DiskInfo, path string) *DiskInfo {
	for i := range disks {
		if disks[i].Path == path {
			return &disks[i]
		}
		if len(disks[i].Children) > 0 {
			if found := FindDisk(disks[i].Children, path); found != nil {
				return found
			}
		}
	}
	return nil
}

func FormatBytes(bytes int64) string {
	if bytes >= 1<<40 {
		return strings.TrimRight(strings.TrimRight(
			strconv.FormatFloat(float64(bytes)/(1<<40), 'f', 2, 64), "0"), ".") + " TB"
	}
	if bytes >= 1<<30 {
		return strings.TrimRight(strings.TrimRight(
			strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 2, 64), "0"), ".") + " GB"
	}
	if bytes >= 1<<20 {
		return strings.TrimRight(strings.TrimRight(
			strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 2, 64), "0"), ".") + " MB"
	}
	if bytes >= 1<<10 {
		return strings.TrimRight(strings.TrimRight(
			strconv.FormatFloat(float64(bytes)/(1<<10), 'f', 2, 64), "0"), ".") + " KB"
	}
	return strconv.FormatInt(bytes, 10) + " B"
}
