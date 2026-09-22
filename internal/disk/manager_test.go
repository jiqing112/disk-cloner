package disk

import "testing"

const lsblkJSON = `{
  "blockdevices": [
    {
      "name": "sda", "size": "500107862016", "type": "disk", "rm": false,
      "mountpoint": null,
      "mountpoints": [null, "/srv/data", "/srv/data/old"],
      "children": [
        { "name": "sda1", "size": "536870912", "type": "part", "mountpoint": null, "mountpoints": [null] },
        { "name": "sda2", "size": "499570922496", "type": "part", "mountpoint": null, "mountpoints": ["/srv/data", null] }
      ]
    },
    {
      "name": "sdb", "size": "160041885696", "type": "disk", "rm": true,
      "children": [
        { "name": "sdb1", "size": "160039720448", "type": "part", "mountpoint": "/media/usb", "mountpoints": ["/media/usb"] }
      ]
    },
    { "name": "ram0", "size": "16777216", "type": "disk", "rm": true, "mountpoint": null, "mountpoints": [] },
    { "name": "loop0", "size": "4096", "type": "loop", "rm": true, "mountpoint": null, "mountpoints": [] }
  ]
}`

// TestParseJSONMountpointsArray: the legacy singular "mountpoint" is null
// on util-linux ≥ 2.37 when a device has multiple mounts — the mountpoints
// array must be used, or a mounted system disk would show up as an
// unmounted, safe dd target.
func TestParseJSONMountpointsArray(t *testing.T) {
	disks, err := ParseJSON(lsblkJSON)
	if err != nil {
		t.Fatal(err)
	}
	var sda *DiskInfo
	for i := range disks {
		if disks[i].Name == "sda" {
			sda = &disks[i]
		}
	}
	if sda == nil {
		t.Fatal("sda missing from parsed output")
	}
	if !sda.IsMounted {
		t.Fatal("sda reported unmounted although mountpoints has entries")
	}
	if sda.Mountpoint != "/srv/data" {
		t.Fatalf("sda.Mountpoint = %q, want first mountpoints entry", sda.Mountpoint)
	}
	// A mounted partition on a non-removable disk must be flagged as system
	// even though the mountpoint (/srv/data) is not a classic system path —
	// it is still a running filesystem on the machine doing the listing.
	if !sda.IsSystem {
		t.Fatal("mounted non-removable sda not flagged IsSystem")
	}
}

// TestParseJSONFiltersVirtualDevices: ram/loop devices must not be offered
// as clone sources or targets.
func TestParseJSONFiltersVirtualDevices(t *testing.T) {
	disks, err := ParseJSON(lsblkJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if d.Name == "ram0" || d.Name == "loop0" {
			t.Fatalf("virtual device %s leaked into the disk list", d.Name)
		}
	}
	if len(disks) != 2 {
		t.Fatalf("got %d top-level disks, want 2 (sda, sdb)", len(disks))
	}
}

// TestParseJSONRemovableMountedNotSystem: a mounted USB stick (rm=true) is
// not a running system.
func TestParseJSONRemovableMountedNotSystem(t *testing.T) {
	disks, err := ParseJSON(lsblkJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if d.Name == "sdb" && d.IsSystem {
			t.Fatal("removable mounted USB flagged IsSystem")
		}
	}
}

const lsblkPseudoJSON = `{
  "blockdevices": [
    { "name": "sda", "size": "500107862016", "type": "disk", "rm": false },
    { "name": "dm-0", "size": "499570922496", "type": "disk", "rm": false },
    { "name": "md0", "size": "1000203091968", "type": "raid1", "rm": false },
    { "name": "nbd0", "size": "1073741824", "type": "disk", "rm": false },
    { "name": "sr0", "size": "1073741312", "type": "rom", "rm": true }
  ]
}`

// TestParseJSONFiltersPseudoDisks: device-mapper (dm-N reports TYPE "disk"
// in lsblk!), md RAID and nbd devices are assembled/synthetic — they must
// not be offered as dd sources or targets.
func TestParseJSONFiltersPseudoDisks(t *testing.T) {
	disks, err := ParseJSON(lsblkPseudoJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].Name != "sda" {
		names := make([]string, 0, len(disks))
		for _, d := range disks {
			names = append(names, d.Name)
		}
		t.Fatalf("disk list = %v, want [sda] only", names)
	}
}
