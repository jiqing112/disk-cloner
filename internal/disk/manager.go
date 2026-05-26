package disk

import (
	"encoding/json"
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
	Name       string        `json:"name"`
	Size       json.Number   `json:"size"`
	Type       string        `json:"type"`
	Mountpoint *string       `json:"mountpoint"`
	Model      *string       `json:"model"`
	Serial     *string       `json:"serial"`
	Tran       *string       `json:"tran"`
	Rota       *bool         `json:"rota"`
	Rm         *bool         `json:"rm"`
	Fstype     *string       `json:"fstype"`
	Label      *string       `json:"label"`
	Children   []LsblkDevice `json:"children"`
}

type LsblkOutput struct {
	Blockdevices []LsblkDevice `json:"blockdevices"`
}

var systemMountPoints = map[string]bool{
	"/": true, "/mnt": true, "/boot": true, "/boot/efi": true, "/efi": true,
}

func GetLocalDisks() ([]DiskInfo, error) {
	cmd := exec.Command("lsblk", "-Jb", "-o", "NAME,SIZE,TYPE,MOUNTPOINT,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return ParseJSON(string(out))
}

func ParseJSON(raw string) ([]DiskInfo, error) {
	var output LsblkOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return nil, err
	}
	return processDevices(output.Blockdevices), nil
}

func processDevices(devices []LsblkDevice) []DiskInfo {
	var result []DiskInfo

	for _, dev := range devices {
		sizeBytes, _ := dev.Size.Int64()
		mp := ""
		if dev.Mountpoint != nil {
			mp = *dev.Mountpoint
		}
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
		removable := false
		if dev.Rm != nil {
			removable = *dev.Rm
		}

		path := "/dev/" + dev.Name

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
			IsMounted:  mp != "",
			IsSystem:   false,
			Children:   []DiskInfo{},
		}

		if mp != "" {
			if systemMountPoints[mp] || strings.HasPrefix(mp, "/boot") {
				disk.IsSystem = true
			}
		}

		if len(dev.Children) > 0 {
			disk.Children = processDevices(dev.Children)
			for _, child := range disk.Children {
				if child.IsSystem {
					disk.IsSystem = true
				}
				if child.IsMounted {
					disk.IsMounted = true
				}
			}
		}

		result = append(result, disk)
	}

	return result
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
